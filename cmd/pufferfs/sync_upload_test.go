package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestCaptureFilesParallelizesStandaloneUploadsAndPreservesOrder(t *testing.T) {
	t.Setenv("PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES", "1")
	t.Setenv("PUFFERFS_UPLOAD_CONCURRENCY", "2")
	dir := t.TempDir()
	candidates := make([]captureCandidate, 4)
	for i := range candidates {
		name := fmt.Sprintf("%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
		candidates[i] = captureCandidate{Path: name, Size: 2}
	}

	var active atomic.Int32
	var maxActive atomic.Int32
	var fileRequests atomic.Int32
	var manifestRequests atomic.Int32
	secondStarted := make(chan struct{})
	var secondStartedOnce sync.Once
	var manifestMu sync.Mutex
	var manifest []bundleManifestEntry
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/roots/root-1/upload":
			fileRequests.Add(1)
			current := active.Add(1)
			updateAtomicMax(&maxActive, current)
			defer active.Add(-1)
			if current >= 2 {
				secondStartedOnce.Do(func() { close(secondStarted) })
			}
			select {
			case <-secondStarted:
			case <-time.After(time.Second):
				t.Error("second standalone upload did not start")
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading upload: %v", err)
			}
			path := r.URL.Query().Get("path")
			time.Sleep(time.Duration(4-int(path[0]-'0')) * time.Millisecond)
			_ = json.NewEncoder(w).Encode(sourceUploadResponse{Key: "objects/" + path, ContentHash: contentHashForTest(data), Size: int64(len(data))})
		case "/roots/root-1/upload-bundle":
			manifestRequests.Add(1)
			if !strings.HasSuffix(r.URL.Query().Get("bundle_id"), "-manifest") {
				t.Errorf("unexpected source bundle %q", r.URL.Query().Get("bundle_id"))
			}
			var got []bundleManifestEntry
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decoding manifest: %v", err)
			}
			manifestMu.Lock()
			manifest = got
			manifestMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "manifest-key"})
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	batch, err := captureFiles(&apiClient{baseURL: server.URL, httpClient: server.Client()}, "root-1", "gen-1", dir, candidates)
	if err != nil {
		t.Fatalf("captureFiles: %v", err)
	}
	if batch.ManifestRef != "manifest-key" {
		t.Fatalf("manifest ref = %q, want manifest-key", batch.ManifestRef)
	}
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("max concurrent standalone uploads = %d, want 2", got)
	}
	if got := fileRequests.Load(); got != 4 {
		t.Fatalf("file requests = %d, want 4", got)
	}
	if got := manifestRequests.Load(); got != 1 {
		t.Fatalf("manifest requests = %d, want 1", got)
	}

	manifestMu.Lock()
	defer manifestMu.Unlock()
	if len(manifest) != len(candidates) {
		t.Fatalf("manifest entries = %d, want %d", len(manifest), len(candidates))
	}
	for i := range candidates {
		wantPath := fmt.Sprintf("%d.txt", i)
		wantKey := "objects/" + wantPath
		capture := batch.Files[wantPath]
		if capture.SourceKey != wantKey || capture.SourceOffset != 0 || capture.SourceLength != 2 {
			t.Fatalf("capture %d source = %#v", i, capture)
		}
		if manifest[i].Path != wantPath || manifest[i].ObjectKey != wantKey || manifest[i].Length != 2 {
			t.Fatalf("manifest entry %d = %#v", i, manifest[i])
		}
	}
}

