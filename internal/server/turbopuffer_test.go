package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpsertRowsIncludesDistanceMetricWhenProvided(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/namespaces/ns-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := body["distance_metric"]; got != "cosine_distance" {
			t.Fatalf("distance_metric = %#v, want cosine_distance", got)
		}
		if body["upsert_rows"] == nil {
			t.Fatalf("missing upsert_rows in %#v", body)
		}
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewTPClientWithURL("test-key", server.URL)
	if err := client.UpsertRows("ns-1", []map[string]any{{"id": "row-1", "vector": []float32{1, 0}}}, "cosine_distance"); err != nil {
		t.Fatalf("upsert rows: %v", err)
	}
}

func TestUpsertRowsOmitsDistanceMetricWhenEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/namespaces/ns-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if _, ok := body["distance_metric"]; ok {
			t.Fatalf("distance_metric should be omitted for no-vector upserts: %#v", body["distance_metric"])
		}
		if body["upsert_rows"] == nil {
			t.Fatalf("missing upsert_rows in %#v", body)
		}
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewTPClientWithURL("test-key", server.URL)
	if err := client.UpsertRows("ns-1", []map[string]any{{"id": "row-1", "content": "hello"}}, ""); err != nil {
		t.Fatalf("upsert rows without vectors: %v", err)
	}
}

func TestPatchByFilterParsesAffectedRows(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/namespaces/ns-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["patch_by_filter"] == nil {
			t.Fatalf("missing patch_by_filter in %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rows_remaining": false,
			"rows_affected":  42,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewTPClientWithURL("test-key", server.URL)
	remaining, affected, err := client.PatchByFilter("ns-1", []any{"file_path", "Eq", "a.txt"}, map[string]any{"x": 1}, true)
	if err != nil {
		t.Fatalf("patch by filter: %v", err)
	}
	if remaining {
		t.Fatal("remaining = true")
	}
	if affected != 42 {
		t.Fatalf("affected = %d, want 42", affected)
	}
}

func TestDeleteNamespace(t *testing.T) {
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/v2/namespaces/ns-1", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewTPClientWithURL("test-key", server.URL)
	if err := client.DeleteNamespace("ns-1"); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestDeleteNamespaceTreatsNotFoundAsDeleted(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/namespaces/ns-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewTPClientWithURL("test-key", server.URL)
	if err := client.DeleteNamespace("ns-1"); err != nil {
		t.Fatalf("delete missing namespace: %v", err)
	}
}
