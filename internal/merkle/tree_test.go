package merkle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pufferfs/pufferfs/internal/ignore"
)

func setupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create directory structure
	os.MkdirAll(filepath.Join(dir, "src", "components"), 0o755)
	os.MkdirAll(filepath.Join(dir, "src", "utils"), 0o755)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)

	// Create files
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Project"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "components", "button.go"), []byte("package components\ntype Button struct{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "components", "input.go"), []byte("package components\ntype Input struct{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "utils", "helpers.go"), []byte("package utils\nfunc Help() {}"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte("# Guide\nSome text."), 0o644)

	return dir
}

func TestBuildTree(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree, err := BuildTree(dir, matcher)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	if tree.Root == nil {
		t.Fatal("root is nil")
	}
	if tree.Root.Hash == "" {
		t.Fatal("root hash is empty")
	}
	if !tree.Root.IsDir {
		t.Fatal("root should be a directory")
	}

	// Check structure
	if _, ok := tree.Root.Children["README.md"]; !ok {
		t.Error("missing README.md")
	}
	src, ok := tree.Root.Children["src"]
	if !ok {
		t.Fatal("missing src/")
	}
	if !src.IsDir {
		t.Error("src should be a directory")
	}
	if _, ok := src.Children["main.go"]; !ok {
		t.Error("missing src/main.go")
	}
	components, ok := src.Children["components"]
	if !ok {
		t.Fatal("missing src/components/")
	}
	if len(components.Children) != 2 {
		t.Errorf("expected 2 files in components, got %d", len(components.Children))
	}
}

func TestBuildTreeDeterministic(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree1, _ := BuildTree(dir, matcher)
	tree2, _ := BuildTree(dir, matcher)

	if tree1.Root.Hash != tree2.Root.Hash {
		t.Errorf("non-deterministic: %s vs %s", tree1.Root.Hash, tree2.Root.Hash)
	}
}

func TestBuildTreeChangedFile(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree1, _ := BuildTree(dir, matcher)
	hash1 := tree1.Root.Hash

	// Modify one file
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\nfunc main() {}"), 0o644)

	tree2, _ := BuildTree(dir, matcher)
	hash2 := tree2.Root.Hash

	if hash1 == hash2 {
		t.Error("root hash should change when a file is modified")
	}

	// src/ hash should change
	if tree1.Root.Children["src"].Hash == tree2.Root.Children["src"].Hash {
		t.Error("src/ hash should change")
	}

	// docs/ hash should NOT change
	if tree1.Root.Children["docs"].Hash != tree2.Root.Children["docs"].Hash {
		t.Error("docs/ hash should NOT change")
	}
}

func TestDiffNoChanges(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree, _ := BuildTree(dir, matcher)
	changes := Diff(tree, tree)

	if len(changes) != 0 {
		t.Errorf("expected 0 changes, got %d", len(changes))
	}
}

func TestDiffModifiedFile(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree1, _ := BuildTree(dir, matcher)

	// Modify one file
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\nfunc main() {}"), 0o644)
	tree2, _ := BuildTree(dir, matcher)

	changes := Diff(tree1, tree2)

	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", len(changes), changes)
	}
	if changes[0].Path != "src/main.go" {
		t.Errorf("expected src/main.go, got %s", changes[0].Path)
	}
	if changes[0].Type != "modified" {
		t.Errorf("expected modified, got %s", changes[0].Type)
	}
}

func TestDiffAddedFile(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree1, _ := BuildTree(dir, matcher)

	os.WriteFile(filepath.Join(dir, "src", "new.go"), []byte("package main"), 0o644)
	tree2, _ := BuildTree(dir, matcher)

	changes := Diff(tree1, tree2)

	found := false
	for _, c := range changes {
		if c.Path == "src/new.go" && c.Type == "added" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing added src/new.go in changes: %+v", changes)
	}
}

func TestDiffRemovedFile(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree1, _ := BuildTree(dir, matcher)

	os.Remove(filepath.Join(dir, "docs", "guide.md"))
	tree2, _ := BuildTree(dir, matcher)

	changes := Diff(tree1, tree2)

	found := false
	for _, c := range changes {
		if c.Path == "docs/guide.md" && c.Type == "removed" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing removed docs/guide.md in changes: %+v", changes)
	}
}

func TestDiffSkipsUnchangedSubtrees(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree1, _ := BuildTree(dir, matcher)

	// Only modify file in src/components/
	os.WriteFile(filepath.Join(dir, "src", "components", "button.go"), []byte("package components\ntype Button struct{ Label string }"), 0o644)
	tree2, _ := BuildTree(dir, matcher)

	changes := Diff(tree1, tree2)

	// Should only have 1 change (button.go), not touch docs/ or utils/
	if len(changes) != 1 {
		t.Errorf("expected 1 change, got %d: %+v", len(changes), changes)
	}
	if changes[0].Path != "src/components/button.go" {
		t.Errorf("expected src/components/button.go, got %s", changes[0].Path)
	}
}

