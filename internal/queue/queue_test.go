package queue

import (
	"context"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

func TestNATSQueueEnqueuePullAck(t *testing.T) {
	ns := runEmbeddedNATS(t)
	q, err := NewNATSQueue(ns.ClientURL())
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	defer q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job := JobMessage{
		JobID:         "job-1",
		SyncJobID:     "sync-1",
		UserID:        "user-1",
		OrgID:         "org-1",
		RootID:        "root-1",
		GenerationID:  "gen-1",
		GenerationSeq: 7,
		Stage:         StageChunk,
		PayloadRef:    "syncs/gen-1/inputs/shard-000000.jsonl",
		ShardIndex:    0,
		TotalShards:   1,
	}
	if err := q.Enqueue(ctx, StageChunk, job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	msgs, err := q.Pull(ctx, StageChunk, 1, time.Second)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("pulled %d messages, want 1", len(msgs))
	}
	if got := msgs[0].Job; got.JobID != job.JobID || got.OrgID != job.OrgID || got.RootID != job.RootID || got.PayloadRef != job.PayloadRef {
		t.Fatalf("pulled wrong job: %#v", got)
	}
	if err := q.Ack(msgs[0]); err != nil {
		t.Fatalf("ack: %v", err)
	}
}

func TestNATSQueueRedeliversAfterNakWithDelay(t *testing.T) {
	ns := runEmbeddedNATS(t)
	q, err := NewNATSQueue(ns.ClientURL())
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	defer q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job := JobMessage{JobID: "job-redeliver", OrgID: "org-1", RootID: "root-1", GenerationID: "gen-1", Stage: StageCommit}
	if err := q.Enqueue(ctx, StageCommit, job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	msgs, err := q.Pull(ctx, StageCommit, 1, time.Second)
	if err != nil {
		t.Fatalf("initial pull: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("initial pull got %d messages, want 1", len(msgs))
	}
	if err := q.NakWithDelay(msgs[0], 10*time.Millisecond); err != nil {
		t.Fatalf("nak with delay: %v", err)
	}
	msgs, err = q.Pull(ctx, StageCommit, 1, time.Second)
	if err != nil {
		t.Fatalf("redelivery pull: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("redelivery got %d messages, want 1", len(msgs))
	}
	if msgs[0].Attempts < 2 {
		t.Fatalf("redelivery attempts = %d, want at least 2", msgs[0].Attempts)
	}
	_ = q.Ack(msgs[0])
}

func TestQueueReplicasFromEnv(t *testing.T) {
	t.Setenv("PUFFERFS_QUEUE_REPLICAS", "")
	if got := queueReplicas(); got != 0 {
		t.Fatalf("unset replicas = %d, want 0", got)
	}

	t.Setenv("PUFFERFS_QUEUE_REPLICAS", "3")
	if got := queueReplicas(); got != 3 {
		t.Fatalf("replicas = %d, want 3", got)
	}

	t.Setenv("PUFFERFS_QUEUE_REPLICAS", "10")
	if got := queueReplicas(); got != 5 {
		t.Fatalf("clamped replicas = %d, want 5", got)
	}

	t.Setenv("PUFFERFS_QUEUE_REPLICAS", "bad")
	if got := queueReplicas(); got != 0 {
		t.Fatalf("invalid replicas = %d, want 0", got)
	}
}

func TestNATSQueueCreatesStreamsWithConfiguredReplicas(t *testing.T) {
	ns := runEmbeddedNATS(t)
	q, err := NewNATSQueue(ns.ClientURL(), WithConsumerPrefix("replica-test"), WithReplicas(1))
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	defer q.Close()

	info, err := q.js.StreamInfo(streamName(StageChunk))
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Config.Replicas != 1 {
		t.Fatalf("stream replicas = %d, want 1", info.Config.Replicas)
	}
}

func runEmbeddedNATS(t *testing.T) *natsserver.Server {
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
