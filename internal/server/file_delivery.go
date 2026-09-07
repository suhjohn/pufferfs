package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/pufferfs/pufferfs/internal/queue"
)

// publishFileWork hands off only work registered by this capture request. A
// crash between SQS acceptance and marking delivery causes duplicate delivery,
// never a missing job. The registration result supplies committed identifiers;
// only scheduled reconciliation needs to scan for undelivered work.
func (s *Server) publishFileWork(ctx context.Context, deliveries []queue.JobMessage) error {
	if s.queue == nil {
		return fmt.Errorf("SQS is required for file delivery")
	}
	byStage := make(map[string][]queue.JobMessage)
	for _, d := range deliveries {
		if d.WorkID == "" || d.FileID == "" || d.VersionID == "" || (d.Stage != queue.StageTransform && d.Stage != queue.StageIndex) {
			return fmt.Errorf("invalid file delivery %q", d.WorkID)
		}
		byStage[d.Stage] = append(byStage[d.Stage], d)
	}
	var confirmed []string
	var sendErr error
send:
	for _, stage := range []string{queue.StageTransform, queue.StageIndex} {
		pending := byStage[stage]
		for start := 0; start < len(pending); start += 10 {
			batch := pending[start:min(start+10, len(pending))]
			if sendErr = s.queue.Enqueue(ctx, stage, batch...); sendErr != nil {
				break send
			}
			for _, d := range batch {
				confirmed = append(confirmed, d.WorkID)
			}
		}
	}
	if len(confirmed) > 0 {
		sendErr = errors.Join(sendErr, s.db.MarkFileWorkEnqueued(ctx, confirmed))
	}
	return sendErr
}
