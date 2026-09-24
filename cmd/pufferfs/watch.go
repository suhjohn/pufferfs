package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/spf13/cobra"
)

func watchCmd() *cobra.Command {
	var (
		name    string
		rootID  string
		options followOptions
	)

	cmd := &cobra.Command{
		Use:        "watch [path]",
		Short:      "Continuously watch and sync a directory",
		Hidden:     true,
		Deprecated: "use `pufferfs sync --follow` for foreground watching or `pufferfs service` for background sync",
		Args:       cobra.MaximumNArgs(1),
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

			return runFollow(cfg, absDir, name, rootID, false, options)
		},
	}

	cmd.Flags().StringVarP(&name, "name", "n", "", "Name alias for this root")
	cmd.Flags().StringVar(&rootID, "id", "", "Root ID to re-attach to")
	addFollowFlags(cmd, &options)

	return cmd
}

type followOptions struct {
	Debounce             time.Duration
	MaxBackoff           time.Duration
	MaxSameFailures      int
	MaxSameFailureWindow time.Duration
	ReconcileInterval    time.Duration
}

func defaultFollowOptions() followOptions {
	return followOptions{
		Debounce:             2 * time.Second,
		MaxBackoff:           60 * time.Second,
		MaxSameFailures:      8,
		MaxSameFailureWindow: 10 * time.Minute,
		ReconcileInterval:    15 * time.Minute,
	}
}

func addFollowFlags(cmd *cobra.Command, options *followOptions) {
	*options = defaultFollowOptions()
	cmd.Flags().DurationVar(&options.Debounce, "debounce", options.Debounce, "Debounce interval for file changes")
	cmd.Flags().DurationVar(&options.MaxBackoff, "max-backoff", options.MaxBackoff, "Maximum retry backoff while following")
	cmd.Flags().IntVar(&options.MaxSameFailures, "max-same-failures", options.MaxSameFailures, "Exit after this many consecutive identical sync failures")
	cmd.Flags().DurationVar(&options.MaxSameFailureWindow, "max-same-failure-window", options.MaxSameFailureWindow, "Exit after identical sync failures persist for this long")
	cmd.Flags().DurationVar(&options.ReconcileInterval, "reconcile-interval", options.ReconcileInterval, "Interval between full filesystem reconciliations while following")
}

func runFollow(cfg *appconfig.Config, dir, name, rootID string, noVector bool, options followOptions) error {
	if name == "" {
		name = filepath.Base(dir)
	}
	options = normalizeFollowOptions(options)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("watched directory unavailable: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer watcher.Close()
	if err := addWatchDirs(watcher, dir, ignore.NewMatcher(dir)); err != nil {
		return fmt.Errorf("adding watch dirs: %w", err)
	}
	changes := newFollowChanges()
	go changes.observe(ctx, watcher, dir)
	changes.mark(nil)
	// Watching starts before the initial scan, closing the scan-to-watch gap.
	fmt.Println("Running initial sync...")
	timer := time.NewTimer(0)
	defer timer.Stop()
	pending := true
	metadata := time.NewTicker(30 * time.Second)
	defer metadata.Stop()
	reconcile := time.NewTicker(options.ReconcileInterval)
	defer reconcile.Stop()
	failures := followFailureTracker{}
	initial := true
	for {
		select {
		case <-ctx.Done():
			fmt.Println("Stopping follow.")
			return nil
		case <-metadata.C:
			changes.mark([]string{})
		case <-reconcile.C:
			changes.mark(nil)
		case <-changes.notice:
			if !pending {
				resetFollowTimer(timer, &pending, options.Debounce)
			}
		case <-timer.C:
			pending = false
			if _, err := os.Stat(dir); err != nil {
				return fmt.Errorf("watched directory unavailable: %w", err)
			}
			paths := changes.take()
			if paths == nil {
				if err := addWatchDirs(watcher, dir, ignore.NewMatcher(dir)); err != nil {
					return fmt.Errorf("repairing watches: %w", err)
				}
			}
			dirty, err := runFollowSync(ctx, cfg, dir, name, rootID, noVector, &failures, options, paths)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return err
			}
			if failures.Active {
				changes.mark(paths)
				delay := failures.NextDelay(options)
				log.Printf("sync failed; retrying in %s: %v", delay, failures.LastError)
				resetFollowTimer(timer, &pending, delay)
				continue
			}
			if initial {
				fmt.Printf("Following %s for changes (debounce: %s)...\n", dir, options.Debounce)
				initial = false
			}
			if len(dirty) > 0 {
				changes.mark(dirty)
				resetFollowTimer(timer, &pending, followCaptureReconcileDelay(options))
			}
		}
	}
}

