package queue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type mockSQSClient struct {
	sendInputs       []*sqs.SendMessageBatchInput
	receiveInput     *sqs.ReceiveMessageInput
	receiveOutput    *sqs.ReceiveMessageOutput
	deleteInputs     []*sqs.DeleteMessageInput
	visibilityInputs []*sqs.ChangeMessageVisibilityInput
}

func (m *mockSQSClient) SendMessageBatch(_ context.Context, input *sqs.SendMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	m.sendInputs = append(m.sendInputs, input)
	return &sqs.SendMessageBatchOutput{}, nil
}

func (m *mockSQSClient) ReceiveMessage(_ context.Context, input *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	m.receiveInput = input
	if m.receiveOutput == nil {
		return &sqs.ReceiveMessageOutput{}, nil
	}
	return m.receiveOutput, nil
}

func (m *mockSQSClient) DeleteMessage(_ context.Context, input *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	m.deleteInputs = append(m.deleteInputs, input)
	return &sqs.DeleteMessageOutput{}, nil
}

func (m *mockSQSClient) ChangeMessageVisibility(_ context.Context, input *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	m.visibilityInputs = append(m.visibilityInputs, input)
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func TestSQSQueueEnqueueBatchesAndPreservesRootOrdering(t *testing.T) {
	client := &mockSQSClient{}
	q, err := NewSQSQueue(client, testSQSURLs())
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	jobs := make([]JobMessage, 11)
	for i := range jobs {
		jobs[i] = JobMessage{JobID: "job-" + string(rune('a'+i)), OrgID: "org-1", RootID: "root-1"}
	}
	if err := q.Enqueue(context.Background(), StageIndex, jobs...); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := len(client.sendInputs); got != 2 {
		t.Fatalf("send batches = %d, want 2", got)
	}
	if len(client.sendInputs[0].Entries) != 10 || len(client.sendInputs[1].Entries) != 1 {
		t.Fatalf("batch sizes = %d,%d, want 10,1", len(client.sendInputs[0].Entries), len(client.sendInputs[1].Entries))
	}
	groupID := aws.ToString(client.sendInputs[0].Entries[0].MessageGroupId)
	for _, input := range client.sendInputs {
		for _, entry := range input.Entries {
			if aws.ToString(entry.MessageGroupId) != groupID {
				t.Fatal("messages for one root received different FIFO group IDs")
			}
			var job JobMessage
			if err := json.Unmarshal([]byte(aws.ToString(entry.MessageBody)), &job); err != nil {
				t.Fatalf("decode message: %v", err)
			}
			if job.Stage != StageIndex || job.EnqueuedAt.IsZero() {
				t.Fatalf("queued job metadata = %#v", job)
			}
		}
	}
}

func TestSQSQueuePullAckAndVisibility(t *testing.T) {
	body, _ := json.Marshal(JobMessage{JobID: "job-1", OrgID: "org-1", RootID: "root-1", Stage: StageChunk})
	client := &mockSQSClient{receiveOutput: &sqs.ReceiveMessageOutput{Messages: []types.Message{{
		Body:          aws.String(string(body)),
		ReceiptHandle: aws.String("receipt-1"),
		Attributes: map[string]string{
			string(types.MessageSystemAttributeNameApproximateReceiveCount): "4",
		},
	}}}}
	q, err := NewSQSQueue(client, testSQSURLs())
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	messages, err := q.Pull(context.Background(), StageChunk, 50, 30*time.Second)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(messages) != 1 || messages[0].Attempts != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	if client.receiveInput.MaxNumberOfMessages != 10 || client.receiveInput.WaitTimeSeconds != 20 {
		t.Fatalf("receive limits = batch %d wait %d", client.receiveInput.MaxNumberOfMessages, client.receiveInput.WaitTimeSeconds)
	}
	if err := q.InProgress(messages[0]); err != nil {
		t.Fatalf("in progress: %v", err)
	}
	if got := client.visibilityInputs[0].VisibilityTimeout; got != 300 {
		t.Fatalf("heartbeat visibility = %d, want 300", got)
	}
	if err := q.NakWithDelay(messages[0], 1500*time.Millisecond); err != nil {
		t.Fatalf("nak delay: %v", err)
	}
	if got := client.visibilityInputs[1].VisibilityTimeout; got != 2 {
		t.Fatalf("retry visibility = %d, want 2", got)
	}
	if err := q.Ack(messages[0]); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if len(client.deleteInputs) != 1 || aws.ToString(client.deleteInputs[0].ReceiptHandle) != "receipt-1" {
		t.Fatalf("delete inputs = %#v", client.deleteInputs)
	}
}

func TestSQSQueueRequiresEveryStageURL(t *testing.T) {
	_, err := NewSQSQueue(&mockSQSClient{}, map[string]string{StageChunk: "chunk-url"})
	if err == nil {
		t.Fatal("expected missing stage URL error")
	}
}

func testSQSURLs() map[string]string {
	return map[string]string{
		StageChunk:   "https://sqs.test/chunk.fifo",
		StageEmbed:   "https://sqs.test/embed.fifo",
		StageIndex:   "https://sqs.test/index.fifo",
		StageCommit:  "https://sqs.test/commit.fifo",
		StageCleanup: "https://sqs.test/cleanup.fifo",
	}
}
