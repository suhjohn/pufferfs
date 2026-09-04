package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestRunCapturedSyncBuildsMetadataFromUploadedBytes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES", "1")
	dir := t.TempDir()
	fileData := []byte("authoritative bytes\n")
	if err := os.WriteFile(filepath.Join(dir, "live.txt"), fileData, 0o600); err != nil {
		t.Fatal(err)
	}
	wantHash := contentHashForTest(fileData)

	var mu sync.Mutex
	artifacts := make(map[string][]byte)
	var submitted models.SyncRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/roots/root-1/sync/init":
			_ = json.NewEncoder(w).Encode(models.SyncInitResponse{
				RootID: "root-1", SyncJobID: "job-1", GenerationID: "gen-1", GenerationSeq: 1,
			})
		case r.URL.Path == "/roots/root-1/upload":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read source: %v", err)
				return
			}
			_ = json.NewEncoder(w).Encode(sourceUploadResponse{
				Key:         "syncs/gen-1/sources/files/.capture-test/live.txt",
				ContentHash: contentHashForTest(body),
				Size:        int64(len(body)),
			})
		case r.URL.Path == "/roots/root-1/upload-bundle":
			body, _ := io.ReadAll(r.Body)
			key := "syncs/gen-1/sources/bundles/" + r.URL.Query().Get("bundle_id")
			mu.Lock()
			artifacts[key] = body
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"key": key})
		case r.URL.Path == "/roots/root-1/sync/gen-1/upload":
			body, _ := io.ReadAll(r.Body)
			key := captureTestArtifactKey(r.URL.Query())
			mu.Lock()
			artifacts[key] = body
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"key": key})
		case r.URL.Path == "/roots/root-1/sync" && r.URL.Query().Get("async") == "true":
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Errorf("decode sync request: %v", err)
				return
			}
			_ = json.NewEncoder(w).Encode(models.SyncResponse{RootID: "root-1", GenerationID: "gen-1", GenerationSeq: 1, FilesProcessed: 1})
		case r.Method == http.MethodDelete && r.URL.Path == "/roots/root-1/sync/gen-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := &appconfig.Config{Server: appconfig.ServerConfig{URL: server.URL}}
	result, err := runCapturedSyncOnce(captureSyncInput{
		Config: cfg, Client: &apiClient{baseURL: server.URL, httpClient: server.Client()},
		Dir: dir, Name: "root", RootID: "root-1", BaseState: map[string]models.FileState{},
		WaitForCompletion: true, Log: io.Discard,
	})
	if err != nil {
		t.Fatalf("runCapturedSyncOnce: %v", err)
	}
	if result.Changes != 1 || len(result.FileChanges) != 1 || result.FileChanges[0].ContentHash != wantHash {
		t.Fatalf("result = %#v, want one change with uploaded hash %s", result, wantHash)
	}
	if submitted.ChangeCount != 1 || len(submitted.ChangeRefs) != 1 {
		t.Fatalf("submitted request = %#v", submitted)
	}

	mu.Lock()
	changeData := append([]byte(nil), artifacts[submitted.ChangeRefs[0]]...)
	stateData := append([]byte(nil), artifacts[submitted.StateRef]...)
	proofData := append([]byte(nil), artifacts[submitted.ContentProofRef]...)
	mu.Unlock()
	var change models.FileChange
	if err := json.Unmarshal(changeData, &change); err != nil {
		t.Fatalf("decode change: %v", err)
	}
	if change.ContentHash != wantHash || change.SourceLength != int64(len(fileData)) {
		t.Fatalf("change = %#v, want uploaded hash and length", change)
	}
	state := decodeCaptureTestState(t, stateData)
	if state["live.txt"].ContentHash != wantHash {
		t.Fatalf("state hash = %q, want %q", state["live.txt"].ContentHash, wantHash)
	}
	var proof models.ContentProofData
	if err := json.Unmarshal(proofData, &proof); err != nil {
		t.Fatalf("decode proof: %v", err)
	}
	if proof.FileHashes["live.txt"] != wantHash {
		t.Fatalf("proof hash = %q, want %q", proof.FileHashes["live.txt"], wantHash)
	}
}

func captureTestArtifactKey(values url.Values) string {
	kind := values.Get("kind")
	name := values.Get("name")
	switch kind {
	case "manifest":
		return "syncs/gen-1/manifests/" + name
	case "proof":
		return "syncs/gen-1/proofs/" + name
	case "state":
		return "syncs/gen-1/state/" + name
	default:
		return "unexpected/" + name
	}
}

func decodeCaptureTestState(t *testing.T, data []byte) map[string]models.FileState {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open state gzip: %v", err)
	}
	defer reader.Close()
	var state map[string]models.FileState
	if err := json.NewDecoder(reader).Decode(&state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return state
}
