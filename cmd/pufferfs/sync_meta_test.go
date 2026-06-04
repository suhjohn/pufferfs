package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestRootMetaPersistsGenerationBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sourcePath := filepath.Join(home, "workspace")
	if err := saveRootMeta("root-1", "workspace", sourcePath, "gen-1", 42); err != nil {
		t.Fatalf("saveRootMeta: %v", err)
	}

	meta, err := loadRootMeta("root-1")
	if err != nil {
		t.Fatalf("loadRootMeta: %v", err)
	}
	if meta.ID != "root-1" || meta.Name != "workspace" || meta.SourcePath != sourcePath {
		t.Fatalf("loaded root meta = %#v", meta)
	}
	if meta.GenerationID != "gen-1" || meta.GenerationSeq != 42 {
		t.Fatalf("loaded generation = %s/%d, want gen-1/42", meta.GenerationID, meta.GenerationSeq)
	}

	found, err := findLocalRootMeta("workspace", sourcePath)
	if err != nil {
		t.Fatalf("findLocalRootMeta: %v", err)
	}
	if found.ID != "root-1" || found.GenerationID != "gen-1" || found.GenerationSeq != 42 {
		t.Fatalf("found root meta = %#v", found)
	}
}

func TestLocalCacheMatchesRemote(t *testing.T) {
	local := &rootMeta{
		GenerationID:  "gen-1",
		GenerationSeq: 7,
	}
	remote := &models.RootMetadata{
		VisibleGenerationID:  "gen-1",
		VisibleGenerationSeq: 7,
	}
	if !localCacheMatchesRemote(local, remote) {
		t.Fatal("matching local and remote generations should use local cache")
	}

	remote.VisibleGenerationID = "gen-2"
	if localCacheMatchesRemote(local, remote) {
		t.Fatal("stale local generation should not use local cache")
	}
}

func TestSyncConflictFromAPIError(t *testing.T) {
	body, err := json.Marshal(models.SyncConflictResponse{
		Error:                   "sync base generation is stale",
		ClientBaseGenerationID:  "gen-1",
		ClientBaseGenerationSeq: 1,
		CurrentGenerationID:     "gen-2",
		CurrentGenerationSeq:    2,
	})
	if err != nil {
		t.Fatalf("marshal conflict response: %v", err)
	}

	conflict, ok := syncConflictFromError(&apiError{StatusCode: 409, Body: body})
	if !ok {
		t.Fatal("syncConflictFromError did not parse conflict response")
	}
	if conflict.CurrentGenerationID != "gen-2" || conflict.CurrentGenerationSeq != 2 {
		t.Fatalf("conflict = %#v", conflict)
	}
}

func TestWithAbsolutePaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	changes := withAbsolutePaths(root, []models.FileChange{{Path: "src/main.go", Status: models.StatusAdded}})
	want := filepath.Join(root, "src", "main.go")
	if len(changes) != 1 || changes[0].AbsolutePath != want {
		t.Fatalf("absolute path = %#v, want %q", changes, want)
	}
}
