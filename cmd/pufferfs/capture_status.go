package main

import (
	"context"
	"fmt"
	"io"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
)

type captureStatusExample struct {
	Path       string                       `json:"path"`
	VersionID  string                       `json:"version_id,omitempty"`
	Status     string                       `json:"status"`
	Processing *models.FileProcessingStatus `json:"processing,omitempty"`
}

type captureStatusReport struct {
	RootID   string                 `json:"root_id"`
	Status   string                 `json:"status"`
	Total    int                    `json:"total"`
	States   map[string]int         `json:"states"`
	Examples []captureStatusExample `json:"examples,omitempty"`
}

// Live, paginated metadata, not an atomic root snapshot. Only the local
// selection (when requested) and at most 20 non-complete examples are retained.
func readCaptureStatus(ctx context.Context, client *apiClient, rootID string, local map[string]models.FileState) (*captureStatusReport, error) {
	result := &captureStatusReport{RootID: rootID, States: make(map[string]int)}
	seen := make(map[string]bool)
	add := func(file models.CapturedFileHead, status string) {
		result.Total++
		result.States[status]++
		if status != "complete" && len(result.Examples) < 20 {
			result.Examples = append(result.Examples, captureStatusExample{file.Path, file.VersionID, status, file.Processing})
		}
	}
	err := client.walkCapturedFiles(ctx, rootID, true, func(file models.CapturedFileHead) error {
		if local != nil {
			expected, ok := local[file.Path]
			if !ok {
				return nil
			}
			seen[file.Path] = true
			if file.Deleted || file.ContentHash != expected.ContentHash || file.Size != expected.Size {
				add(file, "uncaptured")
				return nil
			}
		}
		if file.Processing == nil {
			return fmt.Errorf("server did not return per-file processing status; upgrade the server")
		}
		if file.Processing.Status == "complete" && (file.IndexedVersionID != file.VersionID || file.Processing.ExtractionID == "" || file.Processing.Stage != "index") {
			return fmt.Errorf("server reported completion without matching file publication")
		}
		switch file.Processing.Status {
		case "pending", "running", "waiting_provider", "complete", "failed", "superseded", "missing", "inconsistent":
			add(file, file.Processing.Status)
		default:
			return fmt.Errorf("unknown file processing status %q", file.Processing.Status)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for path := range local {
		if !seen[path] {
			add(models.CapturedFileHead{Path: path}, "uncaptured")
		}
	}
	result.Status = "processing"
	if result.Total == 0 {
		result.Status = "empty"
	} else if result.States["complete"] == result.Total {
		result.Status = "complete"
	} else if result.States["failed"]+result.States["superseded"]+result.States["missing"]+result.States["inconsistent"] > 0 {
		result.Status = "failed"
	}
	return result, nil
}

func printCaptureStatus(out io.Writer, result *captureStatusReport) error {
	_, err := fmt.Fprintf(out, "%s: %d/%d files indexed; pending=%d running=%d provider=%d failed=%d uncaptured=%d\n",
		result.Status, result.States["complete"], result.Total, result.States["pending"], result.States["running"],
		result.States["waiting_provider"], result.States["failed"]+result.States["superseded"]+result.States["missing"]+result.States["inconsistent"], result.States["uncaptured"])
	return err
}

func pollCaptureStatus(ctx context.Context, interval time.Duration, read func(context.Context) (*captureStatusReport, error), emit func(*captureStatusReport) error) (*captureStatusReport, error) {
	var last *captureStatusReport
	for {
		if err := ctx.Err(); err != nil {
			return last, fmt.Errorf("waiting for file indexing: %w", err)
		}
		result, err := read(ctx)
		if err != nil {
			return last, err
		}
		last = result
		if emit != nil {
			if err := emit(result); err != nil {
				return last, err
			}
		}
		switch result.Status {
		case "complete":
			return last, nil
		case "empty":
			return last, fmt.Errorf("no files available for the selected indexing wait")
		case "failed":
			return last, fmt.Errorf("selected files have failed or inconsistent processing; inspect sync status --json")
		}
		timer := time.NewTimer(normalizeSyncPollInterval(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, fmt.Errorf("waiting for file indexing: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func waitForCapturedSelection(ctx context.Context, cfg *appconfig.Config, rootRef string, args []string, spec syncSubsetSpec, interval, timeout time.Duration, emit func(*captureStatusReport) error) (*captureStatusReport, error) {
	if timeout <= 0 {
		timeout = syncPollTimeout()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}
	var read func(context.Context) (*captureStatusReport, error)
	if len(spec.Includes) > 0 || len(spec.Excludes) > 0 {
		client := newAPIClient(cfg)
		root, err := resolveSyncWaitRoot(client, rootRef, args)
		if err != nil {
			return nil, err
		}
		compiled, err := compileSyncSubsetSpec(root.CanonicalSourcePath, spec)
		if err != nil {
			return nil, err
		}
		policy, err := fetchSyncPolicy(client, false)
		if err != nil {
			return nil, err
		}
		read = func(ctx context.Context) (*captureStatusReport, error) {
			local, err := selectedLocalState(root.CanonicalSourcePath, compiled, policy)
			if err != nil {
				return nil, err
			}
			return readCaptureStatus(ctx, client, root.ID, local)
		}
	} else {
		client, rootID, err := syncCommandClientAndRoot(cfg, rootRef, args)
		if err != nil {
			return nil, err
		}
		read = func(ctx context.Context) (*captureStatusReport, error) {
			return readCaptureStatus(ctx, client, rootID, nil)
		}
	}
	return pollCaptureStatus(ctx, interval, read, emit)
}
