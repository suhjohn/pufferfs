package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/pkg/models"
)

var (
	errSyncCommitNotReady    = errors.New("sync commit not ready")
	errSyncGenerationStopped = errors.New("sync generation is terminal")
)

type SyncDispatcher struct {
	server      *Server
	queue       queue.Queue
	stage       string
	concurrency int
}

func NewSyncDispatcher(s *Server, q queue.Queue, stage string, concurrency int) *SyncDispatcher {
	concurrency = min(max(concurrency, 1), 64)
	return &SyncDispatcher{server: s, queue: q, stage: stage, concurrency: concurrency}
}

func (d *SyncDispatcher) Run(ctx context.Context) error {
	slots := make(chan struct{}, d.concurrency)
	for range d.concurrency {
		slots <- struct{}{}
	}
	var wg sync.WaitGroup

run:
	for {
		select {
		case <-ctx.Done():
			break run
		case <-slots:
		}
		available := 1
	drain:
		for available < d.concurrency {
			select {
			case <-slots:
				available++
			default:
				break drain
			}
		}
		msgs, err := d.queue.Pull(ctx, d.stage, available, 30*time.Second)
		for range available - len(msgs) {
			slots <- struct{}{}
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("pulling %s jobs: %v", d.stage, err)
			time.Sleep(time.Second)
			continue
		}
		for _, msg := range msgs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { slots <- struct{}{} }()
				d.processReceived(ctx, msg)
			}()
		}
	}
	wg.Wait()
	return ctx.Err()
}

