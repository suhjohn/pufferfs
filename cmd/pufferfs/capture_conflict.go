package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type captureVersionConflictError struct{ CaptureID string }

func (e *captureVersionConflictError) Error() string {
	return fmt.Sprintf("capture %s conflicts with a newer remote version", e.CaptureID)
}

// Called only for a definitive registration conflict and explicit --force,
// under the root sync lock. Preserve the rejected request and all captured
// bytes; never edit its base version/capture ID or call it accepted. The caller
// subsequently scans the live filesystem and creates a distinct capture.
func retainConflictedCapture(input captureSyncInput, dir, conflictsDir string, conflict *captureVersionConflictError) (string, error) {
	if !input.Force || conflict == nil {
		return "", errors.New("conflict recovery requires explicit --force")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	lock, err := root.OpenFile(".lock", os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return "", fmt.Errorf("conflicted capture is still being submitted: %w", err)
	}
	var journal captureJournal
	if err = readCaptureJSON(filepath.Join(dir, "journal.json"), &journal); err != nil {
		return "", err
	}
	if journal.Accepted != nil || journal.ServerURL != input.Client.baseURL || journal.RootID != input.RootID || journal.Request.CaptureID != conflict.CaptureID {
		return "", errors.New("conflict does not match the unaccepted local capture")
	}
	request, err := resolvedCaptureRequest(journal)
	if err != nil {
		return "", err
	}
	for _, file := range request.Files {
		if input.Select != nil && !input.Select(file.Path) {
			return "", fmt.Errorf("conflicted capture includes unselected path %s; use a full-root --force or select every path in this capture", file.Path)
		}
	}
	if err = os.MkdirAll(conflictsDir, 0700); err != nil {
		return "", err
	}
	destination := filepath.Join(conflictsDir, filepath.Base(dir))
	if _, err = os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("conflict archive destination already exists or is inaccessible: %s", destination)
	}
	if err = os.Rename(dir, destination); err != nil {
		return "", err
	}
	for _, parent := range []string{filepath.Dir(dir), conflictsDir, filepath.Dir(conflictsDir)} {
		file, err := os.Open(parent)
		if err != nil {
			return destination, err
		}
		err = file.Sync()
		file.Close()
		if err != nil {
			return destination, err
		}
	}
	return destination, nil
}
