package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
)

func TestAdminRootGrantCreateCommand(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             "grant-1",
			"org_id":         "org-1",
			"root_id":        "root-1",
			"principal_type": "user",
			"principal_id":   "user-1",
			"permissions":    []string{"read", "sync"},
		})
	}))
	defer server.Close()

	withAdminTestConfig(t, server.URL, "user-key")
	t.Setenv("PUFFERFS_ADMIN_API_KEY", "admin-key")

	cmd := adminRootGrantCreateCmd(&adminOptions{})
	cmd.SetArgs([]string{
		"--org", "org-1",
		"--root", "root-1",
		"--principal-type", "user",
		"--principal-id", "user-1",
		"--permission", "read",
		"--permission", "sync",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/admin/orgs/org-1/roots/root-1/grants" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotAuth != "Bearer admin-key" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBody["principal_type"] != "user" || gotBody["principal_id"] != "user-1" {
		t.Fatalf("body principal = %#v", gotBody)
	}
	perms, ok := gotBody["permissions"].([]any)
	if !ok || len(perms) != 2 || perms[0] != "read" || perms[1] != "sync" {
		t.Fatalf("permissions = %#v", gotBody["permissions"])
	}
}

func TestAdminGroupMemberRemoveCommand(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	}))
	defer server.Close()

	withAdminTestConfig(t, server.URL, "")
	options := &adminOptions{AdminKey: "flag-admin-key"}
	cmd := adminGroupMemberRemoveCmd(options)
	cmd.SetArgs([]string{"--org", "org-1", "--group", "group-1", "--user", "user-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Fatalf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/admin/orgs/org-1/groups/group-1/members/user-1" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotAuth != "Bearer flag-admin-key" {
		t.Fatalf("auth = %q", gotAuth)
	}
}

func TestAdminCreateGroupRequiresName(t *testing.T) {
	withAdminTestConfig(t, "https://example.invalid", "admin-key")
	cmd := adminGroupCreateCmd(&adminOptions{})
	cmd.SetArgs([]string{"--org", "org-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--name is required") {
		t.Fatalf("err = %v, want missing name", err)
	}
}

func withAdminTestConfig(t *testing.T, serverURL, apiKey string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PUFFERFS_SERVER_URL", "")
	t.Setenv("PUFFERFS_API_KEY", "")
	cfg := &appconfig.Config{}
	cfg.Server.URL = serverURL
	cfg.Server.APIKey = apiKey
	if err := os.MkdirAll(filepath.Dir(appconfig.ConfigPath()), 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := appconfig.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}
