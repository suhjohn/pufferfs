package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
)

func TestRunInitWritesHostedCLIConfigOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := runInit(initOptions{
		ServerURL: "https://api.pufferfs.com",
		APIKey:    "pfs_test",
	})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".tpfs", "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)

	for _, want := range []string{
		"[server]",
		`url = "https://api.pufferfs.com"`,
		`api_key = "pfs_test"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"[turbopuffer]",
		"[storage]",
		"gcp-us-central1",
		`bucket = "pufferfs"`,
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("config contains %q:\n%s", unwanted, got)
		}
	}
}

func TestRunInitManualWritesMinimalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := runInit(initOptions{
		ServerURL: "https://api.pufferfs.com",
		Manual:    true,
	})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cfg, err := appconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Server.URL != "https://api.pufferfs.com" {
		t.Fatalf("server URL = %q", cfg.Server.URL)
	}
	if cfg.Server.APIKey != "" {
		t.Fatalf("server API key = %q, want empty", cfg.Server.APIKey)
	}

	data, err := os.ReadFile(appconfig.ConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "[turbopuffer]") || strings.Contains(got, "[storage]") {
		t.Fatalf("manual config includes advanced sections:\n%s", got)
	}
}
