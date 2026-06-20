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

		if err := validateSourceRef(req.RootID, req.GenerationID, &req.Changes[i]); err != nil {
			return fmt.Errorf("invalid source for %q: %w", req.Changes[i].Path, err)
		}
	}

	for i := range req.ChangeRefs {
		req.ChangeRefs[i] = strings.TrimSpace(strings.ReplaceAll(req.ChangeRefs[i], "\\", "/"))
		if err := validateChangeRef(req.RootID, req.GenerationID, req.ChangeRefs[i]); err != nil {
			return err
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

	req.ContentProofRef = strings.TrimSpace(strings.ReplaceAll(req.ContentProofRef, "\\", "/"))
	if err := validateSyncArtifactRef(req.GenerationID, req.ContentProofRef, "proofs", "content_proof_ref"); err != nil {
		return err
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
	if err := validateSyncStateRef(req.RootID, req.GenerationID, req.StateRef); err != nil {
		return err
	}
	req.ManifestRef = strings.TrimSpace(strings.ReplaceAll(req.ManifestRef, "\\", "/"))
	if err := validateManifestRef(req.RootID, req.GenerationID, req.ManifestRef); err != nil {
		return err
	}

	return nil
}

func validateChangeRef(rootID, generationID, ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, fmt.Sprintf("syncs/%s/manifests/", generationID)) {
		return validateSyncArtifactRef(generationID, ref, "manifests", "change_refs")
	}
	return validateBundleObjectRef(rootID, ref, "change_refs")
}

func validateSyncArtifactRef(generationID, ref, dir, field string) error {
	ref = strings.TrimSpace(strings.ReplaceAll(ref, "\\", "/"))
	if ref == "" {
		return nil
	}
	if generationID == "" {
		return fmt.Errorf("%s requires generation_id", field)
	}
	if strings.Contains(ref, "\x00") {
		return fmt.Errorf("%s contains NUL byte", field)
	}
	prefix := fmt.Sprintf("syncs/%s/%s/", generationID, dir)
	if !strings.HasPrefix(ref, prefix) {
		return fmt.Errorf("%s must reference this generation's sync artifact", field)
	}
	name := strings.TrimPrefix(ref, prefix)
	if name == "" || name != safeObjectName(name) {
		return fmt.Errorf("%s sync artifact key is invalid", field)
	}
	return nil
}

func validateSyncStateRef(rootID, generationID, stateRef string) error {
	stateRef = strings.TrimSpace(strings.ReplaceAll(stateRef, "\\", "/"))
	if stateRef == "" {
		return nil
	}
	if generationID != "" && strings.HasPrefix(stateRef, fmt.Sprintf("syncs/%s/state/", generationID)) {
		return validateSyncArtifactRef(generationID, stateRef, "state", "state_ref")
	}
	return validateStateRef(rootID, stateRef)
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

func validateManifestRef(rootID, generationID, ref string) error {
	ref = strings.TrimSpace(strings.ReplaceAll(ref, "\\", "/"))
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, syncSourceBundlePrefix(generationID)) {
		return validateSyncSourceBundleRef(generationID, ref, "manifest_ref")
	}
	return validateBundleObjectRef(rootID, ref, "manifest_ref")
}

func validateSourceRef(rootID, generationID string, change *models.FileChange) error {
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

	if generationID != "" && strings.HasPrefix(change.SourceKey, syncSourceFilePrefix(generationID)) {
		path := strings.TrimPrefix(change.SourceKey, syncSourceFilePrefix(generationID))
		if path != change.Path {
			return fmt.Errorf("source file key must match change path")
		}
		if change.SourceOffset != 0 {
			return fmt.Errorf("source_offset must be zero for file uploads")
		}
		return nil
	}

	if generationID != "" && strings.HasPrefix(change.SourceKey, syncSourceBundlePrefix(generationID)) {
		if err := validateSyncSourceBundleRef(generationID, change.SourceKey, "source_key"); err != nil {
			return err
		}
		if change.SourceLength <= 0 {
			return fmt.Errorf("source_length must be positive for bundled files")
		}
		return nil
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

	return fmt.Errorf("source_key must reference this root's upload, bundle, or generation source")
}

func validateSyncSourceBundleRef(generationID, ref, field string) error {
	if generationID == "" {
		return fmt.Errorf("%s requires generation_id", field)
	}
	if strings.Contains(ref, "\x00") {
		return fmt.Errorf("%s contains NUL byte", field)
	}
	prefix := syncSourceBundlePrefix(generationID)
	if !strings.HasPrefix(ref, prefix) {
		return fmt.Errorf("%s must reference this generation's source bundle", field)
	}
	name := strings.TrimPrefix(ref, prefix)
	if name == "" || name != safeObjectName(name) {
		return fmt.Errorf("%s source bundle key is invalid", field)
	}
	return nil
}

func syncGenerationPrefix(generationID string) string {
	return fmt.Sprintf("syncs/%s/", generationID)
}

func syncSourceFilePrefix(generationID string) string {
	if generationID == "" {
		return ""
	}
	return fmt.Sprintf("syncs/%s/sources/files/", generationID)
}

func syncSourceBundlePrefix(generationID string) string {
	if generationID == "" {
		return ""
	}
	return fmt.Sprintf("syncs/%s/sources/bundles/", generationID)
}

func syncSourceFileKey(generationID, filePath string) string {
	return syncSourceFilePrefix(generationID) + filePath
}

func syncSourceBundleKey(generationID, bundleID string) string {
	return syncSourceBundlePrefix(generationID) + bundleID
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
