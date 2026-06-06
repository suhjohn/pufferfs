// Package ignore implements gitignore-style file exclusion for PufferFs.
package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// alwaysIgnore are paths that are never synced.
var alwaysIgnore = []string{
	".git",
}

// defaultIgnore are built-in ignore patterns.
var defaultIgnore = []string{
	"node_modules",
	"__pycache__",
	".venv",
	"venv",
	"dist",
	"build",
	".tpfs",
	".next",
	".nuxt",
	".cache",
	".DS_Store",
	"Thumbs.db",
	"*.pyc",
	"*.pyo",
	"*.o",
	"*.so",
	"*.dylib",
	"*.class",
}

// secretPatterns are filename patterns that are excluded from sync by default.
var secretPatterns = []string{
	".env",
	".env.*",
	"*.pem",
	"*.key",
	"*_rsa",
	"id_rsa",
	"id_ed25519",
	"id_ecdsa",
	"credentials.json",
	"service-account*.json",
	"*.p12",
	"*.pfx",
	".npmrc",
	".pypirc",
}

// Matcher decides whether to ignore a path.
type Matcher struct {
	patterns []gitignore.Pattern
}

// NewMatcher builds a Matcher from the root directory.
// It loads .gitignore files, .tpfsignore, and global ignore config.
func NewMatcher(rootDir string) *Matcher {
	m := &Matcher{}

	// Always-ignore
	for _, p := range alwaysIgnore {
		m.patterns = append(m.patterns, gitignore.ParsePattern(p, nil))
	}

	// Default ignores
	for _, p := range defaultIgnore {
		m.patterns = append(m.patterns, gitignore.ParsePattern(p, nil))
	}

	// Load .gitignore from root
	m.loadIgnoreFile(filepath.Join(rootDir, ".gitignore"), nil)

	// Load .tpfsignore from root
	m.loadIgnoreFile(filepath.Join(rootDir, ".tpfsignore"), nil)

	// Load global ignore from ~/.tpfs/ignore
	home, err := os.UserHomeDir()
	if err == nil {
		m.loadIgnoreFile(filepath.Join(home, ".tpfs", "ignore"), nil)
	}

	return m
}

// ShouldIgnore returns true if the given relative path should be excluded.
func (m *Matcher) ShouldIgnore(relPath string, isDir bool) bool {
	if IsSecretFile(relPath) {
		return true
	}
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	for _, p := range m.patterns {
		if r := p.Match(parts, isDir); r == gitignore.Exclude {
			return true
		}
	}
	return false
}

// IsSecretFile returns true if the filename matches a secret pattern.
func IsSecretFile(relPath string) bool {
	name := filepath.Base(relPath)
	for _, pattern := range secretPatterns {
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

func (m *Matcher) loadIgnoreFile(path string, pathParts []string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m.patterns = append(m.patterns, gitignore.ParsePattern(line, pathParts))
	}
}