func normalizeFollowOptions(options followOptions) followOptions {
	defaults := defaultFollowOptions()
	if options.Debounce <= 0 {
		options.Debounce = defaults.Debounce
	}
	if options.MaxBackoff <= 0 {
		options.MaxBackoff = defaults.MaxBackoff
	}
	if options.MaxSameFailures <= 0 {
		options.MaxSameFailures = defaults.MaxSameFailures
	}
	if options.MaxSameFailureWindow <= 0 {
		options.MaxSameFailureWindow = defaults.MaxSameFailureWindow
	}
	if options.ReconcileInterval <= 0 {
		options.ReconcileInterval = defaults.ReconcileInterval
	}
	return options
}

func resetFollowTimer(timer *time.Timer, pending *bool, delay time.Duration) {
	if *pending {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	timer.Reset(delay)
	*pending = true
}

func runFollowSync(ctx context.Context, cfg *appconfig.Config, dir, name, rootID string, noVector bool, failures *followFailureTracker, options followOptions, paths []string) ([]string, error) {
	result, err := runSyncSelection(ctx, cfg, dir, syncSubsetSpec{}, name, rootID, "org", noVector, false, false, os.Stdout, paths)
	if err == nil {
		failures.Reset()
		if result != nil {
			return result.dirtyPaths, nil
		}
		return nil, nil
	}
	class := classifyFollowError(err)
	failures.Record(err, class)
	if class.Permanent {
		return nil, fmt.Errorf("permanent sync failure: %w", err)
	}
	if failures.ShouldExit(options) {
		return nil, fmt.Errorf("same sync failure repeated %d times over %s: %w", failures.SameCount, time.Since(failures.FirstSeen).Round(time.Second), err)
	}
	return nil, nil
}

func followCaptureReconcileDelay(options followOptions) time.Duration {
	const minimum = 30 * time.Second
	if options.Debounce > minimum {
		return options.Debounce
	}
	return minimum
}

type followErrorClass struct {
	Key       string
	Permanent bool
}

type followFailureTracker struct {
	Active      bool
	LastKey     string
	LastError   error
	SameCount   int
	FirstSeen   time.Time
	LastSeen    time.Time
	Consecutive int
}

func (t *followFailureTracker) Record(err error, class followErrorClass) {
	now := time.Now()
	key := class.Key
	if key == "" {
		key = normalizeErrorString(err)
	}
	if t.Active && key == t.LastKey {
		t.SameCount++
	} else {
		t.SameCount = 1
		t.FirstSeen = now
	}
	t.Active = true
	t.LastKey = key
	t.LastError = err
	t.LastSeen = now
	t.Consecutive++
}

func (t *followFailureTracker) Reset() {
	*t = followFailureTracker{}
}

func (t *followFailureTracker) NextDelay(options followOptions) time.Duration {
	if !t.Active || t.SameCount <= 1 {
		return time.Second
	}
	delay := time.Second << min(t.SameCount-1, 6)
	if delay > options.MaxBackoff {
		return options.MaxBackoff
	}
	return delay
}

func (t *followFailureTracker) ShouldExit(options followOptions) bool {
	if !t.Active {
		return false
	}
	if options.MaxSameFailures > 0 && t.SameCount >= options.MaxSameFailures {
		return true
	}
	return options.MaxSameFailureWindow > 0 && !t.FirstSeen.IsZero() && time.Since(t.FirstSeen) >= options.MaxSameFailureWindow
}

func classifyFollowError(err error) followErrorClass {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return followErrorClass{Key: fmt.Sprintf("http:%d", apiErr.StatusCode), Permanent: true}
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return followErrorClass{Key: fmt.Sprintf("http:%d", apiErr.StatusCode)}
		default:
			if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
				return followErrorClass{Key: fmt.Sprintf("http:%d", apiErr.StatusCode), Permanent: true}
			}
			return followErrorClass{Key: fmt.Sprintf("http:%d", apiErr.StatusCode)}
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return followErrorClass{Key: "network:" + normalizeErrorString(err)}
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"connection refused", "connection reset", "i/o timeout", "timeout", "temporary failure", "no such host", "server closed idle connection"} {
		if strings.Contains(msg, needle) {
			return followErrorClass{Key: "transient:" + needle}
		}
	}
	for _, needle := range []string{"server url not configured", "unauthorized", "forbidden", "access denied", "permission denied", "root deleted", "watched directory unavailable"} {
		if strings.Contains(msg, needle) {
			return followErrorClass{Key: "permanent:" + needle, Permanent: true}
		}
	}
	return followErrorClass{Key: "unknown:" + normalizeErrorString(err)}
}

func normalizeErrorString(err error) string {
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 180 {
		msg = msg[:180]
	}
	return msg
}

func addWatchDirs(watcher *fsnotify.Watcher, root string, matcher *ignore.Matcher) error {
	return addWatchDirsBelow(watcher, root, root, matcher)
}

func addWatchDirsBelow(watcher *fsnotify.Watcher, root, below string, matcher *ignore.Matcher) error {
	return filepath.WalkDir(below, func(path string, d os.DirEntry, err error) error {
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
