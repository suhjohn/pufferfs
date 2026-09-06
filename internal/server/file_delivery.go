package server

import (
	"context"
	"fmt"

	"github.com/pufferfs/pufferfs/internal/queue"
)

type fileDeliveryLedger interface {
	UnpublishedFileWork(context.Context, int) ([]FileWorkDelivery, error)
	MarkFileWorkEnqueued(context.Context, string) error
}

type fileDeliveryQueue interface {
	Enqueue(context.Context, string, ...queue.JobMessage) error
}

// publishFileWork is shared by immediate publication and reconciliation. A
// crash between SQS acceptance and marking delivery causes duplicate delivery,
// never a missing job. No worker is claimed or run through this database scan.
func publishFileWork(ctx context.Context, ledger fileDeliveryLedger, q fileDeliveryQueue, limit int) (int, error) {
	if q == nil {
		return 0, fmt.Errorf("SQS is required for file delivery")
	}
	deliveries, err := ledger.UnpublishedFileWork(ctx, limit)
	if err != nil {
		return 0, err
	}
	byStage := make(map[string][]FileWorkDelivery)
	for _, d := range deliveries {
		if d.ID == "" || d.FileID == "" || d.VersionID == "" || (d.Stage != queue.StageTransform && d.Stage != queue.StageIndex) {
			return 0, fmt.Errorf("invalid file delivery %q", d.ID)
		}
		byStage[d.Stage] = append(byStage[d.Stage], d)
	}
	published := 0
	for _, stage := range []string{queue.StageTransform, queue.StageIndex} {
		pending := byStage[stage]
		for start := 0; start < len(pending); start += 10 {
			batch := pending[start:min(start+10, len(pending))]
			messages := make([]queue.JobMessage, 0, len(batch))
			for _, d := range batch {
				messages = append(messages, queue.JobMessage{JobID: d.ID, WorkID: d.ID, OrgID: d.OrgID, RootID: d.RootID, FileID: d.FileID, VersionID: d.VersionID, ExtractionID: d.ExtractionID, Stage: d.Stage})
			}
			if err = q.Enqueue(ctx, stage, messages...); err != nil {
				return published, err
			}
			for _, d := range batch {
				if err = ledger.MarkFileWorkEnqueued(ctx, d.ID); err != nil {
					return published, err
				}
				published++
			}
		}
	}
	return published, nil
}
