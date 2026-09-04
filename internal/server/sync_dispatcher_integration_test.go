package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/pufferfs/pufferfs/internal/queue"
)

func TestSyncDispatcherStageTransitionsWithLocalJetStream(t *testing.T) {
	t.Setenv("PUFFERFS_CLEANUP_SYNC_ARTIFACTS", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := runDispatcherTestNATS(t)
	q, err := queue.NewNATSQueue(ns.ClientURL(), queue.WithConsumerPrefix("dispatcher-test"))
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	defer q.Close()

	store := newMemoryObjectStore()
	modal, tp := newFakeSyncServices(t)
	srv := NewWithStore(nil, store, modal, tp)

	initial := queue.JobMessage{
		JobID:             "chunk-job",
		OrgID:             "org-1",
		RootID:            "root-1",
		GenerationID:      "gen-1",
		GenerationSeq:     2,
		BaseGenerationID:  "gen-0",
		BaseGenerationSeq: 1,
		Stage:             queue.StageChunk,
		PayloadRef:        "syncs/gen-1/inputs/shard-000000.jsonl",
		IndexNamespaces: []queue.IndexNamespace{{
			Namespace:  "org-org-1-root-root-1",
			ShardCount: 1,
		}},
		ShardIndex:    0,
		TotalShards:   1,
		FilesInShard:  1,
		DisableVector: true,
	}
	if err := q.Enqueue(ctx, queue.StageChunk, initial); err != nil {
		t.Fatalf("enqueue chunk: %v", err)
	}
	if err := store.Upload(ctx, initial.PayloadRef, []byte("{\"path\":\"a.txt\",\"status\":\"ADDED\",\"content_hash\":\"hash-a\",\"size\":12,\"source_key\":\"files/root-1/a.txt\",\"source_length\":12}\n"), "application/x-ndjson"); err != nil {
		t.Fatalf("upload input artifact: %v", err)
	}
	if err := store.Upload(ctx, "files/root-1/a.txt", []byte("hello world\n"), "text/plain"); err != nil {
		t.Fatalf("upload source: %v", err)
	}

	chunkDispatcher := NewSyncDispatcher(srv, q, queue.StageChunk, 1)
	chunkMsg := pullOne(t, ctx, q, queue.StageChunk)
	if err := chunkDispatcher.Process(ctx, chunkMsg.Job); err != nil {
		t.Fatalf("process chunk: %v", err)
	}
	if err := q.Ack(chunkMsg); err != nil {
		t.Fatalf("ack chunk: %v", err)
	}

	indexMsg := pullOne(t, ctx, q, queue.StageIndex)
	if indexMsg.Job.PayloadRef != "syncs/gen-1/chunks/chunk-job.jsonl.gz" {
		t.Fatalf("index payload ref = %q", indexMsg.Job.PayloadRef)
	}
	if indexMsg.Job.FilesInShard != 1 {
		t.Fatalf("index files_in_shard = %d, want 1", indexMsg.Job.FilesInShard)
	}
	artifact, err := store.Download(ctx, indexMsg.Job.PayloadRef)
	if err != nil {
		t.Fatalf("download compressed chunk artifact: %v", err)
	}
	if len(artifact) < 2 || artifact[0] != 0x1f || artifact[1] != 0x8b {
		t.Fatalf("chunk artifact is not gzip data: prefix=%x", artifact[:min(len(artifact), 2)])
	}
	indexDispatcher := NewSyncDispatcher(srv, q, queue.StageIndex, 1)
	if err := indexDispatcher.Process(ctx, indexMsg.Job); err != nil {
		t.Fatalf("process index: %v", err)
	}
	_ = q.Ack(indexMsg)

	for _, key := range []string{initial.PayloadRef, indexMsg.Job.PayloadRef} {
		if !store.Has(key) {
			t.Fatalf("artifact %s was deleted before terminal cleanup", key)
		}
	}
	commitMsg := pullOne(t, ctx, q, queue.StageCommit)
	if commitMsg.Job.Stage != queue.StageCommit || commitMsg.Job.TotalShards != 1 {
		t.Fatalf("unexpected commit job: %#v", commitMsg.Job)
	}
}

func TestSyncDispatcherVectorShardIndexesThroughModalWithoutIntermediateArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := runDispatcherTestNATS(t)
	q, err := queue.NewNATSQueue(ns.ClientURL(), queue.WithConsumerPrefix("modal-index-test"))
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	defer q.Close()

	var received ModalShardRequest
	modalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode Modal request: %v", err)
		}
		if received.SecretKey != "shared-secret" {
			t.Fatalf("Modal request missing shared secret")
		}
		writeJSONResponse(t, w, ModalShardResponse{Status: "indexed"})
	}))
	defer modalServer.Close()

	store := newMemoryObjectStore()
	modal := &ModalClient{
		indexShardURL: modalServer.URL,
		secretKey:     "shared-secret",
		httpClient:    modalServer.Client(),
	}
	srv := NewWithStore(nil, store, modal, nil)
	initial := queue.JobMessage{
		JobID:             "vector-chunk-job",
		OrgID:             "org-1",
		RootID:            "root-1",
		GenerationID:      "gen-vector",
		GenerationSeq:     2,
		BaseGenerationID:  "gen-0",
		BaseGenerationSeq: 1,
		Stage:             queue.StageChunk,
		PayloadRef:        "syncs/gen-vector/inputs/shard-000000.jsonl",
		IndexNamespaces: []queue.IndexNamespace{{
			Namespace:  "org-org-1-root-root-1",
			ShardCount: 1,
		}},
		TotalShards:  1,
		FilesInShard: 1,
	}
	if err := store.Upload(ctx, initial.PayloadRef, []byte("{\"path\":\"a.txt\",\"status\":\"ADDED\",\"content_hash\":\"hash-a\",\"size\":12,\"source_key\":\"files/root-1/a.txt\",\"source_length\":12}\n"), "application/x-ndjson"); err != nil {
		t.Fatalf("upload input artifact: %v", err)
	}
	if err := store.Upload(ctx, "files/root-1/a.txt", []byte("hello world\n"), "text/plain"); err != nil {
		t.Fatalf("upload source: %v", err)
	}
	if err := q.Enqueue(ctx, queue.StageChunk, initial); err != nil {
		t.Fatalf("enqueue chunk: %v", err)
	}

	chunkDispatcher := NewSyncDispatcher(srv, q, queue.StageChunk, 1)
	chunkMsg := pullOne(t, ctx, q, queue.StageChunk)
	if err := chunkDispatcher.Process(ctx, chunkMsg.Job); err != nil {
		t.Fatalf("process chunk: %v", err)
	}
	_ = q.Ack(chunkMsg)

	indexMsg := pullOne(t, ctx, q, queue.StageIndex)
	indexDispatcher := NewSyncDispatcher(srv, q, queue.StageIndex, 1)
	if err := indexDispatcher.Process(ctx, indexMsg.Job); err != nil {
		t.Fatalf("process Modal index: %v", err)
	}
	_ = q.Ack(indexMsg)

	if received.Job.PayloadRef != indexMsg.Job.PayloadRef || !strings.HasSuffix(received.Job.PayloadRef, ".jsonl.gz") {
		t.Fatalf("Modal payload ref = %q, want %q", received.Job.PayloadRef, indexMsg.Job.PayloadRef)
	}
	if store.HasPrefix("syncs/gen-vector/index_rows/") {
		t.Fatal("indexing wrote an intermediate index_rows artifact")
	}
	commitMsg := pullOne(t, ctx, q, queue.StageCommit)
	if commitMsg.Job.GenerationID != "gen-vector" {
		t.Fatalf("unexpected commit job: %#v", commitMsg.Job)
	}
}

