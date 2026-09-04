package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestCaptureStandaloneUsesUploadedPrefixAndMarksGrowingFileDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	initial := []byte("first\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open append: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = file.WriteString("second\n")
		_ = file.Close()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		sum := sha256.Sum256(body)
		_ = json.NewEncoder(w).Encode(sourceUploadResponse{
			Key:         "syncs/gen-1/sources/files/.capture-id/live.jsonl",
			ContentHash: "sha256:" + hex.EncodeToString(sum[:]),
			Size:        int64(len(body)),
		})
	}))
	defer server.Close()

	capture, err := captureStandaloneFile(
		&apiClient{baseURL: server.URL, httpClient: server.Client()},
		"root-1", "gen-1", "live.jsonl", path,
	)
	if err != nil {
		t.Fatalf("captureStandaloneFile: %v", err)
	}
	if capture.Size != int64(len(initial)) {
		t.Fatalf("capture size = %d, want %d", capture.Size, len(initial))
	}
	if capture.ContentHash != contentHashForTest(initial) {
		t.Fatalf("capture hash = %q, want hash of initial prefix", capture.ContentHash)
	}
	if !capture.Dirty {
		t.Fatal("growing source was not marked dirty")
	}
}

func TestCaptureStandaloneUsesSuccessfulRetryDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.bin")
	first := []byte("first!")
	second := []byte("second")
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if requests.Add(1) == 1 {
			if err := os.WriteFile(path, second, 0o600); err != nil {
				t.Errorf("rewrite source: %v", err)
			}
			http.Error(w, "retry", http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(body)
		_ = json.NewEncoder(w).Encode(sourceUploadResponse{
			Key:         "capture-2",
			ContentHash: "sha256:" + hex.EncodeToString(sum[:]),
			Size:        int64(len(body)),
		})
	}))
	defer server.Close()

	capture, err := captureStandaloneFile(
		&apiClient{baseURL: server.URL, httpClient: server.Client(), sleep: func(_ time.Duration) {}},
		"root-1", "gen-1", "live.bin", path,
	)
	if err != nil {
		t.Fatalf("captureStandaloneFile: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
	if capture.ContentHash != contentHashForTest(second) {
		t.Fatalf("capture hash = %q, want successful retry hash %q", capture.ContentHash, contentHashForTest(second))
	}
	if !capture.Dirty {
		t.Fatal("rewritten source was not marked dirty")
	}
}

func TestSourceChangedAfterCaptureDetectsAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "document.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if !sourceChangedAfterCapture(path, file, before) {
		t.Fatal("atomic path replacement was not detected")
	}
}

func TestCaptureFilesDefersFileTruncatedDuringStandaloneUpload(t *testing.T) {
	t.Setenv("PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "live.bin")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &apiClient{
		baseURL: "http://capture.test",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := os.Truncate(path, 0); err != nil {
				t.Errorf("truncate source: %v", err)
			}
			_, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Body:       io.NopCloser(bytes.NewBufferString("short body")),
				Header:     make(http.Header),
			}, nil
		})},
	}
	batch, err := captureFiles(client, "root-1", "gen-1", dir, []captureCandidate{{Path: "live.bin", Size: 7}})
	if err != nil {
		t.Fatalf("captureFiles: %v", err)
	}
	if len(batch.Files) != 0 || batch.Deferred["live.bin"] == nil {
		t.Fatalf("batch = %#v, want live.bin deferred", batch)
	}
}

func TestCaptureStandaloneDoesNotHideUploadFailureWhenSourceAlsoGrows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.bin")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Errorf("open append: %v", err)
			} else {
				_, _ = file.WriteString("more")
				_ = file.Close()
			}
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := captureStandaloneFile(
		&apiClient{baseURL: server.URL, httpClient: server.Client(), sleep: func(time.Duration) {}},
		"root-1", "gen-1", "live.bin", path,
	)
	if err == nil {
		t.Fatal("capture succeeded, want upload failure")
	}
	var changed *sourceChangedError
	if errors.As(err, &changed) {
		t.Fatalf("upload failure was misclassified as source mutation: %v", err)
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want HTTP 503", err)
	}
}

func TestDiscoverCapturePlanReusesStableCacheButRecapturesDirtyPath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"stable.txt", "dirty.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := make(map[string]models.FileState)
	for _, name := range []string{"stable.txt", "dirty.txt"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		base[name] = models.FileState{Size: info.Size(), Mtime: info.ModTime().UnixNano(), ContentHash: "sha256:" + string(make([]byte, 64))}
	}
	plan, err := discoverCapturePlan(dir, ignore.NewMatcher(dir), base, base, map[string]bool{"dirty.txt": true}, nil, false)
	if err != nil {
		t.Fatalf("discoverCapturePlan: %v", err)
	}
	if plan.CacheHits != 1 || len(plan.Candidates) != 1 || plan.Candidates[0].Path != "dirty.txt" {
		t.Fatalf("plan = %#v, want one stable hit and dirty.txt candidate", plan)
	}
}

func TestCapturedDiffDefersPreviousVersion(t *testing.T) {
	base := map[string]models.FileState{
		"live.txt": {Size: 3, ContentHash: "sha256:old", Mtime: 1},
	}
	current := map[string]models.FileState{
		"live.txt": base["live.txt"],
		"new.txt":  {Size: 3, ContentHash: "sha256:new", Mtime: 2},
	}
	result := capturedDiff(base, current, nil, true, map[string]error{"live.txt": os.ErrNotExist})
	if len(result.Changes) != 1 || result.Changes[0].Path != "new.txt" || result.Changes[0].Status != models.StatusAdded {
		t.Fatalf("changes = %#v, want only new.txt", result.Changes)
	}
}

func contentHashForTest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
