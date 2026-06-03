package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/diff"
	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/internal/merkle"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

func syncCmd() *cobra.Command {
	var (
		dryRun bool
		name   string
		rootID string
	)

	cmd := &cobra.Command{
		Use:   "sync [path]",
		Short: "Sync a directory to PufferFs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}

			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			return runSync(cfg, absDir, name, rootID, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be synced without syncing")
	cmd.Flags().StringVarP(&name, "name", "n", "", "Name alias for this root")
	cmd.Flags().StringVar(&rootID, "id", "", "Root ID to re-attach to")

	return cmd
}

func runSync(cfg *appconfig.Config, dir, name, rootID string, dryRun bool) error {
	// Default name to directory basename
	if name == "" {
		name = filepath.Base(dir)
	}

	if rootID == "" {
		if localRootID, err := findLocalRootID(name, dir); err == nil {
			rootID = localRootID
		} else if !dryRun {
			resolvedRootID, err := resolveOrCreateRoot(newAPIClient(cfg), name, dir)
			if err != nil {
				return err
			}
			rootID = resolvedRootID
		}
	}

	// Build Merkle tree (parallel file hashing)
	matcher := ignore.NewMatcher(dir)
	fmt.Printf("Building Merkle tree for %s...\n", dir)
	start := time.Now()
	currentTree, err := merkle.BuildTree(dir, matcher)
	if err != nil {
		return fmt.Errorf("building Merkle tree: %w", err)
	}
	fmt.Printf("Merkle tree built in %s (root hash: %s)\n", time.Since(start).Round(time.Millisecond), currentTree.Root.Hash[:20]+"...")

	// Extract flat state for backward compatibility with server
	currentState := currentTree.ToFileStateMap()

	// Load previous tree — try local first, then fall back to flat state
	var prevTree *merkle.Tree
	prevTree, err = loadLocalTree(rootID)
	if err != nil {
		// Fall back to flat state for backward compatibility
		var previousState map[string]models.FileState
		previousState, err = loadLocalState(rootID)
		if err != nil {
			if rootID != "" && !dryRun && cfg.Server.URL != "" {
				previousState, _ = loadRemoteState(newAPIClient(cfg), rootID)
			}
		}
		if previousState != nil {
			// Use flat diff as fallback
			result := diff.Compute(previousState, currentState)
			return runSyncWithResult(cfg, dir, name, rootID, dryRun, result, currentState, currentTree)
		}
		// No previous state at all — everything is new
		prevTree = &merkle.Tree{Root: &merkle.Node{IsDir: true, Children: map[string]*merkle.Node{}}}
	}

	// Merkle tree-based diff — only walks changed branches
	if prevTree.Root.Hash == currentTree.Root.Hash {
		fmt.Println("No changes detected (Merkle root hash matches).")
		return nil
	}

	treeChanges := merkle.Diff(prevTree, currentTree)
	fmt.Printf("Merkle diff found %d changed files (skipped unchanged subtrees)\n", len(treeChanges))

	// Convert Merkle changes to DiffResult for compatibility
	result := merkleChangesToDiffResult(treeChanges, prevTree, currentTree)

	return runSyncWithResult(cfg, dir, name, rootID, dryRun, result, currentState, currentTree)
}

// runSyncWithResult executes the sync with a pre-computed DiffResult.
func runSyncWithResult(cfg *appconfig.Config, dir, name, rootID string, dryRun bool, result models.DiffResult, currentState map[string]models.FileState, currentTree *merkle.Tree) error {
	var err error

	// Detect secrets
	secrets := diff.DetectSecrets(currentState)

	if dryRun {
		fmt.Print(diff.FormatDryRun(result, currentState, ignoredPatterns(), secrets))
		return nil
	}

	// Need a server connection for actual sync
	if cfg.Server.URL == "" {
		return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}

	client := newAPIClient(cfg)

	// Get or create root
	if rootID == "" {
		rootID, err = resolveOrCreateRoot(client, name, dir)
		if err != nil {
			return err
		}
	}

	// Upload changed files to S3 via the server
	changeCount := countChanges(result)
	fmt.Printf("Syncing %d changes to root %s...\n", changeCount, rootID)

	// Upload files that are ADDED or MODIFIED
	for _, change := range result.Changes {
		if change.Status == models.StatusAdded || change.Status == models.StatusModified {
			localPath := filepath.Join(dir, filepath.FromSlash(change.Path))
			if err := uploadFile(client, rootID, change.Path, localPath); err != nil {
				return fmt.Errorf("uploading %s: %w", change.Path, err)
			}
		}
	}

	// Build content proof from Merkle tree
	proof := currentTree.BuildContentProof()
	contentProof := &models.ContentProofData{
		FileHashes: proof.FileHashes,
		DirHashes:  proof.DirHashes,
		RootHash:   proof.RootHash,
	}

	// Send sync request with SimHash for index reuse + content proof
	syncReq := models.SyncRequest{
		RootID:       rootID,
		Changes:      filterChanges(result),
		State:        currentState,
		SimHash:      currentTree.SimHashHex(),
		ContentProof: contentProof,
	}

	respBody, err := client.post(fmt.Sprintf("/roots/%s/sync", rootID), syncReq)
	if err != nil {
		return fmt.Errorf("sync request: %w", err)
	}

	var syncResp models.SyncResponse
	if err := json.Unmarshal(respBody, &syncResp); err != nil {
		return fmt.Errorf("parsing sync response: %w", err)
	}

	// Save Merkle tree locally (replaces flat state)
	if err := saveLocalTree(rootID, currentTree); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save Merkle tree: %v\n", err)
	}

	// Also save flat state for backward compatibility
	if err := saveLocalState(rootID, currentState); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save local state: %v\n", err)
	}

	// Cache root info
	if err := saveRootMeta(rootID, name, dir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save root meta: %v\n", err)
	}

	fmt.Printf("Sync complete: %d files processed, %d chunks added, %d removed, %d moved\n",
		syncResp.FilesProcessed, syncResp.ChunksAdded, syncResp.ChunksRemoved, syncResp.ChunksMoved)

	return nil
}

