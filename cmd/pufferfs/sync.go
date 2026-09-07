package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

func syncCmd() *cobra.Command {
	var (
		dryRun   bool
		name     string
		rootID   string
		rootPath string
		includes []string
		excludes []string
		scope    string
		noVector bool
		force    bool
		follow   bool
		jsonOut  bool
		options  followOptions
	)

	cmd := &cobra.Command{
		Use:   "sync [path]",
		Short: "Sync a directory or selected files to PufferFs",
		Long: strings.TrimSpace(`Sync a directory to PufferFs.

By default, sync scans PATH, --root, or the current directory, computes a root
and captures changed bytes into immutable source packs, then registers file versions.
Indexing is asynchronous; use 'sync wait' to wait for searchable results. If
--name is omitted, the root name defaults to the directory basename.

Use --include to sync only files matching one or more root-relative glob
patterns. Multiple --include flags are combined as OR, and --exclude always
wins. Only selected files are updated; unselected files stay visible.`),
		Example: strings.TrimSpace(`  pufferfs sync ./handbook --name handbook
  pufferfs sync --root /Users/me/handbook
  pufferfs sync --root /Users/me/handbook --include 'docs/**' --include README.md
  pufferfs sync --root /Users/me/handbook --include 'docs/**' --exclude 'docs/archive/**'
  pufferfs sync ./handbook --name handbook --dry-run`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			subsetMode := len(includes) > 0 || len(excludes) > 0
			dir, err := resolveSyncDirectoryArg(args, rootPath, subsetMode)
			if err != nil {
				return err
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
				if subsetMode {
					return fmt.Errorf("--include/--exclude cannot be combined with --follow")
				}
				if jsonOut {
					return fmt.Errorf("--json cannot be combined with --follow")
				}
				if dryRun {
					return fmt.Errorf("--follow cannot be combined with --dry-run")
				}
				if force {
					return fmt.Errorf("--follow cannot be combined with --force")
				}
				if cfg.Server.URL == "" {
					return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
				}
				return runFollow(cfg, absDir, name, rootID, noVector, options)
			}
			if subsetMode {
				log := syncLogWriter(jsonOut)
				result, err := runSyncSubset(cfg, absDir, syncSubsetSpec{
					Includes: includes,
					Excludes: excludes,
				}, name, rootID, scope, noVector, force, dryRun, log)
				if err != nil {
					return err
				}
				if jsonOut {
					return writePrettyJSON(os.Stdout, result)
				}
				return nil
			}
			log := syncLogWriter(jsonOut)
			result, err := runSync(cfg, absDir, name, rootID, scope, noVector, force, dryRun, log)
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
	cmd.Flags().StringVarP(&name, "name", "n", "", "Name alias for this root")
	cmd.Flags().StringVar(&rootID, "id", "", "Root ID to re-attach to")
	cmd.Flags().StringVar(&rootPath, "root", "", "Root path to sync")
	cmd.Flags().StringArrayVar(&includes, "include", nil, "Sync files matching this root-relative glob; can be repeated")
	cmd.Flags().StringArrayVar(&excludes, "exclude", nil, "Skip files matching this root-relative glob; can be repeated")
	cmd.Flags().StringVar(&scope, "scope", "org", "Root scope to create when missing: org or user")
	cmd.Flags().BoolVar(&noVector, "no-vector", false, "Create the root without vector search support")
	cmd.Flags().BoolVar(&force, "force", false, "Force reindex and retain rejected conflicting captures before recapturing current files")
	addFollowFlags(cmd, &options)
	cmd.AddCommand(syncStatusCmd(), syncWaitCmd())

	return cmd
}

func resolveSyncDirectoryArg(args []string, rootPath string, onlyMode bool) (string, error) {
	if onlyMode {
		if rootPath != "" {
			if len(args) > 0 {
				return "", fmt.Errorf("sync path specified both as argument and --root")
			}
			return rootPath, nil
		}
		if len(args) > 0 {
			return args[0], nil
		}
		return ".", nil
	}
	if rootPath != "" {
		if len(args) > 0 {
			return "", fmt.Errorf("sync path specified both as argument and --root")
		}
		return rootPath, nil
	}
	if len(args) > 0 {
		return args[0], nil
	}
	return ".", nil
}

type syncCommandResult struct {
	Status            string              `json:"status"`
	RootID            string              `json:"root_id,omitempty"`
	RootName          string              `json:"root_name,omitempty"`
	SourcePath        string              `json:"source_path,omitempty"`
	DryRun            bool                `json:"dry_run,omitempty"`
	Changes           int                 `json:"changes"`
	Stats             *models.DiffStats   `json:"stats,omitempty"`
	FileChanges       []models.FileChange `json:"file_changes,omitempty"`
	Ignored           []string            `json:"ignored_patterns,omitempty"`
	Secrets           []string            `json:"secrets,omitempty"`
	FilesProcessed    int                 `json:"files_processed,omitempty"`
	ConflictsRetained int                 `json:"conflicts_retained,omitempty"`
	dirtyPaths        []string
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
		jsonOut  bool
		watch    bool
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "status [root-id-or-name]",
		Short: "Show sync processing status",
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
			read := func(ctx context.Context) (*captureStatusReport, error) {
				return readCaptureStatus(ctx, client, rootID, nil)
			}
			emit := func(result *captureStatusReport) error {
				if jsonOut {
					return writePrettyJSON(os.Stdout, result)
				}
				return printCaptureStatus(os.Stdout, result)
			}
			if watch {
				ctx, cancel := context.WithTimeout(cmd.Context(), syncPollTimeout())
				defer cancel()
				_, err := pollCaptureStatus(ctx, interval, read, emit)
				return err
			}
			result, err := read(cmd.Context())
			if err != nil {
				return err
			}
			return emit(result)

		},
	}
	cmd.Flags().StringVar(&rootRef, "root", "", "Root ID or name (defaults to the root for the current directory)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print per-file processing status JSON")
	cmd.Flags().BoolVar(&watch, "watch", false, "Poll until indexing completes or processing fails")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Polling interval for --watch")
	return cmd
}

