package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/pufferfs/pufferfs/pkg/models"
	"golang.org/x/sys/unix"
)

// Each path has an independently replaceable local head. A captured head is
// not an indexed head: it advances on API acceptance, including tombstones.
type localCapturedHead struct {
	ServerURL string                       `json:"server_url"`
	RootID    string                       `json:"root_id"`
	Path      string                       `json:"path"`
	Version   models.RegisteredFileVersion `json:"version"`
	Source    *models.SourceManifest       `json:"source,omitempty"`
	Deleted   bool                         `json:"deleted"`
	State     models.FileState             `json:"state"`
	Dirty     bool                         `json:"dirty"`
}

func capturedHeadName(serverURL, rootID, path string) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(serverURL+"\x00"+rootID+"\x00"+path)))
}

func readCaptureJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("capture metadata exceeds 4 MiB")
	}
	return json.Unmarshal(data, value)
}

func loadCapturedHead(dir, serverURL, rootID, path string) (*localCapturedHead, error) {
	var head localCapturedHead
	err := readCaptureJSON(filepath.Join(dir, capturedHeadName(serverURL, rootID, path)), &head)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if head.ServerURL != serverURL || head.RootID != rootID || head.Path != path || head.Version.VersionID == "" || head.Version.Sequence < 1 {
		return nil, errors.New("invalid local captured head")
	}
	return &head, nil
}

func acceptedCaptureHeads(journal captureJournal) ([]localCapturedHead, error) {
	if journal.Accepted == nil || journal.Accepted.CaptureID != journal.Request.CaptureID || len(journal.Accepted.Versions) != len(journal.Request.Files) {
		return nil, errors.New("capture acceptance missing or mismatched")
	}
	request, err := resolvedCaptureRequest(journal)
	if err != nil {
		return nil, err
	}
	heads := make([]localCapturedHead, 0, len(request.Files))
	seen := make(map[string]bool, len(request.Files))
	for i, file := range request.Files {
		version := journal.Accepted.Versions[i]
		if file.Path == "" || seen[file.Path] || version.FileID == "" || version.VersionID == "" || version.Sequence < 1 || version.ExtractionID == "" || version.WorkID == "" || (version.Stage != "transform" && version.Stage != "index") {
			return nil, errors.New("invalid accepted file identity")
		}
		seen[file.Path] = true
		state, hasState := journal.State[file.Path]
		if !file.Deleted && hasState && (state.ContentHash != file.Source.ContentHash || state.Size != file.Source.Size) {
			return nil, errors.New("captured metadata does not match source")
		}
		heads = append(heads, localCapturedHead{ServerURL: journal.ServerURL, RootID: journal.RootID,
			Path: file.Path, Version: version, Source: file.Source, Deleted: file.Deleted, State: state,
			Dirty: !file.Deleted && (!hasState || slices.Contains(journal.Dirty, file.Path))})
	}
	return heads, nil
}

// A crash may leave a subset installed. Replaying the accepted journal repairs
// the rest; version sequences make older journal replays harmless. No root-wide
// JSON rewrite and no all-files transaction is needed for per-file acceptance.
func saveAcceptedCaptureHeads(dir string, journal captureJournal) error {
	heads, err := acceptedCaptureHeads(journal)
	if err != nil {
		return err
	}
	// The root-cache parent is created/durably owned by the caller.
	if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("captured heads busy: %w", err)
	}
	for _, head := range heads {
		current, err := loadCapturedHead(dir, head.ServerURL, head.RootID, head.Path)
		if err != nil {
			return err
		}
		if current != nil {
			if current.Version.Sequence == head.Version.Sequence && current.Version.VersionID != head.Version.VersionID {
				return errors.New("conflicting local captured version sequence")
			}
			if current.Version.Sequence >= head.Version.Sequence {
				continue
			}
		}
		if err = saveCaptureJSON(dir, capturedHeadName(head.ServerURL, head.RootID, head.Path), head); err != nil {
			return err
		}
	}
	parent, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func submitCapture(ctx context.Context, client *apiClient, spoolDir, headsDir string) (models.CaptureVersionsResponse, error) {
	accepted, err := resumeCaptureJournal(ctx, client, spoolDir)
	if err != nil {
		return accepted, err
	}
	var journal captureJournal
	if err = readCaptureJSON(filepath.Join(spoolDir, "journal.json"), &journal); err != nil {
		return accepted, err
	}
	// The accepted journal survives a local write failure; the next invocation
	// retries head installation without another upload or registration request.
	return accepted, saveAcceptedCaptureHeads(headsDir, journal)
}