func TestCaptureFilesBundlesOverlapStandaloneUploadsWithinLimit(t *testing.T) {
	t.Setenv("PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES", "3")
	t.Setenv("PUFFERFS_UPLOAD_BUNDLE_MAX_BYTES", "3")
	t.Setenv("PUFFERFS_UPLOAD_CONCURRENCY", "2")
	dir := t.TempDir()
	files := []struct {
		path string
		data string
	}{
		{path: "large.txt", data: "four"},
		{path: "a.txt", data: "aa"},
		{path: "b.txt", data: "bb"},
	}
	candidates := make([]captureCandidate, len(files))
	for i, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file.path), []byte(file.data), 0o644); err != nil {
			t.Fatal(err)
		}
		candidates[i] = captureCandidate{Path: file.path, Size: int64(len(file.data))}
	}

	var active atomic.Int32
	var maxActive atomic.Int32
	largeStarted := make(chan struct{})
	bundleStarted := make(chan struct{})
	var largeOnce sync.Once
	var bundleOnce sync.Once
	var manifestMu sync.Mutex
	var manifest []bundleManifestEntry
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bundleID := r.URL.Query().Get("bundle_id")
		isManifest := strings.HasSuffix(bundleID, "-manifest")
		if r.URL.Path == "/roots/root-1/upload-bundle" && isManifest {
			var got []bundleManifestEntry
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decoding manifest: %v", err)
			}
			manifestMu.Lock()
			manifest = got
			manifestMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "manifest-key"})
			return
		}

		current := active.Add(1)
		updateAtomicMax(&maxActive, current)
		defer active.Add(-1)
		switch r.URL.Path {
		case "/roots/root-1/upload":
			largeOnce.Do(func() { close(largeStarted) })
			select {
			case <-bundleStarted:
			case <-time.After(time.Second):
				t.Error("source bundle did not overlap standalone upload")
			}
			data, _ := io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(sourceUploadResponse{Key: "file/large.txt", ContentHash: contentHashForTest(data), Size: int64(len(data))})
		case "/roots/root-1/upload-bundle":
			bundleOnce.Do(func() { close(bundleStarted) })
			select {
			case <-largeStarted:
			case <-time.After(time.Second):
				t.Error("standalone upload did not overlap source bundle")
			}
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "bundle/" + bundleID})
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	batch, err := captureFiles(&apiClient{baseURL: server.URL, httpClient: server.Client()}, "root-1", "gen-1", dir, candidates)
	if err != nil {
		t.Fatalf("captureFiles: %v", err)
	}
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("max concurrent source uploads = %d, want 2", got)
	}
	if batch.Files[files[0].path].SourceKey != "file/large.txt" {
		t.Fatalf("standalone source key = %q", batch.Files[files[0].path].SourceKey)
	}
	if batch.Files[files[1].path].SourceKey == "" || batch.Files[files[2].path].SourceKey == "" || batch.Files[files[1].path].SourceKey == batch.Files[files[2].path].SourceKey {
		t.Fatalf("bundle source keys = %q, %q", batch.Files[files[1].path].SourceKey, batch.Files[files[2].path].SourceKey)
	}
	manifestMu.Lock()
	defer manifestMu.Unlock()
	if len(manifest) != 3 || manifest[0].ObjectKey != batch.Files[files[0].path].SourceKey || manifest[1].BundleKey != batch.Files[files[1].path].SourceKey || manifest[2].BundleKey != batch.Files[files[2].path].SourceKey {
		t.Fatalf("manifest/source mapping mismatch: manifest=%#v captures=%#v", manifest, batch.Files)
	}
}

func TestCaptureFilesWaitsForInFlightUploadsOnError(t *testing.T) {
	t.Setenv("PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES", "1")
	t.Setenv("PUFFERFS_UPLOAD_CONCURRENCY", "2")
	dir := t.TempDir()
	candidates := []captureCandidate{
		{Path: "slow.txt", Size: 2},
		{Path: "bad.txt", Size: 2},
	}
	for _, candidate := range candidates {
		if err := os.WriteFile(filepath.Join(dir, candidate.Path), []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var active atomic.Int32
	var finished atomic.Int32
	var manifestRequests atomic.Int32
	var started atomic.Int32
	bothStarted := make(chan struct{})
	badFinished := make(chan struct{})
	var bothStartedOnce sync.Once
	var badFinishedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/roots/root-1/upload-bundle" {
			manifestRequests.Add(1)
			http.Error(w, "manifest should not upload", http.StatusInternalServerError)
			return
		}
		active.Add(1)
		defer active.Add(-1)
		defer finished.Add(1)
		if started.Add(1) >= 2 {
			bothStartedOnce.Do(func() { close(bothStarted) })
		}
		select {
		case <-bothStarted:
		case <-time.After(time.Second):
			t.Error("both uploads did not start")
		}
		if r.URL.Query().Get("path") == "bad.txt" {
			badFinishedOnce.Do(func() { close(badFinished) })
			http.Error(w, "bad file", http.StatusBadRequest)
			return
		}
		select {
		case <-badFinished:
		case <-time.After(time.Second):
			t.Error("failing upload did not finish")
		}
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(sourceUploadResponse{Key: "file/slow.txt", ContentHash: contentHashForTest([]byte("ok")), Size: 2})
	}))
	defer server.Close()

	_, err := captureFiles(&apiClient{baseURL: server.URL, httpClient: server.Client()}, "root-1", "gen-1", dir, candidates)
	if err == nil || !strings.Contains(err.Error(), "capturing bad.txt") {
		t.Fatalf("err = %v, want bad.txt upload error", err)
	}
	if got := active.Load(); got != 0 {
		t.Fatalf("active uploads after return = %d, want 0", got)
	}
	if got := finished.Load(); got != 2 {
		t.Fatalf("finished uploads = %d, want 2", got)
	}
	if got := manifestRequests.Load(); got != 0 {
		t.Fatalf("manifest requests = %d, want 0", got)
	}
}

