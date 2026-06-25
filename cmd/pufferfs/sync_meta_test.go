package main

import (
	"encoding/json"
	"io"
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

func TestResolveOrCreateRootCreatesVectorDisabledRoot(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/roots":
			_ = json.NewEncoder(w).Encode([]models.RootMetadata{})
		case r.Method == http.MethodPost && r.URL.Path == "/roots":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode create root: %v", err)
			}
			requests <- req
			_ = json.NewEncoder(w).Encode(models.RootMetadata{ID: "root-1", Name: "workspace", VectorDisabled: true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	root, err := resolveOrCreateRoot(&apiClient{baseURL: server.URL, httpClient: server.Client()}, "workspace", "/tmp/workspace", "org", true, io.Discard)
	if err != nil {
		t.Fatalf("resolveOrCreateRoot: %v", err)
	}
	if !root.VectorDisabled {
		t.Fatalf("root should be vector-disabled: %#v", root)
	}
	req := <-requests
	if req["vector_disabled"] != true {
		t.Fatalf("vector_disabled payload = %#v", req["vector_disabled"])
	}
}

func TestResolveOrCreateRootRejectsNoVectorForVectorEnabledRoot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/roots" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]models.RootMetadata{{ID: "root-1", Name: "workspace"}})
	}))
	defer server.Close()

	_, err := resolveOrCreateRoot(&apiClient{baseURL: server.URL, httpClient: server.Client()}, "workspace", "/tmp/workspace", "org", true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already supports vector search") {
		t.Fatalf("err = %v, want vector-enabled root rejection", err)
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

func TestResolveSyncDirectoryArg(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		rootPath string
		onlyMode bool
		want     string
		wantErr  string
	}{
		{name: "default current directory", want: "."},
		{name: "positional path", args: []string{"./handbook"}, want: "./handbook"},
		{name: "root path full sync", rootPath: "/Users/me/handbook", want: "/Users/me/handbook"},
		{name: "only mode uses root path", rootPath: "/Users/me/handbook", onlyMode: true, want: "/Users/me/handbook"},
		{name: "only mode defaults current directory", onlyMode: true, want: "."},
		{name: "positional and root conflict", args: []string{"./handbook"}, rootPath: "/Users/me/handbook", wantErr: "both as argument and --root"},
		{name: "only mode positional path", args: []string{"./handbook"}, onlyMode: true, want: "./handbook"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSyncDirectoryArg(tt.args, tt.rootPath, tt.onlyMode)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSyncDirectoryArg: %v", err)
			}
			if got != tt.want {
				t.Fatalf("dir = %q, want %q", got, tt.want)
			}
		})
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

func TestForcedSyncDiffMarksCurrentFilesForReindex(t *testing.T) {
	previous := map[string]models.FileState{
		"same.md":    {Size: 5, ContentHash: "sha256:same"},
		"changed.md": {Size: 3, ContentHash: "sha256:old"},
		"removed.md": {Size: 7, ContentHash: "sha256:removed"},
	}
	current := map[string]models.FileState{
		"same.md":    {Size: 5, ContentHash: "sha256:same"},
		"changed.md": {Size: 4, ContentHash: "sha256:new"},
		"added.md":   {Size: 2, ContentHash: "sha256:added"},
	}

	result := forcedSyncDiff(previous, current)

	if result.Stats.Modified != 2 || result.Stats.Added != 1 || result.Stats.Removed != 1 || result.Stats.Unchanged != 0 {
		t.Fatalf("stats = %#v", result.Stats)
	}
	statuses := map[string]models.FileChangeStatus{}
	for _, change := range result.Changes {
		statuses[change.Path] = change.Status
	}
	if statuses["same.md"] != models.StatusModified {
		t.Fatalf("same.md status = %s, want MODIFIED", statuses["same.md"])
	}
	if statuses["changed.md"] != models.StatusModified {
		t.Fatalf("changed.md status = %s, want MODIFIED", statuses["changed.md"])
	}
	if statuses["added.md"] != models.StatusAdded {
		t.Fatalf("added.md status = %s, want ADDED", statuses["added.md"])
	}
	if statuses["removed.md"] != models.StatusRemoved {
		t.Fatalf("removed.md status = %s, want REMOVED", statuses["removed.md"])
	}
}

func TestResolveSelectedPathRelativeAndAbsolute(t *testing.T) {
	root := t.TempDir()
	rel, abs, err := resolveSelectedPath(root, "docs/a.md", "--include")
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
	rel, abs, err = resolveSelectedPath(root, absoluteInput, "--include")
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

func TestResolveSelectedPathRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	_, _, err := resolveSelectedPath(root, filepath.Join(filepath.Dir(root), "outside.md"), "--include")
	if err == nil || !strings.Contains(err.Error(), "outside root") {
		t.Fatalf("resolve outside err = %v, want outside root", err)
	}
}

