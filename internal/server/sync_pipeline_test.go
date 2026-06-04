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
		t.Fatalf("disabled enqueue: %v", err)
	}
	if len(q.jobs) != 0 {
		t.Fatalf("disabled cleanup enqueued jobs: %#v", q.jobs)
	}

	t.Setenv("PUFFERFS_CLEANUP_SYNC_ARTIFACTS", "true")
	if err := enqueueCleanupBatches(ctx, q, base, []string{"syncs/gen-1/chunks/job.jsonl"}); err != nil {
		t.Fatalf("enabled enqueue: %v", err)
	}
	if len(q.jobs) != 1 || q.stage != queue.StageCleanup {
		t.Fatalf("enabled cleanup stage=%q jobs=%#v", q.stage, q.jobs)
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
