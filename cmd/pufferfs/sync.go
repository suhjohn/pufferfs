package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/diff"
	"github.com/pufferfs/pufferfs/internal/ignore"
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

	// Scan directory
	matcher := ignore.NewMatcher(dir)
	currentState, err := diff.Scan(dir, matcher)
	if err != nil {
		return fmt.Errorf("scanning directory: %w", err)
	}

	// Load previous state — try local first, then fall back to server
	previousState, err := loadLocalState(rootID)
	if err != nil {
		if rootID != "" && !dryRun && cfg.Server.URL != "" {
			previousState, _ = loadRemoteState(newAPIClient(cfg), rootID)
		}
		if previousState == nil {
			previousState = make(map[string]models.FileState)
		}
	}

	// Compute diff
	result := diff.Compute(previousState, currentState)

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
	fmt.Printf("Syncing %d changes to root %s...\n", countChanges(result), rootID)

	// Upload files that are ADDED or MODIFIED
	for _, change := range result.Changes {
		if change.Status == models.StatusAdded || change.Status == models.StatusModified {
			localPath := filepath.Join(dir, filepath.FromSlash(change.Path))
			if err := uploadFile(client, rootID, change.Path, localPath); err != nil {
				return fmt.Errorf("uploading %s: %w", change.Path, err)
			}
		}
	}

	// Send sync request
	syncReq := models.SyncRequest{
		RootID:  rootID,
		Changes: filterChanges(result),
		State:   currentState,
	}

	respBody, err := client.post(fmt.Sprintf("/roots/%s/sync", rootID), syncReq)
	if err != nil {
		return fmt.Errorf("sync request: %w", err)
	}

	var syncResp models.SyncResponse
	if err := json.Unmarshal(respBody, &syncResp); err != nil {
		return fmt.Errorf("parsing sync response: %w", err)
	}

	// Save state locally
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

func uploadFile(client *apiClient, rootID, relPath, localPath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("/roots/%s/upload?path=%s", rootID, relPath)
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
