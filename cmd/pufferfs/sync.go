package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
		dryRun  bool
		name    string
		rootID  string
		scope   string
		follow  bool
		options followOptions
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

			if follow {
				if dryRun {
					return fmt.Errorf("--follow cannot be combined with --dry-run")
				}
				if cfg.Server.URL == "" {
					return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
				}
				return runFollow(cfg, absDir, name, rootID, options)
			}
			return runSync(cfg, absDir, name, rootID, scope, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be synced without syncing")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Continuously sync when files change")
	cmd.Flags().StringVarP(&name, "name", "n", "", "Name alias for this root")
	cmd.Flags().StringVar(&rootID, "id", "", "Root ID to re-attach to")
	cmd.Flags().StringVar(&scope, "scope", "org", "Root scope to create when missing: org or user")
	addFollowFlags(cmd, &options)

	return cmd
}

func runSync(cfg *appconfig.Config, dir, name, rootID, rootScope string, dryRun bool) error {
	// Default name to directory basename
	if name == "" {
		name = filepath.Base(dir)
	}

	var (
		client     *apiClient
		localMeta  *rootMeta
		remoteRoot *models.RootMetadata
	)
	if rootID == "" {
		if meta, err := findLocalRootMeta(name, dir); err == nil {
			rootID = meta.ID
			localMeta = meta
		} else if !dryRun {
			client = newAPIClient(cfg)
			resolvedRoot, err := resolveOrCreateRoot(client, name, dir, rootScope)
			if err != nil {
				return err
			}
			rootID = resolvedRoot.ID
			remoteRoot = resolvedRoot
		}
	} else if meta, err := loadRootMeta(rootID); err == nil {
		localMeta = meta
	}
	if !dryRun && cfg.Server.URL != "" && rootID != "" && remoteRoot == nil {
		if client == nil {
			client = newAPIClient(cfg)
		}
		var err error
		remoteRoot, err = loadRemoteRoot(client, rootID)
		if err != nil {
			return fmt.Errorf("loading remote root metadata: %w", err)
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

	baseGenerationID, baseGenerationSeq := syncBaseFromMeta(localMeta, remoteRoot)
	useLocalCache := localCacheMatchesRemote(localMeta, remoteRoot)
	if !useLocalCache {
		fmt.Println("Remote generation changed; diffing against remote state.")
		previousState, err := loadRemoteState(client, rootID)
		if err != nil {
			return fmt.Errorf("loading remote state: %w", err)
		}
		result := diff.Compute(previousState, currentState)
		if countChanges(result) == 0 {
			fmt.Println("No changes detected (remote state matches local filesystem).")
			return saveLocalSyncCache(rootID, name, dir, currentState, currentTree, baseGenerationID, baseGenerationSeq)
		}
		return runSyncWithConflictRetry(cfg, dir, name, rootID, rootScope, dryRun, result, currentState, currentTree, baseGenerationID, baseGenerationSeq)
	}

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
			return runSyncWithConflictRetry(cfg, dir, name, rootID, rootScope, dryRun, result, currentState, currentTree, baseGenerationID, baseGenerationSeq)
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

	return runSyncWithConflictRetry(cfg, dir, name, rootID, rootScope, dryRun, result, currentState, currentTree, baseGenerationID, baseGenerationSeq)
}

// runSyncWithResult executes the sync with a pre-computed DiffResult.
func runSyncWithResult(cfg *appconfig.Config, dir, name, rootID, rootScope string, dryRun bool, result models.DiffResult, currentState map[string]models.FileState, currentTree *merkle.Tree, baseGenerationID string, baseGenerationSeq int64) error {
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
		resolvedRoot, err := resolveOrCreateRoot(client, name, dir, rootScope)
		if err != nil {
			return err
		}
		rootID = resolvedRoot.ID
		baseGenerationID = resolvedRoot.VisibleGenerationID
		baseGenerationSeq = resolvedRoot.VisibleGenerationSeq
	}

	// Upload changed files to S3 via the server
	changeCount := countChanges(result)
	fmt.Printf("Syncing %d changes to root %s...\n", changeCount, rootID)

	changes := withAbsolutePaths(dir, filterChanges(result))
	manifestRef, err := uploadChangedFiles(client, rootID, dir, changes)
	if err != nil {
		return err
	}

	// Build content proof from Merkle tree
	proof := currentTree.BuildContentProof()
	contentProof := &models.ContentProofData{
		FileHashes: proof.FileHashes,
		DirHashes:  proof.DirHashes,
		RootHash:   proof.RootHash,
	}
	stateRef, err := uploadRootState(client, rootID, currentState)
	if err != nil {
		return fmt.Errorf("uploading root state: %w", err)
	}

	// Send sync request with SimHash for index reuse + content proof
	syncReq := models.SyncRequest{
		ProtocolVersion:   models.SyncProtocolVersion,
		RootID:            rootID,
		BaseGenerationID:  baseGenerationID,
		BaseGenerationSeq: baseGenerationSeq,
		Changes:           changes,
		StateRef:          stateRef,
		SimHash:           currentTree.SimHashHex(),
		ContentProof:      contentProof,
		ManifestRef:       manifestRef,
	}

	respBody, err := client.post(fmt.Sprintf("/roots/%s/sync?async=true", rootID), syncReq)
	if err != nil {
		if conflict, ok := syncConflictFromError(err); ok {
			return conflict
		}
		return fmt.Errorf("sync request: %w", err)
	}

	var syncResp models.SyncResponse
	if err := json.Unmarshal(respBody, &syncResp); err != nil {
		return fmt.Errorf("parsing sync response: %w", err)
	}
	if syncResp.SyncJobID != "" {
		if err := pollSyncJob(client, rootID, syncResp.SyncJobID); err != nil {
			return err
		}
	}

	saveLocalSyncCacheWarnings(rootID, name, dir, currentState, currentTree, syncResp.GenerationID, syncResp.GenerationSeq)

	fmt.Printf("Sync complete: %d files processed, %d chunks added, %d removed, %d moved\n",
		syncResp.FilesProcessed, syncResp.ChunksAdded, syncResp.ChunksRemoved, syncResp.ChunksMoved)

	return nil
}

type syncConflictError struct {
	models.SyncConflictResponse
}

func (e *syncConflictError) Error() string {
	return fmt.Sprintf("remote generation changed from %q/%d to %q/%d", e.ClientBaseGenerationID, e.ClientBaseGenerationSeq, e.CurrentGenerationID, e.CurrentGenerationSeq)
}

func syncConflictFromError(err error) (*syncConflictError, bool) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		return nil, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(apiErr.Body, &raw) != nil {
		return nil, false
	}
	if _, ok := raw["current_generation_id"]; !ok {
		return nil, false
	}
	var resp models.SyncConflictResponse
	if json.Unmarshal(apiErr.Body, &resp) != nil || resp.Error == "" {
		return nil, false
	}
	return &syncConflictError{SyncConflictResponse: resp}, true
}