func (d *SyncDispatcher) processReceived(ctx context.Context, msg queue.ReceivedMessage) {
	skip, err := d.shouldSkipMessage(ctx, msg.Job)
	if err != nil {
		log.Printf("checking sync job stage=%s job_id=%s generation_id=%s: %v", d.stage, msg.Job.JobID, msg.Job.GenerationID, err)
		_ = d.queue.NakWithDelay(msg, time.Second)
		return
	}
	if skip {
		if cleanupErr := d.cleanupLateGeneration(ctx, msg.Job); cleanupErr != nil {
			log.Printf("cleaning skipped sync job stage=%s job_id=%s generation_id=%s: %v", d.stage, msg.Job.JobID, msg.Job.GenerationID, cleanupErr)
			_ = d.queue.NakWithDelay(msg, time.Second)
			return
		}
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
	if errors.Is(err, errSyncGenerationStopped) {
		if cleanupErr := d.cleanupLateGeneration(ctx, msg.Job); cleanupErr != nil {
			log.Printf("cleaning late sync job stage=%s job_id=%s generation_id=%s: %v", d.stage, msg.Job.JobID, msg.Job.GenerationID, cleanupErr)
			_ = d.queue.NakWithDelay(msg, time.Second)
			return
		}
		if ackErr := d.queue.Ack(msg); ackErr != nil {
			log.Printf("acking stopped %s job %s: %v", d.stage, msg.Job.JobID, ackErr)
		}
		return
	}
	if errors.Is(err, errSyncCommitNotReady) {
		_ = d.queue.NakWithDelay(msg, 5*time.Second)
		return
	}
	if skip, statusErr := d.shouldSkipMessage(ctx, msg.Job); statusErr != nil {
		log.Printf("checking failed sync job stage=%s job_id=%s generation_id=%s: %v", d.stage, msg.Job.JobID, msg.Job.GenerationID, statusErr)
		_ = d.queue.NakWithDelay(msg, time.Second)
		return
	} else if skip {
		if cleanupErr := d.cleanupLateGeneration(ctx, msg.Job); cleanupErr != nil {
			log.Printf("cleaning partial sync job stage=%s job_id=%s generation_id=%s: %v", d.stage, msg.Job.JobID, msg.Job.GenerationID, cleanupErr)
			_ = d.queue.NakWithDelay(msg, time.Second)
			return
		}
		if ackErr := d.queue.Ack(msg); ackErr != nil {
			log.Printf("acking stopped %s job %s: %v", d.stage, msg.Job.JobID, ackErr)
		}
		return
	}
	if maxAttempts := syncStageMaxAttempts(d.stage); maxAttempts > 0 && msg.Attempts >= maxAttempts {
		log.Printf("failing sync job stage=%s job_id=%s generation_id=%s after attempts=%d: %v", d.stage, msg.Job.JobID, msg.Job.GenerationID, msg.Attempts, err)
		d.markMessageFailed(ctx, msg.Job, err)
		if ackErr := d.queue.Ack(msg); ackErr != nil {
			log.Printf("acking failed %s job %s: %v", d.stage, msg.Job.JobID, ackErr)
		}
		return
	}
	delay := time.Duration(msg.Attempts*msg.Attempts) * 10 * time.Second
	if delay < time.Second {
		delay = time.Second
	}
	log.Printf("processing %s job %s after %s: %v", d.stage, msg.Job.JobID, elapsed, err)
	_ = d.queue.NakWithDelay(msg, delay)
}

func syncStageMaxAttempts(stage string) int {
	if stage == syncStageCommit {
		return 30
	}
	return 3
}

func (d *SyncDispatcher) markMessageFailed(ctx context.Context, msg queue.JobMessage, cause error) {
	if d == nil || d.server == nil || d.server.db == nil {
		return
	}
	req := d.server.syncRequestForCleanup(ctx, msg.GenerationID)
	if msg.GenerationID != "" {
		_ = d.server.db.MarkSyncGenerationFailed(ctx, msg.GenerationID)
	}
	if msg.SyncJobID != "" {
		_ = d.server.db.CompleteSyncJob(ctx, msg.SyncJobID, "failed", []map[string]string{{"error": cause.Error()}})
	}
	if cleanupErr := d.server.cleanupFailedGeneration(ctx, msg.OrgID, msg.RootID, msg.GenerationID, req); cleanupErr != nil {
		log.Printf("warning: failed generation cleanup for root %s generation %s: %v", msg.RootID, msg.GenerationID, cleanupErr)
	}
	root, _ := d.server.db.GetRoot(ctx, msg.OrgID, msg.RootID)
	d.server.captureSyncFailed(ctx, msg.OrgID, msg.UserID, root, req, &models.SyncJob{ID: msg.SyncJobID}, d.stage+"_worker")
}

func (d *SyncDispatcher) cleanupLateGeneration(ctx context.Context, msg queue.JobMessage) error {
	if d == nil || d.server == nil || d.server.db == nil || msg.GenerationID == "" {
		return nil
	}
	status, err := d.server.db.GetSyncGenerationStatus(ctx, msg.GenerationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return d.server.cleanupTerminalSyncObjects(ctx, msg.RootID, msg.GenerationID, d.server.syncRequestForCleanup(ctx, msg.GenerationID), true)
	}
	if err != nil {
		return err
	}
	if status == "visible" {
		return d.server.cleanupTerminalSyncObjects(ctx, msg.RootID, msg.GenerationID, d.server.syncRequestForCleanup(ctx, msg.GenerationID), false)
	}
	if status != "failed" && status != "cleaning" && status != "superseded" {
		return nil
	}
	if err := d.server.db.MarkSyncGenerationCleanupPending(ctx, msg.GenerationID); err != nil {
		return err
	}
	return d.server.cleanupFailedGeneration(ctx, msg.OrgID, msg.RootID, msg.GenerationID, nil)
}

func (d *SyncDispatcher) startHeartbeat(ctx context.Context, msg queue.ReceivedMessage) func() {
	done := make(chan struct{})
	ticker := time.NewTicker(syncJobHeartbeatInterval())
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = d.queue.InProgress(msg)
				if d.server != nil && d.server.db != nil {
					_ = d.server.db.TouchSyncJob(ctx, msg.Job.SyncJobID)
				}
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
		if msg.SyncJobID != "" && msg.ShardIndex == 0 {
			_ = d.server.db.UpdateSyncJobStatus(ctx, msg.SyncJobID, "chunking")
		}
		filesInShard := msg.FilesInShard
		resultRef, err := p.processChunkJob(ctx, msg, &syncSourceCache{s3: d.server.s3})
		if err != nil {
			return err
		}
		if _, err := d.recordStageProgress(ctx, msg, syncStageChunk, filesInShard); err != nil {
			return err
		}
		next := p.jobMessage(syncStageIndex, msg.JobID+"-index", resultRef, msg.ShardIndex, msg.TotalShards, filesInShard)
		return d.queue.Enqueue(ctx, syncStageIndex, next)
	case syncStageIndex:
		if msg.SyncJobID != "" && msg.ShardIndex == 0 {
			_ = d.server.db.UpdateSyncJobStatus(ctx, msg.SyncJobID, "indexing")
		}
		if !msg.DisableVector && d.server.modal.HasIndexShardEndpoint() {
			if err := d.server.modal.IndexShard(msg); err != nil {
				return err
			}
		} else {
			if err := p.processIndexJob(ctx, msg); err != nil {
				return err
			}
		}
		completed, err := d.recordStageProgress(ctx, msg, syncStageIndex, msg.FilesInShard)
		if err != nil {
			return err
		}
		if completed < msg.TotalShards {
			return nil
		}
		commit := p.jobMessage(syncStageCommit, msg.GenerationID+"-commit", syncRequestKey(msg.GenerationID), 0, msg.TotalShards, 0)
		return d.queue.Enqueue(ctx, syncStageCommit, commit)
	case syncStageCommit:
		return d.processCommit(ctx, msg)
	default:
		return fmt.Errorf("unknown sync stage %q", msg.Stage)
	}
}

