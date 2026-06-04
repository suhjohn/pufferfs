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
		ManifestRef: "bundles/root-1/manifest.json",
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
		"syncs/gen-1/request.json":           true,
		"syncs/gen-1/done/shard-000000.done": true,
		"syncs/gen-1/done/shard-000001.done": true,
		"bundles/root-1/manifest.json":       true,
		"files/root-1/a.txt":                 true,
		"bundles/root-1/bundle-1.bin":        true,
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
		CleanupKeys:       []string{"syncs/gen-1/index_rows/job.jsonl"},
	})
	if !ok {
		t.Fatal("expected next shard")
	}
	if next.ShardIndex != 3 || next.Stage != syncStageChunk || next.PayloadRef != "syncs/gen-1/inputs/shard-000003.jsonl" {
		t.Fatalf("next shard = %#v", next)
	}
	if len(next.CleanupKeys) != 0 {
		t.Fatalf("next cleanup keys = %#v, want none", next.CleanupKeys)
	}
	if _, ok := nextChunkShardMessage(queue.JobMessage{GenerationID: "gen-1", ShardIndex: 3, TotalShards: 5}); ok {
		t.Fatal("unexpected next shard past total")
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
