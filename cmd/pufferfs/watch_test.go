package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClassifyFollowErrorPermanentHTTP(t *testing.T) {
	class := classifyFollowError(&apiError{StatusCode: http.StatusForbidden, Body: []byte("denied")})
	if !class.Permanent || class.Key != "http:403" {
		t.Fatalf("class = %#v, want permanent http:403", class)
	}
}

func TestClassifyFollowErrorTransientHTTP(t *testing.T) {
	class := classifyFollowError(&apiError{StatusCode: http.StatusServiceUnavailable, Body: []byte("try later")})
	if class.Permanent || class.Key != "http:503" {
		t.Fatalf("class = %#v, want transient http:503", class)
	}
}

func TestFollowFailureTrackerBackoffAndExit(t *testing.T) {
	options := followOptions{
		MaxBackoff:           10 * time.Second,
		MaxSameFailures:      3,
		MaxSameFailureWindow: time.Hour,
	}
	var tracker followFailureTracker
	class := followErrorClass{Key: "http:503"}

	tracker.Record(errors.New("HTTP 503: unavailable"), class)
	if got := tracker.NextDelay(options); got != time.Second {
		t.Fatalf("first backoff = %s, want 1s", got)
	}
	if tracker.ShouldExit(options) {
		t.Fatal("first failure should not exit")
	}

	tracker.Record(errors.New("HTTP 503: unavailable"), class)
	if got := tracker.NextDelay(options); got != 2*time.Second {
		t.Fatalf("second backoff = %s, want 2s", got)
	}

	tracker.Record(errors.New("HTTP 503: unavailable"), class)
	if !tracker.ShouldExit(options) {
		t.Fatal("third identical failure should exit")
	}
}

func TestNormalizeErrorString(t *testing.T) {
	err := errors.New("  Request   Failed:   Connection Refused  ")
	got := normalizeErrorString(err)
	if got != "request failed: connection refused" {
		t.Fatalf("normalized error = %q", got)
	}
	long := normalizeErrorString(errors.New(strings.Repeat("x", 300)))
	if len(long) != 180 {
		t.Fatalf("long normalized error len = %d, want 180", len(long))
	}
}

func TestWatchCommandIsHiddenDeprecatedAlias(t *testing.T) {
	cmd := watchCmd()
	if !cmd.Hidden {
		t.Fatal("watch command should be hidden from help")
	}
	if cmd.Deprecated == "" {
		t.Fatal("watch command should be marked deprecated")
	}
	if !strings.Contains(cmd.Deprecated, "sync --follow") {
		t.Fatalf("deprecation should point to sync --follow, got %q", cmd.Deprecated)
	}
}
