package merkle

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestBuildTreeExcludesSecretFilesFromState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	tree, err := BuildTree(root, ignore.NewMatcher(root))
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	state := tree.ToFileStateMap()
	if _, ok := state[".env"]; ok {
		t.Fatalf(".env was included in sync state: %#v", state)
	}
	if _, ok := state["README.md"]; !ok {
		t.Fatalf("README.md missing from sync state: %#v", state)
	}
}

func TestBuildTreeWithStateCacheReusesHashWhenMetadataMatches(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "README.md")
	if err := os.WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mtime := time.Unix(1700000000, 123)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	tree, err := BuildTreeWithStateCache(root, ignore.NewMatcher(root), map[string]models.FileState{
		"README.md": {Size: 6, Mtime: mtime.UnixNano(), ContentHash: "sha256:cached"},
	})
	if err != nil {
		t.Fatalf("BuildTreeWithStateCache: %v", err)
	}
	state := tree.ToFileStateMap()
	if got := state["README.md"].ContentHash; got != "sha256:cached" {
		t.Fatalf("content hash = %q, want cached hash", got)
	}
}
