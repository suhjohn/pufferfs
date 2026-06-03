package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/spf13/cobra"
)

func watchCmd() *cobra.Command {
	var (
		name     string
		rootID   string
		debounce time.Duration
	)

	cmd := &cobra.Command{
		Use:   "watch [path]",
		Short: "Continuously watch and sync a directory",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}

			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			if cfg.Server.URL == "" {
				return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
			}

			return runWatch(cfg, absDir, name, rootID, debounce)
		},
	}

	cmd.Flags().StringVarP(&name, "name", "n", "", "Name alias for this root")
	cmd.Flags().StringVar(&rootID, "id", "", "Root ID to re-attach to")
	cmd.Flags().DurationVar(&debounce, "debounce", 2*time.Second, "Debounce interval for file changes")

	return cmd
}

func runWatch(cfg *appconfig.Config, dir, name, rootID string, debounce time.Duration) error {
	if name == "" {
		name = filepath.Base(dir)
	}

	// Run initial full sync
	fmt.Println("Running initial sync...")
	if err := runSync(cfg, dir, name, rootID, false); err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Set up file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer watcher.Close()

	matcher := ignore.NewMatcher(dir)

	// Add directory tree to watcher
	if err := addWatchDirs(watcher, dir, matcher); err != nil {
		return fmt.Errorf("adding watch dirs: %w", err)
	}

	fmt.Printf("Watching %s for changes (debounce: %s)...\n", dir, debounce)

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			relPath, err := filepath.Rel(dir, event.Name)
			if err != nil {
				continue
			}
			relPath = filepath.ToSlash(relPath)

			if matcher.ShouldIgnore(relPath, false) {
				continue
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) ||
				event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				if !pending {
					timer.Reset(debounce)
					pending = true
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("watcher error: %v", err)

		case <-timer.C:
			pending = false
			fmt.Println("\nChanges detected, syncing...")
			if err := runSync(cfg, dir, name, rootID, false); err != nil {
				log.Printf("sync error: %v", err)
			}
		}
	}
}

func addWatchDirs(watcher *fsnotify.Watcher, root string, matcher *ignore.Matcher) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return watcher.Add(path)
		}

		relPath = filepath.ToSlash(relPath)
		if matcher.ShouldIgnore(relPath, true) {
			return filepath.SkipDir
		}

		return watcher.Add(path)
	})
}
