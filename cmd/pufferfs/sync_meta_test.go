package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/internal/merkle"
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

func TestLargeMovesFallBackToRemoveAndAdd(t *testing.T) {
	t.Setenv("PUFFERFS_MOVE_REUSE_MAX_BYTES", "1024")
	result := merkleChangesToDiffResult([]merkle.DiffChange{
		{Type: "removed", Path: "old.bin", ContentHash: "same", Size: 2048},
		{Type: "added", Path: "new.bin", ContentHash: "same", Size: 2048},
	}, &merkle.Tree{}, &merkle.Tree{})
	if result.Stats.Moved != 0 || result.Stats.Removed != 1 || result.Stats.Added != 1 {
		t.Fatalf("stats = %#v, want removed+added without move", result.Stats)
	}
	statuses := map[models.FileChangeStatus]bool{}
	for _, change := range result.Changes {
		statuses[change.Status] = true
	}
	if !statuses[models.StatusRemoved] || !statuses[models.StatusAdded] || statuses[models.StatusMoved] {
		t.Fatalf("changes = %#v", result.Changes)
	}
}

func TestResolveOnlyPathRelativeAndAbsolute(t *testing.T) {
	root := t.TempDir()
	rel, abs, err := resolveOnlyPath(root, "docs/a.md")
	if err != nil {
		t.Fatalf("resolve relative: %v", err)
	}
	wantAbs, err := canonicalPathAllowMissing(filepath.Join(root, "docs", "a.md"))
	if err != nil {
		t.Fatalf("canonicalPathAllowMissing: %v", err)
	}
	if rel != "docs/a.md" || abs != wantAbs {
		t.Fatalf("relative resolved to rel=%q abs=%q", rel, abs)
	}

	absoluteInput := filepath.Join(root, "docs", "b.md")
	rel, abs, err = resolveOnlyPath(root, absoluteInput)
	if err != nil {
		t.Fatalf("resolve absolute: %v", err)
	}
	wantAbs, err = canonicalPathAllowMissing(absoluteInput)
	if err != nil {
		t.Fatalf("canonicalPathAllowMissing: %v", err)
	}
	if rel != "docs/b.md" || abs != wantAbs {
		t.Fatalf("absolute resolved to rel=%q abs=%q", rel, abs)
	}
}

func TestResolveOnlyPathRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	_, _, err := resolveOnlyPath(root, filepath.Join(filepath.Dir(root), "outside.md"))
	if err == nil || !strings.Contains(err.Error(), "outside root") {
		t.Fatalf("resolve outside err = %v, want outside root", err)
	}
}

func TestResolveOnlyPathCanonicalizesAbsoluteSymlinkPath(t *testing.T) {
	realRoot := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(filepath.Join(realRoot, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir real root: %v", err)
	}
	writeTestFile(t, realRoot, "docs/a.md", "hello\n")
	linkRoot := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonicalRoot, err := canonicalLocalPath(linkRoot)
	if err != nil {
		t.Fatalf("canonicalLocalPath: %v", err)
	}
	rel, abs, err := resolveOnlyPath(canonicalRoot, filepath.Join(linkRoot, "docs", "a.md"))
	if err != nil {
		t.Fatalf("resolve symlink absolute path: %v", err)
	}
	if rel != "docs/a.md" {
		t.Fatalf("rel = %q, want docs/a.md", rel)
	}
	if !strings.HasPrefix(abs, canonicalRoot) {
		t.Fatalf("abs = %q, want under canonical root %q", abs, canonicalRoot)
	}
}

func TestBuildSyncOnlyDiffPatchesModifiedAndPreservesState(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "docs/a.md", "new content\n")
	baseState := map[string]models.FileState{
		"docs/a.md": {Size: 3, ContentHash: "sha256:old", Mtime: 1},
		"docs/b.md": {Size: 4, ContentHash: "sha256:stable", Mtime: 2},
	}
	result, merged, err := buildSyncOnlyDiff(root, []string{"docs/a.md"}, baseState, ignore.NewMatcher(root))
	if err != nil {
		t.Fatalf("buildSyncOnlyDiff: %v", err)
	}
	if countChanges(result) != 1 || result.Stats.Modified != 1 {
		t.Fatalf("result = %#v, want one modified change", result)
	}
	if got := merged["docs/b.md"]; got != baseState["docs/b.md"] {
		t.Fatalf("stable entry changed: %#v", got)
	}
	if got := merged["docs/a.md"]; got.ContentHash == "sha256:old" || got.Size == 0 {
		t.Fatalf("modified entry was not patched: %#v", got)
	}
}