func syncWaitCmd() *cobra.Command {
	var (
		rootRef  string
		jsonOut  bool
		interval time.Duration
		timeout  time.Duration
		includes []string
		excludes []string
	)
	cmd := &cobra.Command{
		Use:   "wait [root-id-or-name]",
		Short: "Wait for indexing or selected synced files",
		Long: strings.TrimSpace(`Wait for sync completion.

This waits for the latest registered extraction of each captured file to be published. Filters compare matching local file hashes
with those captured versions. Unrelated files do not block a filtered wait.
There is no root job ID or server-side commit barrier.`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			var emit func(*captureStatusReport) error
			if !jsonOut {
				emit = func(result *captureStatusReport) error { return printCaptureStatus(os.Stdout, result) }
			}
			result, err := waitForCapturedSelection(cmd.Context(), cfg, rootRef, args,
				syncSubsetSpec{Includes: includes, Excludes: excludes}, interval, timeout, emit)
			if jsonOut && result != nil {
				if writeErr := writePrettyJSON(os.Stdout, result); writeErr != nil {
					return writeErr
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&rootRef, "root", "", "Root ID or name (defaults to the root for the current directory)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print final processing status JSON")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Polling interval")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Maximum time to wait (defaults to PUFFERFS_SYNC_POLL_TIMEOUT or 35m)")
	cmd.Flags().StringArrayVar(&includes, "include", nil, "Wait for files matching this root-relative glob; can be repeated")
	cmd.Flags().StringArrayVar(&excludes, "exclude", nil, "Ignore matching files while waiting; can be repeated")
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

func resolveSyncWaitRoot(client *apiClient, rootRef string, args []string) (*syncOnlyRoot, error) {
	if rootRef != "" && len(args) > 0 {
		return nil, fmt.Errorf("root specified both as argument and --root")
	}
	if rootRef == "" && len(args) > 0 {
		rootRef = args[0]
	}
	if rootRef == "" {
		rootID, err := detectRootFromCwd()
		if err != nil {
			return nil, fmt.Errorf("could not detect root from cwd; use --root to specify: %w", err)
		}
		rootRef = rootID
	}
	if pathLooksLocal(rootRef) {
		return resolveSyncOnlyRoot(client, rootRef, "", "")
	}
	rootID := rootRef
	if !isUUID(rootID) {
		resolvedID, err := resolveRootName(client, rootRef)
		if err != nil {
			return nil, fmt.Errorf("resolving root %q: %w", rootRef, err)
		}
		rootID = resolvedID
	}
	root, err := loadRemoteRoot(client, rootID)
	if err != nil {
		return nil, fmt.Errorf("loading root %s: %w", rootID, err)
	}
	canonicalSource, err := canonicalLocalPath(root.SourcePath)
	if err != nil {
		return nil, fmt.Errorf("resolving root source path %s: %w", root.SourcePath, err)
	}
	return &syncOnlyRoot{RootMetadata: *root, CanonicalSourcePath: canonicalSource}, nil
}

func pathLooksLocal(value string) bool {
	if value == "" {
		return false
	}
	return filepath.IsAbs(value) || strings.HasPrefix(value, ".") || strings.ContainsAny(value, `/\`)
}

func selectedLocalState(rootPath string, spec compiledSyncSubsetSpec, policy ignore.PolicyPatternSet) (map[string]models.FileState, error) {
	state := make(map[string]models.FileState)
	matcher := ignore.NewMatcherWithPolicy(rootPath, policy)
	rootPath = filepath.Clean(rootPath)
	err := filepath.WalkDir(rootPath, func(absPath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(rootPath, absPath)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		relPath = filepath.ToSlash(relPath)
		if matcher.ShouldIgnore(relPath, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !spec.matches(relPath) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", relPath, err)
		}
		fileState, err := fileStateForPath(absPath, info)
		if err != nil {
			return fmt.Errorf("hash %s: %w", relPath, err)
		}
		state[relPath] = fileState
		return nil
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func normalizeSyncPollInterval(interval time.Duration) time.Duration {
	if interval < 100*time.Millisecond {
		return 2 * time.Second
	}
	return interval
}

func runSync(cfg *appconfig.Config, dir, name, rootID, rootScope string, noVector, force, dryRun bool, log io.Writer) (*syncCommandResult, error) {
	return runSyncSubset(cfg, dir, syncSubsetSpec{}, name, rootID, rootScope, noVector, force, dryRun, log)
}

type syncOnlyRoot struct {
	models.RootMetadata
	CanonicalSourcePath string
}

type syncSubsetSpec struct {
	Includes []string
	Excludes []string
}

type compiledSyncSubsetSpec struct {
	includeGlobs []string
	excludeGlobs []string
}

func runSyncSubset(cfg *appconfig.Config, rootPath string, spec syncSubsetSpec, name, rootID, rootScope string, noVector, force, dryRun bool, log io.Writer) (*syncCommandResult, error) {
	canonical, err := canonicalLocalPath(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolving sync root: %w", err)
	}
	if name == "" {
		name = filepath.Base(canonical)
	}
	if dryRun {
		return runFileCapturePreview(context.Background(), cfg, canonical, name, rootID, spec, noVector, force, log)
	}
	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}
	if log == nil {
		log = os.Stdout
	}
	client := newAPIClient(cfg)
	if rootID == "" {
		if meta, err := findLocalRootMeta(name, canonical); err == nil {
			rootID = meta.ID
		} else {
			root, err := resolveOrCreateRoot(client, name, canonical, rootScope, noVector, log)
			if err != nil {
				return nil, err
			}
			rootID = root.ID
		}
	}
	root, err := loadRemoteRoot(client, rootID)
	if err != nil {
		return nil, fmt.Errorf("loading remote root metadata: %w", err)
	}
	if err := validateNoVectorRoot(root, noVector); err != nil {
		return nil, err
	}
	policy, err := fetchSyncPolicy(client, false)
	if err != nil {
		return nil, err
	}
	compiled, err := compileSyncSubsetSpec(canonical, spec)
	if err != nil {
		return nil, err
	}
	input := captureSyncInput{Client: client, Dir: canonical, Name: root.Name, RootID: rootID, Policy: policy, Select: compiled.matches, Force: force, Log: log}
	result, err := runFileCaptureSync(context.Background(), input, fileCaptureCacheDir(input))
	if err != nil {
		return result, err
	}
	if err := saveRootMeta(rootID, root.Name, canonical); err != nil {
		return result, fmt.Errorf("saving local root identity after capture: %w", err)
	}
	return result, nil
}

func fetchSyncPolicy(client *apiClient, dryRun bool) (ignore.PolicyPatternSet, error) {
	var policy ignore.PolicyPatternSet
	if client == nil {
		return policy, nil
	}
	effectivePolicy, err := fetchEffectiveIgnorePolicy(client)
	if err != nil {
		if dryRun {
			return policy, nil
		}
		return policy, fmt.Errorf("loading ignore policy: %w", err)
	}
	policy.OrgPatterns = effectivePolicy.OrgPatterns
	policy.UserPatterns = effectivePolicy.UserPatterns
	return policy, nil
}

func compileSyncSubsetSpec(rootPath string, spec syncSubsetSpec) (compiledSyncSubsetSpec, error) {
	compiled := compiledSyncSubsetSpec{}
	for _, pattern := range spec.Includes {
		clean, err := normalizeRootRelativePattern(rootPath, pattern, "--include")
		if err != nil {
			return compiled, err
		}
		compiled.includeGlobs = append(compiled.includeGlobs, clean)
	}
	for _, pattern := range spec.Excludes {
		clean, err := normalizeRootRelativePattern(rootPath, pattern, "--exclude")
		if err != nil {
			return compiled, err
		}
		compiled.excludeGlobs = append(compiled.excludeGlobs, clean)
	}
	compiled.includeGlobs = dedupeStrings(compiled.includeGlobs)
	compiled.excludeGlobs = dedupeStrings(compiled.excludeGlobs)
	return compiled, nil
}

func normalizeRootRelativePattern(rootPath, pattern, flagName string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", fmt.Errorf("%s pattern is empty", flagName)
	}
	if filepath.IsAbs(pattern) {
		if hasGlobMeta(pattern) {
			return "", fmt.Errorf("%s pattern %q must be root-relative when it contains glob metacharacters", flagName, pattern)
		}
		relPath, _, err := resolveSelectedPath(rootPath, pattern, flagName)
		if err != nil {
			return "", err
		}
		return relPath, nil
	}
	pattern = filepath.ToSlash(pattern)
	pattern = strings.TrimPrefix(pattern, "./")
	pattern = path.Clean(pattern)
	if pattern == "." || strings.HasPrefix(pattern, "../") || strings.HasPrefix(pattern, "/") {
		return "", fmt.Errorf("%s pattern %q must stay inside the sync root", flagName, pattern)
	}
	return pattern, nil
}

func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func resolveSyncOnlyRoot(client *apiClient, rootPath, name, rootID string) (*syncOnlyRoot, error) {
	canonicalRoot, err := canonicalLocalPath(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolving --root: %w", err)
	}
	if rootID != "" {
		root, err := loadRemoteRoot(client, rootID)
		if err != nil {
			return nil, fmt.Errorf("loading root %s: %w", rootID, err)
		}
		canonicalSource, err := canonicalLocalPath(root.SourcePath)
		if err != nil {
			return nil, fmt.Errorf("resolving root source path %s: %w", root.SourcePath, err)
		}
		if canonicalSource != canonicalRoot {
			return nil, fmt.Errorf("--id %s source path is %s, not --root %s", rootID, root.SourcePath, canonicalRoot)
		}
		if name != "" && root.Name != name {
			return nil, fmt.Errorf("--name %s does not match root %s", name, root.Name)
		}
		return &syncOnlyRoot{RootMetadata: *root, CanonicalSourcePath: canonicalSource}, nil
	}

	respBody, err := client.get("/roots")
	if err != nil {
		return nil, fmt.Errorf("listing roots: %w", err)
	}
	var roots []models.RootMetadata
	if err := json.Unmarshal(respBody, &roots); err != nil {
		return nil, fmt.Errorf("parsing roots: %w", err)
	}

	var matches []syncOnlyRoot
	for _, root := range roots {
		canonicalSource, err := canonicalLocalPath(root.SourcePath)
		if err != nil || canonicalSource != canonicalRoot {
			continue
		}
		if name != "" && root.Name != name {
			continue
		}
		matches = append(matches, syncOnlyRoot{RootMetadata: root, CanonicalSourcePath: canonicalSource})
	}
	if len(matches) == 0 {
		if name != "" {
			return nil, fmt.Errorf("no synced root found for %s with name %q; run `pufferfs sync %s --name %s` first", canonicalRoot, name, canonicalRoot, name)
		}
		return nil, fmt.Errorf("no synced root found for %s; run `pufferfs sync %s --name <name>` first", canonicalRoot, canonicalRoot)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple synced roots found for %s; pass --name or --id to disambiguate", canonicalRoot)
	}
	return &matches[0], nil
}

func canonicalLocalPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(resolved)
	}
	return abs, nil
}

func (spec compiledSyncSubsetSpec) matches(relPath string) bool {
	relPath = path.Clean(filepath.ToSlash(relPath))
	included := len(spec.includeGlobs) == 0
	for _, pattern := range spec.includeGlobs {
		if matchRootRelativeGlob(pattern, relPath) {
			included = true
			break
		}
	}
	if !included {
		return false
	}
	for _, pattern := range spec.excludeGlobs {
		if matchRootRelativeGlob(pattern, relPath) {
			return false
		}
	}
	return true
}

func matchRootRelativeGlob(pattern, relPath string) bool {
	pattern = path.Clean(filepath.ToSlash(pattern))
	relPath = path.Clean(filepath.ToSlash(relPath))
	if pattern == relPath {
		return true
	}
	return matchGlobParts(strings.Split(pattern, "/"), strings.Split(relPath, "/"))
}

func matchGlobParts(patternParts, pathParts []string) bool {
	if len(patternParts) == 0 {
		return len(pathParts) == 0
	}
	if patternParts[0] == "**" {
		if matchGlobParts(patternParts[1:], pathParts) {
			return true
		}
		for i := range pathParts {
			if matchGlobParts(patternParts[1:], pathParts[i+1:]) {
				return true
			}
		}
		return false
	}
	if len(pathParts) == 0 {
		return false
	}
	matched, err := path.Match(patternParts[0], pathParts[0])
	if err != nil || !matched {
		return false
	}
	return matchGlobParts(patternParts[1:], pathParts[1:])
}

func resolveSelectedPath(rootPath, requested, flagName string) (string, string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", "", fmt.Errorf("%s path is empty", flagName)
	}
	if canonicalRoot, err := canonicalPathAllowMissing(rootPath); err == nil {
		rootPath = canonicalRoot
	}
	var absPath string
	var err error
	if filepath.IsAbs(requested) {
		absPath, err = filepath.Abs(requested)
	} else {
		absPath, err = filepath.Abs(filepath.Join(rootPath, filepath.FromSlash(requested)))
	}
	if err != nil {
		return "", "", err
	}
	absPath = filepath.Clean(absPath)
	if canonicalPath, err := canonicalPathAllowMissing(absPath); err == nil {
		absPath = canonicalPath
	}
	rel, err := filepath.Rel(rootPath, absPath)
	if err != nil {
		return "", "", err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("%s path %s is outside root %s", flagName, absPath, rootPath)
	}
	rel = filepath.ToSlash(rel)
	return rel, absPath, nil
}

func canonicalPathAllowMissing(path string) (string, error) {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved), nil
	}
	var suffix []string
	current := path
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return path, nil
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			parts := append([]string{filepath.Clean(resolved)}, suffix...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		current = parent
	}
}

func fileStateForPath(path string, info os.FileInfo) (models.FileState, error) {
	file, err := os.Open(path)
	if err != nil {
		return models.FileState{}, err
	}
	defer file.Close()

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return models.FileState{}, err
	}
	return models.FileState{
		Size:        info.Size(),
		ContentHash: "sha256:" + hex.EncodeToString(h.Sum(nil)),
		Mtime:       info.ModTime().UnixNano(),
	}, nil
}

// runSyncWithResult executes the sync with a pre-computed DiffResult.

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

func resolveOrCreateRoot(client *apiClient, name, sourcePath, rootScope string, noVector bool, log io.Writer) (*models.RootMetadata, error) {
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
			if err := validateNoVectorRoot(&r, noVector); err != nil {
				return nil, err
			}
			fmt.Fprintf(log, "Using existing root: %s (%s)\n", r.Name, r.ID)
			return &r, nil
		}
	}

	// Create new root
	createReq := map[string]any{
		"name":            name,
		"source_path":     sourcePath,
		"scope":           rootScope,
		"vector_disabled": noVector,
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

func validateNoVectorRoot(root *models.RootMetadata, noVector bool) error {
	if root == nil || !noVector || root.VectorDisabled {
		return nil
	}
	return fmt.Errorf("--no-vector only applies when creating a new root or syncing a vector-disabled root; root %s (%s) already supports vector search", root.Name, root.ID)
}

type rootMeta struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SourcePath string `json:"source_path"`
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

const (
	defaultUploadConcurrency = 4
	maxUploadConcurrency     = 16
)

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

func saveRootMeta(rootID, name, sourcePath string) error {
	dir := appconfig.RootDir(rootID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	meta := rootMeta{
		ID:         rootID,
		Name:       name,
		SourcePath: sourcePath,
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
