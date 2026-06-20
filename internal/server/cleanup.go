package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	case "0", "false", "no", "off":
		return false
	default:
		return true
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
	keys = keepGenerationManifestRefs(keys, msg.GenerationID)
	return cleanupDeletableKeys(keys)
}

func keepGenerationManifestRefs(keys []string, generationID string) []string {
	if generationID == "" {
		return keys
	}
	prefix := fmt.Sprintf("syncs/%s/manifests/", generationID)
	out := keys[:0]
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, key)
	}
	return out
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
		if req.ContentProofRef != "" {
			keys = append(keys, req.ContentProofRef)
		}
		keys = append(keys, req.ChangeRefs...)
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

func cleanupGenerationKeysWithChangeRefSources(ctx context.Context, s3 objectStore, req *models.SyncRequest, msg queue.JobMessage) ([]string, error) {
	keys := cleanupGenerationKeys(req, msg)
	if req == nil || s3 == nil || len(req.ChangeRefs) == 0 {
		return keys, nil
	}
	for _, ref := range req.ChangeRefs {
		if ref == "" {
			continue
		}
		sourceKeys, err := cleanupSourceKeysFromChangeRef(ctx, s3, msg.RootID, ref)
		if err != nil {
			if isObjectNotFound(err) {
				continue
			}
			return nil, err
		}
		keys = append(keys, sourceKeys...)
	}
	return cleanupDeletableKeys(keys), nil
}

func cleanupSourceKeysFromChangeRef(ctx context.Context, s3 objectStore, rootID, ref string) ([]string, error) {
	data, err := s3.Download(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("downloading cleanup change ref %s: %w", ref, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var keys []string
	for {
		var change models.FileChange
		if err := dec.Decode(&change); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parsing cleanup change ref %s: %w", ref, err)
		}
		if change.Status != models.StatusAdded && change.Status != models.StatusModified {
			continue
		}
		if change.SourceKey != "" {
			keys = append(keys, change.SourceKey)
			continue
		}
		if change.Path != "" {
			keys = append(keys, fmt.Sprintf("files/%s/%s", rootID, change.Path))
		}
	}
	return cleanupDeletableKeys(keys), nil
}

func (s *Server) cleanupTerminalSyncObjects(ctx context.Context, rootID, generationID string, req *models.SyncRequest) error {
	if s == nil || s.s3 == nil || generationID == "" || !cleanupSyncArtifactsEnabled() {
		return nil
	}
	msg := queue.JobMessage{RootID: rootID, GenerationID: generationID}
	keys, err := cleanupGenerationKeysWithChangeRefSources(ctx, s.s3, req, msg)
	if err != nil {
		return err
	}

	prefix := syncGenerationPrefix(generationID)
	legacyKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			continue
		}
		legacyKeys = append(legacyKeys, key)
	}

	if _, err := s.s3.DeletePrefix(ctx, prefix); err != nil {
		return fmt.Errorf("deleting sync generation prefix %s: %w", prefix, err)
	}
	if len(legacyKeys) > 0 {
		if err := s.s3.DeleteMany(ctx, cleanupDeletableKeys(legacyKeys)); err != nil {
			return fmt.Errorf("deleting legacy sync source objects: %w", err)
		}
	}
	return nil
}

func (s *Server) syncRequestForCleanup(ctx context.Context, generationID string) *models.SyncRequest {
	if s == nil || s.s3 == nil || generationID == "" {
		return nil
	}
	data, err := s.s3.Download(ctx, syncRequestKey(generationID))
	if err != nil {
		return nil
	}
	var req models.SyncRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil
	}
	return &req
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

func (s *Server) cleanupFailedGenerationRows(ctx context.Context, orgID, rootID, generationID string) error {
	if s == nil || s.tp == nil || s.db == nil || orgID == "" || rootID == "" || generationID == "" {
		return nil
	}
	namespaces, err := s.db.ListRootIndexNamespaces(ctx, orgID, rootID)
	if err != nil {
		return fmt.Errorf("listing root index namespaces for failed generation cleanup: %w", err)
	}
	activeNamespaces := activeRootIndexNamespaces(namespaces)
	closeFilter := []any{"valid_to_generation", "Eq", generationID}
	reopenPatch := map[string]any{
		"valid_to_generation":     "",
		"valid_to_generation_seq": 0,
	}
	for _, ns := range activeNamespaces {
		for pass := 0; pass < 100; pass++ {
			rowsRemaining, _, err := s.tp.PatchByFilter(ns.Namespace, closeFilter, reopenPatch, true)
			if err != nil {
				return fmt.Errorf("reopening rows closed by failed generation %s in %s: %w", generationID, ns.Namespace, err)
			}
			if !rowsRemaining {
				break
			}
			if pass == 99 {
				return fmt.Errorf("reopening rows closed by failed generation %s in %s: rows remain after repeated patch passes", generationID, ns.Namespace)
			}
		}

		orphanFilter := []any{"generation_id", "Eq", generationID}
		for pass := 0; pass < 100; pass++ {
			rowsRemaining, err := s.tp.DeleteByFilter(ns.Namespace, orphanFilter, true)
			if err != nil {
				return fmt.Errorf("deleting failed generation rows %s in %s: %w", generationID, ns.Namespace, err)
			}
			if !rowsRemaining {
				break
			}
			if pass == 99 {
				return fmt.Errorf("deleting failed generation rows %s in %s: rows remain after repeated delete passes", generationID, ns.Namespace)
			}
		}
	}
	return nil
}

func (s *Server) cleanupFailedGenerationRowsForRoot(ctx context.Context, orgID, rootID string) error {
	if s == nil || s.db == nil || s.tp == nil {
		return nil
	}
	generations, err := s.db.ListFailedSyncGenerations(ctx, orgID, rootID, 100)
	if err != nil {
		return err
	}
	for _, generation := range generations {
		if err := s.cleanupFailedGenerationRows(ctx, generation.OrgID, generation.RootID, generation.ID); err != nil {
			return err
		}
	}
	return nil
}
