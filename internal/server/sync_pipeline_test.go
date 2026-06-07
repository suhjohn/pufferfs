package server

import (
	"context"
	"testing"
	"time"

	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestShardChangesSkipsUnchangedAndBoundsFiles(t *testing.T) {
	changes := []models.FileChange{
		{Path: "a.txt", Status: models.StatusAdded, Size: 10},
		{Path: "b.txt", Status: models.StatusUnchanged, Size: 10},
		{Path: "c.txt", Status: models.StatusModified, Size: 10},
		{Path: "d.txt", Status: models.StatusRemoved, Size: 10},
	}
	shards := shardChanges(changes, 2, 1<<20)
	if len(shards) != 2 {
		t.Fatalf("got %d shards, want 2", len(shards))
	}
	if len(shards[0]) != 2 || shards[0][0].Path != "a.txt" || shards[0][1].Path != "c.txt" {
		t.Fatalf("first shard = %#v", shards[0])
	}
	if len(shards[1]) != 1 || shards[1][0].Path != "d.txt" {
		t.Fatalf("second shard = %#v", shards[1])
	}
}

func TestShardChangesBoundsBytes(t *testing.T) {
	changes := []models.FileChange{
		{Path: "a.txt", Status: models.StatusAdded, Size: 80},
		{Path: "b.txt", Status: models.StatusAdded, SourceLength: 80},
		{Path: "c.txt", Status: models.StatusAdded, Size: 10},
	}
	shards := shardChanges(changes, 5000, 100)
	if len(shards) != 2 {
		t.Fatalf("got %d shards, want 2", len(shards))
	}
	if len(shards[0]) != 1 || len(shards[1]) != 2 {
		t.Fatalf("unexpected shard sizes: %d, %d", len(shards[0]), len(shards[1]))
	}
}

func TestIndexedChunkCarriesAbsolutePath(t *testing.T) {
	row := indexedChunkFromModal("root-1", "gen-1", 1, "hash", map[string]any{
		"file_path":     "src/main.go",
		"absolute_path": "/Users/john/project/src/main.go",
		"chunk_index":   0,
		"content":       "package main",
		"content_hash":  "content-hash",
		"file_type":     "go",
	}).mapRow()
	if row["absolute_path"] != "/Users/john/project/src/main.go" {
		t.Fatalf("absolute_path = %#v", row["absolute_path"])
	}
}

func TestCleanupGenerationKeysIncludesTransientArtifactsOnly(t *testing.T) {
	req := &models.SyncRequest{
		GenerationID:    "gen-1",
		ManifestRef:     "bundles/root-1/manifest.json",
		ContentProofRef: "syncs/gen-1/proofs/content-proof.json",
		ChangeRefs:      []string{"syncs/gen-1/manifests/000000.jsonl", "syncs/gen-1/manifests/000001.jsonl"},
		Changes: []models.FileChange{
			{Path: "a.txt", Status: models.StatusAdded, SourceKey: "files/root-1/a.txt"},
			{Path: "b.txt", Status: models.StatusModified, SourceKey: "bundles/root-1/bundle-1.bin"},
			{Path: "c.txt", Status: models.StatusRemoved},
			{Path: "doc.pdf", Status: models.StatusAdded, SourceKey: "chunks/root-1/doc.pdf.0.jpg"},
		},
	}
	keys := cleanupGenerationKeys(req, queue.JobMessage{
		RootID:       "root-1",
		GenerationID: "gen-1",
		TotalShards:  2,
	})
	want := map[string]bool{
		"syncs/gen-1/request.json":              true,
		"syncs/gen-1/done/shard-000000.done":    true,
		"syncs/gen-1/done/shard-000001.done":    true,
		"bundles/root-1/manifest.json":          true,
		"syncs/gen-1/proofs/content-proof.json": true,
		"syncs/gen-1/manifests/000000.jsonl":    true,
		"syncs/gen-1/manifests/000001.jsonl":    true,
		"files/root-1/a.txt":                    true,
		"bundles/root-1/bundle-1.bin":           true,
	}
	if len(keys) != len(want) {
		t.Fatalf("cleanup keys = %#v, want %d keys", keys, len(want))
	}
	for _, key := range keys {
		if !want[key] {
			t.Fatalf("unexpected cleanup key %s in %#v", key, keys)
		}
	}
}

func TestEnqueueCleanupBatchesHonorsFlag(t *testing.T) {
	ctx := context.Background()
	q := &recordingQueue{}
	base := queue.JobMessage{OrgID: "org-1", RootID: "root-1", GenerationID: "gen-1"}

	t.Setenv("PUFFERFS_CLEANUP_SYNC_ARTIFACTS", "")
	if err := enqueueCleanupBatches(ctx, q, base, []string{"syncs/gen-1/chunks/job.jsonl"}); err != nil {
		t.Fatalf("default enqueue: %v", err)
	}
	if len(q.jobs) != 1 || q.stage != queue.StageCleanup {
		t.Fatalf("default cleanup stage=%q jobs=%#v", q.stage, q.jobs)
	}

	q = &recordingQueue{}
	t.Setenv("PUFFERFS_CLEANUP_SYNC_ARTIFACTS", "false")
	if err := enqueueCleanupBatches(ctx, q, base, []string{"syncs/gen-1/chunks/job.jsonl"}); err != nil {
		t.Fatalf("disabled enqueue: %v", err)
	}
	if len(q.jobs) != 0 {
		t.Fatalf("disabled cleanup enqueued jobs: %#v", q.jobs)
	}
}

func TestChunkShardBackpressureMessages(t *testing.T) {
	t.Setenv("PUFFERFS_SYNC_MAX_IN_FLIGHT_SHARDS", "2")
	msgs := []queue.JobMessage{
		{GenerationID: "gen-1", ShardIndex: 0, TotalShards: 5},
		{GenerationID: "gen-1", ShardIndex: 1, TotalShards: 5},
		{GenerationID: "gen-1", ShardIndex: 2, TotalShards: 5},
		{GenerationID: "gen-1", ShardIndex: 3, TotalShards: 5},
		{GenerationID: "gen-1", ShardIndex: 4, TotalShards: 5},
	}
	if initial := initialChunkShardMessages(msgs); len(initial) != 2 {
		t.Fatalf("initial messages = %d, want 2", len(initial))
	}
	next, ok := nextChunkShardMessage(queue.JobMessage{
		OrgID:             "org-1",
		RootID:            "root-1",
		GenerationID:      "gen-1",
		GenerationSeq:     7,
		BaseGenerationID:  "gen-0",
		BaseGenerationSeq: 6,
		ShardIndex:        1,
		TotalShards:       5,
		FilesInShard:      99,
		CleanupKeys:       []string{"syncs/gen-1/index_rows/job.jsonl"},
	})
	if !ok {
		t.Fatal("expected next shard")
	}
	if next.ShardIndex != 3 || next.Stage != syncStageChunk || next.PayloadRef != "syncs/gen-1/manifests/000003.jsonl" {
		t.Fatalf("next shard = %#v", next)
	}
	if len(next.CleanupKeys) != 0 {
		t.Fatalf("next cleanup keys = %#v, want none", next.CleanupKeys)
	}
	if next.FilesInShard != 0 {
		t.Fatalf("next files_in_shard = %d, want recalculation marker 0", next.FilesInShard)
	}
	if _, ok := nextChunkShardMessage(queue.JobMessage{GenerationID: "gen-1", ShardIndex: 3, TotalShards: 5}); ok {
		t.Fatal("unexpected next shard past total")
	}
}

func TestPathShardIndexStableWithModalContract(t *testing.T) {
	cases := []struct {
		path       string
		shardCount int
		want       int
	}{
		{path: "docs/readme.md", shardCount: 2, want: 1},
		{path: "queued/architecture.md", shardCount: 2, want: 0},
		{path: "ops/runbooks/deploy/rollback.txt", shardCount: 4, want: 2},
	}
	for _, tc := range cases {
		if got := pathShardIndex(tc.path, tc.shardCount); got != tc.want {
			t.Fatalf("pathShardIndex(%q, %d) = %d, want %d", tc.path, tc.shardCount, got, tc.want)
		}
	}
}

func TestRootIndexNamespaceForPathSelectsShard(t *testing.T) {
	namespaces := []models.RootIndexNamespace{
		{Namespace: "ns-0", ShardIndex: 0, ShardCount: 2},
		{Namespace: "ns-1", ShardIndex: 1, ShardCount: 2},
	}
	ns, err := rootIndexNamespaceForPath(namespaces, "docs/readme.md")
	if err != nil {
		t.Fatalf("select namespace: %v", err)
	}
	if ns.Namespace != "ns-1" {
		t.Fatalf("namespace = %q, want ns-1", ns.Namespace)
	}
}

func TestEnsureSyncStateRefUploadsAndClearsInlineState(t *testing.T) {
	ctx := context.Background()
	store := newMemoryObjectStore()
	srv := NewWithStore(nil, store, &ModalClient{}, nil)
	req := &models.SyncRequest{
		RootID: "root-1",
		State: map[string]models.FileState{
			"docs/a.txt": {Size: 7, ContentHash: "hash-a", Mtime: 123},
		},
	}
	if err := srv.ensureSyncStateRef(ctx, "root-1", "gen-1", req); err != nil {
		t.Fatalf("ensure state ref: %v", err)
	}
	if req.State != nil {
		t.Fatalf("inline state was not cleared")
	}
	if req.StateRef != "states/root-1/gen-1.json.gz" {
		t.Fatalf("state_ref = %q", req.StateRef)
	}
	data, err := store.Download(ctx, req.StateRef)
	if err != nil {
		t.Fatalf("download state ref: %v", err)
	}
	state, err := decodeRootState(req.StateRef, data)
	if err != nil {
		t.Fatalf("parse state object: %v", err)
	}
	if state["docs/a.txt"].ContentHash != "hash-a" {
		t.Fatalf("state object = %#v", state)
	}
}

func TestObjectQueuePushDedupesJobIDs(t *testing.T) {
	ctx := context.Background()
	store := newMemoryObjectStore()
	broker := newObjectQueueBroker(store)
	job := newObjectQueueJob("sync-1", "gen-1", 1, syncStageChunk, "syncs/gen-1/inputs/shard-000000.jsonl", 3, 9, 17)
	job.JobID = "job-1"
	if err := broker.Push(ctx, "gen-1", syncStageChunk, job, job); err != nil {
		t.Fatalf("push duplicate jobs: %v", err)
	}
	summary, err := broker.Summary(ctx, "gen-1", syncStageChunk)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Queued != 1 {
		t.Fatalf("queued = %d, want 1", summary.Queued)
	}
}

func TestObjectQueueJobCarriesShardProgressMetadata(t *testing.T) {
	job := newObjectQueueJob("sync-1", "gen-1", 1, syncStageIndex, "syncs/gen-1/index_rows/job.jsonl", 4, 12, 23)
	if job.ShardIndex != 4 || job.TotalShards != 12 || job.FilesInShard != 23 {
		t.Fatalf("shard metadata = %d/%d files=%d, want 4/12 files=23", job.ShardIndex, job.TotalShards, job.FilesInShard)
	}
}

func TestCountIndexArtifactFilesDedupesByPath(t *testing.T) {
	records := []syncIndexArtifact{
		{Op: "upsert", Row: map[string]any{"file_path": "a.txt"}},
		{Op: "upsert", Row: map[string]any{"file_path": "a.txt"}},
		{Op: "upsert", Row: map[string]any{"file_path": "b.txt"}},
		{Op: "close", ClosePath: "b.txt"},
		{Op: "close", ClosePath: "c.txt"},
		{Op: "noop"},
	}
	if got := countIndexArtifactFiles(records); got != 3 {
		t.Fatalf("countIndexArtifactFiles = %d, want 3", got)
	}
}

func TestProgressFileCountFallsBackToInputShard(t *testing.T) {
	ctx := context.Background()
	store := newMemoryObjectStore()
	srv := NewWithStore(nil, store, &ModalClient{}, nil)
	p := &syncPipeline{
		server:     srv,
		generation: &SyncGeneration{ID: "gen-1"},
	}
	input := []byte("{\"path\":\"empty.txt\",\"status\":\"added\"}\n{\"path\":\"skip.bin\",\"status\":\"added\"}\n")
	if err := store.Upload(ctx, syncInputShardKey("gen-1", 0), input, "application/x-ndjson"); err != nil {
		t.Fatalf("upload input shard: %v", err)
	}
	got, err := p.progressFileCount(ctx, objectQueueJob{GenerationID: "gen-1", ShardIndex: 0}, 0)
	if err != nil {
		t.Fatalf("progressFileCount: %v", err)
	}
	if got != 2 {
		t.Fatalf("progressFileCount = %d, want 2", got)
	}
}

type recordingQueue struct {
	stage string
	jobs  []queue.JobMessage
}

func (q *recordingQueue) Enqueue(_ context.Context, stage string, msgs ...queue.JobMessage) error {
	q.stage = stage
	q.jobs = append(q.jobs, msgs...)
	return nil
}

func (q *recordingQueue) Pull(context.Context, string, int, time.Duration) ([]queue.ReceivedMessage, error) {
	return nil, nil
}

func (q *recordingQueue) Ack(queue.ReceivedMessage) error {
	return nil
}

func (q *recordingQueue) Nak(queue.ReceivedMessage) error {
	return nil
}

func (q *recordingQueue) NakWithDelay(queue.ReceivedMessage, time.Duration) error {
	return nil
}

func (q *recordingQueue) InProgress(queue.ReceivedMessage) error {
	return nil
}

func (q *recordingQueue) Close() {}
