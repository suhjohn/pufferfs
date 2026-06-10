package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCapturePayload(t *testing.T) {
	ts := time.Date(2026, 6, 10, 5, 0, 0, 0, time.UTC)
	payload := CapturePayload("phc_test", Event{
		DistinctID: "user-1",
		Name:       "query_submitted",
		Timestamp:  ts,
		Properties: map[string]any{
			"event_source": "backend",
			"org_id":       "org-1",
			"empty":        nil,
		},
	})

	if payload["api_key"] != "phc_test" || payload["event"] != "query_submitted" || payload["distinct_id"] != "user-1" {
		t.Fatalf("payload = %#v", payload)
	}
	props := payload["properties"].(map[string]any)
	if props["event_source"] != "backend" || props["org_id"] != "org-1" {
		t.Fatalf("properties = %#v", props)
	}
	if _, ok := props["empty"]; ok {
		t.Fatalf("nil property was not removed: %#v", props)
	}
	if payload["timestamp"] != ts.Format(time.RFC3339Nano) {
		t.Fatalf("timestamp = %v", payload["timestamp"])
	}
}

func TestDisabledClientIsNoop(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer server.Close()

	client := New(Config{Enabled: false, ProjectKey: "phc_test", Host: server.URL})
	client.Capture(context.Background(), Event{DistinctID: "user-1", Name: "event"})

	if calls != 0 {
		t.Fatalf("disabled client sent %d requests", calls)
	}
}

func TestClientSendsCaptureRequest(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/i/v0/e/" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		requests <- payload
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(Config{Enabled: true, ProjectKey: "phc_test", Host: server.URL, HTTPClient: server.Client()})
	client.Capture(context.Background(), Event{
		DistinctID: "user-1",
		Name:       "root_created",
		Properties: map[string]any{"event_source": "backend"},
	})

	select {
	case payload := <-requests:
		if payload["api_key"] != "phc_test" || payload["event"] != "root_created" || payload["distinct_id"] != "user-1" {
			t.Fatalf("payload = %#v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture request")
	}
}
