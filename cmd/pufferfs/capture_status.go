package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
)

type captureStatusExample = models.CaptureStatusExample
type captureStatusReport = models.CaptureStatusResponse

// Whole-root status is one bounded response. A local selection queries only its
// paths in bounded batches; it never downloads the unrelated remote catalog.
func readCaptureStatus(ctx context.Context, client *apiClient, rootID string, local map[string]models.FileState) (*captureStatusReport, error) {
	result := &captureStatusReport{RootID: rootID, States: make(map[string]int)}
	endpoint := "/roots/" + url.PathEscape(rootID) + "/capture-summary"
	load := func(selection []models.CaptureStatusSelection) error {
		var body []byte
		var err error
		if local == nil {
			body, err = client.requestWithContext(ctx, http.MethodGet, endpoint, nil)
		} else {
			body, err = client.postContext(ctx, endpoint, models.CaptureStatusRequest{Files: selection})
		}
		if err != nil {
			return err
		}
		var page captureStatusReport
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		if page.RootID != rootID || page.Total < 0 || page.States == nil || len(page.Examples) > 20 || (local != nil && page.Total != len(selection)) {
			return fmt.Errorf("invalid capture summary")
		}
		total := 0
		for status, count := range page.States {
			switch status {
			case "pending", "running", "waiting_provider", "complete", "failed", "superseded", "missing", "inconsistent", "uncaptured":
			default:
				return fmt.Errorf("unknown file processing status %q", status)
			}
			if count < 0 {
				return fmt.Errorf("invalid capture summary count")
			}
			total += count
			result.States[status] += count
		}
		if total != page.Total {
			return fmt.Errorf("inconsistent capture summary totals")
		}
		result.Total += total
		result.Examples = append(result.Examples, page.Examples[:min(len(page.Examples), 20-len(result.Examples))]...)
		return nil
	}
	if local == nil {
		if err := load(nil); err != nil {
			return nil, err
		}
	} else {
		paths := make([]string, 0, len(local))
		for path := range local {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for offset := 0; offset < len(paths); offset += 1000 {
			selection := make([]models.CaptureStatusSelection, 0, min(1000, len(paths)-offset))
			for _, path := range paths[offset:min(offset+1000, len(paths))] {
				file := local[path]
				selection = append(selection, models.CaptureStatusSelection{Path: path, ContentHash: file.ContentHash, Size: file.Size})
			}
			if err := load(selection); err != nil {
				return nil, err
			}
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
