package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pufferfs/pufferfs/internal/queue"
)

// FileConsumer is an ECS role. It holds a bounded number of SQS receipts while
// Modal processes work; it never relays source bytes, text chunks or vectors.
type FileConsumer struct {
	server      *Server
	queue       *queue.SQSQueue
	stage       string
	concurrency int
}

var errFileWorkBusy = errors.New("file work already has an active attempt")

func NewFileConsumer(s *Server, q *queue.SQSQueue, stage string, concurrency int) (*FileConsumer, error) {
	if stage != queue.StageTransform && stage != queue.StageIndex {
		return nil, fmt.Errorf("file consumer stage must be transform or index")
	}
	if s == nil || s.db == nil || s.modal == nil || q == nil {
		return nil, fmt.Errorf("file consumer dependencies are required")
	}
	return &FileConsumer{server: s, queue: q, stage: stage, concurrency: min(max(1, concurrency), 64)}, nil
}

func (c *FileConsumer) Run(ctx context.Context) error {
	// Poll only as much as can execute. Do not shovel an unbounded SQS backlog
	// into Modal's internal input queue.
	slots := make(chan struct{}, c.concurrency)
	for range c.concurrency {
		slots <- struct{}{}
	}
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-slots:
		}
		available := 1
	drain:
		for available < c.concurrency {
			select {
			case <-slots:
				available++
			default:
				break drain
			}
		}
		messages, err := c.queue.Pull(ctx, c.stage, available, 20*time.Second)
		for range available - len(messages) {
			slots <- struct{}{}
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("file %s queue receive failed: %v", c.stage, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		for _, message := range messages {
			workers.Add(1)
			go func(msg queue.ReceivedMessage) {
				defer workers.Done()
				defer func() { slots <- struct{}{} }()
				c.process(ctx, msg)
			}(message)
		}
	}
}

func (c *FileConsumer) process(ctx context.Context, msg queue.ReceivedMessage) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.queue.InProgress(msg); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	err := c.Process(ctx, msg.Job)
	cancel()
	<-done
	if err != nil {
		log.Printf("file work %s failed: %v", msg.Job.WorkID, err)
		// SQS owns retry limits and DLQ placement. Never acknowledge a failure
		// merely because an application-side retry counter was exhausted.
		_ = c.queue.NakWithDelay(msg, time.Minute)
		return
	}
	if err = c.queue.Ack(msg); err != nil {
		log.Printf("file work %s acknowledgement failed: %v", msg.Job.WorkID, err)
	}
}

func (c *FileConsumer) Process(ctx context.Context, msg queue.JobMessage) error {
	if msg.WorkID == "" || msg.Stage != c.stage {
		return fmt.Errorf("invalid file work message")
	}
	var status string
	var noVector bool
	var err error
	for {
		var leased bool
		err = c.server.db.pool.QueryRow(ctx, `SELECT w.status,r.vector_disabled,
		COALESCE(w.status='running' AND w.lease_until>NOW()
		    AND v.id=f.captured_version_id AND r.deleting_at IS NULL,FALSE)
		FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id JOIN file_versions v ON v.id=e.version_id
		JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
		WHERE w.id=$1 AND w.stage=$2 AND r.org_id=$3 AND f.root_id=$4 AND f.id=$5 AND v.id=$6 AND e.id=$7`,
			msg.WorkID, c.stage, msg.OrgID, msg.RootID, msg.FileID, msg.VersionID, msg.ExtractionID).Scan(&status, &noVector, &leased)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // root/work was removed
		}
		if err != nil {
			return err
		}
		if fileWorkDurable(status, c.stage) {
			return nil
		}
		if !leased {
			err = c.server.modal.ProcessFileWork(ctx, msg.WorkID, c.stage, uuid.NewString(), noVector)
			if !errors.Is(err, errFileWorkBusy) {
				break
			}
		}
		// Another attempt owns the work. Keep this bounded SQS slot and its
		// visibility heartbeat, rather than consuming redeliveries on "busy".
		// Do not renew the database lease: only its owner can do that. Superseded
		// versions bypass waiting so their worker can finish the stale handoff.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	if err != nil {
		return err
	}
	err = c.server.db.pool.QueryRow(ctx, `SELECT status FROM file_work WHERE id=$1`, msg.WorkID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fileWorkDurable(status, c.stage) {
		return fmt.Errorf("Modal returned before durable %s completion: %s", c.stage, status)
	}
	return nil
}

func fileWorkDurable(status, stage string) bool {
	return status == "complete" || status == "superseded" || (stage == queue.StageTransform && status == "waiting_provider")
}

func (m *ModalClient) ProcessFileWork(ctx context.Context, id, stage, token string, noVector bool) error {
	endpoint := m.transformURL
	if stage == queue.StageIndex {
		endpoint = m.fileIndexURL
		if noVector {
			endpoint = m.fileCPUIndexURL
		}
	}
	if endpoint == "" || m.secretKey == "" {
		return fmt.Errorf("Modal %s endpoint/authentication is not configured", stage)
	}
	body, err := json.Marshal(map[string]string{"work_id": id, "attempt_token": token, "secret_key": m.secretKey})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Modal %s returned HTTP %d", stage, response.StatusCode)
	}
	var result struct {
		Status string `json:"status"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return err
	}
	if result.Status == "busy" {
		return errFileWorkBusy
	}
	if !fileWorkDurable(result.Status, stage) {
		return fmt.Errorf("Modal work status: %s", result.Status)
	}
	return nil
}
