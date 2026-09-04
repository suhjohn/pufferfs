// Package diff computes filesystem diffs between two states.
package diff

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// Compute produces a DiffResult between previous and current filesystem states.
func Compute(prev, curr map[string]models.FileState) models.DiffResult {
	result := models.DiffResult{}

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
		}
	}

	removedPaths := make(map[string]models.FileState)
	removedByHash := make(map[string][]string)
	for path, st := range prev {
		if _, ok := curr[path]; !ok {
			removedPaths[path] = st
			removedByHash[st.ContentHash] = append(removedByHash[st.ContentHash], path)
		}
	}

	addedPaths := make(map[string]models.FileState)
	for path, st := range curr {
		if _, ok := prev[path]; !ok {
			addedPaths[path] = st
		}
	}

	for addedPath, addedSt := range addedPaths {
		paths := removedByHash[addedSt.ContentHash]
		if len(paths) == 0 {
			continue
		}
		removedPath := paths[len(paths)-1]
		removedByHash[addedSt.ContentHash] = paths[:len(paths)-1]
		delete(removedPaths, removedPath)
		delete(addedPaths, addedPath)
		status := classifyMove(removedPath, addedPath)
		result.Changes = append(result.Changes, models.FileChange{
			Path:        addedPath,
			Status:      status,
			OldPath:     removedPath,
			ContentHash: addedSt.ContentHash,
			Size:        addedSt.Size,
		})
		if status == models.StatusMoved {
			result.Stats.Moved++
		} else {
			result.Stats.Renamed++
		}
	}

	for path, st := range removedPaths {
		result.Changes = append(result.Changes, models.FileChange{
			Path:        path,
			Status:      models.StatusRemoved,
			ContentHash: st.ContentHash,
			Size:        st.Size,
		})
		result.Stats.Removed++
	}

	for path, st := range addedPaths {
		result.Changes = append(result.Changes, models.FileChange{
			Path:        path,
			Status:      models.StatusAdded,
			ContentHash: st.ContentHash,
			Size:        st.Size,
		})
		result.Stats.Added++
	}

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