func TestResolveSelectedPathCanonicalizesAbsoluteSymlinkPath(t *testing.T) {
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
	rel, abs, err := resolveSelectedPath(canonicalRoot, filepath.Join(linkRoot, "docs", "a.md"), "--include")
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

func TestCompiledSyncSubsetSpecMatchesIncludesAndExcludes(t *testing.T) {
	root := t.TempDir()
	spec, err := compileSyncSubsetSpec(root, syncSubsetSpec{
		Includes: []string{"docs/**", "README.md", "src/**/*.go"},
		Excludes: []string{"docs/private/**", "src/**/testdata/**"},
	})
	if err != nil {
		t.Fatalf("compileSyncSubsetSpec: %v", err)
	}

	for _, path := range []string{
		"docs/a.md",
		"docs/nested/a.md",
		"README.md",
		"src/main.go",
		"src/pkg/worker.go",
	} {
		if !spec.matches(path) {
			t.Fatalf("matches(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		"docs/private/a.md",
		"src/pkg/testdata/input.go",
		"notes/todo.md",
	} {
		if spec.matches(path) {
			t.Fatalf("matches(%q) = true, want false", path)
		}
	}
}

func TestBuildSyncSubsetDiffPreservesUnselectedAndExcludedState(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "new readme\n")
	writeTestFile(t, root, "docs/a.md", "new docs\n")
	writeTestFile(t, root, "docs/private.md", "new private\n")
	writeTestFile(t, root, "src/main.go", "new src\n")

	baseState := map[string]models.FileState{
		"README.md":       {Size: 3, ContentHash: "sha256:old-readme", Mtime: 1},
		"docs/a.md":       {Size: 3, ContentHash: "sha256:old-docs", Mtime: 1},
		"docs/private.md": {Size: 3, ContentHash: "sha256:old-private", Mtime: 1},
		"src/main.go":     {Size: 3, ContentHash: "sha256:old-src", Mtime: 1},
	}
	spec, err := compileSyncSubsetSpec(root, syncSubsetSpec{
		Includes: []string{"docs/**", "README.md"},
		Excludes: []string{"docs/private.md"},
	})
	if err != nil {
		t.Fatalf("compileSyncSubsetSpec: %v", err)
	}

	result, merged, err := buildSyncSubsetDiff(root, spec, baseState, ignore.NewMatcher(root), false)
	if err != nil {
		t.Fatalf("buildSyncSubsetDiff: %v", err)
	}
	if countChanges(result) != 2 || result.Stats.Modified != 2 {
		t.Fatalf("result = %#v, want two modified changes", result)
	}
	changed := map[string]bool{}
	for _, change := range filterChanges(result) {
		changed[change.Path] = true
	}
	for _, path := range []string{"README.md", "docs/a.md"} {
		if !changed[path] {
			t.Fatalf("missing changed path %s in %#v", path, result.Changes)
		}
		if merged[path].ContentHash == baseState[path].ContentHash {
			t.Fatalf("selected path %s was not updated in merged state", path)
		}
	}
	for _, path := range []string{"docs/private.md", "src/main.go"} {
		if changed[path] {
			t.Fatalf("unselected/excluded path %s changed: %#v", path, result.Changes)
		}
		if merged[path] != baseState[path] {
			t.Fatalf("path %s was not preserved: got %#v want %#v", path, merged[path], baseState[path])
		}
	}
}

func TestBuildSyncSubsetDiffRemovesOnlySelectedMissingPaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "docs/private.md", "private still exists\n")
	writeTestFile(t, root, "src/main.go", "src still exists\n")

	baseState := map[string]models.FileState{
		"docs/a.md":       {Size: 3, ContentHash: "sha256:old-docs", Mtime: 1},
		"docs/private.md": {Size: 3, ContentHash: "sha256:old-private", Mtime: 1},
		"src/main.go":     {Size: 3, ContentHash: "sha256:old-src", Mtime: 1},
	}
	spec, err := compileSyncSubsetSpec(root, syncSubsetSpec{
		Includes: []string{"docs/**"},
		Excludes: []string{"docs/private.md"},
	})
	if err != nil {
		t.Fatalf("compileSyncSubsetSpec: %v", err)
	}

	result, merged, err := buildSyncSubsetDiff(root, spec, baseState, ignore.NewMatcher(root), false)
	if err != nil {
		t.Fatalf("buildSyncSubsetDiff: %v", err)
	}
	if countChanges(result) != 1 || result.Stats.Removed != 1 {
		t.Fatalf("result = %#v, want one removal", result)
	}
	if _, ok := merged["docs/a.md"]; ok {
		t.Fatalf("selected missing path was preserved: %#v", merged)
	}
	for _, path := range []string{"docs/private.md", "src/main.go"} {
		if got := merged[path]; got != baseState[path] {
			t.Fatalf("path %s was not preserved: got %#v want %#v", path, got, baseState[path])
		}
	}
}

