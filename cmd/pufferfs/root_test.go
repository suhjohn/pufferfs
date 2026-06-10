package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestWriteRootList(t *testing.T) {
	roots := []models.RootMetadata{
		{
			ID:                   "root-1",
			Name:                 "workspace",
			SourcePath:           "/tmp/workspace",
			Scope:                models.RootScopeOrg,
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
