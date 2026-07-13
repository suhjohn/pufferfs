package queue

import (
	"context"
	"fmt"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

const (
	BackendNATS = "nats"
	BackendSQS  = "sqs"
)

var sqsQueueEnv = map[string]string{
	StageChunk:   "PUFFERFS_SQS_CHUNK_QUEUE_URL",
	StageEmbed:   "PUFFERFS_SQS_EMBED_QUEUE_URL",
	StageIndex:   "PUFFERFS_SQS_INDEX_QUEUE_URL",
	StageCommit:  "PUFFERFS_SQS_COMMIT_QUEUE_URL",
	StageCleanup: "PUFFERFS_SQS_CLEANUP_QUEUE_URL",
}

// NewFromEnv constructs the selected queue backend. Optional callers get a
// nil queue when neither PUFFERFS_QUEUE_BACKEND nor NATS_URL is configured;
// required callers preserve the local-development NATS default.
func NewFromEnv(ctx context.Context, required bool) (Queue, string, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("PUFFERFS_QUEUE_BACKEND")))
	if backend == "" {
		if os.Getenv("NATS_URL") == "" && !required {
			return nil, "", nil
		}
		backend = BackendNATS
	}
	switch backend {
	case BackendNATS:
		url := strings.TrimSpace(os.Getenv("NATS_URL"))
		if url == "" {
			url = "nats://127.0.0.1:4222"
		}
		q, err := NewNATSQueue(url)
		return q, backend, err
	case BackendSQS:
		urls := make(map[string]string, len(sqsQueueEnv))
		for stage, envName := range sqsQueueEnv {
			url := strings.TrimSpace(os.Getenv(envName))
			if url == "" {
				return nil, backend, fmt.Errorf("%s is required when PUFFERFS_QUEUE_BACKEND=sqs", envName)
			}
			urls[stage] = url
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, backend, fmt.Errorf("loading AWS config for SQS: %w", err)
		}
		q, err := NewSQSQueue(sqs.NewFromConfig(cfg), urls)
		return q, backend, err
	default:
		return nil, backend, fmt.Errorf("unsupported PUFFERFS_QUEUE_BACKEND %q", backend)
	}
}
