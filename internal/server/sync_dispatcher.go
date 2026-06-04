package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/pkg/models"
)

var errSyncCommitNotReady = errors.New("sync commit not ready")

type SyncDispatcher struct {
	server      *Server
	queue       queue.Queue
	stage       string
	concurrency int
}

func NewSyncDispatcher(s *Server, q queue.Queue, stage string, concurrency int) *SyncDispatcher {
	if concurrency < 1 {
		concurrency = 1
	}
	return &SyncDispatcher{server: s, queue: q, stage: stage, concurrency: concurrency}
}

func (d *SyncDispatcher) Run(ctx context.Context) error {
	sem := make(chan struct{}, d.concurrency)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := d.queue.Pull(ctx, d.stage, d.concurrency, 30*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("pulling %s jobs: %v", d.stage, err)
			time.Sleep(time.Second)
			continue
		}
		for _, msg := range msgs {
			sem <- struct{}{}
			go func(msg queue.ReceivedMessage) {
				defer func() { <-sem }()
				d.processReceived(ctx, msg)
			}(msg)
		}
	}
}

func (d *SyncDispatcher) processReceived(ctx context.Context, msg queue.ReceivedMessage) {
	skip, err := d.shouldSkipMessage(ctx, msg.Job)
	if err != nil {
		log.Printf("checking sync job stage=%s job_id=%s generation_id=%s: %v", d.stage, msg.Job.JobID, msg.Job.GenerationID, err)
	} else if skip {
		if ackErr := d.queue.Ack(msg); ackErr != nil {
			log.Printf("acking skipped %s job %s: %v", d.stage, msg.Job.JobID, ackErr)
		}
		return
	}

	stopHeartbeat := d.startHeartbeat(ctx, msg)
	defer stopHeartbeat()

	start := time.Now()
	err = d.Process(ctx, msg.Job)
	elapsed := time.Since(start)
	if err == nil {
		log.Printf("processed sync job stage=%s job_id=%s generation_id=%s shard=%d/%d elapsed=%s", d.stage, msg.Job.JobID, msg.Job.GenerationID, msg.Job.ShardIndex+1, msg.Job.TotalShards, elapsed)
		if ackErr := d.queue.Ack(msg); ackErr != nil {
			log.Printf("acking %s job %s: %v", d.stage, msg.Job.JobID, ackErr)
		}
		return
	}
	if errors.Is(err, errSyncCommitNotReady) {
		_ = d.queue.NakWithDelay(msg, 5*time.Second)
		return
	}
	delay := time.Duration(msg.Attempts*msg.Attempts) * 10 * time.Second
	if delay < time.Second {
		delay = time.Second
	}
	log.Printf("processing %s job %s after %s: %v", d.stage, msg.Job.JobID, elapsed, err)
	_ = d.queue.NakWithDelay(msg, delay)
}

