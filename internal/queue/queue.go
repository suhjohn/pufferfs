package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	StageChunk  = "chunk"
	StageEmbed  = "embed"
	StageIndex  = "index"
	StageCommit = "commit"
)

type JobMessage struct {
	JobID             string    `json:"job_id"`
	SyncJobID         string    `json:"sync_job_id,omitempty"`
	UserID            string    `json:"user_id,omitempty"`
	OrgID             string    `json:"org_id"`
	RootID            string    `json:"root_id"`
	GenerationID      string    `json:"generation_id"`
	GenerationSeq     int64     `json:"generation_seq"`
	BaseGenerationID  string    `json:"base_generation_id"`
	BaseGenerationSeq int64     `json:"base_generation_seq"`
	Stage             string    `json:"stage"`
	PayloadRef        string    `json:"payload_ref,omitempty"`
	ShardIndex        int       `json:"shard_index"`
	TotalShards       int       `json:"total_shards"`
	Priority          int       `json:"priority,omitempty"`
	EnqueuedAt        time.Time `json:"enqueued_at"`
}

type ReceivedMessage struct {
	Job      JobMessage
	Attempts int
	msg      *nats.Msg
}

type Queue interface {
	Enqueue(ctx context.Context, stage string, msgs ...JobMessage) error
	Pull(ctx context.Context, stage string, batchSize int, timeout time.Duration) ([]ReceivedMessage, error)
	Ack(ReceivedMessage) error
	Nak(ReceivedMessage) error
	NakWithDelay(ReceivedMessage, time.Duration) error
	InProgress(ReceivedMessage) error
	Close()
}

type NATSQueue struct {
	nc             *nats.Conn
	js             nats.JetStreamContext
	consumerPrefix string
}

type NATSOption func(*natsQueueConfig)

type natsQueueConfig struct {
	consumerPrefix string
}

func NewNATSQueue(url string, opts ...NATSOption) (*NATSQueue, error) {
	cfg := natsQueueConfig{consumerPrefix: "pufferfs"}
	for _, opt := range opts {
		opt(&cfg)
	}
	nc, err := nats.Connect(url,
		nats.Name("pufferfs-dispatcher"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, err
	}
	js, err := nc.JetStream(nats.PublishAsyncMaxPending(8192))
	if err != nil {
		nc.Close()
		return nil, err
	}
	q := &NATSQueue{nc: nc, js: js, consumerPrefix: cfg.consumerPrefix}
	if err := q.ensureTopology(cfg.consumerPrefix); err != nil {
		nc.Close()
		return nil, err
	}
	return q, nil
}

func WithConsumerPrefix(prefix string) NATSOption {
	return func(cfg *natsQueueConfig) {
		if prefix != "" {
			cfg.consumerPrefix = prefix
		}
	}
}

func (q *NATSQueue) ensureTopology(consumerPrefix string) error {
	for _, stage := range []string{StageChunk, StageEmbed, StageIndex, StageCommit} {
		stream := streamName(stage)
		subject := subjectForStage(stage)
		if _, err := q.js.StreamInfo(stream); err != nil {
			if !errors.Is(err, nats.ErrStreamNotFound) {
				return err
			}
			if _, err := q.js.AddStream(&nats.StreamConfig{
				Name:       stream,
				Subjects:   []string{subject},
				Storage:    nats.FileStorage,
				Retention:  nats.WorkQueuePolicy,
				Duplicates: 5 * time.Minute,
			}); err != nil {
				return err
			}
		}
		consumer := consumerName(consumerPrefix, stage)
		if _, err := q.js.ConsumerInfo(stream, consumer); err != nil {
			if !errors.Is(err, nats.ErrConsumerNotFound) {
				return err
			}
			ackWait := 5 * time.Minute
			maxDeliver := 3
			if stage == StageCommit {
				ackWait = 30 * time.Second
				maxDeliver = 30
			}
			if _, err := q.js.AddConsumer(stream, &nats.ConsumerConfig{
				Durable:       consumer,
				AckPolicy:     nats.AckExplicitPolicy,
				AckWait:       ackWait,
				MaxDeliver:    maxDeliver,
				MaxAckPending: 10000,
				FilterSubject: subject,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (q *NATSQueue) Enqueue(ctx context.Context, stage string, msgs ...JobMessage) error {
	for _, msg := range msgs {
		if msg.EnqueuedAt.IsZero() {
			msg.EnqueuedAt = time.Now().UTC()
		}
		msg.Stage = stage
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		if _, err := q.js.PublishAsync(subject(stage, msg.OrgID, msg.RootID), data, nats.MsgId(msg.JobID)); err != nil {
			return err
		}
	}
	select {
	case <-q.js.PublishAsyncComplete():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *NATSQueue) Pull(ctx context.Context, stage string, batchSize int, timeout time.Duration) ([]ReceivedMessage, error) {
	if batchSize < 1 {
		batchSize = 1
	}
	stream := streamName(stage)
	consumer := consumerName(q.consumerPrefix, stage)
	sub, err := q.js.PullSubscribe(subjectForStage(stage), consumer, nats.Bind(stream, consumer))
	if err != nil {
		return nil, err
	}
	fetchCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		fetchCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	msgs, err := sub.Fetch(batchSize, nats.Context(fetchCtx))
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]ReceivedMessage, 0, len(msgs))
	for _, msg := range msgs {
		var job JobMessage
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			_ = msg.Term()
			return nil, err
		}
		out = append(out, ReceivedMessage{Job: job, Attempts: deliveryCount(msg), msg: msg})
	}
	return out, nil
}

func (q *NATSQueue) Ack(msg ReceivedMessage) error {
	return msg.msg.Ack()
}

func (q *NATSQueue) Nak(msg ReceivedMessage) error {
	return msg.msg.Nak()
}

func (q *NATSQueue) NakWithDelay(msg ReceivedMessage, delay time.Duration) error {
	return msg.msg.NakWithDelay(delay)
}

func (q *NATSQueue) InProgress(msg ReceivedMessage) error {
	return msg.msg.InProgress()
}

func (q *NATSQueue) Close() {
	if q.nc != nil {
		q.nc.Close()
	}
}

func Subject(stage, orgID, rootID string) string {
	return subject(stage, orgID, rootID)
}

func streamName(stage string) string {
	return "PUFFERFS_" + strings.ToUpper(stage)
}

func subjectForStage(stage string) string {
	return fmt.Sprintf("jobs.%s.>", stage)
}

func subject(stage, orgID, rootID string) string {
	return fmt.Sprintf("jobs.%s.%s.%s", stage, orgID, rootID)
}

func consumerName(prefix, stage string) string {
	return fmt.Sprintf("%s-%s-workers", prefix, stage)
}

func deliveryCount(msg *nats.Msg) int {
	meta, err := msg.Metadata()
	if err != nil {
		return 1
	}
	if meta.NumDelivered < 1 {
		return 1
	}
	return int(meta.NumDelivered)
}