func TestBuildSyncOnlyDiffRemovedAndMissingNoop(t *testing.T) {
	root := t.TempDir()
	baseState := map[string]models.FileState{
		"docs/a.md": {Size: 3, ContentHash: "sha256:a", Mtime: 1},
		"docs/b.md": {Size: 4, ContentHash: "sha256:b", Mtime: 2},
	}
	result, merged, err := buildSyncOnlyDiff(root, []string{"docs/a.md", "docs/missing.md"}, baseState, nil)
	if err != nil {
		t.Fatalf("buildSyncOnlyDiff: %v", err)
	}
	if countChanges(result) != 1 || result.Stats.Removed != 1 {
		t.Fatalf("result = %#v, want one removed change", result)
	}
	if _, ok := merged["docs/a.md"]; ok {
		t.Fatalf("removed entry still in merged state: %#v", merged)
	}
	if got := merged["docs/b.md"]; got != baseState["docs/b.md"] {
		t.Fatalf("unselected entry changed: %#v", got)
	}
}

func TestBuildSyncOnlyDiffRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	_, _, err := buildSyncOnlyDiff(root, []string{"docs"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory err = %v, want directory rejection", err)
	}
}

func TestBuildSyncOnlyDiffRejectsIgnoredFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".tpfsignore", "private/\n")
	writeTestFile(t, root, "private/a.md", "secret\n")
	_, _, err := buildSyncOnlyDiff(root, []string{"private/a.md"}, nil, ignore.NewMatcher(root))
	if err == nil || !strings.Contains(err.Error(), "ignored") {
		t.Fatalf("ignored err = %v, want ignored rejection", err)
	}
}

func TestResolveSyncOnlyRootByPathAndName(t *testing.T) {
	rootPath := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/roots" {
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]models.RootMetadata{
			{ID: "root-1", Name: "workspace", SourcePath: rootPath, VisibleGenerationID: "gen-1", VisibleGenerationSeq: 2},
		})
	}))
	defer server.Close()

	root, err := resolveSyncOnlyRoot(&apiClient{baseURL: server.URL, httpClient: server.Client()}, rootPath, "workspace", "")
	if err != nil {
		t.Fatalf("resolveSyncOnlyRoot: %v", err)
	}
	canonicalRoot, err := canonicalLocalPath(rootPath)
	if err != nil {
		t.Fatalf("canonicalLocalPath: %v", err)
	}
	if root.ID != "root-1" || root.CanonicalSourcePath != canonicalRoot {
		t.Fatalf("resolved root = %#v", root)
	}
}

func TestResolveSyncOnlyRootIDMismatch(t *testing.T) {
	rootPath := t.TempDir()
	otherPath := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/roots/root-1" {
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(models.RootMetadata{ID: "root-1", Name: "workspace", SourcePath: otherPath})
	}))
	defer server.Close()

	_, err := resolveSyncOnlyRoot(&apiClient{baseURL: server.URL, httpClient: server.Client()}, rootPath, "", "root-1")
	if err == nil || !strings.Contains(err.Error(), "not --root") {
		t.Fatalf("resolve mismatch err = %v, want not --root", err)
	}
}

func TestLoadSyncOnlyBaseStateUsesMatchingLocalCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rootPath := t.TempDir()
	rootID := "root-1"
	if err := saveRootMeta(rootID, "workspace", rootPath, "gen-1", 7); err != nil {
		t.Fatalf("saveRootMeta: %v", err)
	}
	localState := map[string]models.FileState{
		"docs/a.md": {Size: 10, ContentHash: "sha256:local", Mtime: 1},
	}
	if err := saveLocalState(rootID, localState); err != nil {
		t.Fatalf("saveLocalState: %v", err)
	}
	canonicalRoot, err := canonicalLocalPath(rootPath)
	if err != nil {
		t.Fatalf("canonicalLocalPath: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected remote state request: %s", r.URL.Path)
	}))
	defer server.Close()

	state, err := loadSyncOnlyBaseState(&apiClient{baseURL: server.URL, httpClient: server.Client()}, &syncOnlyRoot{
		RootMetadata: models.RootMetadata{
			ID:                   rootID,
			Name:                 "workspace",
			SourcePath:           rootPath,
			VisibleGenerationID:  "gen-1",
			VisibleGenerationSeq: 7,
		},
		CanonicalSourcePath: canonicalRoot,
	})
	if err != nil {
		t.Fatalf("loadSyncOnlyBaseState: %v", err)
	}
	if got := state["docs/a.md"]; got != localState["docs/a.md"] {
		t.Fatalf("state = %#v, want local cache %#v", state, localState)
	}
}

func writeTestFile(t *testing.T, root, relPath, content string) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(fullPath), err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
}
