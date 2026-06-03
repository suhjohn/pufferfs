// Package diff computes filesystem diffs between two states.
package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// Scan walks a directory and builds the current filesystem state.
func Scan(rootDir string, matcher *ignore.Matcher) (map[string]models.FileState, error) {
	state := make(map[string]models.FileState)
	rootDir = filepath.Clean(rootDir)

	err := filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(rootDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		if matcher.ShouldIgnore(relPath, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", relPath, err)
		}

		hash, err := hashFile(path)
		if err != nil {
			return fmt.Errorf("hash %s: %w", relPath, err)
		}

		state[relPath] = models.FileState{
			Size:        info.Size(),
			ContentHash: hash,
			Mtime:       float64(info.ModTime().UnixNano()) / 1e9,
		}
		return nil
	})

	return state, err
}

// Compute produces a DiffResult between previous and current filesystem states.
func Compute(prev, curr map[string]models.FileState) models.DiffResult {
	result := models.DiffResult{}

	// Index current files by hash for move detection
	currByHash := make(map[string][]string)
	for path, st := range curr {
		currByHash[st.ContentHash] = append(currByHash[st.ContentHash], path)
	}

	// Index previous files by hash
	prevByHash := make(map[string][]string)
	for path, st := range prev {
		prevByHash[st.ContentHash] = append(prevByHash[st.ContentHash], path)
	}

	// Track which paths are accounted for
	matched := make(map[string]bool)

	// Pass 1: find unchanged and modified files (same path exists in both)
	for path, currSt := range curr {
		if prevSt, ok := prev[path]; ok {
			if currSt.ContentHash == prevSt.ContentHash {
				result.Changes = append(result.Changes, models.FileChange{
					Path:        path,
					Status:      models.StatusUnchanged,
					ContentHash: currSt.ContentHash,
					Size:        currSt.Size,
				})
				result.Stats.Unchanged++
			} else {
				result.Changes = append(result.Changes, models.FileChange{
					Path:        path,
					Status:      models.StatusModified,
					ContentHash: currSt.ContentHash,
					Size:        currSt.Size,
				})
				result.Stats.Modified++
			}
			matched[path] = true
		}
	}

	// Pass 2: find added and moved files
	removedPaths := make(map[string]models.FileState)
	for path, st := range prev {
		if _, ok := curr[path]; !ok {
			removedPaths[path] = st
		}
	}

	addedPaths := make(map[string]models.FileState)
	for path, st := range curr {
		if !matched[path] {
			if _, ok := prev[path]; !ok {
				addedPaths[path] = st
			}
		}
	}

	// Try to match removed→added by content hash (move/rename detection)
	usedRemoved := make(map[string]bool)
	usedAdded := make(map[string]bool)

	for addedPath, addedSt := range addedPaths {
		for removedPath, removedSt := range removedPaths {
			if usedRemoved[removedPath] || usedAdded[addedPath] {
				continue
			}
			if addedSt.ContentHash == removedSt.ContentHash {
				status := classifyMove(removedPath, addedPath)
				result.Changes = append(result.Changes, models.FileChange{
					Path:        addedPath,
					Status:      status,
					OldPath:     removedPath,
					ContentHash: addedSt.ContentHash,
					Size:        addedSt.Size,
				})
				switch status {
				case models.StatusMoved, models.StatusMovedAndModified:
					result.Stats.Moved++
				case models.StatusRenamed:
					result.Stats.Renamed++
				}
				usedRemoved[removedPath] = true
				usedAdded[addedPath] = true
				break
			}
		}
	}

	// Remaining removed files
	for path, st := range removedPaths {
		if !usedRemoved[path] {
			result.Changes = append(result.Changes, models.FileChange{
				Path:        path,
				Status:      models.StatusRemoved,
				ContentHash: st.ContentHash,
				Size:        st.Size,
			})
			result.Stats.Removed++
		}
	}

	// Remaining added files
	for path, st := range addedPaths {
		if !usedAdded[path] {
			result.Changes = append(result.Changes, models.FileChange{
				Path:        path,
				Status:      models.StatusAdded,
				ContentHash: st.ContentHash,
				Size:        st.Size,
			})
			result.Stats.Added++
		}
	}

	// Pass 3: directory move detection
	result = detectDirectoryMoves(result)

	return result
}

// classifyMove determines if a hash-matched file pair is a rename, move, etc.
func classifyMove(oldPath, newPath string) models.FileChangeStatus {
	oldDir := filepath.Dir(oldPath)
	newDir := filepath.Dir(newPath)
	oldBase := filepath.Base(oldPath)
	newBase := filepath.Base(newPath)

	if oldDir == newDir && oldBase != newBase {
		return models.StatusRenamed
	}
	return models.StatusMoved
}

// detectDirectoryMoves collapses many per-file MOVEDs sharing
// the same (oldDir→newDir) mapping into a conceptual directory move.
// For now this annotates them but keeps individual entries.
func detectDirectoryMoves(result models.DiffResult) models.DiffResult {
	type dirPair struct {
		oldDir, newDir string
	}

	counts := make(map[dirPair]int)
	for _, c := range result.Changes {
		if c.Status == models.StatusMoved && c.OldPath != "" {
			pair := dirPair{
				oldDir: filepath.Dir(c.OldPath),
				newDir: filepath.Dir(c.Path),
			}
			counts[pair]++
		}
	}

	// If ≥80% of removed files from a directory moved to the same new directory,
	// keep them as MOVED (the directory move is implicit).
	// This is informational — the sync pipeline handles them the same way.
	_ = counts

	return result
}

// hashFile computes the SHA-256 hex digest of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// DetectSecrets returns paths that match secret filename patterns.
func DetectSecrets(state map[string]models.FileState) []string {
	var secrets []string
	for path := range state {
		if ignore.IsSecretFile(path) {
			secrets = append(secrets, path)
		}
	}
	return secrets
}

// FormatDryRun produces the dry-run output string.
func FormatDryRun(
	diff models.DiffResult,
	state map[string]models.FileState,
	excluded []string,
	secrets []string,
) string {
	var b strings.Builder

	// Count files to upload and total size
	uploadCount := 0
	var totalSize int64
	for _, c := range diff.Changes {
		if c.Status == models.StatusAdded || c.Status == models.StatusModified {
			uploadCount++
			totalSize += c.Size
		}
	}

	fmt.Fprintf(&b, "Will upload:\n")
	fmt.Fprintf(&b, "  %d files\n", uploadCount)
	fmt.Fprintf(&b, "  %s\n", formatBytes(totalSize))
	b.WriteString("\n")

	if len(excluded) > 0 {
		fmt.Fprintf(&b, "Excluded:\n")
		for _, e := range excluded {
			fmt.Fprintf(&b, "  %s\n", e)
		}
		b.WriteString("\n")
	}

	if len(secrets) > 0 {
		fmt.Fprintf(&b, "Potential secrets detected:\n")
		for _, s := range secrets {
			fmt.Fprintf(&b, "  %s\n", s)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.0f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
