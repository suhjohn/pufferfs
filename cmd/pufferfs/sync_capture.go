package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// A captureCandidate needs authoritative bytes before it can be compared with
// the committed root. The discovery metadata is only a cache hint; it never
// becomes the file's committed content identity.
type captureCandidate struct {
	Path string
	Size int64
}

type capturePlan struct {
	Candidates []captureCandidate
	Present    map[string]bool
}

type captureSyncInput struct {
	Client       *apiClient
	Dir          string
	Name         string
	RootID       string
	Policy       ignore.PolicyPatternSet
	Select       func(string) bool
	Force        bool
	Log          io.Writer
	ChangedPaths []string // nil reconciles the entire root; otherwise scan only these paths/subtrees.
}

func discoverCapturePlan(root string, matcher *ignore.Matcher, baseState, hashCache map[string]models.FileState, dirty map[string]bool, selectPath func(string) bool, force bool, excludedDirs ...string) (capturePlan, error) {
	return discoverCapturePlanForPaths(root, nil, matcher, baseState, hashCache, dirty, selectPath, force, excludedDirs...)
}

func discoverCapturePlanForPaths(root string, paths []string, matcher *ignore.Matcher, baseState, hashCache map[string]models.FileState, dirty map[string]bool, selectPath func(string) bool, force bool, excludedDirs ...string) (capturePlan, error) {
	plan := capturePlan{Present: make(map[string]bool)}

	root = filepath.Clean(root)
	visit := func(absPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) && absPath != root {
				return nil
			}
			return walkErr
		}
		relPath, err := filepath.Rel(root, absPath)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		relPath = filepath.ToSlash(relPath)
		for _, excluded := range excludedDirs {
			if relPath == excluded || strings.HasPrefix(relPath, excluded+"/") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if matcher != nil && matcher.ShouldIgnore(relPath, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || (selectPath != nil && !selectPath(relPath)) {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("stat %s: %w", relPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(absPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("stat symlink target %s: %w", relPath, err)
			}
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		plan.Present[relPath] = true

		mtime := info.ModTime().UnixNano()
		cached, cacheHit := hashCache[relPath]
		base, existsInBase := baseState[relPath]
		if !force && !dirty[relPath] && cacheHit && existsInBase &&
			cached.ContentHash != "" && cached.ContentHash == base.ContentHash &&
			cached.Size == info.Size() && cached.Mtime == mtime {
			return nil
		}
		plan.Candidates = append(plan.Candidates, captureCandidate{Path: relPath, Size: info.Size()})
		return nil
	}
	if paths == nil {
		if err := filepath.WalkDir(root, visit); err != nil {
			return capturePlan{}, err
		}
	} else {
		for _, path := range paths {
			if !filepath.IsLocal(path) || path == "." {
				return capturePlan{}, errors.New("invalid changed capture path")
			}
			if err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(path)), visit); err != nil {
				return capturePlan{}, err
			}
		}
	}
	sort.Slice(plan.Candidates, func(i, j int) bool { return plan.Candidates[i].Path < plan.Candidates[j].Path })
	return plan, nil
}

func validContentHash(value string) bool {
	raw := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return strings.HasPrefix(value, "sha256:") && err == nil && len(decoded) == sha256.Size
}
