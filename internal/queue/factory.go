package queue

import (
	"context"
	"fmt"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// NewFromEnv requires both SQS queues. There is no in-process fallback.
func NewFromEnv(ctx context.Context) (*SQSQueue, error) {
	urls := make(map[string]string, 2)
	for stage, name := range map[string]string{
		StageTransform: "PUFFERFS_SQS_TRANSFORM_QUEUE_URL",
		StageIndex:     "PUFFERFS_SQS_INDEX_QUEUE_URL",
	} {
		urls[stage] = strings.TrimSpace(os.Getenv(name))
		if urls[stage] == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config for SQS: %w", err)
	}
	return NewSQSQueue(sqs.NewFromConfig(cfg), urls)
}
