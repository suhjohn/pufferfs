package diff

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestComputeUnchanged(t *testing.T) {
	prev := map[string]models.FileState{
		"a.go": {Size: 100, ContentHash: "sha256:abc"},
	}
	curr := map[string]models.FileState{
		"a.go": {Size: 100, ContentHash: "sha256:abc"},
	}
	result := Compute(prev, curr)
	if result.Stats.Unchanged != 1 {
		t.Errorf("expected 1 unchanged, got %d", result.Stats.Unchanged)
	}
}

func TestComputeAdded(t *testing.T) {
	prev := map[string]models.FileState{}
	curr := map[string]models.FileState{
		"new.go": {Size: 50, ContentHash: "sha256:new"},
	}
	result := Compute(prev, curr)
	if result.Stats.Added != 1 {
		t.Errorf("expected 1 added, got %d", result.Stats.Added)
	}
}

func TestComputeRemoved(t *testing.T) {
	prev := map[string]models.FileState{
		"old.go": {Size: 50, ContentHash: "sha256:old"},
	}
	curr := map[string]models.FileState{}
	result := Compute(prev, curr)
	if result.Stats.Removed != 1 {
		t.Errorf("expected 1 removed, got %d", result.Stats.Removed)
	}
}

func TestComputeModified(t *testing.T) {
	prev := map[string]models.FileState{
		"a.go": {Size: 100, ContentHash: "sha256:v1"},
	}
	curr := map[string]models.FileState{
		"a.go": {Size: 120, ContentHash: "sha256:v2"},
	}
	result := Compute(prev, curr)
	if result.Stats.Modified != 1 {
		t.Errorf("expected 1 modified, got %d", result.Stats.Modified)
	}
}

func TestComputeMoved(t *testing.T) {
	prev := map[string]models.FileState{
		"src/a.go": {Size: 100, ContentHash: "sha256:abc"},
	}
	curr := map[string]models.FileState{
		"pkg/a.go": {Size: 100, ContentHash: "sha256:abc"},
	}
	result := Compute(prev, curr)
	if result.Stats.Moved != 1 {
		t.Errorf("expected 1 moved, got %d", result.Stats.Moved)
	}
	for _, c := range result.Changes {
		if c.Status == models.StatusMoved {
			if c.OldPath != "src/a.go" || c.Path != "pkg/a.go" {
				t.Errorf("move mismatch: old=%s new=%s", c.OldPath, c.Path)
			}
		}
	}
}

func TestComputeRenamed(t *testing.T) {
	prev := map[string]models.FileState{
		"src/old.go": {Size: 100, ContentHash: "sha256:abc"},
	}
	curr := map[string]models.FileState{
		"src/new.go": {Size: 100, ContentHash: "sha256:abc"},
	}
	result := Compute(prev, curr)
	if result.Stats.Renamed != 1 {
		t.Errorf("expected 1 renamed, got %d", result.Stats.Renamed)
	}
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "world.go"), []byte("package main"), 0o644)

	matcher := ignore.NewMatcher(dir)
	state, err := Scan(dir, matcher)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 2 {
		t.Errorf("expected 2 files, got %d", len(state))
	}
	if _, ok := state["hello.txt"]; !ok {
		t.Error("missing hello.txt")
	}
	if _, ok := state["sub/world.go"]; !ok {
		t.Error("missing sub/world.go")
	}
}

func TestScanIgnoresGit(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)

	matcher := ignore.NewMatcher(dir)
	state, err := Scan(dir, matcher)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 {
		t.Errorf("expected 1 file (excluding .git), got %d", len(state))
	}
}

func TestDetectSecrets(t *testing.T) {
	state := map[string]models.FileState{
		"main.go":                   {Size: 100},
		".env":                      {Size: 50},
		"certs/server.pem":          {Size: 200},
		"config/credentials.json":   {Size: 300},
		"README.md":                 {Size: 400},
	}
	secrets := DetectSecrets(state)
	if len(secrets) != 3 {
		t.Errorf("expected 3 secrets, got %d: %v", len(secrets), secrets)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1 KB"},
		{1048576, "1 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
