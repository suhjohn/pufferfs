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
	patterns     []gitignore.Pattern
	checkSecrets bool
}

// PolicyPatternSet contains centrally managed ignore rules.
type PolicyPatternSet struct {
	OrgPatterns  string
	UserPatterns string
}

// NewMatcher builds a Matcher from the root directory.
// It loads .gitignore files, .tpfsignore, and global ignore config.
func NewMatcher(rootDir string) *Matcher {
	return NewMatcherWithPolicy(rootDir, PolicyPatternSet{})
}

// NewMatcherWithPolicy builds a Matcher from central, global, and project rules.
func NewMatcherWithPolicy(rootDir string, policy PolicyPatternSet) *Matcher {
	m := &Matcher{checkSecrets: true}

	// Always-ignore
	for _, p := range alwaysIgnore {
		m.patterns = append(m.patterns, gitignore.ParsePattern(p, nil))
	}

	// Default ignores
	for _, p := range defaultIgnore {
		m.patterns = append(m.patterns, gitignore.ParsePattern(p, nil))
	}

	m.loadPatternText(policy.OrgPatterns, nil)
	m.loadPatternText(policy.UserPatterns, nil)

	// Load global ignore from ~/.tpfs/.tpfsignore
	home, err := os.UserHomeDir()
	if err == nil {
		m.loadIgnoreFile(filepath.Join(home, ".tpfs", ".tpfsignore"), nil)
	}

	m.loadIgnoreFiles(rootDir)

	return m
}

// NewPolicyMatcher builds a matcher for centrally managed policy only.
func NewPolicyMatcher(policy PolicyPatternSet) *Matcher {
	m := &Matcher{}
	m.loadPatternText(policy.OrgPatterns, nil)
	m.loadPatternText(policy.UserPatterns, nil)
	return m
}

// ShouldIgnore returns true if the given relative path should be excluded.
func (m *Matcher) ShouldIgnore(relPath string, isDir bool) bool {
	if m.checkSecrets && IsSecretFile(relPath) {
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

	m.loadPatternScanner(bufio.NewScanner(f), pathParts)
}

func (m *Matcher) loadPatternText(text string, pathParts []string) {
	m.loadPatternScanner(bufio.NewScanner(strings.NewReader(text)), pathParts)
}

func (m *Matcher) loadPatternScanner(scanner *bufio.Scanner, pathParts []string) {
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m.patterns = append(m.patterns, gitignore.ParsePattern(line, pathParts))
	}
}

func (m *Matcher) loadIgnoreFiles(rootDir string) {
	rootDir = filepath.Clean(rootDir)
	_ = filepath.WalkDir(rootDir, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name != ".gitignore" && name != ".tpfsignore" {
			return nil
		}
		relDir, err := filepath.Rel(rootDir, filepath.Dir(filePath))
		if err != nil {
			return nil
		}
		var pathParts []string
		if relDir != "." {
			pathParts = strings.Split(filepath.ToSlash(relDir), "/")
		}
		m.loadIgnoreFile(filePath, pathParts)
		return nil
	})
}
