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

		if req.Changes[i].Status == models.StatusMoved || req.Changes[i].Status == models.StatusRenamed {
			if req.Changes[i].OldPath == "" {
				return fmt.Errorf("old_path is required for %s change %q", req.Changes[i].Status, req.Changes[i].Path)
			}
		}

		if err := validateSourceRef(req.RootID, &req.Changes[i]); err != nil {
			return fmt.Errorf("invalid source for %q: %w", req.Changes[i].Path, err)
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

	req.StateRef = strings.TrimSpace(strings.ReplaceAll(req.StateRef, "\\", "/"))
	if err := validateStateRef(req.RootID, req.StateRef); err != nil {
		return err
	}
	req.ManifestRef = strings.TrimSpace(strings.ReplaceAll(req.ManifestRef, "\\", "/"))
	if err := validateBundleObjectRef(req.RootID, req.ManifestRef, "manifest_ref"); err != nil {
		return err
	}

	return nil
}

func validateStateRef(rootID, stateRef string) error {
	stateRef = strings.TrimSpace(strings.ReplaceAll(stateRef, "\\", "/"))
	if stateRef == "" {
		return nil
	}
	if strings.Contains(stateRef, "\x00") {
		return fmt.Errorf("state_ref contains NUL byte")
	}
	statePrefix := fmt.Sprintf("states/%s/", rootID)
	bundlePrefix := fmt.Sprintf("bundles/%s/", rootID)
	if strings.HasPrefix(stateRef, statePrefix) {
		name := strings.TrimPrefix(stateRef, statePrefix)
		if name == "" || name != safeObjectName(name) {
			return fmt.Errorf("state_ref is invalid")
		}
		return nil
	}
	if strings.HasPrefix(stateRef, bundlePrefix) {
		name := strings.TrimPrefix(stateRef, bundlePrefix)
		if name == "" || name != safeObjectName(name) {
			return fmt.Errorf("state_ref bundle key is invalid")
		}
		return nil
	}
	return fmt.Errorf("state_ref must reference this root's state object")
}

func validateBundleObjectRef(rootID, ref, field string) error {
	ref = strings.TrimSpace(strings.ReplaceAll(ref, "\\", "/"))
	if ref == "" {
		return nil
	}
	if strings.Contains(ref, "\x00") {
		return fmt.Errorf("%s contains NUL byte", field)
	}
	bundlePrefix := fmt.Sprintf("bundles/%s/", rootID)
	if !strings.HasPrefix(ref, bundlePrefix) {
		return fmt.Errorf("%s must reference this root's bundle object", field)
	}
	name := strings.TrimPrefix(ref, bundlePrefix)
	if name == "" || name != safeObjectName(name) {
		return fmt.Errorf("%s bundle key is invalid", field)
	}
	return nil
}

func validateSourceRef(rootID string, change *models.FileChange) error {
	if change.SourceOffset < 0 {
		return fmt.Errorf("source_offset must be non-negative")
	}
	if change.SourceLength < 0 {
		return fmt.Errorf("source_length must be non-negative")
	}
	if change.Status != models.StatusAdded && change.Status != models.StatusModified {
		if change.SourceKey != "" || change.SourceOffset != 0 || change.SourceLength != 0 {
			return fmt.Errorf("source fields are only valid for added or modified files")
		}
		return nil
	}
	change.SourceKey = strings.TrimSpace(strings.ReplaceAll(change.SourceKey, "\\", "/"))
	if change.SourceKey == "" {
		if change.SourceOffset != 0 || change.SourceLength != 0 {
			return fmt.Errorf("source range requires source_key")
		}
		return nil
	}
	if strings.Contains(change.SourceKey, "\x00") {
		return fmt.Errorf("source_key contains NUL byte")
	}

	fileKey := fmt.Sprintf("files/%s/%s", rootID, change.Path)
	if change.SourceKey == fileKey {
		if change.SourceOffset != 0 {
			return fmt.Errorf("source_offset must be zero for file uploads")
		}
		return nil
	}

	bundlePrefix := fmt.Sprintf("bundles/%s/", rootID)
	if strings.HasPrefix(change.SourceKey, bundlePrefix) {
		name := strings.TrimPrefix(change.SourceKey, bundlePrefix)
		if name == "" || name != safeObjectName(name) {
			return fmt.Errorf("source bundle key is invalid")
		}
		if change.SourceLength <= 0 {
			return fmt.Errorf("source_length must be positive for bundled files")
		}
		return nil
	}

	return fmt.Errorf("source_key must reference this root's upload or bundle")
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
