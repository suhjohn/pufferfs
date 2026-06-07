//go:build cli_integration
// +build cli_integration

package tests

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLargeTextCorpusManifestSession(t *testing.T) {
	rawCount := os.Getenv("PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT")
	if rawCount == "" {
		t.Skip("PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT not set")
	}
	fileCount, err := strconv.Atoi(rawCount)
	if err != nil || fileCount < 1 {
		t.Fatalf("invalid PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT=%q", rawCount)
	}

	services := requireRealServices(t)
	setupE2EInfra(t)

	env := newE2EEnv(t, services, "")
	homeDir := t.TempDir()
	initPufferFS(t, env, homeDir)

	projectDir := filepath.Join(homeDir, "large-text-workspace")
	for i := 0; i < fileCount; i++ {
		ext := ".txt"
		if i%2 == 1 {
			ext = ".md"
		}
		rel := filepath.Join(fmt.Sprintf("batch-%04d", i/1000), fmt.Sprintf("doc-%07d%s", i, ext))
		writeFile(t, projectDir, rel, fmt.Sprintf("large text-only manifest corpus file %d\nunique-token-%07d\n", i, i))
	}

	start := time.Now()
	stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
	t.Logf("large text-only sync files=%d elapsed=%s", fileCount, time.Since(start))
	if err != nil {
		t.Fatalf("large text-only sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	requireOutputContains(t, stdout, "Sync complete")

	rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
	namespaces := rootIndexNamespaces(t, rootID)
	cleanupDone := false
	t.Cleanup(func() {
		if !cleanupDone {
			adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
			deleteTPNamespaces(t, services, namespaces)
		}
	})

	lastExt := ".txt"
	if (fileCount-1)%2 == 1 {
		lastExt = ".md"
	}
	lastPath := filepath.ToSlash(filepath.Join(fmt.Sprintf("batch-%04d", (fileCount-1)/1000), fmt.Sprintf("doc-%07d%s", fileCount-1, lastExt)))
	assertCLIQuery(t, homeDir, env, fmt.Sprintf("unique-token-%07d", fileCount-1), env.rootName, "hybrid", "", lastPath)

	deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
	cleanupDone = true
}
