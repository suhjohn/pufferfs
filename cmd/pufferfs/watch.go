package main

import (
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

			return runFollow(cfg, absDir, name, rootID, options)
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
}

func defaultFollowOptions() followOptions {
	return followOptions{
		Debounce:             2 * time.Second,
		MaxBackoff:           60 * time.Second,
		MaxSameFailures:      8,
		MaxSameFailureWindow: 10 * time.Minute,
	}
}

func addFollowFlags(cmd *cobra.Command, options *followOptions) {
	*options = defaultFollowOptions()
	cmd.Flags().DurationVar(&options.Debounce, "debounce", options.Debounce, "Debounce interval for file changes")
	cmd.Flags().DurationVar(&options.MaxBackoff, "max-backoff", options.MaxBackoff, "Maximum retry backoff while following")
	cmd.Flags().IntVar(&options.MaxSameFailures, "max-same-failures", options.MaxSameFailures, "Exit after this many consecutive identical sync failures")
	cmd.Flags().DurationVar(&options.MaxSameFailureWindow, "max-same-failure-window", options.MaxSameFailureWindow, "Exit after identical sync failures persist for this long")
}

func runWatch(cfg *appconfig.Config, dir, name, rootID string, debounce time.Duration) error {
	options := defaultFollowOptions()
	options.Debounce = debounce
	return runFollow(cfg, dir, name, rootID, options)
}

func runFollow(cfg *appconfig.Config, dir, name, rootID string, options followOptions) error {
	if name == "" {
		name = filepath.Base(dir)
	}
	options = normalizeFollowOptions(options)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("watched directory unavailable: %w", err)
	}

	fmt.Println("Running initial sync...")
	failures := followFailureTracker{}
	for {
		if err := runFollowSync(cfg, dir, name, rootID, &failures, options); err != nil {
			return fmt.Errorf("initial sync: %w", err)
		}
		if !failures.Active {
			break
		}
		delay := failures.NextDelay(options)
		log.Printf("initial sync failed; retrying in %s: %v", delay, failures.LastError)
		time.Sleep(delay)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer watcher.Close()

	matcher := ignore.NewMatcher(dir)

	if err := addWatchDirs(watcher, dir, matcher); err != nil {
		return fmt.Errorf("adding watch dirs: %w", err)
	}

	fmt.Printf("Following %s for changes (debounce: %s)...\n", dir, options.Debounce)

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false
	dirty := false

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	for {
		select {
		case sig := <-signals:
			fmt.Printf("\nReceived %s; stopping follow.\n", sig)
			return nil
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

			// If a new directory was created, add it to the watcher
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if !matcher.ShouldIgnore(relPath, true) {
						_ = watcher.Add(event.Name)
					}
				}
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) ||
				event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				dirty = true
				resetFollowTimer(timer, &pending, options.Debounce)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("watcher error: %v", err)

		case <-timer.C:
			pending = false
			if !dirty {
				continue
			}
			if _, err := os.Stat(dir); err != nil {
				return fmt.Errorf("watched directory unavailable: %w", err)
			}
			fmt.Println("\nChanges detected, syncing...")
			if err := runFollowSync(cfg, dir, name, rootID, &failures, options); err != nil {
				return err
			}
			if failures.Active {
				dirty = true
				delay := failures.NextDelay(options)
				log.Printf("sync failed; retrying in %s: %v", delay, failures.LastError)
				resetFollowTimer(timer, &pending, delay)
				continue
			}
			dirty = false
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

func runFollowSync(cfg *appconfig.Config, dir, name, rootID string, failures *followFailureTracker, options followOptions) error {
	err := runSync(cfg, dir, name, rootID, false)
	if err == nil {
		failures.Reset()
		return nil
	}
	class := classifyFollowError(err)
	failures.Record(err, class)
	if class.Permanent {
		return fmt.Errorf("permanent sync failure: %w", err)
	}
	if failures.ShouldExit(options) {
		return fmt.Errorf("same sync failure repeated %d times over %s: %w", failures.SameCount, time.Since(failures.FirstSeen).Round(time.Second), err)
	}
	return nil
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