func TestToFileStateMap(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree, _ := BuildTree(dir, matcher)
	state := tree.ToFileStateMap()

	if len(state) != 6 {
		t.Errorf("expected 6 files, got %d", len(state))
	}

	for _, path := range []string{"README.md", "src/main.go", "src/components/button.go", "src/components/input.go", "src/utils/helpers.go", "docs/guide.md"} {
		if _, ok := state[path]; !ok {
			t.Errorf("missing %s in state map", path)
		}
	}
}

func TestSimHash(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree1, _ := BuildTree(dir, matcher)
	sh1 := tree1.SimHash()

	// Same tree → same SimHash
	tree2, _ := BuildTree(dir, matcher)
	sh2 := tree2.SimHash()

	if sh1 != sh2 {
		t.Error("same tree should produce same SimHash")
	}

	// Small change → small Hamming distance
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\nfunc main() {}"), 0o644)
	tree3, _ := BuildTree(dir, matcher)
	sh3 := tree3.SimHash()

	dist := HammingDistance(sh1, sh3)
	if dist == 0 {
		t.Error("different tree should have non-zero Hamming distance")
	}
	// With 6 files, changing 1 should keep distance relatively small
	similarity := SimHashSimilarity(sh1, sh3)
	if similarity < 0.5 {
		t.Errorf("expected high similarity for 1/6 file change, got %.2f (distance=%d)", similarity, dist)
	}
}

func TestSimHashDifferentTrees(t *testing.T) {
	dir1 := t.TempDir()
	os.WriteFile(filepath.Join(dir1, "a.go"), []byte("package a"), 0o644)
	os.WriteFile(filepath.Join(dir1, "b.go"), []byte("package b"), 0o644)

	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "x.go"), []byte("completely different content here"), 0o644)
	os.WriteFile(filepath.Join(dir2, "y.go"), []byte("more different content"), 0o644)

	matcher1 := ignore.NewMatcher(dir1)
	matcher2 := ignore.NewMatcher(dir2)

	tree1, _ := BuildTree(dir1, matcher1)
	tree2, _ := BuildTree(dir2, matcher2)

	sh1 := tree1.SimHash()
	sh2 := tree2.SimHash()

	if sh1 == sh2 {
		t.Error("completely different trees should have different SimHashes")
	}
}

func TestContentProof(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree, _ := BuildTree(dir, matcher)
	proof := tree.BuildContentProof()

	// Should have all 6 files
	if len(proof.FileHashes) != 6 {
		t.Errorf("expected 6 file hashes, got %d", len(proof.FileHashes))
	}

	// RootHash should match tree root
	if proof.RootHash != tree.Root.Hash {
		t.Error("root hash mismatch")
	}

	// HasAccess should return true for existing files
	if !proof.HasAccess("src/main.go") {
		t.Error("should have access to src/main.go")
	}
	if proof.HasAccess("nonexistent.go") {
		t.Error("should not have access to nonexistent.go")
	}

	// HasFile should verify hash
	mainHash := proof.FileHashes["src/main.go"]
	if !proof.HasFile("src/main.go", mainHash) {
		t.Error("HasFile should return true with correct hash")
	}
	if proof.HasFile("src/main.go", "sha256:wrong") {
		t.Error("HasFile should return false with wrong hash")
	}
}

func TestTreeSerializationRoundTrip(t *testing.T) {
	dir := setupTestDir(t)
	matcher := ignore.NewMatcher(dir)

	tree, _ := BuildTree(dir, matcher)

	data, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var tree2 Tree
	if err := json.Unmarshal(data, &tree2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if tree.Root.Hash != tree2.Root.Hash {
		t.Errorf("hash mismatch after roundtrip: %s vs %s", tree.Root.Hash, tree2.Root.Hash)
	}

	// Verify diff is empty
	changes := Diff(tree, &tree2)
	if len(changes) != 0 {
		t.Errorf("expected 0 changes after roundtrip, got %d", len(changes))
	}
}

func TestHammingDistance(t *testing.T) {
	var a, b [32]byte
	// Identical
	if HammingDistance(a, b) != 0 {
		t.Error("identical bytes should have distance 0")
	}

	// One bit different
	a[0] = 0x01
	if HammingDistance(a, b) != 1 {
		t.Errorf("expected distance 1, got %d", HammingDistance(a, b))
	}

	// All bits different
	for i := range a {
		a[i] = 0xFF
	}
	if HammingDistance(a, b) != 256 {
		t.Errorf("expected distance 256, got %d", HammingDistance(a, b))
	}
}
