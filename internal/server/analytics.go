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
