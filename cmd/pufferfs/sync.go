package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
		dryRun     bool
		name       string
		rootID     string
		scope      string
		follow     bool
		jsonOut    bool
		background bool
		detach     bool
		options    followOptions
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
				if jsonOut {
					return fmt.Errorf("--json cannot be combined with --follow")
				}
				if dryRun {
					return fmt.Errorf("--follow cannot be combined with --dry-run")
				}
				if background || detach {
					return fmt.Errorf("--background/--detach cannot be combined with --follow")
				}
				if cfg.Server.URL == "" {
					return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
				}
				return runFollow(cfg, absDir, name, rootID, options)
			}
			if dryRun && (background || detach) {
				return fmt.Errorf("--background/--detach cannot be combined with --dry-run")
			}
			log := syncLogWriter(jsonOut)
			result, err := runSync(cfg, absDir, name, rootID, scope, dryRun, !background && !detach, log)
			if err != nil {
				return err
			}
			if jsonOut {
				return writePrettyJSON(os.Stdout, result)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be synced without syncing")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Continuously sync when files change")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print sync result as JSON")
	cmd.Flags().BoolVar(&background, "background", false, "Start sync and return immediately with a sync job ID")
	cmd.Flags().BoolVar(&detach, "detach", false, "Alias for --background")
	cmd.Flags().StringVarP(&name, "name", "n", "", "Name alias for this root")
	cmd.Flags().StringVar(&rootID, "id", "", "Root ID to re-attach to")
	cmd.Flags().StringVar(&scope, "scope", "org", "Root scope to create when missing: org or user")
	addFollowFlags(cmd, &options)
	cmd.AddCommand(syncStatusCmd(), syncJobsCmd(), syncWaitCmd())

	return cmd
}

type syncCommandResult struct {
	Status         string              `json:"status"`
	RootID         string              `json:"root_id,omitempty"`
	RootName       string              `json:"root_name,omitempty"`
	SourcePath     string              `json:"source_path,omitempty"`
	DryRun         bool                `json:"dry_run,omitempty"`
	Changes        int                 `json:"changes"`
	Stats          *models.DiffStats   `json:"stats,omitempty"`
	FileChanges    []models.FileChange `json:"file_changes,omitempty"`
	Ignored        []string            `json:"ignored_patterns,omitempty"`
	Secrets        []string            `json:"secrets,omitempty"`
	SyncJobID      string              `json:"sync_job_id,omitempty"`
	GenerationID   string              `json:"generation_id,omitempty"`
	GenerationSeq  int64               `json:"generation_seq,omitempty"`
	ChunksAdded    int                 `json:"chunks_added,omitempty"`
	ChunksRemoved  int                 `json:"chunks_removed,omitempty"`
	ChunksMoved    int                 `json:"chunks_moved,omitempty"`
	FilesProcessed int                 `json:"files_processed,omitempty"`
}

func syncLogWriter(jsonOutput bool) io.Writer {
	if jsonOutput {
		return os.Stderr
	}
	return os.Stdout
}

func syncStatusCmd() *cobra.Command {
	var (
		rootRef  string
		jobID    string
		jsonOut  bool
		watch    bool
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "status [root-id-or-name]",
		Short: "Show sync job status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			client, rootID, err := syncCommandClientAndRoot(cfg, rootRef, args)
			if err != nil {
				return err
			}
			if watch {
				return watchSyncStatus(client, rootID, jobID, interval, jsonOut)
			}
			job, raw, err := getSyncJob(client, rootID, jobID)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeRawJSONLine(os.Stdout, raw)
			}
			printSyncJob(os.Stdout, job)
			return nil
		},
	}
	cmd.Flags().StringVar(&rootRef, "root", "", "Root ID or name (defaults to the root for the current directory)")
	cmd.Flags().StringVar(&jobID, "job-id", "", "Specific sync job ID (defaults to latest)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print raw sync job JSON")
	cmd.Flags().BoolVar(&watch, "watch", false, "Poll until the sync job completes or fails")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Polling interval for --watch")
	return cmd
}

