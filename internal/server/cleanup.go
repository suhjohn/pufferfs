package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func isObjectNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "nosuchkey") || strings.Contains(msg, "not found") || strings.Contains(msg, "status code: 404")
}

func cleanupSyncArtifactsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PUFFERFS_CLEANUP_SYNC_ARTIFACTS"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
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

func (s *Server) cleanupTerminalSyncObjects(ctx context.Context, rootID, generationID string, req *models.SyncRequest, deleteState bool) error {
	if s == nil || s.s3 == nil || generationID == "" || !cleanupSyncArtifactsEnabled() {
		return nil
	}
	prefix := syncGenerationPrefix(generationID)
	var legacyKeys []string
	addLegacy := func(key string) {
		if !strings.HasPrefix(key, prefix) {
			legacyKeys = append(legacyKeys, key)
		}
	}
	addSource := func(change models.FileChange) {
		if change.Status != models.StatusAdded && change.Status != models.StatusModified {
			return
		}
		key := change.SourceKey
		if key == "" && change.Path != "" {
			key = fmt.Sprintf("files/%s/%s", rootID, change.Path)
		}
		addLegacy(key)
	}
	if req != nil {
		addLegacy(req.ContentProofRef)
		for _, ref := range req.ChangeRefs {
			addLegacy(ref)
		}
		for _, change := range req.Changes {
			addSource(change)
		}
		for _, ref := range req.ChangeRefs {
			if ref == "" {
				continue
			}
			if err := eachJSONL(ctx, s.s3, ref, func(change models.FileChange) error {
				addSource(change)
				return nil
			}); err != nil && !isObjectNotFound(err) {
				return fmt.Errorf("reading cleanup change ref %s: %w", ref, err)
			}
		}
	}

	legacyKeys = cleanupDeletableKeys(legacyKeys)
	if deleteState {
		legacyKeys = append(legacyKeys, stateObjectKey(rootID, generationID))
	}
	if len(legacyKeys) > 0 {
		if err := s.s3.DeleteMany(ctx, legacyKeys); err != nil {
			return fmt.Errorf("deleting legacy sync source objects: %w", err)
		}
	}
	if _, err := s.s3.DeletePrefix(ctx, prefix); err != nil {
		return fmt.Errorf("deleting sync generation prefix %s: %w", prefix, err)
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

func (s *Server) cleanupFailedGeneration(ctx context.Context, orgID, rootID, generationID string, req *models.SyncRequest) (err error) {
	if s == nil || s.db == nil || orgID == "" || rootID == "" || generationID == "" {
		return nil
	}
	status, err := s.db.GetSyncGenerationStatus(ctx, generationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("checking failed generation %s: %w", generationID, err)
	}
	if status != "failed" && status != "cleaning" {
		return nil
	}
	if err := s.db.MarkSyncGenerationCleaning(ctx, generationID); err != nil {
		return fmt.Errorf("claiming failed generation %s cleanup: %w", generationID, err)
	}
	defer func() {
		if err == nil {
			return
		}
		if markErr := s.db.MarkSyncGenerationCleanupPending(ctx, generationID); markErr != nil {
			err = errors.Join(err, fmt.Errorf("preserving failed generation %s for cleanup retry: %w", generationID, markErr))
		}
	}()
	if req == nil {
		req = s.syncRequestForCleanup(ctx, generationID)
	}
	rowErr := func() error {
		namespaces, err := s.db.ListRootIndexNamespaces(ctx, orgID, rootID)
		if err != nil {
			return fmt.Errorf("listing root index namespaces for failed generation cleanup: %w", err)
		}
		activeNamespaces := activeRootIndexNamespaces(namespaces)
		if len(activeNamespaces) > 0 && s.tp == nil {
			return fmt.Errorf("cleaning failed generation %s: index client is unavailable", generationID)
		}
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
	}()
	if err := errors.Join(rowErr, s.cleanupTerminalSyncObjects(ctx, rootID, generationID, req, true)); err != nil {
		return err
	}
	return s.db.MarkSyncGenerationCleaned(ctx, generationID)
}

func (s *Server) cleanupFailedGenerations(ctx context.Context, orgID, rootID string) error {
	ids, err := s.db.ListFailedSyncGenerationIDs(ctx, orgID, rootID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.cleanupFailedGeneration(ctx, orgID, rootID, id, s.syncRequestForCleanup(ctx, id)); err != nil {
			return err
		}
	}
	return nil
}
