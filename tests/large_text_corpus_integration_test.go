//go:build cli_integration
// +build cli_integration

package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLargeTextCorpusManifestSession(t *testing.T) {
	rawCount := os.Getenv("PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT")
	if rawCount == "" {
		t.Skip("PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT not set")
	}
	fileCount, err := strconv.Atoi(rawCount)
	if err != nil || fileCount < 1 {
		t.Fatalf("invalid PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT=%q", rawCount)
	}

	services := requireRealServices(t)
	setupE2EInfra(t)

	env := newE2EEnv(t, services, "")
	recorder := newSyncRequestRecorder(t, env.serverURL)
	defer recorder.Close()
	env.serverURL = recorder.URL

	homeDir := t.TempDir()
	initPufferFS(t, env, homeDir)

	projectDir := filepath.Join(homeDir, "large-text-workspace")
	for i := 0; i < fileCount; i++ {
		ext := ".txt"
		if i%2 == 1 {
			ext = ".md"
		}
		rel := filepath.Join(fmt.Sprintf("batch-%04d", i/1000), fmt.Sprintf("doc-%07d%s", i, ext))
		writeFile(t, projectDir, rel, fmt.Sprintf("large text-only manifest corpus file %d\nunique-token-%07d\n", i, i))
	}

	recorder.Reset()
	start := time.Now()
	stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
	syncElapsed := time.Since(start)
	metrics := recorder.Snapshot(fileCount, syncElapsed)
	writeLargeTextMetrics(t, metrics)
	if err != nil {
		t.Fatalf("large text-only sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	requireOutputContains(t, stdout, "Sync complete")
	if metrics.FinalSyncRequestBytes <= 0 {
		t.Fatalf("expected to observe final sync request, metrics=%+v", metrics)
	}
	if metrics.FinalSyncRequestBytes > 1<<20 {
		t.Fatalf("final sync request = %d bytes, want <= 1MiB; metrics=%+v", metrics.FinalSyncRequestBytes, metrics)
	}
	if fileCount >= 100 && metrics.WriteRequests >= fileCount/2 {
		t.Fatalf("write/control requests = %d for %d files, expected batching rather than per-file requests; metrics=%+v", metrics.WriteRequests, fileCount, metrics)
	}

	rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
	namespaces := rootIndexNamespaces(t, rootID)
	cleanupDone := false
	t.Cleanup(func() {
		if !cleanupDone {
			adminDelete(t, env.serverURL, "/admin/orgs/"+neturl.PathEscape(env.orgID))
			deleteTPNamespaces(t, services, namespaces)
		}
	})

	lastExt := ".txt"
	if (fileCount-1)%2 == 1 {
		lastExt = ".md"
	}
	lastPath := filepath.ToSlash(filepath.Join(fmt.Sprintf("batch-%04d", (fileCount-1)/1000), fmt.Sprintf("doc-%07d%s", fileCount-1, lastExt)))
	assertCLIQuery(t, homeDir, env, fmt.Sprintf("unique-token-%07d", fileCount-1), env.rootName, "hybrid", "", lastPath)

	deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
	cleanupDone = true
}

type syncRequestMetrics struct {
	FileCount             int           `json:"file_count"`
	ElapsedMillis         int64         `json:"elapsed_millis"`
	Requests              int           `json:"requests"`
	WriteRequests         int           `json:"write_requests"`
	FinalSyncRequestBytes int64         `json:"final_sync_request_bytes"`
	MaxRequestBytes       int64         `json:"max_request_bytes"`
	ArtifactUploads       int           `json:"artifact_uploads"`
	BundleUploads         int           `json:"bundle_uploads"`
	LegacyFileUploads     int           `json:"legacy_file_uploads"`
	Elapsed               time.Duration `json:"-"`
}

type recordedRequest struct {
	Method string
	Path   string
	Bytes  int64
}

type syncRequestRecorder struct {
	*httptest.Server
	mu      sync.Mutex
	records []recordedRequest
}

func newSyncRequestRecorder(t *testing.T, targetURL string) *syncRequestRecorder {
	t.Helper()
	target, err := neturl.Parse(targetURL)
	if err != nil {
		t.Fatalf("parsing target URL %s: %v", targetURL, err)
	}
	rec := &syncRequestRecorder{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
	}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter := &countingReadCloser{ReadCloser: r.Body}
		r.Body = counter
		proxy.ServeHTTP(w, r)
		rec.mu.Lock()
		rec.records = append(rec.records, recordedRequest{Method: r.Method, Path: r.URL.Path, Bytes: counter.bytes})
		rec.mu.Unlock()
	}))
	return rec
}

func (r *syncRequestRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
}

func (r *syncRequestRecorder) Snapshot(fileCount int, elapsed time.Duration) syncRequestMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	metrics := syncRequestMetrics{FileCount: fileCount, Elapsed: elapsed, ElapsedMillis: elapsed.Milliseconds()}
	for _, record := range r.records {
		metrics.Requests++
		if record.Bytes > metrics.MaxRequestBytes {
			metrics.MaxRequestBytes = record.Bytes
		}
		if record.Method == http.MethodPost || record.Method == http.MethodPut || record.Method == http.MethodPatch {
			metrics.WriteRequests++
		}
		if record.Method == http.MethodPost && strings.HasSuffix(record.Path, "/sync") {
			metrics.FinalSyncRequestBytes = record.Bytes
		}
		switch {
		case strings.Contains(record.Path, "/sync/") && strings.HasSuffix(record.Path, "/upload"):
			metrics.ArtifactUploads++
		case strings.HasSuffix(record.Path, "/upload-bundle"):
			metrics.BundleUploads++
		case strings.HasSuffix(record.Path, "/upload"):
			metrics.LegacyFileUploads++
		}
	}
	return metrics
}

type countingReadCloser struct {
	io.ReadCloser
	bytes int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.bytes += int64(n)
	return n, err
}

func writeLargeTextMetrics(t *testing.T, metrics syncRequestMetrics) {
	t.Helper()
	data, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		t.Fatalf("marshaling large text metrics: %v", err)
	}
	t.Logf("large text corpus metrics: %s", data)
	if path := os.Getenv("PUFFERFS_E2E_LARGE_TEXT_METRICS"); path != "" {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("writing large text metrics to %s: %v", path, err)
		}
	}
}
