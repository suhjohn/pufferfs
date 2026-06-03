package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAlwaysIgnoreGit(t *testing.T) {
	dir := t.TempDir()
	m := NewMatcher(dir)

	if !m.ShouldIgnore(".git", true) {
		t.Error(".git should be ignored")
	}
	if !m.ShouldIgnore(".git/HEAD", false) {
		t.Error(".git/HEAD should be ignored")
	}
}

func TestDefaultIgnores(t *testing.T) {
	dir := t.TempDir()
	m := NewMatcher(dir)

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"__pycache__", true, true},
		{".venv", true, true},
		{".DS_Store", false, true},
		{"main.go", false, false},
		{"src/app.py", false, false},
	}

	for _, c := range cases {
		got := m.ShouldIgnore(c.path, c.isDir)
		if got != c.want {
			t.Errorf("ShouldIgnore(%q, %v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestGitignorePatterns(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\nbuild/\n"), 0o644)

	m := NewMatcher(dir)

	if !m.ShouldIgnore("app.log", false) {
		t.Error("*.log should be ignored via .gitignore")
	}
	if !m.ShouldIgnore("build", true) {
		t.Error("build/ should be ignored via .gitignore")
	}
	if m.ShouldIgnore("main.go", false) {
		t.Error("main.go should not be ignored")
	}
}

func TestTpfsignore(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".tpfsignore"), []byte("secret_dir/\n*.tmp\n"), 0o644)

	m := NewMatcher(dir)

	if !m.ShouldIgnore("secret_dir", true) {
		t.Error("secret_dir should be ignored via .tpfsignore")
	}
	if !m.ShouldIgnore("temp.tmp", false) {
		t.Error("*.tmp should be ignored via .tpfsignore")
	}
}

func TestIsSecretFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".env", true},
		{".env.production", true},
		{"server.pem", true},
		{"id_rsa", true},
		{"id_ed25519", true},
		{"credentials.json", true},
		{"main.go", false},
		{"README.md", false},
		{"config.toml", false},
	}

	for _, c := range cases {
		got := IsSecretFile(c.path)
		if got != c.want {
			t.Errorf("IsSecretFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
