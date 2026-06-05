package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	StageChunk   = "chunk"
	StageEmbed   = "embed"
	StageIndex   = "index"
	StageCommit  = "commit"
	StageCleanup = "cleanup"
)

type JobMessage struct {
	JobID             string           `json:"job_id"`
	SyncJobID         string           `json:"sync_job_id,omitempty"`
	UserID            string           `json:"user_id,omitempty"`
	OrgID             string           `json:"org_id"`
	RootID            string           `json:"root_id"`
	GenerationID      string           `json:"generation_id"`
	GenerationSeq     int64            `json:"generation_seq"`
	BaseGenerationID  string           `json:"base_generation_id"`
	BaseGenerationSeq int64            `json:"base_generation_seq"`
	Stage             string           `json:"stage"`
	PayloadRef        string           `json:"payload_ref,omitempty"`
	CleanupKeys       []string         `json:"cleanup_keys,omitempty"`
	IndexNamespaces   []IndexNamespace `json:"index_namespaces,omitempty"`
	ShardIndex        int              `json:"shard_index"`
	TotalShards       int              `json:"total_shards"`
	Priority          int              `json:"priority,omitempty"`
	EnqueuedAt        time.Time        `json:"enqueued_at"`
}

type IndexNamespace struct {
	Namespace  string `json:"namespace"`
	ShardIndex int    `json:"shard_index"`
	ShardCount int    `json:"shard_count"`
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
	replicas       int
}

func NewNATSQueue(url string, opts ...NATSOption) (*NATSQueue, error) {
	cfg := natsQueueConfig{consumerPrefix: "pufferfs", replicas: queueReplicas()}
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
	if err := q.ensureTopology(cfg.consumerPrefix, cfg.replicas); err != nil {
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

func WithReplicas(replicas int) NATSOption {
	return func(cfg *natsQueueConfig) {
		cfg.replicas = normalizeQueueReplicas(replicas)
	}
}

func (q *NATSQueue) ensureTopology(consumerPrefix string, replicas int) error {
	for _, stage := range []string{StageChunk, StageEmbed, StageIndex, StageCommit, StageCleanup} {
		stream := streamName(stage)
		subject := subjectForStage(stage)
		streamConfig := nats.StreamConfig{
			Name:       stream,
			Subjects:   []string{subject},
			Storage:    nats.FileStorage,
			Retention:  nats.WorkQueuePolicy,
			Duplicates: duplicateWindow(),
		}
		if replicas > 0 {
			streamConfig.Replicas = replicas
		}
		streamInfo, err := q.js.StreamInfo(stream)
		if err != nil {
			if !errors.Is(err, nats.ErrStreamNotFound) {
				return err
			}
			if _, err := q.js.AddStream(&streamConfig); err != nil {
				return err
			}
		} else if streamNeedsUpdate(streamInfo.Config, streamConfig, replicas) {
			cfg := streamInfo.Config
			cfg.Duplicates = streamConfig.Duplicates
			if replicas > 0 {
				cfg.Replicas = replicas
			}
			if _, err := q.js.UpdateStream(&cfg); err != nil {
				return err
			}
		}
		consumer := consumerName(consumerPrefix, stage)
		desired := consumerConfig(stage, consumer, subject, replicas)
		consumerInfo, err := q.js.ConsumerInfo(stream, consumer)
		if err != nil {
			if !errors.Is(err, nats.ErrConsumerNotFound) {
				return err
			}
			if _, err := q.js.AddConsumer(stream, &desired); err != nil {
				return err
			}
		} else if consumerInfo.Config.AckWait != desired.AckWait ||
			consumerInfo.Config.MaxDeliver != desired.MaxDeliver ||
			consumerInfo.Config.MaxAckPending != desired.MaxAckPending ||
			consumerInfo.Config.FilterSubject != desired.FilterSubject ||
			consumerReplicasNeedUpdate(consumerInfo.Config, desired) {
			cfg := consumerInfo.Config
			cfg.AckWait = desired.AckWait
			cfg.MaxDeliver = desired.MaxDeliver
			cfg.MaxAckPending = desired.MaxAckPending
			cfg.FilterSubject = desired.FilterSubject
			if desired.Replicas > 0 {
				cfg.Replicas = desired.Replicas
			}
			if _, err := q.js.UpdateConsumer(stream, &cfg); err != nil {
				return err
			}
		}
	}
	return nil
}

func streamNeedsUpdate(current, desired nats.StreamConfig, replicas int) bool {
	if current.Duplicates != desired.Duplicates {
		return true
	}
	return replicas > 0 && current.Replicas != replicas
}

func consumerReplicasNeedUpdate(current, desired nats.ConsumerConfig) bool {
	return desired.Replicas > 0 && current.Replicas != desired.Replicas
}

func consumerConfig(stage, consumer, subject string, replicas int) nats.ConsumerConfig {
	ackWait := 5 * time.Minute
	maxDeliver := 5
	if stage == StageCommit || stage == StageCleanup {
		ackWait = 30 * time.Second
		maxDeliver = 30
	}
	cfg := nats.ConsumerConfig{
		Durable:       consumer,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliver,
		MaxAckPending: 10000,
		FilterSubject: subject,
	}
	if replicas > 0 {
		cfg.Replicas = replicas
	}
	return cfg
}

func duplicateWindow() time.Duration {
	const defaultWindow = 24 * time.Hour
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_QUEUE_DEDUPE_WINDOW"))
	if raw == "" {
		return defaultWindow
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultWindow
}

func queueReplicas() int {
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_QUEUE_REPLICAS"))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return normalizeQueueReplicas(n)
}

func normalizeQueueReplicas(n int) int {
	if n < 1 {
		return 0
	}
	if n > 5 {
		return 5
	}
	return n
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
