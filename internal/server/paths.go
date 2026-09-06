package server

import (
	"fmt"
	pathpkg "path"
	"strings"
)

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

func safeObjectName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
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
