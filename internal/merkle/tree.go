// Package merkle implements a Merkle tree for filesystem state.
//
// Each leaf node represents a file (hash = SHA-256 of content).
// Each directory node's hash = SHA-256(sorted child hashes concatenated).
// This lets us diff two trees by walking top-down and only descending
// into subtrees where hashes differ.
package merkle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// Node represents a node in the Merkle tree.
type Node struct {
	Name     string           `json:"name"`
	Hash     string           `json:"hash"`
	IsDir    bool             `json:"is_dir"`
	Size     int64            `json:"size,omitempty"`
	Mtime    int64            `json:"mtime,omitempty"`
	Children map[string]*Node `json:"children,omitempty"`
}

// Tree is the root of a Merkle tree representing a directory.
type Tree struct {
	Root    *Node  `json:"root"`
	RootDir string `json:"root_dir"`
}

// BuildTree constructs a Merkle tree from a directory.
// Files are hashed with SHA-256. Directory hashes are derived from sorted child hashes.
// Uses a worker pool for parallel file hashing.
func BuildTree(rootDir string, matcher *ignore.Matcher) (*Tree, error) {
	rootDir = filepath.Clean(rootDir)
	root := &Node{
		Name:     filepath.Base(rootDir),
		IsDir:    true,
		Children: make(map[string]*Node),
	}

	// Collect all files first, then hash in parallel
	type fileEntry struct {
		relPath string
		absPath string
		size    int64
		mtime   int64
	}

	var files []fileEntry
	err := filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(rootDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		if matcher.ShouldIgnore(relPath, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			// Ensure directory node exists
			ensureDir(root, relPath)
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", relPath, err)
		}

		files = append(files, fileEntry{
			relPath: relPath,
			absPath: path,
			size:    info.Size(),
			mtime:   info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Parallel file hashing
	workers := runtime.NumCPU()
	if workers > 16 {
		workers = 16
	}
	if workers < 1 {
		workers = 1
	}

	type hashResult struct {
		index int
		hash  string
		err   error
	}

	results := make([]hashResult, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for i, f := range files {
		wg.Add(1)
		go func(idx int, entry fileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			hash, err := hashFile(entry.absPath)
			results[idx] = hashResult{index: idx, hash: hash, err: err}
		}(i, f)
	}
	wg.Wait()

	// Insert file nodes into tree
	for i, f := range files {
		if results[i].err != nil {
			return nil, fmt.Errorf("hashing %s: %w", f.relPath, results[i].err)
		}
		insertFile(root, f.relPath, results[i].hash, f.size, f.mtime)
	}

	// Compute directory hashes bottom-up
	computeDirHash(root)

	return &Tree{Root: root, RootDir: rootDir}, nil
}

// ensureDir creates directory nodes along a path.
func ensureDir(root *Node, relPath string) {
	parts := strings.Split(relPath, "/")
	current := root
	for _, part := range parts {
		if current.Children == nil {
			current.Children = make(map[string]*Node)
		}
		child, ok := current.Children[part]
		if !ok {
			child = &Node{
				Name:     part,
				IsDir:    true,
				Children: make(map[string]*Node),
			}
			current.Children[part] = child
		}
		current = child
	}
}

// insertFile adds a file node at the given relative path.
func insertFile(root *Node, relPath, hash string, size, mtime int64) {
	parts := strings.Split(relPath, "/")
	current := root

	// Create/traverse directories
	for _, dir := range parts[:len(parts)-1] {
		if current.Children == nil {
			current.Children = make(map[string]*Node)
		}
		child, ok := current.Children[dir]
		if !ok {
			child = &Node{
				Name:     dir,
				IsDir:    true,
				Children: make(map[string]*Node),
			}
			current.Children[dir] = child
		}
		current = child
	}

	// Insert file leaf
	fileName := parts[len(parts)-1]
	if current.Children == nil {
		current.Children = make(map[string]*Node)
	}
	current.Children[fileName] = &Node{
		Name:  fileName,
		Hash:  hash,
		IsDir: false,
		Size:  size,
		Mtime: mtime,
	}
}

// computeDirHash recursively computes directory hashes from child hashes.
func computeDirHash(node *Node) string {
	if !node.IsDir {
		return node.Hash
	}

	// Sort child names for deterministic hashing
	names := make([]string, 0, len(node.Children))
	for name := range node.Children {
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		child := node.Children[name]
		childHash := computeDirHash(child)
		h.Write([]byte(name + ":" + childHash + "\n"))
	}

	node.Hash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return node.Hash
}

// hashFile computes the SHA-256 hex digest of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// Diffing
// ---------------------------------------------------------------------------

// DiffChange represents a change detected between two Merkle trees.
type DiffChange struct {
	Path        string
	Type        string // "added", "removed", "modified"
	ContentHash string
	Size        int64
	Mtime       int64
}

// Diff compares two Merkle trees and returns only the changed files.
// It walks top-down and skips entire subtrees where hashes match.
func Diff(prev, curr *Tree) []DiffChange {
	var changes []DiffChange
	diffNodes(prev.Root, curr.Root, "", &changes)
	return changes
}

// diffNodes recursively compares two nodes.
func diffNodes(prev, curr *Node, prefix string, changes *[]DiffChange) {
	// Both nil — nothing
	if prev == nil && curr == nil {
		return
	}

	// Added subtree
	if prev == nil && curr != nil {
		collectAll(curr, prefix, "added", changes)
		return
	}

	// Removed subtree
	if prev != nil && curr == nil {
		collectAll(prev, prefix, "removed", changes)
		return
	}

	// Hashes match — skip entire subtree
	if prev.Hash == curr.Hash {
		return
	}

	// Both are files — modified
	if !prev.IsDir && !curr.IsDir {
		*changes = append(*changes, DiffChange{
			Path:        prefix,
			Type:        "modified",
			ContentHash: curr.Hash,
			Size:        curr.Size,
			Mtime:       curr.Mtime,
		})
		return
	}

	// One is file, other is dir — remove old, add new
	if prev.IsDir != curr.IsDir {
		collectAll(prev, prefix, "removed", changes)
		collectAll(curr, prefix, "added", changes)
		return
	}

	// Both are directories — recurse into differing children
	allChildren := make(map[string]bool)
	for name := range prev.Children {
		allChildren[name] = true
	}
	for name := range curr.Children {
		allChildren[name] = true
	}

	for name := range allChildren {
		childPath := name
		if prefix != "" {
			childPath = prefix + "/" + name
		}
		diffNodes(prev.Children[name], curr.Children[name], childPath, changes)
	}
}

// collectAll recursively collects all files under a node as a given change type.
func collectAll(node *Node, prefix string, changeType string, changes *[]DiffChange) {
	if !node.IsDir {
		*changes = append(*changes, DiffChange{
			Path:        prefix,
			Type:        changeType,
			ContentHash: node.Hash,
			Size:        node.Size,
			Mtime:       node.Mtime,
		})
		return
	}

	for name, child := range node.Children {
		childPath := name
		if prefix != "" {
			childPath = prefix + "/" + name
		}
		collectAll(child, childPath, changeType, changes)
	}
}

// ---------------------------------------------------------------------------
// State extraction (compatibility with existing flat map)
// ---------------------------------------------------------------------------

// ToFileStateMap extracts a flat map[path]FileState from the tree.
func (t *Tree) ToFileStateMap() map[string]models.FileState {
	state := make(map[string]models.FileState)
	extractState(t.Root, "", state)
	return state
}

func extractState(node *Node, prefix string, state map[string]models.FileState) {
	if !node.IsDir {
		state[prefix] = models.FileState{
			Size:        node.Size,
			ContentHash: node.Hash,
			Mtime:       node.Mtime,
		}
		return
	}
	for name, child := range node.Children {
		childPath := name
		if prefix != "" {
			childPath = prefix + "/" + name
		}
		extractState(child, childPath, state)
	}
}

// ---------------------------------------------------------------------------
// SimHash — locality-sensitive hash for finding similar trees
// ---------------------------------------------------------------------------

// SimHash computes a 256-bit SimHash from the file content hashes in the tree.
// Similar trees produce similar SimHashes (small Hamming distance).
// This is used to find existing indexes to reuse within an org.
func (t *Tree) SimHash() [32]byte {
	// Collect all file hashes
	var hashes []string
	collectFileHashes(t.Root, &hashes)

	// SimHash: for each bit position, count +1 for set bits, -1 for unset bits
	var counts [256]int
	for _, hashStr := range hashes {
		// Parse the hex hash (skip "sha256:" prefix)
		hexStr := strings.TrimPrefix(hashStr, "sha256:")
		hashBytes, err := hex.DecodeString(hexStr)
		if err != nil {
			continue
		}
		for i, b := range hashBytes {
			for bit := 0; bit < 8; bit++ {
				if i*8+bit >= 256 {
					break
				}
				if b&(1<<uint(7-bit)) != 0 {
					counts[i*8+bit]++
				} else {
					counts[i*8+bit]--
				}
			}
		}
	}

	// Threshold: majority vote
	var result [32]byte
	for i := 0; i < 256; i++ {
		if counts[i] > 0 {
			result[i/8] |= 1 << uint(7-i%8)
		}
	}
	return result
}

// SimHashHex returns the SimHash as a hex string.
func (t *Tree) SimHashHex() string {
	h := t.SimHash()
	return hex.EncodeToString(h[:])
}

// HammingDistance computes the Hamming distance between two SimHashes.
func HammingDistance(a, b [32]byte) int {
	dist := 0
	for i := 0; i < 32; i++ {
		xor := a[i] ^ b[i]
		for xor != 0 {
			dist += int(xor & 1)
			xor >>= 1
		}
	}
	return dist
}

// SimHashSimilarity returns a similarity score [0, 1] between two SimHashes.
// 1.0 = identical, 0.0 = completely different.
func SimHashSimilarity(a, b [32]byte) float64 {
	d := HammingDistance(a, b)
	return 1.0 - float64(d)/256.0
}

func collectFileHashes(node *Node, hashes *[]string) {
	if !node.IsDir {
		*hashes = append(*hashes, node.Hash)
		return
	}
	for _, child := range node.Children {
		collectFileHashes(child, hashes)
	}
}

// ---------------------------------------------------------------------------
// Content Proofs
// ---------------------------------------------------------------------------

// ContentProof is a set of file content hashes that a client can prove it has.
// Used to filter search results so clients only see results for files they possess.
type ContentProof struct {
	// FileHashes maps file path → content hash
	FileHashes map[string]string `json:"file_hashes"`
	// DirHashes maps directory path → directory Merkle hash
	DirHashes map[string]string `json:"dir_hashes"`
	// RootHash is the top-level Merkle root hash
	RootHash string `json:"root_hash"`
}

// BuildContentProof extracts a ContentProof from a Merkle tree.
func (t *Tree) BuildContentProof() *ContentProof {
	proof := &ContentProof{
		FileHashes: make(map[string]string),
		DirHashes:  make(map[string]string),
		RootHash:   t.Root.Hash,
	}
	extractProofs(t.Root, "", proof)
	return proof
}

func extractProofs(node *Node, prefix string, proof *ContentProof) {
	if !node.IsDir {
		proof.FileHashes[prefix] = node.Hash
		return
	}
	if prefix != "" {
		proof.DirHashes[prefix] = node.Hash
	}
	for name, child := range node.Children {
		childPath := name
		if prefix != "" {
			childPath = prefix + "/" + name
		}
		extractProofs(child, childPath, proof)
	}
}

// HasFile checks if the proof contains a given file path with a matching hash.
func (p *ContentProof) HasFile(filePath, contentHash string) bool {
	if h, ok := p.FileHashes[filePath]; ok {
		return h == contentHash
	}
	return false
}

// HasAccess checks if the proof contains a file path (regardless of hash).
// Used for basic "does the client have this file" checks.
func (p *ContentProof) HasAccess(filePath string) bool {
	_, ok := p.FileHashes[filePath]
	return ok
}

// ---------------------------------------------------------------------------
// Serialization
// ---------------------------------------------------------------------------

// MarshalJSON serializes the tree to JSON.
func (t *Tree) MarshalJSON() ([]byte, error) {
	type treeJSON struct {
		Root    *Node  `json:"root"`
		RootDir string `json:"root_dir"`
	}
	return json.Marshal(treeJSON{Root: t.Root, RootDir: t.RootDir})
}

// UnmarshalJSON deserializes the tree from JSON.
func (t *Tree) UnmarshalJSON(data []byte) error {
	type treeJSON struct {
		Root    *Node  `json:"root"`
		RootDir string `json:"root_dir"`
	}
	var tj treeJSON
	if err := json.Unmarshal(data, &tj); err != nil {
		return err
	}
	t.Root = tj.Root
	t.RootDir = tj.RootDir
	return nil
}
