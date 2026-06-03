package server

import (
	"fmt"
	pathpkg "path"
	"strings"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func normalizeSyncRequest(req *models.SyncRequest) error {
	for i := range req.Changes {
		path, err := cleanFilePath(req.Changes[i].Path)
		if err != nil {
			return fmt.Errorf("invalid change path %q: %w", req.Changes[i].Path, err)
		}
		req.Changes[i].Path = path

		if req.Changes[i].OldPath != "" {
			oldPath, err := cleanFilePath(req.Changes[i].OldPath)
			if err != nil {
				return fmt.Errorf("invalid old path %q: %w", req.Changes[i].OldPath, err)
			}
			req.Changes[i].OldPath = oldPath
		}
	}

	if req.State != nil {
		state := make(map[string]models.FileState, len(req.State))
		for rawPath, fileState := range req.State {
			path, err := cleanFilePath(rawPath)
			if err != nil {
				return fmt.Errorf("invalid state path %q: %w", rawPath, err)
			}
			if _, exists := state[path]; exists {
				return fmt.Errorf("duplicate state path after normalization: %s", path)
			}
			state[path] = fileState
		}
		req.State = state
	}

	if req.ContentProof != nil {
		fileHashes := make(map[string]string, len(req.ContentProof.FileHashes))
		for rawPath, hash := range req.ContentProof.FileHashes {
			path, err := cleanFilePath(rawPath)
			if err != nil {
				return fmt.Errorf("invalid proof file path %q: %w", rawPath, err)
			}
			if _, exists := fileHashes[path]; exists {
				return fmt.Errorf("duplicate proof file path after normalization: %s", path)
			}
			fileHashes[path] = hash
		}
		req.ContentProof.FileHashes = fileHashes

		dirHashes := make(map[string]string, len(req.ContentProof.DirHashes))
		for rawPath, hash := range req.ContentProof.DirHashes {
			path, err := cleanFilePath(rawPath)
			if err != nil {
				return fmt.Errorf("invalid proof dir path %q: %w", rawPath, err)
			}
			if _, exists := dirHashes[path]; exists {
				return fmt.Errorf("duplicate proof dir path after normalization: %s", path)
			}
			dirHashes[path] = hash
		}
		req.ContentProof.DirHashes = dirHashes
	}

	return nil
}

func cleanFilePath(filePath string) (string, error) {
	if strings.Contains(filePath, "\x00") {
		return "", fmt.Errorf("path contains NUL byte")
	}
	filePath = strings.TrimSpace(strings.ReplaceAll(filePath, "\\", "/"))
	if filePath == "" {
		return "", fmt.Errorf("path is empty")
	}
	cleaned := pathpkg.Clean(filePath)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("path must be a clean relative path")
	}
	return cleaned, nil
}

func cleanPathPrefix(prefix string) (string, error) {
	if strings.Contains(prefix, "\x00") {
		return "", fmt.Errorf("path prefix contains NUL byte")
	}
	prefix = strings.TrimSpace(strings.ReplaceAll(prefix, "\\", "/"))
	if prefix == "" {
		prefix = "/"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	cleaned := pathpkg.Clean(prefix)
	if cleaned == "/.." || strings.HasPrefix(cleaned, "/../") {
		return "", fmt.Errorf("path prefix must stay within the root")
	}
	if cleaned != "/" {
		cleaned += "/"
	}
	return cleaned, nil
}
