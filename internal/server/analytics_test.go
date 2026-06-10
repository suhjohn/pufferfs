package server

import (
	"context"
	"testing"

	productanalytics "github.com/pufferfs/pufferfs/internal/analytics"
	"github.com/pufferfs/pufferfs/internal/auth"
)

type recordingAnalytics struct {
	events []productanalytics.Event
}

func (r *recordingAnalytics) Capture(_ context.Context, event productanalytics.Event) {
	r.events = append(r.events, event)
}

func TestCaptureBackendEventAddsSharedProperties(t *testing.T) {
	recorder := &recordingAnalytics{}
	s := &Server{}
	s.SetAnalytics(recorder)

	s.captureBackendEvent(context.Background(), &auth.Identity{
		UserID: "user-1",
		OrgID:  "org-1",
		Role:   auth.RoleAdmin,
	}, "query_submitted", map[string]any{"root_scope": "org", "nil": nil})

	if len(recorder.events) != 1 {
		t.Fatalf("events = %d", len(recorder.events))
	}
	event := recorder.events[0]
	if event.DistinctID != "user-1" || event.Name != "query_submitted" {
		t.Fatalf("event = %#v", event)
	}
	if event.Properties["event_source"] != "backend" ||
		event.Properties["org_id"] != "org-1" ||
		event.Properties["user_id"] != "user-1" ||
		event.Properties["role"] != "admin" ||
		event.Properties["root_scope"] != "org" {
		t.Fatalf("properties = %#v", event.Properties)
	}
	if _, ok := event.Properties["nil"]; ok {
		t.Fatalf("nil property was not removed: %#v", event.Properties)
	}
	groups, ok := event.Properties["$groups"].(map[string]string)
	if !ok || groups["organization"] != "org-1" {
		t.Fatalf("groups = %#v", event.Properties["$groups"])
	}
}

func TestCaptureOrgBackendEventUsesOrgDistinctIDFallback(t *testing.T) {
	recorder := &recordingAnalytics{}
	s := &Server{}
	s.SetAnalytics(recorder)

	s.captureOrgBackendEvent(context.Background(), "org-1", "", "billing_subscription_updated", nil)

	if len(recorder.events) != 1 {
		t.Fatalf("events = %d", len(recorder.events))
	}
	event := recorder.events[0]
	if event.DistinctID != "org:org-1" {
		t.Fatalf("distinct_id = %q", event.DistinctID)
	}
	if event.Properties["org_id"] != "org-1" || event.Properties["event_source"] != "backend" {
		t.Fatalf("properties = %#v", event.Properties)
	}
}