// merkleChangesToDiffResult converts Merkle tree changes to the existing DiffResult format.
// Includes move detection by matching removed→added files with the same content hash.
func merkleChangesToDiffResult(changes []merkle.DiffChange, prev, curr *merkle.Tree) models.DiffResult {
	result := models.DiffResult{}

	// Separate added and removed for move detection
	var added, removed []merkle.DiffChange
	for _, c := range changes {
		switch c.Type {
		case "added":
			added = append(added, c)
		case "removed":
			removed = append(removed, c)
		case "modified":
			result.Changes = append(result.Changes, models.FileChange{
				Path:        c.Path,
				Status:      models.StatusModified,
				ContentHash: c.ContentHash,
				Size:        c.Size,
			})
			result.Stats.Modified++
		}
	}

	// Move detection: match removed→added by content hash
	usedRemoved := make(map[int]bool)
	usedAdded := make(map[int]bool)

	for ai, a := range added {
		for ri, r := range removed {
			if usedRemoved[ri] || usedAdded[ai] {
				continue
			}
			if a.ContentHash == r.ContentHash {
				result.Changes = append(result.Changes, models.FileChange{
					Path:        a.Path,
					Status:      models.StatusMoved,
					OldPath:     r.Path,
					ContentHash: a.ContentHash,
					Size:        a.Size,
				})
				result.Stats.Moved++
				usedRemoved[ri] = true
				usedAdded[ai] = true
				break
			}
		}
	}

	for ri, r := range removed {
		if !usedRemoved[ri] {
			result.Changes = append(result.Changes, models.FileChange{
				Path:        r.Path,
				Status:      models.StatusRemoved,
				ContentHash: r.ContentHash,
				Size:        r.Size,
			})
			result.Stats.Removed++
		}
	}

	for ai, a := range added {
		if !usedAdded[ai] {
			result.Changes = append(result.Changes, models.FileChange{
				Path:        a.Path,
				Status:      models.StatusAdded,
				ContentHash: a.ContentHash,
				Size:        a.Size,
			})
			result.Stats.Added++
		}
	}

	return result
}

