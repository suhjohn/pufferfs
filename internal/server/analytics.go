package server

import (
	"context"
	"strings"

	productanalytics "github.com/pufferfs/pufferfs/internal/analytics"
	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func (s *Server) captureBackendEvent(ctx context.Context, id *auth.Identity, name string, props map[string]any) {
	if s == nil || s.analytics == nil || id == nil || id.UserID == "" || name == "" {
		return
	}
	s.analytics.Capture(ctx, productanalytics.Event{
		DistinctID: id.UserID,
		Name:       name,
		Properties: backendEventProperties(id, props),
	})
}

func (s *Server) captureOrgBackendEvent(ctx context.Context, orgID, userID, name string, props map[string]any) {
	if s == nil || s.analytics == nil || orgID == "" || name == "" {
		return
	}
	distinctID := userID
	if distinctID == "" {
		distinctID = "org:" + orgID
	}
	properties := map[string]any{
		"event_source": "backend",
		"org_id":       orgID,
		"$groups":      map[string]string{"organization": orgID},
	}
	if userID != "" {
		properties["user_id"] = userID
	}
	for key, value := range props {
		if value != nil {
			properties[key] = value
		}
	}
	s.analytics.Capture(ctx, productanalytics.Event{
		DistinctID: distinctID,
		Name:       name,
		Properties: properties,
	})
}

func backendEventProperties(id *auth.Identity, props map[string]any) map[string]any {
	properties := map[string]any{
		"event_source": "backend",
		"org_id":       id.OrgID,
		"user_id":      id.UserID,
		"role":         string(id.Role),
		"$groups":      map[string]string{"organization": id.OrgID},
	}
	for key, value := range props {
		if value != nil {
			properties[key] = value
		}
	}
	return properties
}

func rootScopeProperty(root *models.RootMetadata) string {
	if root == nil || root.Scope == "" {
		return models.RootScopeOrg
	}
	return root.Scope
}

func emailDomain(email string) string {
	_, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok {
		return ""
	}
	return domain
}

func hasScope(scopes []string, target string) bool {
	for _, scope := range scopes {
		if scope == target || scope == "*" {
			return true
		}
	}
	return false
}

func syncAnalyticsProps(root *models.RootMetadata, req *models.SyncRequest, job *models.SyncJob, extra map[string]any) map[string]any {
	props := map[string]any{
		"root_scope": rootScopeProperty(root),
	}
	if job != nil && job.ID != "" {
		props["sync_job_id"] = job.ID
	}
	if req != nil {
		props["change_count"] = len(req.Changes)
		props["change_ref_count"] = len(req.ChangeRefs)
		props["has_manifest_ref"] = req.ManifestRef != ""
		props["has_state_ref"] = req.StateRef != ""
		props["has_base_generation"] = req.BaseGenerationID != "" || req.BaseGenerationSeq > 0
		if req.ChangeCount > 0 {
			props["reported_change_count"] = req.ChangeCount
		}
		if req.ProtocolVersion > 0 {
			props["sync_protocol_version"] = req.ProtocolVersion
		}
	}
	for key, value := range extra {
		if value != nil {
			props[key] = value
		}
	}
	return props
}

func (s *Server) captureSyncStarted(ctx context.Context, id *auth.Identity, root *models.RootMetadata, req *models.SyncRequest, job *models.SyncJob, totalFiles int) {
	props := syncAnalyticsProps(root, req, job, map[string]any{
		"total_files": totalFiles,
	})
	s.captureBackendEvent(ctx, id, "sync_started", props)
}

func (s *Server) captureSyncCompleted(ctx context.Context, orgID, userID string, root *models.RootMetadata, req *models.SyncRequest, job *models.SyncJob, resp *models.SyncResponse) {
	extra := map[string]any{"status": "completed"}
	if resp != nil {
		extra["files_processed"] = resp.FilesProcessed
		extra["chunks_added"] = resp.ChunksAdded
		extra["chunks_removed"] = resp.ChunksRemoved
		extra["chunks_moved"] = resp.ChunksMoved
	}
	s.captureOrgBackendEvent(ctx, orgID, userID, "sync_completed", syncAnalyticsProps(root, req, job, extra))
}

func (s *Server) captureSyncFailed(ctx context.Context, orgID, userID string, root *models.RootMetadata, req *models.SyncRequest, job *models.SyncJob, failureStage string) {
	s.captureOrgBackendEvent(ctx, orgID, userID, "sync_failed", syncAnalyticsProps(root, req, job, map[string]any{
		"status":        "failed",
		"failure_stage": failureStage,
	}))
}
