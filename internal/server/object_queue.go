package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type queueJobStatus string

const (
	queueJobQueued  queueJobStatus = "queued"
	queueJobRunning queueJobStatus = "running"
	queueJobDone    queueJobStatus = "done"
	queueJobFailed  queueJobStatus = "failed"
)

type objectQueueJob struct {
	JobID         string         `json:"job_id"`
	SyncID        string         `json:"sync_id"`
	GenerationID  string         `json:"generation_id"`
	GenerationSeq int64          `json:"generation_seq,omitempty"`
	Stage         string         `json:"stage"`
	ShardIndex    int            `json:"shard_index,omitempty"`
	TotalShards   int            `json:"total_shards,omitempty"`
	FilesInShard  int            `json:"files_in_shard,omitempty"`
	PayloadRef    string         `json:"payload_ref"`
	ResultRef     string         `json:"result_ref,omitempty"`
	Status        queueJobStatus `json:"status"`
	LeaseUntil    time.Time      `json:"lease_until,omitempty"`
	WorkerID      string         `json:"worker_id,omitempty"`
	Attempts      int            `json:"attempts"`
	Error         string         `json:"error,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type objectQueueState struct {
	QueueID      string           `json:"queue_id"`
	GenerationID string           `json:"generation_id"`
	Stage        string           `json:"stage"`
	Epoch        int64            `json:"epoch"`
	UpdatedAt    time.Time        `json:"updated_at"`
	Jobs         []objectQueueJob `json:"jobs"`
}

type objectQueueSummary struct {
	Queued  int
	Running int
	Done    int
	Failed  int
}

type objectQueueBroker struct {
	s3 objectStore
	mu sync.Mutex
}

func newObjectQueueBroker(s3 objectStore) *objectQueueBroker {
	return &objectQueueBroker{s3: s3}
}

func newObjectQueueJob(syncID, generationID string, generationSeq int64, stage, payloadRef string, shardIndex, totalShards, filesInShard int) objectQueueJob {
	now := time.Now().UTC()
	return objectQueueJob{
		JobID:         uuid.New().String(),
		SyncID:        syncID,
		GenerationID:  generationID,
		GenerationSeq: generationSeq,
		Stage:         stage,
		ShardIndex:    shardIndex,
		TotalShards:   totalShards,
		FilesInShard:  filesInShard,
		PayloadRef:    payloadRef,
		Status:        queueJobQueued,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func (b *objectQueueBroker) Push(ctx context.Context, generationID, stage string, jobs ...objectQueueJob) error {
	if len(jobs) == 0 {
		return nil
	}
	return b.update(ctx, generationID, stage, func(state *objectQueueState) error {
		now := time.Now().UTC()
		for i := range jobs {
			if jobs[i].JobID == "" {
				jobs[i].JobID = uuid.New().String()
			}
			exists := false
			for j := range state.Jobs {
				if state.Jobs[j].JobID == jobs[i].JobID {
					exists = true
					break
				}
			}
			if exists {
				continue
			}
			jobs[i].Status = queueJobQueued
			if jobs[i].CreatedAt.IsZero() {
				jobs[i].CreatedAt = now
			}
			jobs[i].UpdatedAt = now
			state.Jobs = append(state.Jobs, jobs[i])
		}
		return nil
	})
}

func (b *objectQueueBroker) Claim(ctx context.Context, generationID, stage, workerID string, limit int, lease time.Duration) ([]objectQueueJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	var claimed []objectQueueJob
	err := b.update(ctx, generationID, stage, func(state *objectQueueState) error {
		now := time.Now().UTC()
		for i := range state.Jobs {
			job := &state.Jobs[i]
			if job.Status == queueJobRunning && !job.LeaseUntil.IsZero() && job.LeaseUntil.Before(now) {
				job.Status = queueJobQueued
				job.WorkerID = ""
				job.UpdatedAt = now
			}
			if job.Status != queueJobQueued || len(claimed) >= limit {
				continue
			}
			job.Status = queueJobRunning
			job.WorkerID = workerID
			job.LeaseUntil = now.Add(lease)
			job.Attempts++
			job.UpdatedAt = now
			claimed = append(claimed, *job)
		}
		return nil
	})
	return claimed, err
}

func (b *objectQueueBroker) Complete(ctx context.Context, generationID, stage, jobID, resultRef string, nextJobs ...objectQueueJob) error {
	for _, next := range nextJobs {
		if err := b.Push(ctx, generationID, next.Stage, next); err != nil {
			return err
		}
	}
	if err := b.updateJob(ctx, generationID, stage, jobID, func(job *objectQueueJob) {
		job.Status = queueJobDone
		job.ResultRef = resultRef
		job.LeaseUntil = time.Time{}
		job.WorkerID = ""
		job.Error = ""
	}); err != nil {
		return err
	}
	return nil
}

func (b *objectQueueBroker) Fail(ctx context.Context, generationID, stage, jobID, reason string, maxAttempts int) error {
	return b.updateJob(ctx, generationID, stage, jobID, func(job *objectQueueJob) {
		if maxAttempts > 0 && job.Attempts < maxAttempts {
			job.Status = queueJobQueued
			job.WorkerID = ""
			job.LeaseUntil = time.Time{}
		} else {
			job.Status = queueJobFailed
		}
		job.Error = reason
	})
}

func (b *objectQueueBroker) Summary(ctx context.Context, generationID, stage string) (objectQueueSummary, error) {
	state, _, err := b.load(ctx, generationID, stage)
	if err != nil {
		return objectQueueSummary{}, err
	}
	var summary objectQueueSummary
	for _, job := range state.Jobs {
		switch job.Status {
		case queueJobQueued:
			summary.Queued++
		case queueJobRunning:
			summary.Running++
		case queueJobDone:
			summary.Done++
		case queueJobFailed:
			summary.Failed++
		}
	}
	return summary, nil
}

func (b *objectQueueBroker) updateJob(ctx context.Context, generationID, stage, jobID string, mutate func(*objectQueueJob)) error {
	if jobID == "" {
		return errors.New("queue job id is required")
	}
	return b.update(ctx, generationID, stage, func(state *objectQueueState) error {
		for i := range state.Jobs {
			if state.Jobs[i].JobID == jobID {
				mutate(&state.Jobs[i])
				state.Jobs[i].UpdatedAt = time.Now().UTC()
				return nil
			}
		}
		return fmt.Errorf("queue job %s not found in %s", jobID, stage)
	})
}

func (b *objectQueueBroker) update(ctx context.Context, generationID, stage string, mutate func(*objectQueueState) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for attempt := 0; attempt < 5; attempt++ {
		state, etag, err := b.load(ctx, generationID, stage)
		if err != nil {
			return err
		}
		if err := mutate(state); err != nil {
			return err
		}
		state.Epoch++
		state.UpdatedAt = time.Now().UTC()
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		key := objectQueueKey(generationID, stage)
		if etag == "" {
			_, err = b.s3.UploadCAS(ctx, key, data, "application/json", "", "*")
		} else {
			_, err = b.s3.UploadCAS(ctx, key, data, "application/json", etag, "")
		}
		if err == nil {
			return nil
		}
		if !isObjectPreconditionError(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
	return fmt.Errorf("queue %s CAS retries exhausted", objectQueueKey(generationID, stage))
}

func (b *objectQueueBroker) load(ctx context.Context, generationID, stage string) (*objectQueueState, string, error) {
	key := objectQueueKey(generationID, stage)
	data, etag, err := b.s3.DownloadWithETag(ctx, key)
	if err != nil {
		if !isObjectNotFound(err) {
			return nil, "", err
		}
		return &objectQueueState{
			QueueID:      key,
			GenerationID: generationID,
			Stage:        stage,
			UpdatedAt:    time.Now().UTC(),
		}, "", nil
	}
	var state objectQueueState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, "", fmt.Errorf("parsing queue state %s: %w", key, err)
	}
	return &state, etag, nil
}

func objectQueueKey(generationID, stage string) string {
	return fmt.Sprintf("syncs/%s/queues/%s.queue.json", generationID, safeObjectName(stage))
}

func isObjectNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "nosuchkey") || strings.Contains(msg, "notfound") || strings.Contains(msg, "not found") || strings.Contains(msg, "status code: 404")
}

func isObjectPreconditionError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "precondition") || strings.Contains(msg, "status code: 409") || strings.Contains(msg, "status code: 412")
}
