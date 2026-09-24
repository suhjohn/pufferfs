package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/pufferfs/pufferfs/internal/ignore"
)

const maxFollowChangedPaths = 10000

// The reader drains OS events while capture performs network IO. Each sync
// takes one set; events arriving during that sync enter the next set. Overflow
// discards path hints and requests a full reconciliation. On process restart
// we always reconcile, so these hints never replace durable capture journals.
type followChanges struct {
	mu     sync.Mutex
	paths  map[string]bool
	full   bool
	notice chan struct{}
}

func newFollowChanges() *followChanges {
	return &followChanges{paths: make(map[string]bool), notice: make(chan struct{}, 1)}
}

func (c *followChanges) mark(paths []string) {
	c.mu.Lock()
	if paths == nil {
		c.full = true
		clear(c.paths)
	}
	if !c.full {
		for _, path := range paths {
			c.paths[path] = true
		}
		if len(c.paths) > maxFollowChangedPaths {
			c.full = true
			clear(c.paths)
		}
	}
	c.mu.Unlock()
	select {
	case c.notice <- struct{}{}:
	default:
	}
}

func (c *followChanges) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var paths []string
	if !c.full {
		paths = make([]string, 0, len(c.paths))
		for path := range c.paths {
			paths = append(paths, path)
		}
	}
	c.full = false
	clear(c.paths)
	return paths
}

func (c *followChanges) observe(ctx context.Context, watcher *fsnotify.Watcher, root string) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if !(event.Has(fsnotify.Create) || event.Has(fsnotify.Write) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)) {
				continue
			}
			rel, err := filepath.Rel(root, event.Name)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			rel = filepath.ToSlash(rel)
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					matcher := ignore.NewMatcherForPathsWithPolicy(root, []string{rel}, ignore.PolicyPatternSet{})
					if err = addWatchDirsBelow(watcher, root, event.Name, matcher); err != nil {
						log.Printf("watch directory repair needed: %v", err)
						c.mark(nil)
					}
				}
			}
			// Enqueue the subtree after installing its watches. Changes made
			// before installation are covered by the subtree scan; later ones
			// produce their own events.
			if rel == "." || filepath.Base(rel) == ".gitignore" || filepath.Base(rel) == ".tpfsignore" {
				c.mark(nil)
			} else {
				c.mark([]string{rel})
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher requires full reconciliation: %v", err)
			c.mark(nil)
		}
	}
}
