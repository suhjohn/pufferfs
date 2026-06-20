package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestRunIgnoreGetEffective(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ignore-policy" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing auth header")
		}
		_ = json.NewEncoder(w).Encode(models.EffectiveIgnorePolicy{
			OrgPatterns:  "org-private/\n",
			UserPatterns: "user-private/\n",
		})
	}))
	defer server.Close()

	cfg := &appconfig.Config{Server: appconfig.ServerConfig{URL: server.URL, APIKey: "test-key"}}
	var out bytes.Buffer
	if err := runIgnoreGet(cfg, "effective", false, &out); err != nil {
		t.Fatalf("runIgnoreGet: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "org-private/") || !strings.Contains(got, "user-private/") {
		t.Fatalf("output missing policies:\n%s", got)
	}
}

func TestRunIgnoreSetUser(t *testing.T) {
	var gotReq models.IgnorePolicyUpdateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/ignore-policy/user" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(models.IgnorePolicy{
			OrgID:     "org-1",
			UserID:    "user-1",
			Patterns:  gotReq.Patterns,
			UpdatedAt: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
		})
	}))
	defer server.Close()

	cfg := &appconfig.Config{Server: appconfig.ServerConfig{URL: server.URL, APIKey: "test-key"}}
	var out bytes.Buffer
	if err := runIgnoreSet(cfg, "user", "-", strings.NewReader("scratch/\n"), &out); err != nil {
		t.Fatalf("runIgnoreSet: %v", err)
	}
	if gotReq.Patterns != "scratch/\n" {
		t.Fatalf("patterns = %q", gotReq.Patterns)
	}
	if !strings.Contains(out.String(), "Updated user ignore policy") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestNormalizeIgnoreLevelRejectsEffectiveForSet(t *testing.T) {
	if _, err := normalizeIgnoreLevel("effective", false); err == nil {
		t.Fatal("normalizeIgnoreLevel accepted effective for writable policy")
	}
}