func runSyncWithConflictRetry(cfg *appconfig.Config, dir, name, rootID, rootScope string, dryRun bool, result models.DiffResult, currentState map[string]models.FileState, currentTree *merkle.Tree, baseGenerationID string, baseGenerationSeq int64) error {
	err := runSyncWithResult(cfg, dir, name, rootID, rootScope, dryRun, result, currentState, currentTree, baseGenerationID, baseGenerationSeq)
	var conflict *syncConflictError
	if !errors.As(err, &conflict) {
		return err
	}
	if dryRun {
		return err
	}

	fmt.Println("Remote generation changed during sync; reconciling against latest remote state.")
	client := newAPIClient(cfg)
	previousState, loadErr := loadRemoteState(client, rootID)
	if loadErr != nil {
		return fmt.Errorf("loading remote state after sync conflict: %w", loadErr)
	}
	reconciled := diff.Compute(previousState, currentState)
	if countChanges(reconciled) == 0 {
		fmt.Println("No changes detected (remote state matches local filesystem).")
		return saveLocalSyncCache(rootID, name, dir, currentState, currentTree, conflict.CurrentGenerationID, conflict.CurrentGenerationSeq)
	}
	return runSyncWithResult(cfg, dir, name, rootID, rootScope, dryRun, reconciled, currentState, currentTree, conflict.CurrentGenerationID, conflict.CurrentGenerationSeq)
}