func syncJobsCmd() *cobra.Command {
	var (
		rootRef string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "jobs [root-id-or-name]",
		Short: "List recent sync jobs for a root",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			client, rootID, err := syncCommandClientAndRoot(cfg, rootRef, args)
			if err != nil {
				return err
			}
			raw, err := client.get(fmt.Sprintf("/roots/%s/sync/jobs", url.PathEscape(rootID)))
			if err != nil {
				return fmt.Errorf("listing sync jobs: %w", err)
			}
			if jsonOut {
				return writeRawJSONLine(os.Stdout, raw)
			}
			var jobs []models.SyncJob
			if err := json.Unmarshal(raw, &jobs); err != nil {
				return fmt.Errorf("parsing sync jobs: %w", err)
			}
			printSyncJobs(os.Stdout, jobs)
			return nil
		},
	}
	cmd.Flags().StringVar(&rootRef, "root", "", "Root ID or name (defaults to the root for the current directory)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print raw sync jobs JSON")
	return cmd
}

func syncWaitCmd() *cobra.Command {
	var (
		rootRef  string
		jobID    string
		jsonOut  bool
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "wait [root-id-or-name]",
		Short: "Wait for a sync job to complete",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			client, rootID, err := syncCommandClientAndRoot(cfg, rootRef, args)
			if err != nil {
				return err
			}
			job, err := waitForSyncJob(client, rootID, jobID, interval, !jsonOut)
			if err != nil {
				return err
			}
			if jsonOut {
				return writePrettyJSON(os.Stdout, job)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rootRef, "root", "", "Root ID or name (defaults to the root for the current directory)")
	cmd.Flags().StringVar(&jobID, "job-id", "", "Specific sync job ID (defaults to latest)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print final sync job JSON")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Polling interval")
	return cmd
}

func syncCommandClientAndRoot(cfg *appconfig.Config, rootRef string, args []string) (*apiClient, string, error) {
	if cfg.Server.URL == "" {
		return nil, "", fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}
	if rootRef != "" && len(args) > 0 {
		return nil, "", fmt.Errorf("root specified both as argument and --root")
	}
	if rootRef == "" && len(args) > 0 {
		rootRef = args[0]
	}
	if rootRef == "" {
		var err error
		rootRef, err = detectRootFromCwd()
		if err != nil {
			return nil, "", fmt.Errorf("could not detect root from cwd; use --root to specify: %w", err)
		}
	}
	client := newAPIClient(cfg)
	rootID := rootRef
	if !isUUID(rootID) {
		resolvedID, err := resolveRootName(client, rootID)
		if err != nil {
			return nil, "", fmt.Errorf("resolving root %q: %w", rootRef, err)
		}
		rootID = resolvedID
	}
	return client, rootID, nil
}

func getSyncJob(client *apiClient, rootID, jobID string) (*models.SyncJob, []byte, error) {
	path := fmt.Sprintf("/roots/%s/sync/status", url.PathEscape(rootID))
	if jobID != "" {
		path += "?job_id=" + url.QueryEscape(jobID)
	}
	raw, err := client.get(path)
	if err != nil {
		return nil, nil, fmt.Errorf("getting sync status: %w", err)
	}
	var job models.SyncJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, nil, fmt.Errorf("parsing sync status: %w", err)
	}
	return &job, raw, nil
}

func watchSyncStatus(client *apiClient, rootID, jobID string, interval time.Duration, jsonOut bool) error {
	deadline := time.Now().Add(syncPollTimeout())
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for sync job status")
		}
		job, raw, err := getSyncJob(client, rootID, jobID)
		if err != nil {
			return err
		}
		if jsonOut {
			if err := writeRawJSONLine(os.Stdout, raw); err != nil {
				return err
			}
		} else {
			printSyncJob(os.Stdout, job)
		}
		if syncJobTerminal(job.Status) {
			if job.Status == "failed" {
				return fmt.Errorf("sync job failed: %s", string(job.Errors))
			}
			return nil
		}
		time.Sleep(normalizeSyncPollInterval(interval))
	}
}

func waitForSyncJob(client *apiClient, rootID, jobID string, interval time.Duration, logProgress bool) (*models.SyncJob, error) {
	deadline := time.Now().Add(syncPollTimeout())
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for sync job")
		}
		job, _, err := getSyncJob(client, rootID, jobID)
		if err != nil {
			return nil, err
		}
		switch job.Status {
		case "completed":
			if logProgress {
				fmt.Fprintf(os.Stdout, "Sync job %s completed (%d/%d files).\n", job.ID, job.Processed, job.TotalFiles)
			}
			return job, nil
		case "failed":
			return job, fmt.Errorf("sync job failed: %s", string(job.Errors))
		default:
			if logProgress {
				fmt.Fprintf(os.Stdout, "Sync status: %s (%d/%d files)\n", job.Status, job.Processed, job.TotalFiles)
			}
			time.Sleep(normalizeSyncPollInterval(interval))
		}
	}
}

