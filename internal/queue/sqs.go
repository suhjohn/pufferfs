package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const (
	maxSQSBatchSize       = 10
	maxSQSWaitTime        = 20 * time.Second
	maxSQSVisibilityDelay = 12 * time.Hour
	defaultSQSVisibility  = 5 * time.Minute
	queueOperationTimeout = 30 * time.Second
)

type sqsClient interface {
	SendMessageBatch(context.Context, *sqs.SendMessageBatchInput, ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}

type SQSQueue struct {
	client    sqsClient
	queueURLs map[string]string
}

type sqsReceipt struct {
	queueURL      string
	receiptHandle string
}

func NewSQSQueue(client sqsClient, queueURLs map[string]string) (*SQSQueue, error) {
	if client == nil {
		return nil, errors.New("SQS client is required")
	}
	urls := make(map[string]string, len(allStages()))
	for _, stage := range allStages() {
		url := queueURLs[stage]
		if url == "" {
			return nil, fmt.Errorf("SQS queue URL is required for stage %q", stage)
		}
		urls[stage] = url
	}
	return &SQSQueue{client: client, queueURLs: urls}, nil
}

func (q *SQSQueue) Enqueue(ctx context.Context, stage string, msgs ...JobMessage) error {
	queueURL, err := q.queueURL(stage)
	if err != nil {
		return err
	}
	for start := 0; start < len(msgs); start += maxSQSBatchSize {
		end := start + maxSQSBatchSize
		if end > len(msgs) {
			end = len(msgs)
		}
		entries := make([]types.SendMessageBatchRequestEntry, 0, end-start)
		for i, msg := range msgs[start:end] {
			if msg.JobID == "" {
				return errors.New("queue job_id is required")
			}
			if msg.EnqueuedAt.IsZero() {
				msg.EnqueuedAt = time.Now().UTC()
			}
			msg.Stage = stage
			body, marshalErr := json.Marshal(msg)
			if marshalErr != nil {
				return marshalErr
			}
			entries = append(entries, types.SendMessageBatchRequestEntry{
				Id:                     aws.String(strconv.Itoa(i)),
				MessageBody:            aws.String(string(body)),
				MessageGroupId:         aws.String(sqsStableID(msg.OrgID, msg.RootID)),
				MessageDeduplicationId: aws.String(sqsStableID(msg.JobID)),
			})
		}
		output, sendErr := q.client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
			QueueUrl: aws.String(queueURL),
			Entries:  entries,
		})
		if sendErr != nil {
			return sendErr
		}
		if len(output.Failed) > 0 {
			failure := output.Failed[0]
			return fmt.Errorf("SQS rejected %d messages (first id=%s code=%s): %s", len(output.Failed), aws.ToString(failure.Id), aws.ToString(failure.Code), aws.ToString(failure.Message))
		}
	}
	return nil
}

func (q *SQSQueue) Pull(ctx context.Context, stage string, batchSize int, timeout time.Duration) ([]ReceivedMessage, error) {
	queueURL, err := q.queueURL(stage)
	if err != nil {
		return nil, err
	}
	if batchSize < 1 {
		batchSize = 1
	}
	if batchSize > maxSQSBatchSize {
		batchSize = maxSQSBatchSize
	}
	if timeout < 0 {
		timeout = 0
	}
	if timeout > maxSQSWaitTime {
		timeout = maxSQSWaitTime
	}
	output, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: int32(batchSize),
		WaitTimeSeconds:     int32(timeout / time.Second),
		VisibilityTimeout:   int32(defaultSQSVisibility / time.Second),
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameApproximateReceiveCount,
		},
	})
	if err != nil {
		return nil, err
	}
	messages := make([]ReceivedMessage, 0, len(output.Messages))
	for _, message := range output.Messages {
		var job JobMessage
		if err := json.Unmarshal([]byte(aws.ToString(message.Body)), &job); err != nil {
			// Leave malformed messages unacked so SQS moves them to the stage DLQ
			// after the configured receive limit, preserving evidence for repair.
			return nil, fmt.Errorf("decoding SQS job message: %w", err)
		}
		attempts, _ := strconv.Atoi(message.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
		if attempts < 1 {
			attempts = 1
		}
		messages = append(messages, ReceivedMessage{
			Job:      job,
			Attempts: attempts,
			receipt:  sqsReceipt{queueURL: queueURL, receiptHandle: aws.ToString(message.ReceiptHandle)},
		})
	}
	return messages, nil
}

func (q *SQSQueue) Ack(msg ReceivedMessage) error {
	receipt, err := getSQSReceipt(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), queueOperationTimeout)
	defer cancel()
	_, err = q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(receipt.queueURL),
		ReceiptHandle: aws.String(receipt.receiptHandle),
	})
	return err
}

func (q *SQSQueue) Nak(msg ReceivedMessage) error {
	return q.changeVisibility(msg, 0)
}

func (q *SQSQueue) NakWithDelay(msg ReceivedMessage, delay time.Duration) error {
	if delay < 0 {
		delay = 0
	}
	if delay > maxSQSVisibilityDelay {
		delay = maxSQSVisibilityDelay
	}
	seconds := int32((delay + time.Second - 1) / time.Second)
	return q.changeVisibility(msg, seconds)
}

func (q *SQSQueue) InProgress(msg ReceivedMessage) error {
	return q.changeVisibility(msg, int32(defaultSQSVisibility/time.Second))
}

func (q *SQSQueue) Close() {}

func (q *SQSQueue) changeVisibility(msg ReceivedMessage, seconds int32) error {
	receipt, err := getSQSReceipt(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), queueOperationTimeout)
	defer cancel()
	_, err = q.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(receipt.queueURL),
		ReceiptHandle:     aws.String(receipt.receiptHandle),
		VisibilityTimeout: seconds,
	})
	return err
}

func (q *SQSQueue) queueURL(stage string) (string, error) {
	url := q.queueURLs[stage]
	if url == "" {
		return "", fmt.Errorf("unknown queue stage %q", stage)
	}
	return url, nil
}

func getSQSReceipt(msg ReceivedMessage) (sqsReceipt, error) {
	receipt, ok := msg.receipt.(sqsReceipt)
	if !ok || receipt.queueURL == "" || receipt.receiptHandle == "" {
		return sqsReceipt{}, errors.New("queue message does not contain an SQS receipt")
	}
	return receipt, nil
}

func sqsStableID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func allStages() []string {
	return []string{StageChunk, StageEmbed, StageIndex, StageCommit, StageCleanup}
}
