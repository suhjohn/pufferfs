package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// Resolve a preview target without creating a root or changing local metadata.
// A missing remote root is a new-root preview, not permission to POST /roots.
func runFileCapturePreview(ctx context.Context, cfg *appconfig.Config, dir, name, rootID string, spec syncSubsetSpec, noVector, force bool, log io.Writer) (*syncCommandResult, error) {
	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("capture preview requires a server URL to read the current catalog and ignore policy")
	}
	canonical, err := canonicalLocalPath(dir)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = filepath.Base(canonical)
	}
	if log == nil {
		log = os.Stdout
	}
	client := newAPIClient(cfg)
	if rootID == "" {
		if meta, err := findLocalRootMeta(name, dir); err == nil {
			rootID = meta.ID
		}
	}
	var remote *models.RootMetadata
	if rootID != "" {
		remote, err = loadRemoteRoot(client, rootID)
		if err != nil {
			return nil, fmt.Errorf("loading preview root: %w", err)
		}
	} else {
		body, err := client.get("/roots")
		if err != nil {
			return nil, fmt.Errorf("resolving preview root: %w", err)
		}
		var roots []models.RootMetadata
		if err = json.Unmarshal(body, &roots); err != nil {
			return nil, err
		}
		for _, root := range roots {
			if root.Name == name {
				remote = &root
				rootID = root.ID
				break
			}
		}
	}
	if err = validateNoVectorRoot(remote, noVector); err != nil {
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
	input := captureSyncInput{Client: client, Dir: canonical, Name: name, RootID: rootID,
		Policy: policy, Select: compiled.matches, Force: force, Log: log}
	if rootID == "" {
		fmt.Fprintln(log, "New-root preview; no root will be created.")
	}
	return previewFileCapture(ctx, input, fileCaptureCacheDir(input))
}

func previewFileCapture(ctx context.Context, input captureSyncInput, cacheDir string) (*syncCommandResult, error) {
	if input.Log == nil {
		input.Log = os.Stdout
	}
	base := make(map[string]models.FileState)
	if input.RootID != "" {
		if input.Client == nil {
			return nil, fmt.Errorf("capture preview requires a catalog client")
		}
		if err := input.Client.walkCapturedFiles(ctx, input.RootID, false, func(file models.CapturedFileHead) error {
			if !file.Deleted && (input.Select == nil || input.Select(file.Path)) {
				base[file.Path] = models.FileState{Size: file.Size, ContentHash: file.ContentHash}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	var excluded []string
	if relative, err := filepath.Rel(input.Dir, cacheDir); err == nil && filepath.IsLocal(relative) {
		if relative == "." {
			return nil, fmt.Errorf("capture cache cannot be the source root")
		}
		excluded = append(excluded, filepath.ToSlash(relative))
	}
	// No metadata cache is trusted here, and no proof bootstrap or pending
	// journal is executed. Preview can run while another capture holds its lock.
	plan, err := discoverCapturePlan(input.Dir, ignore.NewMatcherWithPolicy(input.Dir, input.Policy), base, nil, nil, input.Select, input.Force, excluded...)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(input.Dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	current := make(map[string]models.FileState, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		state, stable, err := hashCapturedFile(ctx, root, candidate.Path)
		if err != nil {
			return nil, fmt.Errorf("preview %s: %w", candidate.Path, err)
		}
		if !stable {
			return nil, fmt.Errorf("file changed during preview: %s; retry", candidate.Path)
		}
		current[candidate.Path] = state
	}
	changes := capturePreviewDiff(base, current, input.Force)
	var secrets []string
	for path := range current {
		if ignore.IsSecretFile(path) {
			secrets = append(secrets, path)
		}
	}
	sort.Strings(secrets)
	fmt.Fprintf(input.Log, "Capture preview: %d added, %d modified, %d removed, %d unchanged.\n",
		changes.Stats.Added, changes.Stats.Modified, changes.Stats.Removed, changes.Stats.Unchanged)
	fmt.Fprintln(input.Log, "No uploads, version registrations, proof updates, or local capture writes. Pending captures are not resumed; actual sync resumes them first.")
	for _, path := range secrets {
		fmt.Fprintf(input.Log, "Potential secret file: %s\n", path)
	}
	return dryRunSyncResult(input.RootID, input.Name, input.Dir, changes, input.Policy, secrets), nil
}

// Per-file capture does not have a move operation. Renames preview as an
// addition and a tombstone, just as the registration protocol represents them.
func capturePreviewDiff(base, current map[string]models.FileState, force bool) models.DiffResult {
	paths := make([]string, 0, len(base)+len(current))
	for path := range base {
		paths = append(paths, path)
	}
	for path := range current {
		if _, exists := base[path]; !exists {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	result := models.DiffResult{}
	for _, path := range paths {
		previous, existed := base[path]
		state, exists := current[path]
		change := models.FileChange{Path: path, ContentHash: state.ContentHash, Size: state.Size}
		switch {
		case !exists:
			change.Status, change.ContentHash, change.Size = models.StatusRemoved, previous.ContentHash, previous.Size
			result.Stats.Removed++
		case !existed:
			change.Status = models.StatusAdded
			result.Stats.Added++
		case force || state.ContentHash != previous.ContentHash || state.Size != previous.Size:
			change.Status = models.StatusModified
			result.Stats.Modified++
		default:
			change.Status = models.StatusUnchanged
			result.Stats.Unchanged++
		}
		result.Changes = append(result.Changes, change)
	}
	return result
}
