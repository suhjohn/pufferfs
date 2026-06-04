package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/pufferfs/pufferfs/internal/queue"
)

func TestSyncDispatcherStageTransitionsWithLocalJetStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ns := runDispatcherTestNATS(t)
	q, err := queue.NewNATSQueue(ns.ClientURL(), queue.WithConsumerPrefix("dispatcher-test"))
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	defer q.Close()

	store := newMemoryObjectStore()
	modal := newFakeShardModal(t)
	srv := NewWithStore(nil, store, modal, nil)

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
		ShardIndex:        0,
		TotalShards:       1,
	}
	if err := q.Enqueue(ctx, queue.StageChunk, initial); err != nil {
		t.Fatalf("enqueue chunk: %v", err)
	}

	chunkDispatcher := NewSyncDispatcher(srv, q, queue.StageChunk, 1)
	chunkMsg := pullOne(t, ctx, q, queue.StageChunk)
	if err := chunkDispatcher.Process(ctx, chunkMsg.Job); err != nil {
		t.Fatalf("process chunk: %v", err)
	}
	if err := q.Ack(chunkMsg); err != nil {
		t.Fatalf("ack chunk: %v", err)
	}

	embedMsg := pullOne(t, ctx, q, queue.StageEmbed)
	if embedMsg.Job.PayloadRef != "syncs/gen-1/chunks/chunk-job.jsonl" {
		t.Fatalf("embed payload ref = %q", embedMsg.Job.PayloadRef)
	}
	embedDispatcher := NewSyncDispatcher(srv, q, queue.StageEmbed, 1)
	if err := embedDispatcher.Process(ctx, embedMsg.Job); err != nil {
		t.Fatalf("process embed: %v", err)
	}
	_ = q.Ack(embedMsg)

	indexMsg := pullOne(t, ctx, q, queue.StageIndex)
	if indexMsg.Job.PayloadRef != "syncs/gen-1/index_rows/chunk-job-embed.jsonl" {
		t.Fatalf("index payload ref = %q", indexMsg.Job.PayloadRef)
	}
	indexDispatcher := NewSyncDispatcher(srv, q, queue.StageIndex, 1)
	if err := indexDispatcher.Process(ctx, indexMsg.Job); err != nil {
		t.Fatalf("process index: %v", err)
	}
	_ = q.Ack(indexMsg)

	if _, err := store.Download(ctx, syncShardDoneKey("gen-1", 0)); err != nil {
		t.Fatalf("done marker not written: %v", err)
	}
	commitMsg := pullOne(t, ctx, q, queue.StageCommit)
	if commitMsg.Job.Stage != queue.StageCommit || commitMsg.Job.TotalShards != 1 {
		t.Fatalf("unexpected commit job: %#v", commitMsg.Job)
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

func newFakeShardModal(t *testing.T) *ModalClient {
	t.Helper()
	mux := http.NewServeMux()
	var serverURL string
	mux.HandleFunc("/chunk", func(w http.ResponseWriter, r *http.Request) {
		job := decodeModalJob(t, r)
		writeJSONResponse(t, w, map[string]any{"result_ref": fmt.Sprintf("syncs/%s/chunks/%s.jsonl", job["generation_id"], job["job_id"]), "count": 1})
	})
	mux.HandleFunc("/embed", func(w http.ResponseWriter, r *http.Request) {
		job := decodeModalJob(t, r)
		writeJSONResponse(t, w, map[string]any{"result_ref": fmt.Sprintf("syncs/%s/index_rows/%s.jsonl", job["generation_id"], job["job_id"]), "count": 1})
	})
	mux.HandleFunc("/index", func(w http.ResponseWriter, r *http.Request) {
		_ = decodeModalJob(t, r)
		writeJSONResponse(t, w, map[string]any{"count": 1})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	serverURL = srv.URL
	return &ModalClient{
		chunkShardURL: serverURL + "/chunk",
		embedShardURL: serverURL + "/embed",
		indexShardURL: serverURL + "/index",
		httpClient:    srv.Client(),
	}
}

func decodeModalJob(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var payload struct {
		Job map[string]any `json:"job"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding modal request: %v", err)
	}
	if payload.Job["job_id"] == "" || payload.Job["stage"] == "" {
		t.Fatalf("modal request missing job fields: %#v", payload.Job)
	}
	return payload.Job
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

func (s *memoryObjectStore) UploadCAS(ctx context.Context, key string, data []byte, contentType, _, _ string) (string, error) {
	if err := s.Upload(ctx, key, data, contentType); err != nil {
		return "", err
	}
	return "etag", nil
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

func (s *memoryObjectStore) DownloadWithETag(ctx context.Context, key string) ([]byte, string, error) {
	data, err := s.Download(ctx, key)
	if err != nil {
		return nil, "", err
	}
	return data, "etag", nil
}

func (s *memoryObjectStore) DownloadRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	data, err := s.Download(ctx, key)
	if err != nil {
		return nil, err
	}
	if length <= 0 {
		return data, nil
	}
	end := offset + length
	if offset < 0 || end > int64(len(data)) {
		return nil, fmt.Errorf("range %d-%d outside object %s length %d", offset, end, key, len(data))
	}
	return append([]byte(nil), data[offset:end]...), nil
}
