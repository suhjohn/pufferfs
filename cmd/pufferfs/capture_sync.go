package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
	"golang.org/x/sys/unix"
)

func fileCaptureCacheDir(input captureSyncInput) string {
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(input.Client.baseURL+"\x00"+input.Dir)))
	return filepath.Join(appconfig.RootDir(input.RootID), "file-capture-"+identity)
}

func runFileCaptureSync(ctx context.Context, input captureSyncInput, cacheDir string) (*syncCommandResult, error) {
	if input.Log == nil {
		input.Log = os.Stdout
	}
	if input.Client == nil || input.RootID == "" {
		return nil, errors.New("capture client and root are required")
	}
	var excludedDirs []string
	if relative, err := filepath.Rel(input.Dir, cacheDir); err == nil && filepath.IsLocal(relative) {
		if relative == "." {
			return nil, errors.New("capture cache cannot be the source root")
		}
		excludedDirs = append(excludedDirs, filepath.ToSlash(relative))
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(cacheDir, ".sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("root capture already running: %w", err)
	}
	pendingDir, completedDir, headsDir := filepath.Join(cacheDir, "pending"), filepath.Join(cacheDir, "completed"), filepath.Join(cacheDir, "heads")
	for _, dir := range []string{pendingDir, completedDir} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	spoolLimit, err := captureSpoolLimit()
	if err != nil {
		return nil, err
	}
	cleanup := func() error {
		released, err := retainCaptureSpools(cacheDir, input.Client.baseURL, input.RootID)
		if released > 0 {
			fmt.Fprintf(input.Log, "Released %d bytes of accepted local capture data; originals remain in S3.\n", released)
		}
		return err
	}
	if err = cleanup(); err != nil {
		return nil, err
	}
	released, err := discardIncompleteCaptures(pendingDir)
	if released > 0 {
		fmt.Fprintf(input.Log, "Discarded %d bytes from incomplete, unsubmitted captures.\n", released)
	}
	if err != nil {
		return nil, err
	}
	result := &syncCommandResult{Status: "unchanged", RootID: input.RootID, RootName: input.Name, SourcePath: input.Dir}
	submit := func(dir string) error {
		accepted, err := submitCapture(ctx, input.Client, dir, headsDir)
		if err != nil {
			return err
		}
		var journal captureJournal
		if err = readCaptureJSON(filepath.Join(dir, "journal.json"), &journal); err != nil {
			return err
		}
		result.Changes += len(accepted.Versions)
		result.FilesProcessed += len(accepted.Versions)
		result.Status = "captured"
		result.dirtyPaths = append(result.dirtyPaths, journal.Dirty...)
		// Only accepted captures with durable heads enter cleanup eligibility.
		if err = os.Rename(dir, filepath.Join(completedDir, filepath.Base(dir))); err != nil {
			return err
		}
		for _, parent := range []string{pendingDir, completedDir} {
			f, err := os.Open(parent)
			if err != nil {
				return err
			}
			err = f.Sync()
			f.Close()
			if err != nil {
				return err
			}
		}
		return cleanup()
	}
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(pendingDir, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "journal.json")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		if err = submit(dir); err != nil {
			var conflict *captureVersionConflictError
			if errors.As(err, &conflict) {
				if !input.Force {
					return nil, fmt.Errorf("%w; captured bytes remain at %s; rerun sync --force to retain this spool and capture the current local files against the latest catalog", err, dir)
				}
				destination, archiveErr := retainConflictedCapture(input, dir, filepath.Join(cacheDir, "conflicts"), conflict)
				if archiveErr != nil {
					if destination != "" {
						return nil, fmt.Errorf("capture moved to %s but archive durability could not be confirmed: %w", destination, archiveErr)
					}
					return nil, archiveErr
				}
				result.ConflictsRetained++
				fmt.Fprintf(input.Log, "Conflicted capture retained at %s; rescanning current local files with --force.\n", destination)
				continue
			}
			return nil, fmt.Errorf("resuming captured bytes: %w", err)
		}
	}
	remote := make(map[string]models.CapturedFileHead)
	base, cache := make(map[string]models.FileState), make(map[string]models.FileState)
	dirty := make(map[string]bool)
	err = input.Client.walkCapturedFiles(ctx, input.RootID, false, func(file models.CapturedFileHead) error {
		remote[file.Path] = file
		if file.Deleted {
			return nil
		}
		base[file.Path] = models.FileState{Size: file.Size, ContentHash: file.ContentHash}
		head, err := loadCapturedHead(headsDir, input.Client.baseURL, input.RootID, file.Path)
		if err != nil {
			return err
		}
		if head != nil && head.Version.VersionID == file.VersionID && file.ProofCurrent {
			cache[file.Path], dirty[file.Path] = head.State, head.Dirty
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("loading captured catalog: %w", err)
	}
	matcher := ignore.NewMatcherWithPolicy(input.Dir, input.Policy)
	plan, err := discoverCapturePlan(input.Dir, matcher, base, cache, dirty, input.Select, input.Force, excludedDirs...)
	if err != nil {
		return nil, err
	}
	files := make([]models.CaptureFile, 0, len(plan.Candidates))
	if !input.Force {
		plan.Candidates, err = bootstrapCapturedProofs(ctx, input, headsDir, remote, plan.Candidates)
		if err != nil {
			return nil, err
		}
	}
	for _, candidate := range plan.Candidates {
		files = append(files, models.CaptureFile{Path: candidate.Path, PreviousVersionID: remote[candidate.Path].VersionID})
	}
	for path, file := range remote {
		if !file.Deleted && !plan.Present[path] && (input.Select == nil || input.Select(path)) {
			files = append(files, models.CaptureFile{Path: path, PreviousVersionID: file.VersionID, Deleted: true})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for offset := 0; offset < len(files); {
		batch := files[offset:min(offset+128, len(files))]
		// Retain source extents only for this batch, not the entire root.
		previous := make(map[string]localCapturedHead, len(batch))
		for _, file := range batch {
			if file.Deleted {
				continue
			}
			head, err := loadCapturedHead(headsDir, input.Client.baseURL, input.RootID, file.Path)
			if err != nil {
				return nil, err
			}
			if head != nil {
				previous[file.Path] = *head
			}
		}
		available, err := remainingCaptureSpoolBytes(cacheDir, spoolLimit)
		if err != nil {
			return nil, err
		}
		dir, captured, err := createCaptureSpool(ctx, pendingDir, input.Client.baseURL, input.RootID, input.Dir, batch, previous, 32<<20, available)
		if err != nil {
			return nil, err
		}
		if err = submit(dir); err != nil {
			var conflict *captureVersionConflictError
			if errors.As(err, &conflict) {
				return nil, fmt.Errorf("%w; captured bytes remain at %s; retry with --force to rescan against the latest catalog", err, dir)
			}
			return nil, err
		}
		offset += captured
		fmt.Fprintf(input.Log, "Capture accepted: %d files; indexing continues independently.\n", result.FilesProcessed)
	}
	if result.Status == "unchanged" {
		fmt.Fprintln(input.Log, "No capture changes detected.")
	}
	return result, nil
}