func pollSyncJob(client *apiClient, rootID, jobID string) error {
	fmt.Printf("Sync job %s started; polling until committed...\n", jobID)
	deadline := time.Now().Add(syncPollTimeout())
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for sync job %s", jobID)
		}
		body, err := client.get(fmt.Sprintf("/roots/%s/sync/status?job_id=%s", rootID, url.QueryEscape(jobID)))
		if err != nil {
			return fmt.Errorf("polling sync job: %w", err)
		}
		var job models.SyncJob
		if err := json.Unmarshal(body, &job); err != nil {
			return fmt.Errorf("parsing sync job status: %w", err)
		}
		switch job.Status {
		case "completed":
			return nil
		case "failed":
			return fmt.Errorf("sync job failed: %s", string(job.Errors))
		default:
			fmt.Printf("Sync status: %s (%d/%d files)\n", job.Status, job.Processed, job.TotalFiles)
			time.Sleep(2 * time.Second)
		}
	}
}

func syncPollTimeout() time.Duration {
	const defaultTimeout = 35 * time.Minute
	raw := os.Getenv("PUFFERFS_SYNC_POLL_TIMEOUT")
	if raw == "" {
		return defaultTimeout
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < time.Second {
		return defaultTimeout
	}
	return timeout
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
			if a.ContentHash == r.ContentHash && a.Size <= moveReuseMaxBytes() {
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

func moveReuseMaxBytes() int64 {
	const defaultBytes = 64 << 20
	raw := os.Getenv("PUFFERFS_MOVE_REUSE_MAX_BYTES")
	if raw == "" {
		return defaultBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return defaultBytes
	}
	return value
}

func resolveOrCreateRoot(client *apiClient, name, sourcePath, rootScope string) (*models.RootMetadata, error) {
	// Try to find existing root by name
	respBody, err := client.get("/roots")
	if err != nil {
		return nil, fmt.Errorf("listing roots: %w", err)
	}

	var roots []models.RootMetadata
	if err := json.Unmarshal(respBody, &roots); err != nil {
		return nil, err
	}

	for _, r := range roots {
		if r.Name == name {
			fmt.Printf("Using existing root: %s (%s)\n", r.Name, r.ID)
			return &r, nil
		}
	}

	// Create new root
	createReq := map[string]string{
		"name":        name,
		"source_path": sourcePath,
		"scope":       rootScope,
	}
	respBody, err = client.post("/roots", createReq)
	if err != nil {
		return nil, fmt.Errorf("creating root: %w", err)
	}

	var root models.RootMetadata
	if err := json.Unmarshal(respBody, &root); err != nil {
		return nil, err
	}

	fmt.Printf("Created root: %s (%s)\n", root.Name, root.ID)
	return &root, nil
}

type rootMeta struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	SourcePath    string `json:"source_path"`
	GenerationID  string `json:"generation_id"`
	GenerationSeq int64  `json:"generation_seq"`
}

func findLocalRootMeta(name, sourcePath string) (*rootMeta, error) {
	rootsDir := filepath.Join(appconfig.DefaultConfigDir(), "roots")
	entries, err := os.ReadDir(rootsDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := loadRootMeta(entry.Name())
		if err != nil {
			continue
		}
		if meta.Name == name && meta.SourcePath == sourcePath {
			return meta, nil
		}
	}

	return nil, fmt.Errorf("local root metadata not found")
}

type bundleManifestEntry struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	Size        int64  `json:"size"`
	BundleKey   string `json:"bundle_key,omitempty"`
	ObjectKey   string `json:"object_key,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Length      int64  `json:"length,omitempty"`
}

func uploadChangedFiles(client *apiClient, rootID, dir string, changes []models.FileChange) (string, error) {
	smallLimit := uploadBundleSmallFileLimit()
	maxBundleBytes := uploadBundleMaxBytes()
	var manifest []bundleManifestEntry
	var bundle bytes.Buffer
	bundleID := fmt.Sprintf("%d", time.Now().UnixNano())
	bundleIndex := 0
	var bundleKey string

	flushBundle := func() error {
		if bundle.Len() == 0 {
			return nil
		}
		key, err := uploadBundle(client, rootID, fmt.Sprintf("%s-%06d", bundleID, bundleIndex), bundle.Bytes(), "application/octet-stream")
		if err != nil {
			return err
		}
		for i := range changes {
			if changes[i].SourceKey == "__pending_bundle__" {
				changes[i].SourceKey = key
			}
		}
		for i := range manifest {
			if manifest[i].BundleKey == "__pending_bundle__" {
				manifest[i].BundleKey = key
			}
		}
		bundle.Reset()
		bundleIndex++
		bundleKey = ""
		return nil
	}

	for i := range changes {
		change := &changes[i]
		if change.Status != models.StatusAdded && change.Status != models.StatusModified {
			continue
		}
		localPath := filepath.Join(dir, filepath.FromSlash(change.Path))
		info, err := os.Stat(localPath)
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", change.Path, err)
		}
		if info.Size() == 0 || info.Size() > smallLimit {
			key, err := uploadFile(client, rootID, change.Path, localPath)
			if err != nil {
				return "", fmt.Errorf("uploading %s: %w", change.Path, err)
			}
			change.SourceKey = key
			change.SourceOffset = 0
			change.SourceLength = info.Size()
			manifest = append(manifest, bundleManifestEntry{
				Path:        change.Path,
				ContentHash: change.ContentHash,
				Size:        info.Size(),
				ObjectKey:   key,
				Length:      info.Size(),
			})
			continue
		}

		data, err := os.ReadFile(localPath)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", change.Path, err)
		}
		if bundle.Len() > 0 && int64(bundle.Len()+len(data)) > maxBundleBytes {
			if err := flushBundle(); err != nil {
				return "", err
			}
		}
		if bundleKey == "" {
			bundleKey = "__pending_bundle__"
		}
		offset := int64(bundle.Len())
		if _, err := bundle.Write(data); err != nil {
			return "", err
		}
		change.SourceKey = bundleKey
		change.SourceOffset = offset
		change.SourceLength = int64(len(data))
		manifest = append(manifest, bundleManifestEntry{
			Path:        change.Path,
			ContentHash: change.ContentHash,
			Size:        int64(len(data)),
			BundleKey:   bundleKey,
			Offset:      offset,
			Length:      int64(len(data)),
		})
	}
	if err := flushBundle(); err != nil {
		return "", err
	}
	if len(manifest) == 0 {
		return "", nil
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	key, err := uploadBundle(client, rootID, bundleID+"-manifest", manifestBytes, "application/json")
	if err != nil {
		return "", fmt.Errorf("uploading source manifest: %w", err)
	}
	return key, nil
}

func uploadFile(client *apiClient, rootID, relPath, localPath string) (string, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	url := fmt.Sprintf("/roots/%s/upload?path=%s", rootID, url.QueryEscape(relPath))
	respBody, err := client.postStream(url, file, "application/octet-stream")
	if err != nil {
		return "", err
	}
	var resp struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", err
	}
	return resp.Key, nil
}

func uploadBundle(client *apiClient, rootID, bundleID string, data []byte, contentType string) (string, error) {
	url := fmt.Sprintf("/roots/%s/upload-bundle?bundle_id=%s", rootID, url.QueryEscape(bundleID))
	respBody, err := client.postRaw(url, data, contentType)
	if err != nil {
		return "", err
	}
	var resp struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", err
	}
	return resp.Key, nil
}

func uploadRootState(client *apiClient, rootID string, state map[string]models.FileState) (string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(state); err != nil {
		_ = gz.Close()
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return uploadBundle(client, rootID, fmt.Sprintf("state-%d.json.gz", time.Now().UnixNano()), buf.Bytes(), "application/gzip")
}

func uploadBundleSmallFileLimit() int64 {
	const defaultBytes = 8 << 20
	raw := os.Getenv("PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES")
	if raw == "" {
		return defaultBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		return defaultBytes
	}
	return value
}

func uploadBundleMaxBytes() int64 {
	const defaultBytes = 256 << 20
	raw := os.Getenv("PUFFERFS_UPLOAD_BUNDLE_MAX_BYTES")
	if raw == "" {
		return defaultBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		return defaultBytes
	}
	return value
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

func loadRemoteRoot(client *apiClient, rootID string) (*models.RootMetadata, error) {
	respBody, err := client.get(fmt.Sprintf("/roots/%s", rootID))
	if err != nil {
		return nil, err
	}
	var root models.RootMetadata
	if err := json.Unmarshal(respBody, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

func syncBaseFromMeta(localMeta *rootMeta, remoteRoot *models.RootMetadata) (string, int64) {
	if remoteRoot != nil {
		return remoteRoot.VisibleGenerationID, remoteRoot.VisibleGenerationSeq
	}
	if localMeta != nil {
		return localMeta.GenerationID, localMeta.GenerationSeq
	}
	return "", 0
}

func localCacheMatchesRemote(localMeta *rootMeta, remoteRoot *models.RootMetadata) bool {
	if remoteRoot == nil {
		return true
	}
	if remoteRoot.VisibleGenerationID == "" {
		return true
	}
	if localMeta == nil {
		return false
	}
	if localMeta.GenerationID != remoteRoot.VisibleGenerationID {
		return false
	}
	if localMeta.GenerationSeq != 0 && localMeta.GenerationSeq != remoteRoot.VisibleGenerationSeq {
		return false
	}
	return true
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

func loadRootMeta(rootID string) (*rootMeta, error) {
	if rootID == "" {
		return nil, fmt.Errorf("no root ID")
	}
	data, err := os.ReadFile(filepath.Join(appconfig.RootDir(rootID), "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta rootMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if meta.ID == "" {
		meta.ID = rootID
	}
	return &meta, nil
}

func saveRootMeta(rootID, name, sourcePath, generationID string, generationSeq int64) error {
	dir := appconfig.RootDir(rootID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	meta := rootMeta{
		ID:            rootID,
		Name:          name,
		SourcePath:    sourcePath,
		GenerationID:  generationID,
		GenerationSeq: generationSeq,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o600)
}

func saveLocalSyncCache(rootID, name, dir string, currentState map[string]models.FileState, currentTree *merkle.Tree, generationID string, generationSeq int64) error {
	if err := saveLocalTree(rootID, currentTree); err != nil {
		return fmt.Errorf("saving Merkle tree: %w", err)
	}
	if err := saveLocalState(rootID, currentState); err != nil {
		return fmt.Errorf("saving local state: %w", err)
	}
	if err := saveRootMeta(rootID, name, dir, generationID, generationSeq); err != nil {
		return fmt.Errorf("saving root meta: %w", err)
	}
	return nil
}

func saveLocalSyncCacheWarnings(rootID, name, dir string, currentState map[string]models.FileState, currentTree *merkle.Tree, generationID string, generationSeq int64) {
	if err := saveLocalTree(rootID, currentTree); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save Merkle tree: %v\n", err)
	}
	if err := saveLocalState(rootID, currentState); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save local state: %v\n", err)
	}
	if err := saveRootMeta(rootID, name, dir, generationID, generationSeq); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save root meta: %v\n", err)
	}
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

func withAbsolutePaths(rootDir string, changes []models.FileChange) []models.FileChange {
	out := make([]models.FileChange, len(changes))
	for i, change := range changes {
		out[i] = change
		out[i].AbsolutePath = filepath.Join(rootDir, filepath.FromSlash(change.Path))
	}
	return out
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