func (d *SyncDispatcher) startHeartbeat(ctx context.Context, msg queue.ReceivedMessage) func() {
	done := make(chan struct{})
	ticker := time.NewTicker(2 * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = d.queue.InProgress(msg)
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func (d *SyncDispatcher) Process(ctx context.Context, msg queue.JobMessage) error {
	p := d.pipelineFor(msg)
	switch msg.Stage {
	case syncStageChunk:
		if msg.SyncJobID != "" {
			_ = d.server.db.UpdateSyncJobStatus(ctx, msg.SyncJobID, "chunking", 0)
		}
		cleanupKeys := append([]string(nil), msg.CleanupKeys...)
		if msg.PayloadRef != "" {
			cleanupKeys = append(cleanupKeys, msg.PayloadRef)
		}
		if d.server.modal.HasChunkShardEndpoint() {
			resp, err := d.server.modal.ChunkShard(modalJob(msg))
			if err != nil {
				return err
			}
			next := p.jobMessage(syncStageEmbed, msg.JobID+"-embed", resp.ResultRef, msg.ShardIndex, msg.TotalShards)
			next.CleanupKeys = cleanupKeys
			return d.queue.Enqueue(ctx, syncStageEmbed, next)
		}
		sourceCache := newSyncSourceCache(d.server.s3)
		resultRef, err := p.processChunkJob(ctx, objectQueueJobFromMessage(msg), sourceCache)
		if err != nil {
			return err
		}
		next := p.jobMessage(syncStageEmbed, msg.JobID+"-embed", resultRef, msg.ShardIndex, msg.TotalShards)
		next.CleanupKeys = cleanupKeys
		return d.queue.Enqueue(ctx, syncStageEmbed, next)
	case syncStageEmbed:
		if msg.SyncJobID != "" {
			_ = d.server.db.UpdateSyncJobStatus(ctx, msg.SyncJobID, "embedding", 0)
		}
		cleanupKeys := append([]string(nil), msg.CleanupKeys...)
		if msg.PayloadRef != "" {
			cleanupKeys = append(cleanupKeys, msg.PayloadRef)
		}
		if d.server.modal.HasEmbedShardEndpoint() {
			resp, err := d.server.modal.EmbedShard(modalJob(msg))
			if err != nil {
				return err
			}
			next := p.jobMessage(syncStageIndex, msg.JobID+"-index", resp.ResultRef, msg.ShardIndex, msg.TotalShards)
			next.CleanupKeys = cleanupKeys
			return d.queue.Enqueue(ctx, syncStageIndex, next)
		}
		resultRef, err := p.processEmbedJob(ctx, objectQueueJobFromMessage(msg))
		if err != nil {
			return err
		}
		next := p.jobMessage(syncStageIndex, msg.JobID+"-index", resultRef, msg.ShardIndex, msg.TotalShards)
		next.CleanupKeys = cleanupKeys
		return d.queue.Enqueue(ctx, syncStageIndex, next)
	case syncStageIndex:
		if msg.SyncJobID != "" {
			_ = d.server.db.UpdateSyncJobStatus(ctx, msg.SyncJobID, "indexing", 0)
		}
		if d.server.modal.HasIndexShardEndpoint() {
			if _, err := d.server.modal.IndexShard(modalJob(msg)); err != nil {
				return err
			}
		} else {
			if err := p.processIndexJob(ctx, objectQueueJobFromMessage(msg)); err != nil {
				return err
			}
		}
		if err := d.writeShardDone(ctx, msg); err != nil {
			return err
		}
		if err := d.enqueueNextChunkShard(ctx, msg); err != nil {
			return err
		}
		if err := enqueueCleanupBatches(ctx, d.queue, msg, cleanupShardKeys(msg)); err != nil {
			return err
		}
		return d.queue.Enqueue(ctx, syncStageCommit, p.jobMessage(syncStageCommit, msg.JobID+"-commit", syncRequestKey(msg.GenerationID), msg.ShardIndex, msg.TotalShards))
	case syncStageCommit:
		return d.processCommit(ctx, msg)
	case syncStageCleanup:
		return d.processCleanup(ctx, msg)
	default:
		return fmt.Errorf("unknown sync stage %q", msg.Stage)
	}
}

func (d *SyncDispatcher) pipelineFor(msg queue.JobMessage) *syncPipeline {
	job := &models.SyncJob{ID: msg.SyncJobID, OrgID: msg.OrgID, RootID: msg.RootID, UserID: msg.UserID}
	if msg.SyncJobID == "" {
		job = nil
	}
	generation := &SyncGeneration{
		ID:                msg.GenerationID,
		OrgID:             msg.OrgID,
		RootID:            msg.RootID,
		BaseGenerationID:  msg.BaseGenerationID,
		Seq:               msg.GenerationSeq,
		BaseGenerationSeq: msg.BaseGenerationSeq,
	}
	return &syncPipeline{
		server:     d.server,
		orgID:      msg.OrgID,
		rootID:     msg.RootID,
		generation: generation,
		job:        job,
		userID:     msg.UserID,
		req:        &models.SyncRequest{RootID: msg.RootID},
		resp: &models.SyncResponse{
			RootID:        msg.RootID,
			SyncJobID:     msg.SyncJobID,
			GenerationID:  msg.GenerationID,
			GenerationSeq: msg.GenerationSeq,
		},
	}
}

func (d *SyncDispatcher) shouldSkipMessage(ctx context.Context, msg queue.JobMessage) (bool, error) {
	if d == nil || d.server == nil || d.server.db == nil || msg.GenerationID == "" || msg.Stage == syncStageCleanup {
		return false, nil
	}
	status, err := d.server.db.GetSyncGenerationStatus(ctx, msg.GenerationID)
	if err != nil {
		return false, err
	}
	if status != "failed" {
		return false, nil
	}
	if err := d.server.cleanupFailedGenerationRows(ctx, msg.OrgID, msg.RootID, msg.GenerationID); err != nil {
		log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", msg.RootID, msg.GenerationID, err)
	}
	log.Printf("skipping sync job stage=%s job_id=%s generation_id=%s status=failed", msg.Stage, msg.JobID, msg.GenerationID)
	return true, nil
}

func (d *SyncDispatcher) enqueueNextChunkShard(ctx context.Context, msg queue.JobMessage) error {
	next, ok := nextChunkShardMessage(msg)
	if !ok {
		return nil
	}
	return d.queue.Enqueue(ctx, syncStageChunk, next)
}

func objectQueueJobFromMessage(msg queue.JobMessage) objectQueueJob {
	return objectQueueJob{
		JobID:         msg.JobID,
		SyncID:        msg.SyncJobID,
		GenerationID:  msg.GenerationID,
		GenerationSeq: msg.GenerationSeq,
		Stage:         msg.Stage,
		PayloadRef:    msg.PayloadRef,
		Attempts:      1,
		CreatedAt:     msg.EnqueuedAt,
		UpdatedAt:     time.Now().UTC(),
	}
}

func modalJob(msg queue.JobMessage) map[string]any {
	return map[string]any{
		"job_id":              msg.JobID,
		"sync_job_id":         msg.SyncJobID,
		"user_id":             msg.UserID,
		"org_id":              msg.OrgID,
		"root_id":             msg.RootID,
		"generation_id":       msg.GenerationID,
		"generation_seq":      msg.GenerationSeq,
		"base_generation_id":  msg.BaseGenerationID,
		"base_generation_seq": msg.BaseGenerationSeq,
		"stage":               msg.Stage,
		"payload_ref":         msg.PayloadRef,
		"shard_index":         msg.ShardIndex,
		"total_shards":        msg.TotalShards,
		"priority":            msg.Priority,
		"enqueued_at":         msg.EnqueuedAt.Format(time.RFC3339Nano),
	}
}

func (d *SyncDispatcher) writeShardDone(ctx context.Context, msg queue.JobMessage) error {
	key := syncShardDoneKey(msg.GenerationID, msg.ShardIndex)
	return d.server.s3.Upload(ctx, key, []byte("done\n"), "text/plain")
}

func (d *SyncDispatcher) processCommit(ctx context.Context, msg queue.JobMessage) error {
	status, err := d.server.db.GetSyncGenerationStatus(ctx, msg.GenerationID)
	if err == nil && status == "visible" {
		req, readErr := d.readSyncRequest(ctx, msg.GenerationID)
		if readErr != nil {
			return nil
		}
		return enqueueCleanupBatches(ctx, d.queue, msg, cleanupGenerationKeys(req, msg))
	}
	if err != nil {
		return err
	}
	for i := 0; i < msg.TotalShards; i++ {
		if _, err := d.server.s3.Download(ctx, syncShardDoneKey(msg.GenerationID, i)); err != nil {
			return errSyncCommitNotReady
		}
	}
	req, err := d.readSyncRequest(ctx, msg.GenerationID)
	if err != nil {
		return err
	}
	generation := &SyncGeneration{
		ID:                msg.GenerationID,
		RootID:            msg.RootID,
		BaseGenerationID:  msg.BaseGenerationID,
		Seq:               msg.GenerationSeq,
		BaseGenerationSeq: msg.BaseGenerationSeq,
	}
	if req.ContentProof != nil {
		proofBytes, _ := json.Marshal(req.ContentProof)
		if err := d.server.db.UpsertContentProof(ctx, msg.OrgID, msg.UserID, msg.RootID, req.ContentProof.RootHash, proofBytes); err != nil {
			_ = d.server.db.MarkSyncGenerationFailed(ctx, msg.GenerationID)
			if cleanupErr := d.server.cleanupFailedGenerationRows(ctx, msg.OrgID, msg.RootID, msg.GenerationID); cleanupErr != nil {
				log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", msg.RootID, msg.GenerationID, cleanupErr)
			}
			if msg.SyncJobID != "" {
				_ = d.server.db.CompleteSyncJob(ctx, msg.SyncJobID, "failed", []map[string]string{{"error": err.Error()}})
			}
			return fmt.Errorf("storing content proof: %w", err)
		}
	}
	if err := d.server.db.CommitSyncGeneration(ctx, generation, req.State); err != nil {
		_ = d.server.db.MarkSyncGenerationFailed(ctx, msg.GenerationID)
		if cleanupErr := d.server.cleanupFailedGenerationRows(ctx, msg.OrgID, msg.RootID, msg.GenerationID); cleanupErr != nil {
			log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", msg.RootID, msg.GenerationID, cleanupErr)
		}
		if msg.SyncJobID != "" {
			_ = d.server.db.CompleteSyncJob(ctx, msg.SyncJobID, "failed", []map[string]string{{"error": err.Error()}})
		}
		return err
	}
	if msg.SyncJobID != "" {
		if err := d.server.db.CompleteSyncJob(ctx, msg.SyncJobID, "completed", nil); err != nil {
			return err
		}
	}
	return enqueueCleanupBatches(ctx, d.queue, msg, cleanupGenerationKeys(req, msg))
}

func (d *SyncDispatcher) readSyncRequest(ctx context.Context, generationID string) (*models.SyncRequest, error) {
	data, err := d.server.s3.Download(ctx, syncRequestKey(generationID))
	if err != nil {
		return nil, err
	}
	var req models.SyncRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func syncShardDoneKey(generationID string, shardIndex int) string {
	return fmt.Sprintf("syncs/%s/done/shard-%06d.done", generationID, shardIndex)
}
