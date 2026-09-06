package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const (
	maxSQSBatchSize       = 10
	maxSQSBatchBytes      = 1 << 20
	maxSQSMessageBytes    = 256 << 10
	maxSQSWaitTime        = 20 * time.Second
	maxSQSVisibilityDelay = 12 * time.Hour
	defaultSQSVisibility  = 5 * time.Minute
	queueOperationTimeout = 30 * time.Second
)

type SQSQueue struct {
	client    *sqs.Client
	queueURLs map[string]string
}

type sqsReceipt struct {
	queueURL      string
	receiptHandle string
}

func NewSQSQueue(client *sqs.Client, queueURLs map[string]string) (*SQSQueue, error) {
	if client == nil {
		return nil, errors.New("SQS client is required")
	}
	stages := []string{StageTransform, StageIndex}
	urls := make(map[string]string, len(stages))
	for _, stage := range stages {
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
	entries := make([]types.SendMessageBatchRequestEntry, 0, maxSQSBatchSize)
	batchBytes := 0
	flush := func() error {
		if len(entries) == 0 {
			return nil
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
		entries = make([]types.SendMessageBatchRequestEntry, 0, maxSQSBatchSize)
		batchBytes = 0
		return nil
	}
	for _, msg := range msgs {
		if msg.JobID == "" || msg.WorkID == "" {
			return errors.New("queue job_id and work_id are required")
		}
		msg.Stage = stage
		body, marshalErr := json.Marshal(msg)
		if marshalErr != nil {
			return marshalErr
		}
		if len(body) > maxSQSMessageBytes {
			return fmt.Errorf("SQS message %s is %d bytes; maximum is %d", msg.JobID, len(body), maxSQSMessageBytes)
		}
		if len(entries) == maxSQSBatchSize || (len(entries) > 0 && batchBytes+len(body) > maxSQSBatchBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		entries = append(entries, types.SendMessageBatchRequestEntry{
			Id:                     aws.String(strconv.Itoa(len(entries))),
			MessageBody:            aws.String(string(body)),
			MessageGroupId:         aws.String(sqsMessageGroupID(stage, msg)),
			MessageDeduplicationId: aws.String(sqsStableID(msg.JobID)),
		})
		batchBytes += len(body)
	}
	return flush()
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
			// after the receive limit. Still return the valid jobs from this batch:
			// discarding them would strand their receipts until visibility expires.
			// Log identity only; a malformed body can contain sensitive data.
			log.Printf("SQS %s message %s is not valid job JSON; left unacknowledged (receive batch size=%d)",
				stage, aws.ToString(message.MessageId), len(output.Messages))
			continue
		}
		messages = append(messages, ReceivedMessage{
			Job:     job,
			receipt: sqsReceipt{queueURL: queueURL, receiptHandle: aws.ToString(message.ReceiptHandle)},
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

func (q *SQSQueue) queueURL(stage string) (string, error) {
	url := q.queueURLs[stage]
	if url == "" {
		return "", fmt.Errorf("unknown queue stage %q", stage)
	}
	return url, nil
}

func getSQSReceipt(msg ReceivedMessage) (sqsReceipt, error) {
	receipt := msg.receipt
	if receipt.queueURL == "" || receipt.receiptHandle == "" {
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

func sqsMessageGroupID(stage string, msg JobMessage) string {
	if stage == StageIndex {
		// Serialize deliveries per file; workers also enforce version order.
		return sqsStableID(msg.OrgID, msg.RootID, msg.FileID, stage)
	}
	return sqsStableID(msg.OrgID, msg.RootID, msg.WorkID, stage)
}
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
