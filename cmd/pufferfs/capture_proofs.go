package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pufferfs/pufferfs/pkg/models"
	"golang.org/x/sys/unix"
)

// Called under the root capture-cache lock. Only locally verified matching
// bytes may become proof-only updates. Catalog hashes are not local evidence.
func bootstrapCapturedProofs(ctx context.Context, input captureSyncInput, headsDir string, remote map[string]models.CapturedFileHead, candidates []captureCandidate) ([]captureCandidate, error) {
	root, err := os.OpenRoot(input.Dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var changed []captureCandidate
	var heads []localCapturedHead
	var proofs []models.CapturedFileProof
	flush := func() error {
		if len(proofs) > 0 {
			body, err := input.Client.postContext(ctx, "/roots/"+url.PathEscape(input.RootID)+"/captured-proofs", models.CapturedProofsRequest{Files: proofs})
			if err != nil {
				return err
			}
			var response struct {
				Status string `json:"status"`
				Files  int    `json:"files"`
			}
			if err = json.Unmarshal(body, &response); err != nil {
				return err
			}
			if response.Status != "complete" || response.Files != len(proofs) {
				return errors.New("proof acknowledgement mismatch")
			}
		}
		if len(heads) > 0 {
			if err := os.MkdirAll(headsDir, 0700); err != nil {
				return err
			}
		}
		for _, head := range heads {
			current, err := loadCapturedHead(headsDir, head.ServerURL, head.RootID, head.Path)
			if err != nil {
				return err
			}
			if current != nil {
				if current.Version.Sequence > head.Version.Sequence {
					continue
				}
				if current.Version.Sequence == head.Version.Sequence && current.Version.VersionID != head.Version.VersionID {
					return errors.New("conflicting proof head sequence")
				}
				if current.Version.VersionID == head.Version.VersionID {
					// Retain append extents and processing IDs from prior capture.
					head.Source, head.Version = current.Source, current.Version
				}
			}
			if err = saveCaptureJSON(headsDir, capturedHeadName(head.ServerURL, head.RootID, head.Path), head); err != nil {
				return err
			}
		}
		heads, proofs = nil, nil
		return nil
	}
	for _, candidate := range candidates {
		file, exists := remote[candidate.Path]
		if !exists || file.Deleted || candidate.Size != file.Size {
			changed = append(changed, candidate)
			continue
		}
		state, stable, err := hashCapturedFile(ctx, root, file.Path)
		if err != nil {
			return nil, err
		}
		if !stable || state.ContentHash != file.ContentHash || state.Size != file.Size {
			changed = append(changed, candidate)
			continue
		}
		heads = append(heads, localCapturedHead{ServerURL: input.Client.baseURL, RootID: input.RootID, Path: file.Path,
			Version: models.RegisteredFileVersion{FileID: file.FileID, VersionID: file.VersionID, Sequence: file.Sequence}, State: state})
		if !file.ProofCurrent {
			proofs = append(proofs, models.CapturedFileProof{Path: file.Path, VersionID: file.VersionID, ContentHash: state.ContentHash})
		}
		if len(heads) == 128 {
			if err = flush(); err != nil {
				return nil, err
			}
		}
	}
	if err = flush(); err != nil {
		return nil, err
	}
	return changed, nil
}

// Hash a confined regular file with bounded reads. The boolean is false when
// its identity/metadata changed during the read; no caller may cache that hash.
func hashCapturedFile(ctx context.Context, root *os.Root, path string) (models.FileState, bool, error) {
	var empty models.FileState
	file, err := root.OpenFile(filepath.FromSlash(path), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return empty, false, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return empty, false, err
	}
	if !before.Mode().IsRegular() {
		return empty, false, nil
	}
	digest := sha256.New()
	reader := io.NewSectionReader(file, 0, before.Size())
	buffer := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return empty, false, err
		}
		n, readErr := reader.Read(buffer)
		_, _ = digest.Write(buffer[:n])
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return empty, false, readErr
		}
	}
	after, err := file.Stat()
	if err != nil {
		return empty, false, err
	}
	pathInfo, err := root.Stat(filepath.FromSlash(path))
	if err != nil {
		return empty, false, err
	}
	hash := fmt.Sprintf("sha256:%x", digest.Sum(nil))
	if !os.SameFile(before, pathInfo) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(pathInfo.ModTime()) {
		return empty, false, nil
	}
	return models.FileState{ContentHash: hash, Size: before.Size(), Mtime: before.ModTime().UnixNano()}, true, nil
}
