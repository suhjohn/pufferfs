package server

import (
	"fmt"
	pathpkg "path"
	"strings"
)

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