func syncJobTerminal(status string) bool {
	return status == "completed" || status == "failed"
}

func normalizeSyncPollInterval(interval time.Duration) time.Duration {
	if interval < 100*time.Millisecond {
		return 2 * time.Second
	}
	return interval
}

func printSyncJob(w io.Writer, job *models.SyncJob) {
	fmt.Fprintf(w, "sync_job_id: %s\n", job.ID)
	fmt.Fprintf(w, "root_id: %s\n", job.RootID)
	fmt.Fprintf(w, "status: %s\n", job.Status)
	fmt.Fprintf(w, "progress: %d/%d files\n", job.Processed, job.TotalFiles)
	fmt.Fprintf(w, "started_at: %s\n", job.StartedAt.Format(time.RFC3339))
	if job.FinishedAt != nil {
		fmt.Fprintf(w, "finished_at: %s\n", job.FinishedAt.Format(time.RFC3339))
	}
	if len(job.Errors) > 0 && string(job.Errors) != "null" {
		fmt.Fprintf(w, "errors: %s\n", string(job.Errors))
	}
}

func printSyncJobs(w io.Writer, jobs []models.SyncJob) {
	if len(jobs) == 0 {
		fmt.Fprintln(w, "No sync jobs found.")
		return
	}
	for i := range jobs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		printSyncJob(w, &jobs[i])
	}
}

func runSync(cfg *appconfig.Config, dir, name, rootID, rootScope string, dryRun, waitForCompletion bool, log io.Writer) (*syncCommandResult, error) {
	if log == nil {
		log = os.Stdout
	}
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
			resolvedRoot, err := resolveOrCreateRoot(client, name, dir, rootScope, log)
			if err != nil {
				return nil, err
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
			return nil, fmt.Errorf("loading remote root metadata: %w", err)
		}
	}
	baseGenerationID, baseGenerationSeq := syncBaseFromMeta(localMeta, remoteRoot)
	useLocalCache := localCacheMatchesRemote(localMeta, remoteRoot)
	var hashCache map[string]models.FileState
	if useLocalCache && rootID != "" {
		hashCache, _ = loadLocalState(rootID)
	}

	policy := ignore.PolicyPatternSet{}
	if cfg.Server.URL != "" && rootID != "" {
		if client == nil {
			client = newAPIClient(cfg)
		}
		effectivePolicy, err := fetchEffectiveIgnorePolicy(client)
		if err != nil {
			if !dryRun {
				return nil, fmt.Errorf("loading ignore policy: %w", err)
			}
			fmt.Fprintf(log, "Warning: could not load server ignore policy: %v\n", err)
		} else {
			policy.OrgPatterns = effectivePolicy.OrgPatterns
			policy.UserPatterns = effectivePolicy.UserPatterns
		}
	}

	// Build Merkle tree (parallel file hashing)
	matcher := ignore.NewMatcherWithPolicy(dir, policy)
	fmt.Fprintf(log, "Building Merkle tree for %s...\n", dir)
	start := time.Now()
	currentTree, err := merkle.BuildTreeWithStateCache(dir, matcher, hashCache)
	if err != nil {
		return nil, fmt.Errorf("building Merkle tree: %w", err)
	}
	fmt.Fprintf(log, "Merkle tree built in %s (root hash: %s)\n", time.Since(start).Round(time.Millisecond), currentTree.Root.Hash[:20]+"...")

	// Extract flat state for backward compatibility with server
	currentState := currentTree.ToFileStateMap()

	if !useLocalCache {
		fmt.Fprintln(log, "Remote generation changed; diffing against remote state.")
		previousState, err := loadRemoteState(client, rootID)
		if err != nil {
			return nil, fmt.Errorf("loading remote state: %w", err)
		}
		result := diff.Compute(previousState, currentState)
		if countChanges(result) == 0 {
			fmt.Fprintln(log, "No changes detected (remote state matches local filesystem).")
			if err := saveLocalSyncCache(rootID, name, dir, currentState, currentTree, baseGenerationID, baseGenerationSeq); err != nil {
				return nil, err
			}
			return unchangedSyncResult(rootID, name, dir, baseGenerationID, baseGenerationSeq), nil
		}
		return runSyncWithConflictRetry(cfg, dir, name, rootID, rootScope, dryRun, waitForCompletion, result, currentState, currentTree, baseGenerationID, baseGenerationSeq, policy, log)
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
			return runSyncWithConflictRetry(cfg, dir, name, rootID, rootScope, dryRun, waitForCompletion, result, currentState, currentTree, baseGenerationID, baseGenerationSeq, policy, log)
		}
		// No previous state at all — everything is new
		prevTree = &merkle.Tree{Root: &merkle.Node{IsDir: true, Children: map[string]*merkle.Node{}}}
	}

	// Merkle tree-based diff — only walks changed branches
	if prevTree.Root.Hash == currentTree.Root.Hash {
		fmt.Fprintln(log, "No changes detected (Merkle root hash matches).")
		return unchangedSyncResult(rootID, name, dir, baseGenerationID, baseGenerationSeq), nil
	}

	treeChanges := merkle.Diff(prevTree, currentTree)
	fmt.Fprintf(log, "Merkle diff found %d changed files (skipped unchanged subtrees)\n", len(treeChanges))

	// Convert Merkle changes to DiffResult for compatibility
	result := merkleChangesToDiffResult(treeChanges, prevTree, currentTree)

	return runSyncWithConflictRetry(cfg, dir, name, rootID, rootScope, dryRun, waitForCompletion, result, currentState, currentTree, baseGenerationID, baseGenerationSeq, policy, log)
}

