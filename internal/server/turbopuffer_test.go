package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
