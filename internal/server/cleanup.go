package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/pkg/models"
)

const syncStageCleanup = queue.StageCleanup

func cleanupSyncArtifactsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PUFFERFS_CLEANUP_SYNC_ARTIFACTS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func cleanupDeleteBatchSize() int {
	const defaultBatchSize = 1000
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_CLEANUP_BATCH_SIZE"))
	if raw == "" {
		return defaultBatchSize
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 1 {
		return defaultBatchSize
	}
	if size > 1000 {
		return 1000
	}
	return size
}

func enqueueCleanupBatches(ctx context.Context, q queue.Queue, base queue.JobMessage, keys []string) error {
	if !cleanupSyncArtifactsEnabled() {
		return nil
	}
	keys = cleanupDeletableKeys(keys)
	if len(keys) == 0 {
		return nil
	}
	batchSize := cleanupDeleteBatchSize()
	msgs := make([]queue.JobMessage, 0, (len(keys)+batchSize-1)/batchSize)
	for start := 0; start < len(keys); start += batchSize {
		end := start + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		msgs = append(msgs, queue.JobMessage{
			JobID:             fmt.Sprintf("cleanup-%s-%s", base.GenerationID, uuid.NewString()),
			SyncJobID:         base.SyncJobID,
			UserID:            base.UserID,
			OrgID:             base.OrgID,
			RootID:            base.RootID,
			GenerationID:      base.GenerationID,
			GenerationSeq:     base.GenerationSeq,
			BaseGenerationID:  base.BaseGenerationID,
			BaseGenerationSeq: base.BaseGenerationSeq,
			Stage:             syncStageCleanup,
			CleanupKeys:       append([]string(nil), keys[start:end]...),
			ShardIndex:        base.ShardIndex,
			TotalShards:       base.TotalShards,
			Priority:          base.Priority,
		})
	}
	return q.Enqueue(ctx, syncStageCleanup, msgs...)
}

func cleanupDeletableKeys(keys []string) []string {
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(strings.ReplaceAll(key, "\\", "/"))
		if key == "" || seen[key] || !cleanupDeletableKey(key) {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func cleanupDeletableKey(key string) bool {
	return strings.HasPrefix(key, "syncs/") ||
		strings.HasPrefix(key, "files/") ||
		strings.HasPrefix(key, "bundles/")
}

func cleanupShardKeys(msg queue.JobMessage) []string {
	keys := append([]string(nil), msg.CleanupKeys...)
	if msg.PayloadRef != "" {
		keys = append(keys, msg.PayloadRef)
	}
	return cleanupDeletableKeys(keys)
}

func cleanupGenerationKeys(req *models.SyncRequest, msg queue.JobMessage) []string {
	keys := []string{syncRequestKey(msg.GenerationID)}
	for i := 0; i < msg.TotalShards; i++ {
		keys = append(keys, syncShardDoneKey(msg.GenerationID, i))
	}
	if req != nil {
		if req.ManifestRef != "" {
			keys = append(keys, req.ManifestRef)
		}
		for _, change := range req.Changes {
			if change.Status != models.StatusAdded && change.Status != models.StatusModified {
				continue
			}
			if change.SourceKey != "" {
				keys = append(keys, change.SourceKey)
				continue
			}
			keys = append(keys, fmt.Sprintf("files/%s/%s", msg.RootID, change.Path))
		}
	}
	return cleanupDeletableKeys(keys)
}

func (d *SyncDispatcher) processCleanup(ctx context.Context, msg queue.JobMessage) error {
	keys := cleanupDeletableKeys(msg.CleanupKeys)
	if len(keys) == 0 {
		return nil
	}
	batchSize := cleanupDeleteBatchSize()
	for start := 0; start < len(keys); start += batchSize {
		end := start + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		if err := d.server.s3.DeleteMany(ctx, keys[start:end]); err != nil {
			return fmt.Errorf("deleting cleanup batch: %w", err)
		}
		log.Printf("cleanup deleted %d sync artifact objects generation_id=%s", end-start, msg.GenerationID)
	}
	return nil
}