// runSyncWithResult executes the sync with a pre-computed DiffResult.
func runSyncWithResult(cfg *appconfig.Config, dir, name, rootID, rootScope string, dryRun, waitForCompletion bool, result models.DiffResult, currentState map[string]models.FileState, currentTree *merkle.Tree, baseGenerationID string, baseGenerationSeq int64, policy ignore.PolicyPatternSet, log io.Writer) (*syncCommandResult, error) {
	var err error

	// Detect secrets
	secrets := diff.DetectSecrets(currentState)

	if dryRun {
		fmt.Fprint(log, diff.FormatDryRun(result, currentState, ignoredPatterns(policy), secrets))
		return dryRunSyncResult(rootID, name, dir, result, policy, secrets), nil
	}

	// Need a server connection for actual sync
	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}

	client := newAPIClient(cfg)

	// Get or create root
	if rootID == "" {
		resolvedRoot, err := resolveOrCreateRoot(client, name, dir, rootScope, log)
		if err != nil {
			return nil, err
		}
		rootID = resolvedRoot.ID
		baseGenerationID = resolvedRoot.VisibleGenerationID
		baseGenerationSeq = resolvedRoot.VisibleGenerationSeq
	}

	// Upload changed files to S3 via the server
	changeCount := countChanges(result)
	fmt.Fprintf(log, "Syncing %d changes to root %s...\n", changeCount, rootID)

	syncInit, err := initSyncSession(client, rootID, baseGenerationID, baseGenerationSeq, changeCount)
	if err != nil {
		if conflict, ok := syncConflictFromError(err); ok {
			return nil, conflict
		}
		return nil, fmt.Errorf("initializing sync session: %w", err)
	}
	syncSubmitted := false
	defer func() {
		if !syncSubmitted {
			_ = abortSyncSession(client, rootID, syncInit.GenerationID)
		}
	}()
	baseGenerationID = syncInit.BaseGenerationID
	baseGenerationSeq = syncInit.BaseGenerationSeq

	changes := withAbsolutePaths(dir, filterChanges(result))
	manifestRef, err := uploadChangedFiles(client, rootID, syncInit.GenerationID, dir, changes)
	if err != nil {
		return nil, err
	}
	changeRefs, err := uploadChangeShards(client, rootID, syncInit.GenerationID, changes)
	if err != nil {
		return nil, fmt.Errorf("uploading change shards: %w", err)
	}

	// Build content proof from Merkle tree
	proof := currentTree.BuildContentProof()
	contentProof := &models.ContentProofData{
		FileHashes: proof.FileHashes,
		DirHashes:  proof.DirHashes,
		RootHash:   proof.RootHash,
	}
	contentProofRef, err := uploadContentProof(client, rootID, syncInit.GenerationID, contentProof)
	if err != nil {
		return nil, fmt.Errorf("uploading content proof: %w", err)
	}
	stateRef, err := uploadRootState(client, rootID, syncInit.GenerationID, currentState)
	if err != nil {
		return nil, fmt.Errorf("uploading root state: %w", err)
	}

	// Send sync request with SimHash for index reuse + content proof
	syncReq := models.SyncRequest{
		ProtocolVersion:   models.SyncProtocolVersion,
		RootID:            rootID,
		GenerationID:      syncInit.GenerationID,
		BaseGenerationID:  baseGenerationID,
		BaseGenerationSeq: baseGenerationSeq,
		ChangeRefs:        changeRefs,
		ChangeCount:       changeCount,
		StateRef:          stateRef,
		SimHash:           currentTree.SimHashHex(),
		ContentProofRef:   contentProofRef,
		ManifestRef:       manifestRef,
	}

	respBody, err := client.post(fmt.Sprintf("/roots/%s/sync?async=true", rootID), syncReq)
	if err != nil {
		if conflict, ok := syncConflictFromError(err); ok {
			return nil, conflict
		}
		return nil, fmt.Errorf("sync request: %w", err)
	}
	syncSubmitted = true

	var syncResp models.SyncResponse
	if err := json.Unmarshal(respBody, &syncResp); err != nil {
		return nil, fmt.Errorf("parsing sync response: %w", err)
	}
	if syncResp.RootID == "" {
		syncResp.RootID = rootID
	}
	var completedJob *models.SyncJob
	if syncResp.SyncJobID != "" && waitForCompletion {
		completedJob, err = pollSyncJob(client, rootID, syncResp.SyncJobID, log)
		if err != nil {
			return nil, err
		}
		syncResp.FilesProcessed = completedJob.Processed
	}

	saveLocalSyncCacheWarnings(rootID, name, dir, currentState, currentTree, syncResp.GenerationID, syncResp.GenerationSeq)

	if !waitForCompletion {
		fmt.Fprintf(log, "Sync job %s started for root %s. Check status with: pufferfs sync status --root %s --job-id %s\n",
			syncResp.SyncJobID, rootID, rootID, syncResp.SyncJobID)
		return backgroundSyncResult(name, dir, changeCount, syncResp), nil
	}

	if completedJob != nil {
		fmt.Fprintf(log, "Sync complete: %d/%d files processed\n", completedJob.Processed, completedJob.TotalFiles)
	} else {
		fmt.Fprintf(log, "Sync complete: %d files processed, %d chunks added, %d removed, %d moved\n",
			syncResp.FilesProcessed, syncResp.ChunksAdded, syncResp.ChunksRemoved, syncResp.ChunksMoved)
	}

	return completedSyncResult(name, dir, changeCount, syncResp), nil
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