func resolveOrCreateRoot(client *apiClient, name, sourcePath string) (string, error) {
	// Try to find existing root by name
	respBody, err := client.get("/roots")
	if err != nil {
		return "", fmt.Errorf("listing roots: %w", err)
	}

	var roots []models.RootMetadata
	if err := json.Unmarshal(respBody, &roots); err != nil {
		return "", err
	}

	for _, r := range roots {
		if r.Name == name {
			fmt.Printf("Using existing root: %s (%s)\n", r.Name, r.ID)
			return r.ID, nil
		}
	}

	// Create new root
	createReq := map[string]string{
		"name":        name,
		"source_path": sourcePath,
	}
	respBody, err = client.post("/roots", createReq)
	if err != nil {
		return "", fmt.Errorf("creating root: %w", err)
	}

	var root models.RootMetadata
	if err := json.Unmarshal(respBody, &root); err != nil {
		return "", err
	}

	fmt.Printf("Created root: %s (%s)\n", root.Name, root.ID)
	return root.ID, nil
}

func findLocalRootID(name, sourcePath string) (string, error) {
	rootsDir := filepath.Join(appconfig.DefaultConfigDir(), "roots")
	entries, err := os.ReadDir(rootsDir)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(rootsDir, entry.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			SourcePath string `json:"source_path"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if meta.Name == name && meta.SourcePath == sourcePath {
			return meta.ID, nil
		}
	}

	return "", fmt.Errorf("local root metadata not found")
}

func uploadFile(client *apiClient, rootID, relPath, localPath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("/roots/%s/upload?path=%s", rootID, url.QueryEscape(relPath))
	_, err = client.postRaw(url, data, "application/octet-stream")
	return err
}

func loadRemoteState(client *apiClient, rootID string) (map[string]models.FileState, error) {
	respBody, err := client.get(fmt.Sprintf("/roots/%s/state", rootID))
	if err != nil {
		return nil, err
	}
	var state map[string]models.FileState
	if err := json.Unmarshal(respBody, &state); err != nil {
		return nil, err
	}
	return state, nil
}

func loadLocalTree(rootID string) (*merkle.Tree, error) {
	if rootID == "" {
		return nil, fmt.Errorf("no root ID")
	}
	path := filepath.Join(appconfig.RootDir(rootID), "tree.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tree merkle.Tree
	return &tree, json.Unmarshal(data, &tree)
}

func saveLocalTree(rootID string, tree *merkle.Tree) error {
	dir := appconfig.RootDir(rootID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(tree)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "tree.json"), data, 0o600)
}

func loadLocalState(rootID string) (map[string]models.FileState, error) {
	if rootID == "" {
		return nil, fmt.Errorf("no root ID")
	}
	path := filepath.Join(appconfig.RootDir(rootID), "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state map[string]models.FileState
	return state, json.Unmarshal(data, &state)
}

func saveLocalState(rootID string, state map[string]models.FileState) error {
	dir := appconfig.RootDir(rootID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600)
}

func saveRootMeta(rootID, name, sourcePath string) error {
	dir := appconfig.RootDir(rootID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	meta := map[string]string{
		"id":          rootID,
		"name":        name,
		"source_path": sourcePath,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o600)
}

func filterChanges(result models.DiffResult) []models.FileChange {
	var changes []models.FileChange
	for _, c := range result.Changes {
		if c.Status != models.StatusUnchanged {
			changes = append(changes, c)
		}
	}
	return changes
}

func countChanges(result models.DiffResult) int {
	count := 0
	for _, c := range result.Changes {
		if c.Status != models.StatusUnchanged {
			count++
		}
	}
	return count
}

func ignoredPatterns() []string {
	patterns := []string{".git/", "node_modules/", ".venv/", "__pycache__/", ".DS_Store"}
	sort.Strings(patterns)
	return patterns
}