func (d *SyncDispatcher) recordStageProgress(ctx context.Context, msg queue.JobMessage, stage string, files int) (int, error) {
	if msg.SyncJobID == "" || d.server == nil || d.server.db == nil {
		return msg.TotalShards, nil
	}
	completed, status, err := d.server.db.RecordSyncJobShard(ctx, msg.SyncJobID, stage, msg.ShardIndex, files)
	if err == nil && (status == "failed" || status == "cleaning" || status == "superseded" || status == "visible") {
		err = errSyncGenerationStopped
	}
	return completed, err
}

func (d *SyncDispatcher) pipelineFor(msg queue.JobMessage) *syncPipeline {
	generation := &SyncGeneration{
		ID:                msg.GenerationID,
		BaseGenerationID:  msg.BaseGenerationID,
		Seq:               msg.GenerationSeq,
		BaseGenerationSeq: msg.BaseGenerationSeq,
	}
	return &syncPipeline{
		server:                d.server,
		orgID:                 msg.OrgID,
		rootID:                msg.RootID,
		generation:            generation,
		jobID:                 msg.SyncJobID,
		userID:                msg.UserID,
		req:                   &models.SyncRequest{RootID: msg.RootID, DisableVector: msg.DisableVector},
		indexNamespaces:       modelIndexNamespaces(msg.IndexNamespaces, msg.OrgID, msg.RootID),
		indexNamespacesLoaded: len(msg.IndexNamespaces) > 0,
		resp: &models.SyncResponse{
			RootID:        msg.RootID,
			SyncJobID:     msg.SyncJobID,
			GenerationID:  msg.GenerationID,
			GenerationSeq: msg.GenerationSeq,
		},
	}
}

func (d *SyncDispatcher) shouldSkipMessage(ctx context.Context, msg queue.JobMessage) (bool, error) {
	if d == nil || d.server == nil || d.server.db == nil || msg.GenerationID == "" {
		return false, nil
	}
	status, err := d.server.db.GetSyncGenerationStatus(ctx, msg.GenerationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return false, err
	}
	if status != "failed" && status != "cleaning" && status != "superseded" && (status != "visible" || msg.Stage == syncStageCommit) {
		return false, nil
	}
	log.Printf("skipping sync job stage=%s job_id=%s generation_id=%s status=%s", msg.Stage, msg.JobID, msg.GenerationID, status)
	return true, nil
}

func (d *SyncDispatcher) processCommit(ctx context.Context, msg queue.JobMessage) error {
	var req *models.SyncRequest
	root, _ := d.server.db.GetRoot(ctx, msg.OrgID, msg.RootID)
	job := &models.SyncJob{ID: msg.SyncJobID}

	status, err := d.server.db.GetSyncGenerationStatus(ctx, msg.GenerationID)
	if err == nil && status == "visible" {
		req, readErr := d.readSyncRequest(ctx, msg.GenerationID)
		if readErr != nil {
			req = nil
		}
		if msg.SyncJobID != "" {
			if completeErr := d.server.db.CompleteSyncJob(ctx, msg.SyncJobID, "completed", nil); completeErr != nil {
				return completeErr
			}
		}
		return d.server.cleanupTerminalSyncObjects(ctx, msg.RootID, msg.GenerationID, req, false)
	}
	if err != nil {
		return err
	}
	if err := d.server.cleanupFailedGenerations(ctx, msg.OrgID, msg.RootID); err != nil {
		return fmt.Errorf("cleaning failed generations: %w", err)
	}
	if msg.TotalShards > 0 && msg.SyncJobID != "" {
		completed, err := d.server.db.CountCompletedSyncJobShards(ctx, msg.SyncJobID, syncStageIndex)
		if err != nil {
			return err
		}
		if completed < msg.TotalShards {
			return errSyncCommitNotReady
		}
	}
	req, err = d.readSyncRequest(ctx, msg.GenerationID)
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
	if err := d.server.storeSyncContentProof(ctx, msg.OrgID, msg.UserID, msg.RootID, req); err != nil {
		return fmt.Errorf("storing content proof: %w", err)
	}
	if msg.SyncJobID != "" {
		_ = d.server.db.UpdateSyncJobStatus(ctx, msg.SyncJobID, "committing")
	}
	if err := d.server.db.CommitSyncGeneration(ctx, generation, req.State, req.StateRef); err != nil {
		return fmt.Errorf("committing generation: %w", err)
	}
	if msg.SyncJobID != "" {
		if err := d.server.db.CompleteSyncJob(ctx, msg.SyncJobID, "completed", nil); err != nil {
			return err
		}
	}
	d.server.captureSyncCompleted(ctx, msg.OrgID, msg.UserID, root, req, job, nil)
	return d.server.cleanupTerminalSyncObjects(ctx, msg.RootID, msg.GenerationID, req, false)
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