func runSyncWithConflictRetry(cfg *appconfig.Config, dir, name, rootID, rootScope string, dryRun, waitForCompletion bool, result models.DiffResult, currentState map[string]models.FileState, currentTree *merkle.Tree, baseGenerationID string, baseGenerationSeq int64, policy ignore.PolicyPatternSet, log io.Writer) (*syncCommandResult, error) {
	syncResult, err := runSyncWithResult(cfg, dir, name, rootID, rootScope, dryRun, waitForCompletion, result, currentState, currentTree, baseGenerationID, baseGenerationSeq, policy, log)
	var conflict *syncConflictError
	if !errors.As(err, &conflict) {
		return syncResult, err
	}
	if dryRun {
		return nil, err
	}

	fmt.Fprintln(log, "Remote generation changed during sync; reconciling against latest remote state.")
	client := newAPIClient(cfg)
	previousState, loadErr := loadRemoteState(client, rootID)
	if loadErr != nil {
		return nil, fmt.Errorf("loading remote state after sync conflict: %w", loadErr)
	}
	reconciled := diff.Compute(previousState, currentState)
	if countChanges(reconciled) == 0 {
		fmt.Fprintln(log, "No changes detected (remote state matches local filesystem).")
		if err := saveLocalSyncCache(rootID, name, dir, currentState, currentTree, conflict.CurrentGenerationID, conflict.CurrentGenerationSeq); err != nil {
			return nil, err
		}
		return unchangedSyncResult(rootID, name, dir, conflict.CurrentGenerationID, conflict.CurrentGenerationSeq), nil
	}
	return runSyncWithResult(cfg, dir, name, rootID, rootScope, dryRun, waitForCompletion, reconciled, currentState, currentTree, conflict.CurrentGenerationID, conflict.CurrentGenerationSeq, policy, log)
}

