package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestWriteRootList(t *testing.T) {
	roots := []models.RootMetadata{
		{
			ID:                   "root-1",
			Name:                 "workspace",
			SourcePath:           "/tmp/workspace",
			Scope:                models.RootScopeOrg,
			Access:               []string{"read", "sync"},
			VisibleGenerationID:  "gen-1",
			VisibleGenerationSeq: 3,
		},
	}

	var out bytes.Buffer
	writeRootList(&out, roots)
	got := out.String()

	for _, want := range []string{
		"NAME",
		"workspace",
		"root-1",
		"org",
		"read,sync",
		"gen-1/3",
		"/tmp/workspace",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunRootCurrentDetectsCwdRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sourcePath := filepath.Join(home, "workspace")
	nestedPath := filepath.Join(sourcePath, "src")
	if err := saveRootMeta("root-1", "workspace", sourcePath, "gen-1", 3); err != nil {
		t.Fatalf("saveRootMeta: %v", err)
	}
	if err := os.MkdirAll(nestedPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(nestedPath)

	rootID, err := detectRootFromCwd()
	if err != nil {
		t.Fatalf("detectRootFromCwd: %v", err)
	}
	if rootID != "root-1" {
		t.Fatalf("rootID = %q, want root-1", rootID)
	}
}

func TestRootDeleteCommandAllowsCurrentRoot(t *testing.T) {
	cmd := rootDeleteCmd()
	if err := cmd.Args(cmd, nil); err != nil {
		t.Fatalf("root delete without args rejected: %v", err)
	}
	if err := cmd.Args(cmd, []string{"workspace"}); err != nil {
		t.Fatalf("root delete with one arg rejected: %v", err)
	}
	if err := cmd.Args(cmd, []string{"workspace", "extra"}); err == nil {
		t.Fatal("root delete with two args unexpectedly accepted")
	}
}

func TestRunRootDeleteDefaultsToCurrentRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sourcePath := filepath.Join(home, "workspace")
	nestedPath := filepath.Join(sourcePath, "src")
	if err := saveRootMeta("root-1", "workspace", sourcePath, "gen-1", 3); err != nil {
		t.Fatalf("saveRootMeta: %v", err)
	}
	if err := os.MkdirAll(nestedPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(nestedPath)

	var deletedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/roots/root-1":
			writeTestJSON(t, w, models.RootMetadata{
				ID:         "root-1",
				Name:       "workspace",
				SourcePath: sourcePath,
				Scope:      models.RootScopeOrg,
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/roots/root-1":
			deletedPath = r.URL.Path
			writeTestJSON(t, w, deleteRootResponse{
				Status:           "deleted",
				RootID:           "root-1",
				Name:             "workspace",
				TurbopufferNS:    "ns-root-1",
				S3ObjectsDeleted: 2,
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := &appconfig.Config{Server: appconfig.ServerConfig{URL: server.URL}}
	if err := runRootDelete(cfg, "", true); err != nil {
		t.Fatalf("runRootDelete: %v", err)
	}
	if deletedPath != "/roots/root-1" {
		t.Fatalf("deleted path = %q", deletedPath)
	}
	if _, err := os.Stat(appconfig.RootDir("root-1")); !os.IsNotExist(err) {
		t.Fatalf("local root cache still exists or unexpected stat error: %v", err)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write JSON: %v", err)
	}
}
