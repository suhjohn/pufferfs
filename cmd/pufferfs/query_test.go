package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestWriteQueryResultsUsesSimpleKeyValueOutput(t *testing.T) {
	longContent := strings.Repeat("x", 600)
	resp := models.QueryResponse{
		Results: []models.QueryResult{{
			FilePath:   "notes.md",
			ChunkIndex: 2,
			Content:    longContent,
			Score:      0.812345,
			FileType:   "markdown",
		}},
	}

	var out bytes.Buffer
	writeQueryResults(&out, resp)
	got := out.String()

	if strings.Contains(got, "---") {
		t.Fatalf("output contains decorative separator: %q", got)
	}
	if !strings.Contains(got, "1.\n") {
		t.Fatalf("output does not contain numbered result: %q", got)
	}
	if !strings.Contains(got, "score: 0.8123") {
		t.Fatalf("output does not contain formatted score: %q", got)
	}
	if !strings.Contains(got, longContent) {
		t.Fatalf("output truncated content: %q", got)
	}
}

func TestWriteQueryResultsFlattensJSONContent(t *testing.T) {
	resp := models.QueryResponse{
		Results: []models.QueryResult{{
			FilePath:   "data.json",
			ChunkIndex: 0,
			Content:    `{"user":{"name":"Ada","roles":["admin","editor"]},"active":true}`,
			Score:      1,
			FileType:   "json",
		}},
	}

	var out bytes.Buffer
	writeQueryResults(&out, resp)
	got := out.String()

	for _, want := range []string{
		"content.active: true",
		"content.user.name: Ada",
		"content.user.roles[0]: admin",
		"content.user.roles[1]: editor",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestWriteQueryResultsShowsRootForMultiRootResponse(t *testing.T) {
	resp := models.QueryResponse{
		RootsSearched: 2,
		Results: []models.QueryResult{{
			RootID:     "root-1",
			RootName:   "contracts",
			FilePath:   "data.json",
			ChunkIndex: 0,
			Content:    "renewal terms",
			Score:      1,
			FileType:   "json",
		}},
	}

	var out bytes.Buffer
	writeQueryResults(&out, resp)
	got := out.String()

	if !strings.Contains(got, "root: contracts") {
		t.Fatalf("output missing root label:\n%s", got)
	}
}

func TestWriteQueryResultsHidesRootForSingleRootResponse(t *testing.T) {
	resp := models.QueryResponse{
		RootsSearched: 1,
		Results: []models.QueryResult{{
			RootID:     "root-1",
			RootName:   "contracts",
			FilePath:   "data.json",
			ChunkIndex: 0,
			Content:    "renewal terms",
			Score:      1,
			FileType:   "json",
		}},
	}

	var out bytes.Buffer
	writeQueryResults(&out, resp)
	got := out.String()

	if strings.Contains(got, "root:") {
		t.Fatalf("single-root output should not include root label:\n%s", got)
	}
}

func TestRunQuerySendsMultipleRootIDs(t *testing.T) {
	requests := make(chan models.QueryRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests <- req
		_ = json.NewEncoder(w).Encode(models.QueryResponse{Results: []models.QueryResult{}, Query: req.Query, Mode: req.Mode})
	}))
	defer server.Close()

	cfg := &appconfig.Config{Server: appconfig.ServerConfig{URL: server.URL}}
	err := runQuery(cfg, "renewal terms", "hybrid", "", []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
	}, false, 10, true)
	if err != nil {
		t.Fatalf("runQuery: %v", err)
	}

	req := <-requests
	if req.RootID != "" || req.AllRoots {
		t.Fatalf("unexpected single/all selector: %#v", req)
	}
	if len(req.RootIDs) != 2 || req.RootIDs[0] != "11111111-1111-1111-1111-111111111111" || req.RootIDs[1] != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("root_ids = %#v", req.RootIDs)
	}
}

func TestRunQuerySendsAllRoots(t *testing.T) {
	requests := make(chan models.QueryRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests <- req
		_ = json.NewEncoder(w).Encode(models.QueryResponse{Results: []models.QueryResult{}, Query: req.Query, Mode: req.Mode})
	}))
	defer server.Close()

	cfg := &appconfig.Config{Server: appconfig.ServerConfig{URL: server.URL}}
	if err := runQuery(cfg, "renewal terms", "hybrid", "", nil, true, 10, true); err != nil {
		t.Fatalf("runQuery: %v", err)
	}

	req := <-requests
	if !req.AllRoots || req.RootID != "" || len(req.RootIDs) != 0 {
		t.Fatalf("unexpected selector: %#v", req)
	}
}