func TestBuildSyncSubsetDiffForceReindexesSelectedUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "docs/a.md", "same\n")
	writeTestFile(t, root, "docs/private.md", "same\n")

	stateA, err := fileStateForPath(filepath.Join(root, "docs", "a.md"), mustStat(t, filepath.Join(root, "docs", "a.md")))
	if err != nil {
		t.Fatalf("file state a: %v", err)
	}
	privateState, err := fileStateForPath(filepath.Join(root, "docs", "private.md"), mustStat(t, filepath.Join(root, "docs", "private.md")))
	if err != nil {
		t.Fatalf("file state private: %v", err)
	}
	baseState := map[string]models.FileState{
		"docs/a.md":       stateA,
		"docs/private.md": privateState,
	}
	spec, err := compileSyncSubsetSpec(root, syncSubsetSpec{
		Includes: []string{"docs/**"},
		Excludes: []string{"docs/private.md"},
	})
	if err != nil {
		t.Fatalf("compileSyncSubsetSpec: %v", err)
	}

	result, merged, err := buildSyncSubsetDiff(root, spec, baseState, ignore.NewMatcher(root), true)
	if err != nil {
		t.Fatalf("buildSyncSubsetDiff: %v", err)
	}
	if result.Stats.Modified != 1 || countChanges(result) != 1 {
		t.Fatalf("result = %#v, want one forced modified file", result)
	}
	if len(result.Changes) != 1 || result.Changes[0].Path != "docs/a.md" || result.Changes[0].Status != models.StatusModified {
		t.Fatalf("changes = %#v", result.Changes)
	}
	if merged["docs/private.md"] != privateState {
		t.Fatalf("excluded path changed in merged state")
	}
}

func TestSelectedLocalStateUsesSyncIncludeExcludeSemantics(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "docs/a.md", "a\n")
	writeTestFile(t, root, "docs/nested/b.md", "b\n")
	writeTestFile(t, root, "docs/archive/old.md", "old\n")
	writeTestFile(t, root, "README.md", "readme\n")

	spec, err := compileSyncSubsetSpec(root, syncSubsetSpec{
		Includes: []string{"docs/**", "README.md"},
		Excludes: []string{"docs/archive/**"},
	})
	if err != nil {
		t.Fatalf("compileSyncSubsetSpec: %v", err)
	}
	state, err := selectedLocalState(root, spec, ignore.PolicyPatternSet{})
	if err != nil {
		t.Fatalf("selectedLocalState: %v", err)
	}
	for _, path := range []string{"docs/a.md", "docs/nested/b.md", "README.md"} {
		if _, ok := state[path]; !ok {
			t.Fatalf("selected state missing %s: %#v", path, state)
		}
	}
	if _, ok := state["docs/archive/old.md"]; ok {
		t.Fatalf("excluded path was selected: %#v", state)
	}
}

func TestCompareCommittedSelectionReportsSyncedMissingAndStale(t *testing.T) {
	local := map[string]models.FileState{
		"docs/a.md":       {Size: 10, ContentHash: "sha256:a"},
		"docs/missing.md": {Size: 20, ContentHash: "sha256:missing"},
		"docs/stale.md":   {Size: 30, ContentHash: "sha256:new"},
	}
	remote := map[string]models.FileState{
		"docs/a.md":     {Size: 10, ContentHash: "sha256:a"},
		"docs/stale.md": {Size: 30, ContentHash: "sha256:old"},
	}

	result := compareCommittedSelection(&models.RootMetadata{
		ID:                   "root-1",
		Name:                 "workspace",
		VisibleGenerationID:  "gen-1",
		VisibleGenerationSeq: 4,
	}, "/tmp/workspace", local, remote, nil, syncSubsetSpec{Includes: []string{"docs/**"}})

	if result.Status != "pending" || result.Matched != 1 || result.Total != 3 {
		t.Fatalf("result status/match = %#v", result)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "docs/missing.md" {
		t.Fatalf("missing = %#v", result.Missing)
	}
	if len(result.Stale) != 1 || result.Stale[0].Path != "docs/stale.md" || result.Stale[0].Expected != "sha256:new" || result.Stale[0].Actual != "sha256:old" {
		t.Fatalf("stale = %#v", result.Stale)
	}
	if result.VisibleGenerationID != "gen-1" || result.VisibleGenerationSeq != 4 {
		t.Fatalf("generation = %s/%d", result.VisibleGenerationID, result.VisibleGenerationSeq)
	}
}

func TestCompareCommittedSelectionSyncedAndEmpty(t *testing.T) {
	root := &models.RootMetadata{ID: "root-1", Name: "workspace"}
	state := map[string]models.FileState{
		"README.md": {Size: 7, ContentHash: "sha256:readme"},
	}
	synced := compareCommittedSelection(root, "/tmp/workspace", state, state, nil, syncSubsetSpec{Includes: []string{"README.md"}})
	if synced.Status != "synced" || synced.Matched != 1 || synced.Total != 1 {
		t.Fatalf("synced result = %#v", synced)
	}

	empty := compareCommittedSelection(root, "/tmp/workspace", nil, state, nil, syncSubsetSpec{Includes: []string{"missing/**"}})
	if empty.Status != "empty" || empty.Total != 0 {
		t.Fatalf("empty result = %#v", empty)
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

func TestLoadSyncSubsetBaseStateUsesMatchingLocalCache(t *testing.T) {
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

	state, err := loadSyncSubsetBaseState(&apiClient{baseURL: server.URL, httpClient: server.Client()}, &syncOnlyRoot{
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
		t.Fatalf("loadSyncSubsetBaseState: %v", err)
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

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info
}