func TestTerminalCleanupPreservesCommittedArtifactsAndDeletesFailedState(t *testing.T) {
	t.Setenv("PUFFERFS_CLEANUP_SYNC_ARTIFACTS", "true")
	ctx := context.Background()
	store := newMemoryObjectStore()
	imageKey := "chunks/root-1/document.pdf.0.jpg"
	stateKey := "states/root-1/gen-1.json.gz"
	syncKey := "syncs/gen-1/chunks/job.jsonl"
	for key, data := range map[string]string{imageKey: "image", stateKey: "state", syncKey: "artifact"} {
		if err := store.Upload(ctx, key, []byte(data), "application/octet-stream"); err != nil {
			t.Fatalf("upload %s: %v", key, err)
		}
	}
	srv := NewWithStore(nil, store, &ModalClient{}, nil)
	if err := srv.cleanupTerminalSyncObjects(ctx, "root-1", "gen-1", nil, false); err != nil {
		t.Fatalf("terminal cleanup: %v", err)
	}
	if !store.Has(imageKey) {
		t.Fatalf("cleanup deleted indexed image artifact %s", imageKey)
	}
	if !store.Has(stateKey) {
		t.Fatalf("cleanup deleted committed state %s", stateKey)
	}
	if store.Has(syncKey) {
		t.Fatalf("cleanup left sync artifact %s", syncKey)
	}
	failedStateKey := "states/root-1/gen-2.json.gz"
	if err := store.Upload(ctx, failedStateKey, []byte("state"), "application/gzip"); err != nil {
		t.Fatalf("upload failed state: %v", err)
	}
	if err := srv.cleanupTerminalSyncObjects(ctx, "root-1", "gen-2", nil, true); err != nil {
		t.Fatalf("failed terminal cleanup: %v", err)
	}
	if store.Has(failedStateKey) {
		t.Fatalf("cleanup left failed state %s", failedStateKey)
	}
}

func pullOne(t *testing.T, ctx context.Context, q queue.Queue, stage string) queue.ReceivedMessage {
	t.Helper()
	msgs, err := q.Pull(ctx, stage, 1, time.Second)
	if err != nil {
		t.Fatalf("pull %s: %v", stage, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("pull %s got %d messages, want 1", stage, len(msgs))
	}
	return msgs[0]
}

func newFakeSyncServices(t *testing.T) (*ModalClient, *TPClient) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/namespaces/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &ModalClient{}, NewTPClientWithURL("test", srv.URL)
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("writing response: %v", err)
	}
}

func runDispatcherTestNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("creating embedded NATS: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded NATS did not become ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	return ns
}

type memoryObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemoryObjectStore() *memoryObjectStore {
	return &memoryObjectStore{objects: map[string][]byte{}}
}

func (s *memoryObjectStore) Upload(_ context.Context, key string, data []byte, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append([]byte(nil), data...)
	return nil
}

func (s *memoryObjectStore) UploadStream(_ context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	return s.Upload(context.Background(), key, data, "")
}

func (s *memoryObjectStore) Download(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %s not found", key)
	}
	return append([]byte(nil), data...), nil
}

func (s *memoryObjectStore) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	data, err := s.Download(ctx, key)
	if err != nil {
		return nil, err
	}
	if length > 0 {
		end := offset + length
		if offset < 0 || end > int64(len(data)) {
			return nil, fmt.Errorf("range %d-%d outside object %s length %d", offset, end, key, len(data))
		}
		data = data[offset:end]
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *memoryObjectStore) DeleteMany(_ context.Context, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		delete(s.objects, key)
	}
	return nil
}

func (s *memoryObjectStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			delete(s.objects, key)
			deleted++
		}
	}
	return deleted, nil
}

func (s *memoryObjectStore) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

func (s *memoryObjectStore) HasPrefix(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