func pollSyncJob(client *apiClient, rootID, jobID string, log io.Writer) (*models.SyncJob, error) {
	fmt.Fprintf(log, "Sync job %s started; polling until committed...\n", jobID)
	deadline := time.Now().Add(syncPollTimeout())
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for sync job %s", jobID)
		}
		body, err := client.get(fmt.Sprintf("/roots/%s/sync/status?job_id=%s", rootID, url.QueryEscape(jobID)))
		if err != nil {
			return nil, fmt.Errorf("polling sync job: %w", err)
		}
		var job models.SyncJob
		if err := json.Unmarshal(body, &job); err != nil {
			return nil, fmt.Errorf("parsing sync job status: %w", err)
		}
		switch job.Status {
		case "completed":
			return &job, nil
		case "failed":
			return &job, fmt.Errorf("sync job failed: %s", string(job.Errors))
		default:
			fmt.Fprintf(log, "Sync status: %s (%d/%d files)\n", job.Status, job.Processed, job.TotalFiles)
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

func unchangedSyncResult(rootID, name, dir, generationID string, generationSeq int64) *syncCommandResult {
	return &syncCommandResult{
		Status:        "unchanged",
		RootID:        rootID,
		RootName:      name,
		SourcePath:    dir,
		Changes:       0,
		GenerationID:  generationID,
		GenerationSeq: generationSeq,
	}
}

func dryRunSyncResult(rootID, name, dir string, result models.DiffResult, policy ignore.PolicyPatternSet, secrets []string) *syncCommandResult {
	stats := result.Stats
	return &syncCommandResult{
		Status:      "dry_run",
		RootID:      rootID,
		RootName:    name,
		SourcePath:  dir,
		DryRun:      true,
		Changes:     countChanges(result),
		Stats:       &stats,
		FileChanges: filterChanges(result),
		Ignored:     ignoredPatterns(policy),
		Secrets:     secrets,
	}
}

func completedSyncResult(name, dir string, changes int, resp models.SyncResponse) *syncCommandResult {
	return &syncCommandResult{
		Status:         "synced",
		RootID:         resp.RootID,
		RootName:       name,
		SourcePath:     dir,
		Changes:        changes,
		SyncJobID:      resp.SyncJobID,
		GenerationID:   resp.GenerationID,
		GenerationSeq:  resp.GenerationSeq,
		ChunksAdded:    resp.ChunksAdded,
		ChunksRemoved:  resp.ChunksRemoved,
		ChunksMoved:    resp.ChunksMoved,
		FilesProcessed: resp.FilesProcessed,
	}
}

func backgroundSyncResult(name, dir string, changes int, resp models.SyncResponse) *syncCommandResult {
	return &syncCommandResult{
		Status:        "started",
		RootID:        resp.RootID,
		RootName:      name,
		SourcePath:    dir,
		Changes:       changes,
		SyncJobID:     resp.SyncJobID,
		GenerationID:  resp.GenerationID,
		GenerationSeq: resp.GenerationSeq,
	}
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

func resolveOrCreateRoot(client *apiClient, name, sourcePath, rootScope string, log io.Writer) (*models.RootMetadata, error) {
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
			fmt.Fprintf(log, "Using existing root: %s (%s)\n", r.Name, r.ID)
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

	fmt.Fprintf(log, "Created root: %s (%s)\n", root.Name, root.ID)
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

func uploadChangedFiles(client *apiClient, rootID, generationID, dir string, changes []models.FileChange) (string, error) {
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
		key, err := uploadBundle(client, rootID, generationID, fmt.Sprintf("%s-%06d", bundleID, bundleIndex), bundle.Bytes(), "application/octet-stream")
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
			key, err := uploadFile(client, rootID, generationID, change.Path, localPath)
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
	key, err := uploadBundle(client, rootID, generationID, bundleID+"-manifest", manifestBytes, "application/json")
	if err != nil {
		return "", fmt.Errorf("uploading source manifest: %w", err)
	}
	return key, nil
}

func initSyncSession(client *apiClient, rootID, baseGenerationID string, baseGenerationSeq int64, totalFiles int) (*models.SyncInitResponse, error) {
	req := models.SyncInitRequest{
		ProtocolVersion:   models.SyncProtocolVersion,
		BaseGenerationID:  baseGenerationID,
		BaseGenerationSeq: baseGenerationSeq,
		TotalFiles:        totalFiles,
	}
	respBody, err := client.post(fmt.Sprintf("/roots/%s/sync/init", rootID), req)
	if err != nil {
		return nil, err
	}
	var resp models.SyncInitResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, err
	}
	if resp.GenerationID == "" || resp.SyncJobID == "" {
		return nil, fmt.Errorf("sync init response missing generation or job id")
	}
	return &resp, nil
}

func uploadSyncArtifact(client *apiClient, rootID, generationID, kind, name string, data []byte, contentType string) (string, error) {
	path := fmt.Sprintf("/roots/%s/sync/%s/upload?kind=%s&name=%s", rootID, generationID, url.QueryEscape(kind), url.QueryEscape(name))
	respBody, err := client.postRaw(path, data, contentType)
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

func abortSyncSession(client *apiClient, rootID, generationID string) error {
	if client == nil || rootID == "" || generationID == "" {
		return nil
	}
	_, err := client.delete(fmt.Sprintf("/roots/%s/sync/%s", rootID, generationID))
	return err
}

func uploadChangeShards(client *apiClient, rootID, generationID string, changes []models.FileChange) ([]string, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	shardSize := uploadChangeShardMaxFiles()
	refs := make([]string, 0, (len(changes)+shardSize-1)/shardSize)
	for start := 0; start < len(changes); start += shardSize {
		end := start + shardSize
		if end > len(changes) {
			end = len(changes)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, change := range changes[start:end] {
			if err := enc.Encode(change); err != nil {
				return nil, err
			}
		}
		key, err := uploadSyncArtifact(client, rootID, generationID, "manifest", fmt.Sprintf("%06d.jsonl", len(refs)), buf.Bytes(), "application/x-ndjson")
		if err != nil {
			return nil, err
		}
		refs = append(refs, key)
	}
	return refs, nil
}

func uploadContentProof(client *apiClient, rootID, generationID string, proof *models.ContentProofData) (string, error) {
	if proof == nil {
		return "", nil
	}
	data, err := json.Marshal(proof)
	if err != nil {
		return "", err
	}
	return uploadSyncArtifact(client, rootID, generationID, "proof", "content-proof.json", data, "application/json")
}

func uploadFile(client *apiClient, rootID, generationID, relPath, localPath string) (string, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	path := fmt.Sprintf("/roots/%s/upload?generation_id=%s&path=%s", rootID, url.QueryEscape(generationID), url.QueryEscape(relPath))
	respBody, err := client.postStream(path, file, "application/octet-stream")
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

func uploadBundle(client *apiClient, rootID, generationID, bundleID string, data []byte, contentType string) (string, error) {
	path := fmt.Sprintf("/roots/%s/upload-bundle?generation_id=%s&bundle_id=%s", rootID, url.QueryEscape(generationID), url.QueryEscape(bundleID))
	respBody, err := client.postRaw(path, data, contentType)
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

func uploadRootState(client *apiClient, rootID, generationID string, state map[string]models.FileState) (string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(state); err != nil {
		_ = gz.Close()
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return uploadSyncArtifact(client, rootID, generationID, "state", "state.json.gz", buf.Bytes(), "application/gzip")
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

func uploadChangeShardMaxFiles() int {
	const defaultFiles = 5000
	raw := os.Getenv("PUFFERFS_UPLOAD_CHANGE_SHARD_MAX_FILES")
	if raw == "" {
		return defaultFiles
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return defaultFiles
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

func ignoredPatterns(policy ignore.PolicyPatternSet) []string {
	patterns := []string{".git/", "node_modules/", ".venv/", "__pycache__/", ".DS_Store"}
	patterns = appendPolicyPatterns(patterns, "org", policy.OrgPatterns)
	patterns = appendPolicyPatterns(patterns, "user", policy.UserPatterns)
	sort.Strings(patterns)
	return patterns
}

func appendPolicyPatterns(patterns []string, label, text string) []string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, fmt.Sprintf("%s policy: %s", label, line))
	}
	return patterns
}
