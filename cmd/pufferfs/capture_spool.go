package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/pufferfs/pufferfs/internal/sourcecapture"
	"github.com/pufferfs/pufferfs/pkg/models"
	"golang.org/x/sys/unix"
)

// Inputs have already passed discovery/ignore selection. This captures bytes,
// not a root-wide snapshot, and performs no network operations. The caller owns
// the returned directory, including on failure. A spool without journal.json
// is incomplete and must never be submitted. Failure after journal publication
// may leave a resumable capture; no remote writes have occurred either way.
func createCaptureSpool(ctx context.Context, parent, serverURL, rootID, sourceDir string, files []models.CaptureFile, previous map[string]localCapturedHead, packBytes, remainingBytes int64) (string, error) {
	if serverURL == "" || rootID == "" || len(files) < 1 || len(files) > 128 || packBytes < 1 || packBytes > 128<<20 {
		return "", errors.New("invalid capture spool inputs")
	}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if !filepath.IsLocal(file.Path) || path.Clean(file.Path) != file.Path || file.Path == "." || seen[file.Path] || file.Source != nil {
			return "", errors.New("capture paths must be unique root-relative paths without supplied sources")
		}
		seen[file.Path] = true
	}
	sourceRoot, err := os.OpenRoot(sourceDir)
	if err != nil {
		return "", err
	}
	defer sourceRoot.Close()
	dir, err := os.MkdirTemp(parent, "capture-*")
	if err != nil {
		return "", err
	}
	journal := captureJournal{Format: 1, ServerURL: serverURL, RootID: rootID,
		Request: models.CaptureVersionsRequest{CaptureID: uuid.NewString(), Files: append([]models.CaptureFile(nil), files...)},
		State:   make(map[string]models.FileState)}
	var pack *os.File
	var packDigest hash.Hash
	var packSize int64
	var packName string
	defer func() {
		if pack != nil {
			pack.Close()
		}
	}()
	finishPack := func() error {
		if pack == nil {
			return nil
		}
		if err := pack.Chmod(0400); err != nil {
			return err
		}
		if err := pack.Sync(); err != nil {
			return err
		}
		if err := pack.Close(); err != nil {
			return err
		}
		journal.Packs = append(journal.Packs, journalPack{Name: packName, Size: packSize, Digest: fmt.Sprintf("sha256:%x", packDigest.Sum(nil))})
		pack = nil
		return nil
	}
	buffer := make([]byte, 64<<10)
	for i := range journal.Request.Files {
		if err := ctx.Err(); err != nil {
			return dir, err
		}
		file := &journal.Request.Files[i]
		if file.Deleted {
			continue
		}
		err := func() error {
			// Opening a replaced FIFO must not block before the regular-file check.
			source, err := sourceRoot.OpenFile(filepath.FromSlash(file.Path), os.O_RDONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			defer source.Close()
			before, err := source.Stat()
			if err != nil {
				return err
			}
			if !before.Mode().IsRegular() {
				return errors.New("capture source is not a regular file")
			}
			manifest := models.SourceManifest{Format: 1, Size: before.Size()}
			digest := sha256.New()
			remaining := before.Size()
			prior, exists := previous[file.Path]
			if exists && !prior.Deleted && prior.Source != nil && prior.ServerURL == serverURL && prior.RootID == rootID && prior.Path == file.Path && prior.Version.VersionID == file.PreviousVersionID {
				verified, err := sourcecapture.HashVerifiedPrefix(captureContextReader{ctx, source}, before.Size(), *prior.Source)
				if err == nil {
					digest = verified
					manifest.Extents = append([]models.SourceExtent(nil), prior.Source.Extents...)
					remaining -= prior.Source.Size
				} else if errors.Is(err, sourcecapture.ErrPrefixChanged) {
					if _, err = source.Seek(0, io.SeekStart); err != nil {
						return err
					}
				} else {
					return err
				}
			}
			for remaining > 0 {
				if pack == nil {
					packName = fmt.Sprintf("pack-%06d", len(journal.Packs))
					pack, err = os.OpenFile(filepath.Join(dir, packName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
					if err != nil {
						return err
					}
					packSize, packDigest = 0, sha256.New()
				}
				length := min(remaining, packBytes-packSize)
				if length > remainingBytes {
					return errors.New("capture exceeds the local spool limit; raise PUFFERFS_CAPTURE_SPOOL_BYTES")
				}
				// Copy the fixed observed extent. Later appends belong to a new
				// capture; truncation produces an error, never a shorter success.
				n, err := io.CopyBuffer(io.MultiWriter(pack, packDigest, digest), io.LimitReader(captureContextReader{ctx, source}, length), buffer)
				if err != nil {
					return err
				}
				if n != length {
					return io.ErrUnexpectedEOF
				}
				manifest.Extents = append(manifest.Extents, models.SourceExtent{ObjectKey: "local:" + packName, Offset: packSize, Length: length})
				packSize += length
				remainingBytes -= length
				remaining -= length
				if packSize == packBytes {
					if err := finishPack(); err != nil {
						return err
					}
				}
			}
			manifest.ContentHash = fmt.Sprintf("sha256:%x", digest.Sum(nil))
			file.Source = &manifest
			journal.State[file.Path] = models.FileState{Size: manifest.Size, ContentHash: manifest.ContentHash, Mtime: before.ModTime().UnixNano()}
			after, descriptorErr := source.Stat()
			atPath, pathErr := sourceRoot.Stat(filepath.FromSlash(file.Path))
			if descriptorErr != nil || pathErr != nil || !os.SameFile(before, atPath) || before.Size() != after.Size() || before.Size() != atPath.Size() || !before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(atPath.ModTime()) {
				journal.Dirty = append(journal.Dirty, file.Path)
			}
			return nil
		}()
		if err != nil {
			return dir, fmt.Errorf("capturing %s: %w", file.Path, err)
		}
	}
	if err = finishPack(); err != nil {
		return dir, err
	}
	if err = ctx.Err(); err != nil {
		return dir, err
	}
	// Publish the journal last, after every referenced pack is durable.
	if err = saveCaptureJournal(dir, journal); err != nil {
		return dir, err
	}
	parentDir, err := os.Open(parent)
	if err != nil {
		return dir, err
	}
	defer parentDir.Close()
	if err = parentDir.Sync(); err != nil {
		return dir, err
	}
	return dir, nil
}

type captureContextReader struct {
	ctx context.Context
	io.Reader
}

func (r captureContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}
