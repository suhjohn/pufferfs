package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const captureJournalReserve = int64(4 << 20)

func captureSpoolLimit() (int64, error) {
	value := os.Getenv("PUFFERFS_CAPTURE_SPOOL_BYTES")
	if value == "" {
		return 2 << 30, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 8<<20 {
		return 0, errors.New("PUFFERFS_CAPTURE_SPOOL_BYTES must be an integer of at least 8 MiB")
	}
	return limit, nil
}

// Called under the root sync lock. Heads are the durable append-reuse mapping;
// accepted pack bytes are redundant once those heads and API acceptance persist.
// Pending and conflicted captures are never evicted to make room for new work.
func retainCaptureSpools(cacheDir, serverURL, rootID string) (int64, error) {
	completed := filepath.Join(cacheDir, "completed")
	entries, err := os.ReadDir(completed)
	if err != nil {
		return 0, err
	}
	type receipt struct {
		name     string
		modified int64
	}
	var receipts []receipt
	var released int64
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "capture-") {
			return released, errors.New("unexpected completed capture entry; refusing cleanup")
		}
		dir := filepath.Join(completed, entry.Name())
		var journal captureJournal
		if err = readCaptureJSON(filepath.Join(dir, "journal.json"), &journal); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Receipt deletion may have stopped between unlink and rmdir.
				// Remove succeeds only for an empty directory.
				if err = os.Remove(dir); err == nil {
					continue
				}
			}
			return released, err
		}
		if journal.ServerURL != serverURL || journal.RootID != rootID {
			return released, errors.New("completed capture ownership mismatch")
		}
		heads, err := acceptedCaptureHeads(journal)
		if err != nil {
			return released, err
		}
		for _, head := range heads {
			current, err := loadCapturedHead(filepath.Join(cacheDir, "heads"), serverURL, rootID, head.Path)
			if err != nil {
				return released, err
			}
			if current == nil || current.Version.Sequence < head.Version.Sequence ||
				(current.Version.Sequence == head.Version.Sequence && current.Version.VersionID != head.Version.VersionID) {
				return released, errors.New("accepted capture heads are not durable; retaining pack bytes")
			}
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return released, err
		}
		for _, pack := range journal.Packs {
			info, statErr := root.Lstat(pack.Name)
			if errors.Is(statErr, os.ErrNotExist) {
				continue // A prior cleanup may have stopped after some removals.
			}
			if statErr != nil || !info.Mode().IsRegular() || info.Size() != pack.Size {
				root.Close()
				return released, errors.New("accepted pack changed; refusing cleanup")
			}
			if err = root.Remove(pack.Name); err != nil {
				root.Close()
				return released, err
			}
			released += info.Size()
		}
		root.Close()
		info, err := os.Stat(filepath.Join(dir, "journal.json"))
		if err != nil {
			return released, err
		}
		receipts = append(receipts, receipt{entry.Name(), info.ModTime().UnixNano()})
	}
	// Keep bounded diagnostic receipts; source manifests live in heads and S3.
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].modified > receipts[j].modified })
	for i := 64; i < len(receipts); i++ {
		if err := removeCaptureReceipt(filepath.Join(completed, receipts[i].name)); err != nil {
			return released, err
		}
	}
	return released, nil
}

// Remove only ordinary files in an already validated, flat capture directory.
// No recursive deletion and no traversal through symlinked files/directories.
func removeCaptureReceipt(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := file.ReadDir(-1)
	file.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || (entry.Name() != "journal.json" && entry.Name() != ".lock") {
			return errors.New("unexpected receipt contents; refusing cleanup")
		}
	}
	// Keep the journal until last so interrupted deletion remains resumable.
	for _, name := range []string{".lock", "journal.json"} {
		if err = root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Remove(dir)
}

func remainingCaptureSpoolBytes(cacheDir string, limit int64) (int64, error) {
	used := int64(0)
	for _, category := range []string{"pending", "completed", "conflicts"} {
		err := filepath.WalkDir(filepath.Join(cacheDir, category), func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) && path == filepath.Join(cacheDir, category) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return errors.New("unexpected nonregular spool entry")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > limit-used {
				return errors.New("capture spool limit exceeded; pending and conflicting captures were retained")
			}
			used += info.Size()
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	if limit-used <= captureJournalReserve {
		return 0, fmt.Errorf("capture spool is full; raise PUFFERFS_CAPTURE_SPOOL_BYTES or resolve retained captures")
	}
	return limit - used - captureJournalReserve, nil
}

// No published journal means creation never became submit-ready. Such files
// are temporary, unlike a pending/conflicted journal's immutable retry input.
func discardIncompleteCaptures(pendingDir string) (int64, error) {
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return 0, err
	}
	var released int64
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "capture-") {
			return released, errors.New("unexpected pending capture entry")
		}
		dir := filepath.Join(pendingDir, entry.Name())
		if _, err = os.Lstat(filepath.Join(dir, "journal.json")); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return released, err
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return released, err
		}
		file, err := root.Open(".")
		if err != nil {
			root.Close()
			return released, err
		}
		children, err := file.ReadDir(-1)
		file.Close()
		if err != nil {
			root.Close()
			return released, err
		}
		for _, child := range children {
			name := child.Name()
			if !child.Type().IsRegular() || (!strings.HasPrefix(name, "pack-") && !strings.HasPrefix(name, ".journal-")) {
				root.Close()
				return released, errors.New("unexpected incomplete capture contents; refusing cleanup")
			}
		}
		for _, child := range children {
			info, err := child.Info()
			if err == nil {
				err = root.Remove(child.Name())
			}
			if err != nil {
				root.Close()
				return released, err
			}
			released += info.Size()
		}
		root.Close()
		if err = os.Remove(dir); err != nil {
			return released, err
		}
	}
	return released, nil
}