func TestUploadSyncMetadataRunsBoundedAndKeepsShardOrder(t *testing.T) {
	t.Setenv("PUFFERFS_UPLOAD_CHANGE_SHARD_MAX_FILES", "1")
	t.Setenv("PUFFERFS_UPLOAD_CONCURRENCY", "2")
	changes := []models.FileChange{{Path: "a"}, {Path: "b"}, {Path: "c"}}

	var active atomic.Int32
	var maxActive atomic.Int32
	var heavyActive atomic.Int32
	var maxHeavyActive atomic.Int32
	secondStarted := make(chan struct{})
	var secondOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		updateAtomicMax(&maxActive, current)
		defer active.Add(-1)
		if current >= 2 {
			secondOnce.Do(func() { close(secondStarted) })
		}
		select {
		case <-secondStarted:
		case <-time.After(time.Second):
			t.Error("second metadata upload did not start")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		kind := r.URL.Query().Get("kind")
		name := r.URL.Query().Get("name")
		if kind == "proof" || kind == "state" {
			heavy := heavyActive.Add(1)
			updateAtomicMax(&maxHeavyActive, heavy)
			defer heavyActive.Add(-1)
			time.Sleep(20 * time.Millisecond)
		}
		if name == "000000.jsonl" {
			time.Sleep(10 * time.Millisecond)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"key": kind + "/" + name})
	}))
	defer server.Close()

	refs, err := uploadSyncMetadata(
		&apiClient{baseURL: server.URL, httpClient: server.Client()},
		"root-1",
		"gen-1",
		changes,
		&models.ContentProofData{RootHash: "root-hash"},
		map[string]models.FileState{"a": {Size: 1}},
	)
	if err != nil {
		t.Fatalf("uploadSyncMetadata: %v", err)
	}
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("max concurrent metadata uploads = %d, want 2", got)
	}
	if got := maxHeavyActive.Load(); got != 1 {
		t.Fatalf("max concurrent proof/state uploads = %d, want 1", got)
	}
	wantChangeRefs := []string{"manifest/000000.jsonl", "manifest/000001.jsonl", "manifest/000002.jsonl"}
	if fmt.Sprint(refs.ChangeRefs) != fmt.Sprint(wantChangeRefs) {
		t.Fatalf("change refs = %v, want %v", refs.ChangeRefs, wantChangeRefs)
	}
	if refs.ContentProofRef != "proof/content-proof.json" || refs.StateRef != "state/state.json.gz" {
		t.Fatalf("metadata refs = %#v", refs)
	}
}

func TestUploadConcurrencyDefaultsAndCaps(t *testing.T) {
	t.Setenv("PUFFERFS_UPLOAD_CONCURRENCY", "invalid")
	if got := uploadConcurrency(); got != 4 {
		t.Fatalf("invalid concurrency = %d, want 4", got)
	}
	t.Setenv("PUFFERFS_UPLOAD_CONCURRENCY", "100")
	if got := uploadConcurrency(); got != 16 {
		t.Fatalf("capped concurrency = %d, want 16", got)
	}
}

func updateAtomicMax(max *atomic.Int32, value int32) {
	for {
		current := max.Load()
		if value <= current || max.CompareAndSwap(current, value) {
			return
		}
	}
}
