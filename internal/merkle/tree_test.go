package merkle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pufferfs/pufferfs/internal/ignore"
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
