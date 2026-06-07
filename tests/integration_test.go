//go:build cli_integration
// +build cli_integration

package tests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/pufferfs/pufferfs/pkg/models"
)

const (
	e2eDBUser = "pufferfs_e2e_test"
	e2eDBPass = "testpass"
	e2eDBName = "pufferfs_e2e_test"
	e2eDBPort = "25432"

	e2eMinioPort   = "29000"
	e2eMinioUser   = "minioadmin"
	e2eMinioPass   = "minioadmin"
	e2eMinioBucket = "pufferfs-e2e-test"

	e2eJWTSecret         = "e2e-integration-test-jwt-secret!"
	e2eAdminKey          = "pfs_e2e_platform_admin_key"
	e2eTPNamespaceShards = "2"

	llmWikiURL = "https://gist.githubusercontent.com/karpathy/442a6bf555914893e9891c11519de94f/raw/ac46de1ad27f92b28ac95459c782c07f6b8c964a/llm-wiki.md"
)

var (
	e2eSetupOnce      sync.Once
	e2eCLIBinPath     string
	e2eServerBinPath  string
	e2eWorkerBinPath  string
	e2ePgContainer    = "pufferfs-e2e-test-pg"
	e2eMinioContainer = "pufferfs-e2e-test-minio"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = exec.Command("docker", "rm", "-f", e2ePgContainer).Run()
	_ = exec.Command("docker", "rm", "-f", e2eMinioContainer).Run()
	os.Exit(code)
}

func TestPufferFSEndToEnd(t *testing.T) {
	services := requireRealServices(t)
	setupE2EInfra(t)

	t.Run("admin provisioning creates member-scoped tenant access", func(t *testing.T) {
		suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
		srv := startServer(t, services, "")
		serverURL := fmt.Sprintf("http://%s", srv.addr)
		cleanupDone := false

		org := adminProvisionOrg(t, serverURL, map[string]any{
			"name":        "Integration Tenant " + suffix,
			"slug":        "integration-tenant-" + suffix,
			"external_id": "tenant-" + suffix,
		})
		t.Cleanup(func() {
			if !cleanupDone {
				adminDelete(t, serverURL, "/admin/orgs/"+url.PathEscape(org.ID))
			}
		})

		adminUser := adminProvisionUser(t, serverURL, map[string]any{
			"email":       "tenant-admin-" + suffix + "@example.com",
			"name":        "Tenant Admin",
			"external_id": "tenant-admin-" + suffix,
		})
		memberA := adminProvisionUser(t, serverURL, map[string]any{
			"email":       "member-a-" + suffix + "@example.com",
			"name":        "Member A",
			"external_id": "member-a-" + suffix,
		})
		memberB := adminProvisionUser(t, serverURL, map[string]any{
			"email":       "member-b-" + suffix + "@example.com",
			"name":        "Member B",
			"external_id": "member-b-" + suffix,
		})

		adminUpsertMember(t, serverURL, org.ID, adminUser.ID, "admin")
		adminUpsertMember(t, serverURL, org.ID, memberA.ID, "viewer")
		adminUpsertMember(t, serverURL, org.ID, memberB.ID, "viewer")

		adminMemberKey := adminCreateMemberAPIKey(t, serverURL, org.ID, adminUser.ID, []string{"query", "root:delete"})
		adminKeyWriteKey := adminCreateMemberAPIKey(t, serverURL, org.ID, adminUser.ID, []string{"api_keys:write", "api_keys:read"})
		memberAKey := adminCreateMemberAPIKey(t, serverURL, org.ID, memberA.ID, []string{"query"})
		memberASyncKey := adminCreateMemberAPIKey(t, serverURL, org.ID, memberA.ID, []string{"query", "sync"})
		memberBKey := adminCreateMemberAPIKey(t, serverURL, org.ID, memberB.ID, []string{"query"})

		assertSelfServiceAPIKeyScopes(t, serverURL, adminKeyWriteKey)

		orgRoot := adminCreateRoot(t, serverURL, org.ID, map[string]any{
			"name":        "shared-root-" + suffix,
			"source_path": "/tenant/shared",
			"scope":       "org",
		})
		memberARoot := adminCreateRoot(t, serverURL, org.ID, map[string]any{
			"name":          "member-a-root-" + suffix,
			"source_path":   "/tenant/member-a",
			"scope":         "user",
			"owner_user_id": memberA.ID,
		})
		memberBRoot := adminCreateRoot(t, serverURL, org.ID, map[string]any{
			"name":          "member-b-root-" + suffix,
			"source_path":   "/tenant/member-b",
			"scope":         "user",
			"owner_user_id": memberB.ID,
		})

		assertVisibleRoots(t, serverURL, memberAKey,
			[]string{orgRoot.Name, memberARoot.Name},
			[]string{memberBRoot.Name},
		)
		assertVisibleRoots(t, serverURL, memberBKey,
			[]string{orgRoot.Name, memberBRoot.Name},
			[]string{memberARoot.Name},
		)
		assertVisibleRoots(t, serverURL, adminMemberKey,
			[]string{orgRoot.Name, memberARoot.Name, memberBRoot.Name},
			nil,
		)
		assertRootStatus(t, serverURL, memberAKey, memberBRoot.ID, http.StatusNotFound)
		assertRootStatus(t, serverURL, adminMemberKey, memberBRoot.ID, http.StatusOK)

		var memberCreatedRoot models.RootMetadata
		status, body := jsonRequest(t, http.MethodPost, serverURL+"/roots", memberASyncKey, map[string]any{
			"name":        "member-a-created-" + suffix,
			"source_path": "/tenant/member-a-created",
			"scope":       "user",
		}, &memberCreatedRoot)
		if status != http.StatusCreated {
			t.Fatalf("member user-root create: HTTP %d: %s", status, string(body))
		}
		if memberCreatedRoot.Scope != "user" || memberCreatedRoot.OwnerUserID != memberA.ID {
			t.Fatalf("unexpected member-created root: %#v", memberCreatedRoot)
		}
		status, body = jsonRequest(t, http.MethodPost, serverURL+"/roots", memberASyncKey, map[string]any{
			"name":        "member-a-org-root-" + suffix,
			"source_path": "/tenant/member-a-org",
			"scope":       "org",
		}, nil)
		if status != http.StatusForbidden {
			t.Fatalf("member org-root create: HTTP %d, want 403: %s", status, string(body))
		}

		homeDir := t.TempDir()
		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, memberAKey, "root", "delete", orgRoot.ID, "--yes")
		if err == nil {
			t.Fatalf("query-only member key unexpectedly deleted root\nstdout: %s\nstderr: %s", stdout, stderr)
		}
		requireOutputContains(t, stdout+stderr, "root delete scope required")

		projectDir := filepath.Join(homeDir, "member-workspace")
		writeFile(t, projectDir, "notes.txt", "query-only member keys cannot create or sync roots\n")
		stdout, stderr, err = runPufferfs(t, homeDir, serverURL, memberAKey, "sync", projectDir, "--name", "member-created-"+suffix, "--scope", "user")
		if err == nil {
			t.Fatalf("query-only member key unexpectedly synced root\nstdout: %s\nstderr: %s", stdout, stderr)
		}
		requireOutputContains(t, stdout+stderr, "sync scope required")

		stdout, stderr, err = runPufferfs(t, t.TempDir(), serverURL, adminMemberKey, "root", "delete", memberBRoot.ID, "--yes")
		if err != nil {
			t.Fatalf("admin member key failed to delete user root: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Deleted root")
		assertVisibleRoots(t, serverURL, memberBKey,
			[]string{orgRoot.Name},
			[]string{memberBRoot.Name},
		)

		deleteCreatedDataAndAssertGone(t, serverURL, org.ID,
			[]string{adminUser.ID, memberA.ID, memberB.ID},
			[]string{orgRoot.ID, memberARoot.ID, memberBRoot.ID, memberCreatedRoot.ID},
		)
		cleanupDone = true
	})

	t.Run("cli sync query modify move remove and watch with nested project", func(t *testing.T) {
		env := newE2EEnv(t, services, "")
		homeDir := t.TempDir()
		initPufferFS(t, env, homeDir)

		projectDir := filepath.Join(homeDir, "workspace")
		fixtures := createNestedProject(t, projectDir)

		stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("initial sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Sync complete")

		rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
		namespaces := rootIndexNamespaces(t, rootID)
		assertRootIndexNamespaceCount(t, namespaces, 2)
		cleanupDone := false
		t.Cleanup(func() {
			if !cleanupDone {
				adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
				deleteTPNamespaces(t, services, namespaces)
			}
		})
		assertMinioHasPrefix(t, fmt.Sprintf("bundles/%s/", rootID))
		assertMinioHasPrefix(t, "syncs/")

		assertNestedRowsIndexed(t, services, namespaces, fixtures)
		assertPDFPageRows(t, services, namespaces, fixtures.pdfs)
		assertOfficeDocumentPageRows(t, services, namespaces, fixtures.officeDocs)
		assertLargeMarkdownRows(t, services, namespaces, fixtures.largeMarkdownPath, 2)
		assertCLIQuery(t, homeDir, env, "roadmap retention metrics", env.rootName, "hybrid", "", "docs/product/strategy/roadmap.md")
		assertCLIQuery(t, homeDir, env, "rollback procedure", env.rootName, "fts", "", "ops/runbooks/deploy/rollback.txt")
		if fixtures.largeMarkdownPath != "" {
			assertCLIQuery(t, homeDir, env, "persistent compounding artifact wiki", env.rootName, "hybrid", "", fixtures.largeMarkdownPath)
		}
		if hasIndexedPDF(fixtures.pdfs) {
			assertCLIQuery(t, homeDir, env, "PDF example", env.rootName, "vector", "", "")
		}

		modifyPath := fixtures.modifyPath
		stableBefore := make(map[string]rowDigest)
		for _, relPath := range fixtures.stablePaths {
			stableBefore[relPath] = rowsDigest(queryTPRowsForPath(t, services, namespaces, relPath))
		}
		modifyBefore := rowsDigest(queryTPRowsForPath(t, services, namespaces, modifyPath))

		appendFile(t, projectDir, modifyPath, "\n\nRuntime update: add enterprise audit exports and tighten retention controls.\n")
		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("modify sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Merkle diff found 1 changed files")
		requireOutputContains(t, stdout, "Syncing 1 changes")

		modifyAfter := rowsDigest(queryTPRowsForPath(t, services, namespaces, modifyPath))
		if modifyBefore.equal(modifyAfter) {
			t.Fatalf("modified %s kept identical indexed row digest: %#v", modifyPath, modifyAfter)
		}
		for relPath, before := range stableBefore {
			after := rowsDigest(queryTPRowsForPath(t, services, namespaces, relPath))
			if !before.equal(after) {
				t.Fatalf("unchanged %s was reindexed: before %#v after %#v", relPath, before, after)
			}
		}

		oldMovePath := fixtures.movePath
		newMovePath := fixtures.moveTargetPath
		mkdirAll(t, filepath.Join(projectDir, filepath.Dir(newMovePath)))
		if err := os.Rename(filepath.Join(projectDir, filepath.FromSlash(oldMovePath)), filepath.Join(projectDir, filepath.FromSlash(newMovePath))); err != nil {
			t.Fatalf("moving nested markdown file: %v", err)
		}
		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("move sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Sync complete")
		assertNoTPRows(t, services, namespaces, oldMovePath)
		assertClosedTPRows(t, services, namespaces, oldMovePath)
		assertHasTPRows(t, services, namespaces, newMovePath)

		removedPath := fixtures.removePath
		if err := os.Remove(filepath.Join(projectDir, filepath.FromSlash(removedPath))); err != nil {
			t.Fatalf("removing nested txt file: %v", err)
		}
		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("remove sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Sync complete")
		assertNoTPRows(t, services, namespaces, removedPath)
		assertClosedTPRows(t, services, namespaces, removedPath)

		watchPath := fixtures.watchPath
		beforeWatch := rowsDigest(queryTPRowsForPath(t, services, namespaces, watchPath))
		watch := startPufferfsWatch(t, homeDir, env, projectDir, env.rootName, 300*time.Millisecond)
		watch.waitForOutput(t, "Following", 30*time.Second)
		appendFile(t, projectDir, watchPath, "\n3. Verify invoice webhooks and search indexing.\n")
		waitForRowDigestChange(t, services, namespaces, watchPath, beforeWatch, 90*time.Second)
		watch.stop(t)

		deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
		cleanupDone = true
	})

	t.Run("failed indexing does not save local state and clean retry indexes incrementally from scratch", func(t *testing.T) {
		env := newE2EEnv(t, services, "definitely-invalid-turbopuffer-key")
		homeDir := t.TempDir()
		initPufferFS(t, env, homeDir)

		projectDir := filepath.Join(homeDir, "interrupted-workspace")
		writeFile(t, projectDir, "docs/deep/retry/incident.md", "# Incident Retry\n\nThis file must survive a failed indexing attempt.\n")
		writeFile(t, projectDir, "docs/deep/retry/evidence.txt", "transient Turbopuffer failure should not mark sync complete\n")

		stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err == nil {
			t.Fatalf("sync with invalid Turbopuffer key unexpectedly succeeded\nstdout: %s\nstderr: %s", stdout, stderr)
		}
		if strings.Contains(stdout, "Sync complete") {
			t.Fatalf("failed sync printed completion: %s", stdout)
		}

		rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
		rootStateDir := filepath.Join(homeDir, ".tpfs", "roots", rootID)
		assertFileMissing(t, filepath.Join(rootStateDir, "tree.json"))
		assertFileMissing(t, filepath.Join(rootStateDir, "state.json"))
		assertFileMissing(t, filepath.Join(rootStateDir, "meta.json"))
		env.stop(t)

		env = newE2EEnvWithIdentity(t, services, "", env.orgID, env.userID, env.apiKey, env.rootName)
		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("retry sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Syncing 2 changes")
		requireOutputContains(t, stdout, "Sync complete")
		if _, err := os.Stat(filepath.Join(rootStateDir, "tree.json")); err != nil {
			t.Fatalf("retry did not save local Merkle tree: %v", err)
		}

		namespaces := rootIndexNamespaces(t, rootID)
		assertRootIndexNamespaceCount(t, namespaces, 2)
		cleanupDone := false
		t.Cleanup(func() {
			if !cleanupDone {
				adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
				deleteTPNamespaces(t, services, namespaces)
			}
		})
		assertHasTPRows(t, services, namespaces, "docs/deep/retry/incident.md")
		assertHasTPRows(t, services, namespaces, "docs/deep/retry/evidence.txt")
		assertCLIQuery(t, homeDir, env, "failed indexing retry", env.rootName, "hybrid", "", "docs/deep/retry/incident.md")

		deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
		cleanupDone = true
	})

	t.Run("queued NATS dispatcher sync flow indexes and queries", func(t *testing.T) {
		nats := startE2ENATS(t)
		env := newQueuedE2EEnv(t, services, nats.ClientURL())
		homeDir := t.TempDir()
		initPufferFS(t, env, homeDir)
		workers := startStageWorkers(t, services, nats.ClientURL(), queueStages()...)
		defer stopWorkerProcesses(t, workers)

		projectDir := filepath.Join(homeDir, "queued-workspace")
		writeFile(t, projectDir, "queued/architecture.md", "# Queued Architecture\n\nNATS JetStream dispatchers invoke Modal compute and commit generations.\n")
		writeFile(t, projectDir, "queued/search.txt", "Dispatcher integration test document for semantic retrieval.\n")
		writePDF(t, projectDir, "queued/document.pdf", "Queued PDF document chunking through Modal and JetStream.")

		syncStart := time.Now()
		stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		t.Logf("queued CLI sync command elapsed=%s", time.Since(syncStart))
		if err != nil {
			t.Fatalf("queued sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Sync complete")

		rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
		generationID := visibleGenerationID(t, env.serverURL, env.apiKey, rootID)
		namespaces := rootIndexNamespaces(t, rootID)
		assertRootIndexNamespaceCount(t, namespaces, 2)
		cleanupDone := false
		t.Cleanup(func() {
			if !cleanupDone {
				adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
				deleteTPNamespaces(t, services, namespaces)
			}
		})
		assertMinioHasPrefix(t, fmt.Sprintf("syncs/%s/done/", generationID))
		assertHasTPRows(t, services, namespaces, "queued/architecture.md")
		assertHasTPRows(t, services, namespaces, "queued/document.pdf")
		assertCLIQuery(t, homeDir, env, "JetStream dispatchers Modal compute", env.rootName, "hybrid", "", "queued/architecture.md")

		deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
		cleanupDone = true
	})

	t.Run("queued text-only sync (no Modal chunking)", func(t *testing.T) {
		nats := startE2ENATS(t)
		env := newQueuedE2EEnv(t, services, nats.ClientURL())
		homeDir := t.TempDir()
		initPufferFS(t, env, homeDir)
		workers := startStageWorkers(t, services, nats.ClientURL(), queueStages()...)
		defer stopWorkerProcesses(t, workers)

		projectDir := filepath.Join(homeDir, "text-workspace")
		writeFile(t, projectDir, "docs/readme.md", "# Local Chunking\n\nThis file is chunked entirely in Go without calling Modal.\n")
		writeFile(t, projectDir, "docs/notes.txt", "Plain text file chunked locally by the Go worker.\n")
		writeFile(t, projectDir, "src/main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")

		syncStart := time.Now()
		stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		t.Logf("text-only queued CLI sync command elapsed=%s", time.Since(syncStart))
		if err != nil {
			t.Fatalf("text-only queued sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Sync complete")

		rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
		namespaces := rootIndexNamespaces(t, rootID)
		assertRootIndexNamespaceCount(t, namespaces, 2)
		cleanupDone := false
		t.Cleanup(func() {
			if !cleanupDone {
				adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
				deleteTPNamespaces(t, services, namespaces)
			}
		})
		assertHasTPRows(t, services, namespaces, "docs/readme.md")
		assertCLIQuery(t, homeDir, env, "chunked locally Go worker", env.rootName, "hybrid", "", "docs/readme.md")

		deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
		cleanupDone = true
	})

	t.Run("blocking sync with manifest-session shards remains queryable", func(t *testing.T) {
		t.Setenv("PUFFERFS_UPLOAD_CHANGE_SHARD_MAX_FILES", "1")
		env := newE2EEnv(t, services, "")
		homeDir := t.TempDir()
		initPufferFS(t, env, homeDir)

		projectDir := filepath.Join(homeDir, "sharded-workspace")
		writeFile(t, projectDir, "docs/one.md", "# One\n\nAlpha sharded change reference content.\n")
		writeFile(t, projectDir, "docs/two.md", "# Two\n\nBeta sharded change reference content.\n")
		writeFile(t, projectDir, "docs/three.md", "# Three\n\nGamma sharded change reference content.\n")

		stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("sharded blocking sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Sync complete")
		assertMinioHasPrefix(t, "syncs/")

		rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
		namespaces := rootIndexNamespaces(t, rootID)
		cleanupDone := false
		t.Cleanup(func() {
			if !cleanupDone {
				adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
				deleteTPNamespaces(t, services, namespaces)
			}
		})
		assertCLIQuery(t, homeDir, env, "Gamma sharded change reference", env.rootName, "hybrid", "", "docs/three.md")

		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("sharded no-op sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "No changes detected")

		deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
		cleanupDone = true
	})

	t.Run("optional large text-only manifest-session corpus", func(t *testing.T) {
		rawCount := os.Getenv("PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT")
		if rawCount == "" {
			t.Skip("PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT not set")
		}
		fileCount, err := strconv.Atoi(rawCount)
		if err != nil || fileCount < 1 {
			t.Fatalf("invalid PUFFERFS_E2E_LARGE_TEXT_FILE_COUNT=%q", rawCount)
		}
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
	})

	t.Run("background sync status jobs and wait CLI", func(t *testing.T) {
		env := newE2EEnv(t, services, "")
		homeDir := t.TempDir()
		initPufferFS(t, env, homeDir)

		projectDir := filepath.Join(homeDir, "background-workspace")
		writeFile(t, projectDir, "docs/background.md", "# Background Sync\n\nDetached sync jobs can be inspected before querying committed generations.\n")

		stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName, "--background", "--json")
		if err != nil {
			t.Fatalf("background sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		var started struct {
			Status       string `json:"status"`
			RootID       string `json:"root_id"`
			SyncJobID    string `json:"sync_job_id"`
			GenerationID string `json:"generation_id"`
		}
		if err := json.Unmarshal([]byte(stdout), &started); err != nil {
			t.Fatalf("parsing background sync JSON: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if started.Status != "started" || started.RootID == "" || started.SyncJobID == "" || started.GenerationID == "" {
			t.Fatalf("unexpected background sync result: %#v\nstderr: %s", started, stderr)
		}

		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", "status", "--root", env.rootName, "--job-id", started.SyncJobID, "--json")
		if err != nil {
			t.Fatalf("sync status failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		var status models.SyncJob
		if err := json.Unmarshal([]byte(stdout), &status); err != nil {
			t.Fatalf("parsing sync status JSON: %v\nstdout: %s", err, stdout)
		}
		if status.ID != started.SyncJobID || status.RootID != started.RootID {
			t.Fatalf("unexpected sync status: %#v", status)
		}

		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", "wait", "--root", env.rootName, "--job-id", started.SyncJobID, "--json")
		if err != nil {
			t.Fatalf("sync wait failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		var completed models.SyncJob
		if err := json.Unmarshal([]byte(stdout), &completed); err != nil {
			t.Fatalf("parsing sync wait JSON: %v\nstdout: %s", err, stdout)
		}
		if completed.ID != started.SyncJobID || completed.Status != "completed" {
			t.Fatalf("unexpected completed sync job: %#v", completed)
		}

		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", "jobs", "--root", env.rootName, "--json")
		if err != nil {
			t.Fatalf("sync jobs failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		var jobs []models.SyncJob
		if err := json.Unmarshal([]byte(stdout), &jobs); err != nil {
			t.Fatalf("parsing sync jobs JSON: %v\nstdout: %s", err, stdout)
		}
		foundJob := false
		for _, job := range jobs {
			if job.ID == started.SyncJobID {
				foundJob = true
				break
			}
		}
		if !foundJob {
			t.Fatalf("sync jobs did not include %s: %#v", started.SyncJobID, jobs)
		}

		rootID := started.RootID
		namespaces := rootIndexNamespaces(t, rootID)
		cleanupDone := false
		t.Cleanup(func() {
			if !cleanupDone {
				adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
				deleteTPNamespaces(t, services, namespaces)
			}
		})
		assertHasTPRows(t, services, namespaces, "docs/background.md")
		assertCLIQuery(t, homeDir, env, "detached sync jobs committed generations", env.rootName, "hybrid", "", "docs/background.md")

		deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
		cleanupDone = true
	})

	t.Run("acme-corp R2 source corpus sync and query", func(t *testing.T) {
		runAcmeCorpSync(t, services)
	})
}

type realServices struct {
	modalChunkURL      string
	modalEmbedURL      string
	modalQueryEmbedURL string
	modalChunkShardURL string
	modalEmbedShardURL string
	modalIndexShardURL string
	turbopufferAPIKey  string
	turbopufferAPIURL  string
	storageEnv         []string
}

func requireRealServices(t *testing.T) realServices {
	t.Helper()

	useRealS3 := os.Getenv("PUFFERFS_E2E_USE_REAL_S3") == "1"
	cfg := realServices{
		modalChunkURL:      os.Getenv("MODAL_CHUNK_ENDPOINT"),
		modalEmbedURL:      os.Getenv("MODAL_EMBED_ENDPOINT"),
		modalQueryEmbedURL: os.Getenv("MODAL_QUERY_EMBED_ENDPOINT"),
		turbopufferAPIKey:  os.Getenv("TURBOPUFFER_API_KEY"),
		turbopufferAPIURL:  os.Getenv("TURBOPUFFER_API_URL"),
		storageEnv:         e2eStorageEnv(),
	}
	if useRealS3 {
		cfg.modalChunkShardURL = os.Getenv("MODAL_CHUNK_SHARD_ENDPOINT")
		cfg.modalEmbedShardURL = os.Getenv("MODAL_EMBED_SHARD_ENDPOINT")
		cfg.modalIndexShardURL = os.Getenv("MODAL_INDEX_SHARD_ENDPOINT")
	}

	var missing []string
	if cfg.modalChunkURL == "" {
		missing = append(missing, "MODAL_CHUNK_ENDPOINT")
	}
	if cfg.modalEmbedURL == "" {
		missing = append(missing, "MODAL_EMBED_ENDPOINT")
	}
	if cfg.modalQueryEmbedURL == "" {
		missing = append(missing, "MODAL_QUERY_EMBED_ENDPOINT")
	}
	if cfg.turbopufferAPIKey == "" {
		missing = append(missing, "TURBOPUFFER_API_KEY")
	}
	if useRealS3 {
		for _, name := range []string{"AWS_ENDPOINT_URL", "AWS_BUCKET_NAME", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
			if os.Getenv(name) == "" {
				missing = append(missing, name)
			}
		}
	}
	if len(missing) > 0 {
		t.Skipf("real Modal/Turbopuffer integration requires env vars: %s", strings.Join(missing, ", "))
	}
	return cfg
}

func e2eStorageEnv() []string {
	if os.Getenv("PUFFERFS_E2E_USE_REAL_S3") == "1" {
		return []string{
			"AWS_ENDPOINT_URL=" + os.Getenv("AWS_ENDPOINT_URL"),
			"AWS_BUCKET_NAME=" + os.Getenv("AWS_BUCKET_NAME"),
			"AWS_ACCESS_KEY_ID=" + os.Getenv("AWS_ACCESS_KEY_ID"),
			"AWS_SECRET_ACCESS_KEY=" + os.Getenv("AWS_SECRET_ACCESS_KEY"),
		}
	}
	return []string{
		"AWS_ENDPOINT_URL=" + fmt.Sprintf("http://localhost:%s", e2eMinioPort),
		"AWS_BUCKET_NAME=" + e2eMinioBucket,
		"AWS_ACCESS_KEY_ID=" + e2eMinioUser,
		"AWS_SECRET_ACCESS_KEY=" + e2eMinioPass,
	}
}

func setupE2EInfra(t *testing.T) {
	t.Helper()

	e2eSetupOnce.Do(func() {
		buildBinaries(t)
		startPostgres(t)
		startMinIO(t)
		waitForTCP(t, fmt.Sprintf("localhost:%s", e2eDBPort), 30*time.Second)
		waitForPostgres(t, 30*time.Second)
		waitForTCP(t, fmt.Sprintf("localhost:%s", e2eMinioPort), 30*time.Second)
		createMinioBucket(t)
	})
}

func buildBinaries(t *testing.T) {
	t.Helper()

	repoRoot := repoRoot(t)
	tmpDir := t.TempDir()

	e2eCLIBinPath = filepath.Join(tmpDir, "pufferfs")
	cmd := exec.Command("go", "build", "-o", e2eCLIBinPath, "./cmd/pufferfs")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building CLI: %v\n%s", err, out)
	}

	e2eServerBinPath = filepath.Join(tmpDir, "pufferfs-server")
	cmd = exec.Command("go", "build", "-o", e2eServerBinPath, "./cmd/server")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building server: %v\n%s", err, out)
	}

	e2eWorkerBinPath = filepath.Join(tmpDir, "pufferfs-worker")
	cmd = exec.Command("go", "build", "-o", e2eWorkerBinPath, "./cmd/worker")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building worker: %v\n%s", err, out)
	}
}

func startPostgres(t *testing.T) {
	t.Helper()

	_ = exec.Command("docker", "rm", "-f", e2ePgContainer).Run()
	cmd := exec.Command("docker", "run", "-d",
		"--name", e2ePgContainer,
		"-e", "POSTGRES_USER="+e2eDBUser,
		"-e", "POSTGRES_PASSWORD="+e2eDBPass,
		"-e", "POSTGRES_DB="+e2eDBName,
		"-p", e2eDBPort+":5432",
		"postgres:16-alpine",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("starting postgres: %v\n%s", err, out)
	}
}

func startMinIO(t *testing.T) {
	t.Helper()

	_ = exec.Command("docker", "rm", "-f", e2eMinioContainer).Run()
	cmd := exec.Command("docker", "run", "-d",
		"--name", e2eMinioContainer,
		"-e", "MINIO_ROOT_USER="+e2eMinioUser,
		"-e", "MINIO_ROOT_PASSWORD="+e2eMinioPass,
		"-p", e2eMinioPort+":9000",
		"minio/minio", "server", "/data",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("starting minio: %v\n%s", err, out)
	}
}

func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", addr)
}

func waitForPostgres(t *testing.T, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", "exec", e2ePgContainer, "pg_isready", "-U", e2eDBUser)
		if err := cmd.Run(); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("postgres not ready after %v", timeout)
}

func createMinioBucket(t *testing.T) {
	t.Helper()
	if os.Getenv("PUFFERFS_E2E_USE_REAL_S3") == "1" {
		return
	}

	endpoint := fmt.Sprintf("http://localhost:%s", e2eMinioPort)
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider(e2eMinioUser, e2eMinioPass, "")),
	)
	if err != nil {
		t.Fatalf("loading AWS config: %v", err)
	}
	s3Client := s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	_, err = s3Client.CreateBucket(context.Background(), &s3sdk.CreateBucketInput{
		Bucket: aws.String(e2eMinioBucket),
	})
	if err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
		t.Logf("create bucket warning: %v", err)
	}
}

func newMinioClient(t *testing.T) *s3sdk.Client {
	t.Helper()

	endpoint := fmt.Sprintf("http://localhost:%s", e2eMinioPort)
	accessKey := e2eMinioUser
	secretKey := e2eMinioPass
	if os.Getenv("PUFFERFS_E2E_USE_REAL_S3") == "1" {
		endpoint = os.Getenv("AWS_ENDPOINT_URL")
		accessKey = os.Getenv("AWS_ACCESS_KEY_ID")
		secretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		t.Fatalf("loading AWS config: %v", err)
	}
	return s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

func assertMinioHasPrefix(t *testing.T, prefix string) {
	t.Helper()

	client := newMinioClient(t)
	bucket := e2eMinioBucket
	if os.Getenv("PUFFERFS_E2E_USE_REAL_S3") == "1" {
		bucket = os.Getenv("AWS_BUCKET_NAME")
	}
	resp, err := client.ListObjectsV2(context.Background(), &s3sdk.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("listing MinIO prefix %s: %v", prefix, err)
	}
	if len(resp.Contents) == 0 {
		t.Fatalf("expected at least one object with prefix %s", prefix)
	}
}

func assertStoragePrefixEmpty(t *testing.T, prefix string) {
	t.Helper()

	client := newMinioClient(t)
	bucket := e2eMinioBucket
	if os.Getenv("PUFFERFS_E2E_USE_REAL_S3") == "1" {
		bucket = os.Getenv("AWS_BUCKET_NAME")
	}
	resp, err := client.ListObjectsV2(context.Background(), &s3sdk.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("listing storage prefix %s: %v", prefix, err)
	}
	if len(resp.Contents) != 0 {
		t.Fatalf("expected no objects with prefix %s after delete", prefix)
	}
}

type e2eEnv struct {
	server    *serverProcess
	serverURL string
	apiKey    string
	orgID     string
	userID    string
	rootName  string
}

func newE2EEnv(t *testing.T, services realServices, turbopufferKeyOverride string) *e2eEnv {
	t.Helper()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	orgID := "e2e-org-" + suffix
	userID := "e2e-user-" + suffix
	apiKey := "pfs_e2e_" + suffix
	rootName := "workspace-" + suffix
	return newE2EEnvWithIdentity(t, services, turbopufferKeyOverride, orgID, userID, apiKey, rootName)
}

func newE2EEnvWithIdentity(t *testing.T, services realServices, turbopufferKeyOverride, orgID, userID, apiKey, rootName string) *e2eEnv {
	t.Helper()

	srv := startServer(t, services, turbopufferKeyOverride)
	createUserAndAPIKey(t, orgID, userID, apiKey)
	return &e2eEnv{
		server:    srv,
		serverURL: fmt.Sprintf("http://%s", srv.addr),
		apiKey:    apiKey,
		orgID:     orgID,
		userID:    userID,
		rootName:  rootName,
	}
}

func newQueuedE2EEnv(t *testing.T, services realServices, natsURL string) *e2eEnv {
	t.Helper()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	orgID := "e2e-org-queued-" + suffix
	userID := "e2e-user-queued-" + suffix
	apiKey := "pfs_e2e_queued_" + suffix
	rootName := "queued-workspace-" + suffix
	srv := startServerWithExtraEnv(t, services, "", []string{"NATS_URL=" + natsURL})
	createUserAndAPIKey(t, orgID, userID, apiKey)
	return &e2eEnv{
		server:    srv,
		serverURL: fmt.Sprintf("http://%s", srv.addr),
		apiKey:    apiKey,
		orgID:     orgID,
		userID:    userID,
		rootName:  rootName,
	}
}

func (e *e2eEnv) stop(t *testing.T) {
	t.Helper()
	e.server.stop(t)
}

type serverProcess struct {
	cmd  *exec.Cmd
	addr string
}

func startServer(t *testing.T, services realServices, turbopufferKeyOverride string) *serverProcess {
	t.Helper()

	return startServerWithExtraEnv(t, services, turbopufferKeyOverride, nil)
}

func startServerWithExtraEnv(t *testing.T, services realServices, turbopufferKeyOverride string, extraEnv []string) *serverProcess {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	tpKey := services.turbopufferAPIKey
	if turbopufferKeyOverride != "" {
		tpKey = turbopufferKeyOverride
	}

	dbURL := fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable", e2eDBUser, e2eDBPass, e2eDBPort, e2eDBName)
	cmd := exec.Command(e2eServerBinPath)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"LISTEN_ADDR="+addr,
		"JWT_SECRET="+e2eJWTSecret,
		"PUFFERFS_ADMIN_KEY="+e2eAdminKey,
		"PUFFERFS_TP_NAMESPACE_SHARDS="+e2eTPNamespaceShards,
		"MIGRATIONS_DIR="+filepath.Join(repoRoot(t), "migrations"),
		"MODAL_CHUNK_ENDPOINT="+services.modalChunkURL,
		"MODAL_EMBED_ENDPOINT="+services.modalEmbedURL,
		"MODAL_QUERY_EMBED_ENDPOINT="+services.modalQueryEmbedURL,
		"TURBOPUFFER_API_KEY="+tpKey,
		"TURBOPUFFER_API_URL="+services.turbopufferAPIURL,
	)
	cmd.Env = append(cmd.Env, services.storageEnv...)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Dir = repoRoot(t)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}

	proc := &serverProcess{cmd: cmd, addr: addr}
	t.Cleanup(func() {
		proc.stop(t)
	})

	waitForTCP(t, addr, 30*time.Second)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return proc
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("server health check did not pass")
	return nil
}

func (p *serverProcess) stop(t *testing.T) {
	t.Helper()
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	p.cmd = nil
}

func startE2ENATS(t *testing.T) *natsserver.Server {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("creating embedded NATS: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded NATS did not become ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	return ns
}

func queueStages() []string {
	return []string{"chunk", "embed", "index", "commit"}
}

func startStageWorkers(t *testing.T, services realServices, natsURL string, stages ...string) []*exec.Cmd {
	t.Helper()
	workers := make([]*exec.Cmd, 0, len(stages))
	for _, stage := range stages {
		cmd := exec.Command(e2eWorkerBinPath, "--stage="+stage, "--concurrency=2")
		cmd.Env = append(os.Environ(),
			"DATABASE_URL="+fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable", e2eDBUser, e2eDBPass, e2eDBPort, e2eDBName),
			"NATS_URL="+natsURL,
			"JWT_SECRET="+e2eJWTSecret,
			"MODAL_CHUNK_ENDPOINT="+services.modalChunkURL,
			"MODAL_EMBED_ENDPOINT="+services.modalEmbedURL,
			"MODAL_QUERY_EMBED_ENDPOINT="+services.modalQueryEmbedURL,
			"MODAL_CHUNK_SHARD_ENDPOINT="+services.modalChunkShardURL,
			"MODAL_EMBED_SHARD_ENDPOINT="+services.modalEmbedShardURL,
			"MODAL_INDEX_SHARD_ENDPOINT="+services.modalIndexShardURL,
			"TURBOPUFFER_API_KEY="+services.turbopufferAPIKey,
			"TURBOPUFFER_API_URL="+services.turbopufferAPIURL,
			"PUFFERFS_TP_NAMESPACE_SHARDS="+e2eTPNamespaceShards,
		)
		cmd.Env = append(cmd.Env, services.storageEnv...)
		cmd.Dir = repoRoot(t)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			stopWorkerProcesses(t, workers)
			t.Fatalf("starting %s worker: %v", stage, err)
		}
		workers = append(workers, cmd)
	}
	return workers
}

func stopWorkerProcesses(t *testing.T, workers []*exec.Cmd) {
	t.Helper()
	for _, cmd := range workers {
		if cmd == nil || cmd.Process == nil {
			continue
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

func adminProvisionOrg(t *testing.T, serverURL string, payload map[string]any) models.Organization {
	t.Helper()
	var org models.Organization
	adminJSON(t, http.MethodPost, serverURL, "/admin/orgs", payload, http.StatusOK, &org)
	return org
}

func adminProvisionUser(t *testing.T, serverURL string, payload map[string]any) models.User {
	t.Helper()
	var user models.User
	adminJSON(t, http.MethodPost, serverURL, "/admin/users", payload, http.StatusOK, &user)
	return user
}

func adminUpsertMember(t *testing.T, serverURL, orgID, userID, role string) {
	t.Helper()
	var member models.OrgMember
	adminJSON(t, http.MethodPut, serverURL, "/admin/orgs/"+url.PathEscape(orgID)+"/members/"+url.PathEscape(userID), map[string]any{
		"role": role,
	}, http.StatusOK, &member)
	if member.UserID != userID || member.Role != role {
		t.Fatalf("unexpected member response: %#v", member)
	}
}

func adminCreateMemberAPIKey(t *testing.T, serverURL, orgID, userID string, scopes []string) string {
	t.Helper()
	var resp struct {
		Key string `json:"key"`
	}
	adminJSON(t, http.MethodPost, serverURL, "/admin/orgs/"+url.PathEscape(orgID)+"/users/"+url.PathEscape(userID)+"/api-keys", map[string]any{
		"name":   "member-key",
		"scopes": scopes,
	}, http.StatusCreated, &resp)
	if resp.Key == "" {
		t.Fatal("admin API key creation returned empty key")
	}
	return resp.Key
}

func adminCreateRoot(t *testing.T, serverURL, orgID string, payload map[string]any) models.RootMetadata {
	t.Helper()
	var root models.RootMetadata
	adminJSON(t, http.MethodPost, serverURL, "/admin/orgs/"+url.PathEscape(orgID)+"/roots", payload, http.StatusCreated, &root)
	if root.ID == "" {
		t.Fatalf("admin root creation returned empty root: %#v", root)
	}
	return root
}

func assertSelfServiceAPIKeyScopes(t *testing.T, serverURL, apiKey string) {
	t.Helper()
	status, body := jsonRequest(t, http.MethodPost, serverURL+"/auth/api-keys", apiKey, map[string]any{
		"name":   "empty-scope-key",
		"scopes": []string{},
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("POST /auth/api-keys with empty scopes: HTTP %d, want %d: %s", status, http.StatusBadRequest, string(body))
	}

	var resp struct {
		Key string `json:"key"`
	}
	status, body = jsonRequest(t, http.MethodPost, serverURL+"/auth/api-keys", apiKey, map[string]any{
		"name":   "query-only-key",
		"scopes": []string{"query"},
	}, &resp)
	if status != http.StatusCreated {
		t.Fatalf("POST /auth/api-keys with explicit scopes: HTTP %d, want %d: %s", status, http.StatusCreated, string(body))
	}
	if resp.Key == "" {
		t.Fatal("POST /auth/api-keys with explicit scopes returned empty key")
	}
}

func adminJSON(t *testing.T, method, serverURL, requestPath string, payload any, expectedStatus int, out any) {
	t.Helper()
	status, body := jsonRequest(t, method, serverURL+requestPath, e2eAdminKey, payload, out)
	if status != expectedStatus {
		t.Fatalf("%s %s: HTTP %d, want %d: %s", method, requestPath, status, expectedStatus, string(body))
	}
}

func adminDelete(t *testing.T, serverURL, requestPath string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, serverURL+requestPath, nil)
	if err != nil {
		t.Logf("cleanup request error: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+e2eAdminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("cleanup delete %s failed: %v", requestPath, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Logf("cleanup delete %s: HTTP %d: %s", requestPath, resp.StatusCode, string(body))
	}
}

func deleteCreatedDataAndAssertGone(t *testing.T, serverURL, orgID string, userIDs, rootIDs []string) {
	t.Helper()

	waitForNoActiveSyncs(t, rootIDs)
	generationIDs := syncGenerationIDsForRoots(t, rootIDs)
	for _, userID := range userIDs {
		status, body := jsonRequest(t, http.MethodDelete, serverURL+"/admin/users/"+url.PathEscape(userID), e2eAdminKey, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("deleting user %s: HTTP %d: %s", userID, status, string(body))
		}
	}
	status, body := jsonRequest(t, http.MethodDelete, serverURL+"/admin/orgs/"+url.PathEscape(orgID), e2eAdminKey, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("deleting org %s: HTTP %d: %s", orgID, status, string(body))
	}

	assertDBCountZero(t, "organization", `SELECT COUNT(*) FROM organizations WHERE id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org roots", `SELECT COUNT(*) FROM roots WHERE org_id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org members", `SELECT COUNT(*) FROM org_members WHERE org_id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org API keys", `SELECT COUNT(*) FROM api_keys WHERE org_id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org ACLs", `SELECT COUNT(*) FROM root_acls WHERE org_id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org sync jobs", `SELECT COUNT(*) FROM sync_jobs WHERE org_id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org sync generations", `SELECT COUNT(*) FROM sync_generations WHERE org_id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org content proofs", `SELECT COUNT(*) FROM content_proofs WHERE org_id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org embedding cache", `SELECT COUNT(*) FROM embedding_cache WHERE org_id = `+sqlQuote(orgID))
	assertDBCountZero(t, "org root index namespaces", `SELECT COUNT(*) FROM root_index_namespaces WHERE org_id = `+sqlQuote(orgID))
	if len(userIDs) > 0 {
		assertDBCountZero(t, "created users", `SELECT COUNT(*) FROM users WHERE id IN `+sqlStringList(userIDs))
	}
	if len(rootIDs) > 0 {
		assertDBCountZero(t, "root states", `SELECT COUNT(*) FROM root_states WHERE root_id IN `+sqlStringList(rootIDs))
	}

	for _, rootID := range rootIDs {
		assertStoragePrefixEmpty(t, fmt.Sprintf("files/%s/", rootID))
		assertStoragePrefixEmpty(t, fmt.Sprintf("bundles/%s/", rootID))
		assertStoragePrefixEmpty(t, fmt.Sprintf("states/%s/", rootID))
		assertStoragePrefixEmpty(t, fmt.Sprintf("chunks/%s/", rootID))
	}
	for _, generationID := range generationIDs {
		assertStoragePrefixEmpty(t, fmt.Sprintf("syncs/%s/", generationID))
	}
}

func waitForNoActiveSyncs(t *testing.T, rootIDs []string) {
	t.Helper()
	if len(rootIDs) == 0 {
		return
	}

	activeSQL := `SELECT (
		(SELECT COUNT(*) FROM sync_generations WHERE root_id IN ` + sqlStringList(rootIDs) + ` AND status = 'building') +
		(SELECT COUNT(*) FROM sync_jobs WHERE root_id IN ` + sqlStringList(rootIDs) + ` AND status NOT IN ('completed', 'failed'))
	)`
	deadline := time.Now().Add(90 * time.Second)
	for {
		raw := strings.TrimSpace(psqlOutput(t, activeSQL))
		active, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("parsing active sync count %q: %v", raw, err)
		}
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			detail := psqlOutput(t, `
				SELECT 'generation', id, root_id, status FROM sync_generations
				 WHERE root_id IN `+sqlStringList(rootIDs)+` AND status = 'building'
				UNION ALL
				SELECT 'job', id, root_id, status FROM sync_jobs
				 WHERE root_id IN `+sqlStringList(rootIDs)+` AND status NOT IN ('completed', 'failed')
				ORDER BY 1, 2`)
			t.Fatalf("timed out waiting for active syncs to finish before delete; active=%d\n%s", active, detail)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func syncGenerationIDsForRoots(t *testing.T, rootIDs []string) []string {
	t.Helper()
	if len(rootIDs) == 0 {
		return nil
	}
	out := psqlOutput(t, `SELECT id FROM sync_generations WHERE root_id IN `+sqlStringList(rootIDs)+` ORDER BY id`)
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

func assertDBCountZero(t *testing.T, label, sql string) {
	t.Helper()
	raw := strings.TrimSpace(psqlOutput(t, sql))
	count, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("parsing %s count %q: %v", label, raw, err)
	}
	if count != 0 {
		t.Fatalf("expected no %s rows after delete, got %d", label, count)
	}
}

func psqlOutput(t *testing.T, sql string) string {
	t.Helper()
	cmd := exec.Command("docker", "exec", "-i", e2ePgContainer, "psql", "-U", e2eDBUser, "-d", e2eDBName, "-At", "-c", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running psql %q: %v\n%s", sql, err, out)
	}
	return string(out)
}

func sqlStringList(values []string) string {
	if len(values) == 0 {
		return "(NULL)"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, sqlQuote(value))
	}
	return "(" + strings.Join(quoted, ",") + ")"
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func assertVisibleRoots(t *testing.T, serverURL, apiKey string, expected, absent []string) {
	t.Helper()
	var roots []models.RootMetadata
	status, body := jsonRequest(t, http.MethodGet, serverURL+"/roots", apiKey, nil, &roots)
	if status != http.StatusOK {
		t.Fatalf("listing roots: HTTP %d: %s", status, string(body))
	}
	names := make(map[string]bool, len(roots))
	for _, root := range roots {
		names[root.Name] = true
	}
	for _, name := range expected {
		if !names[name] {
			t.Fatalf("expected visible root %q in %#v", name, names)
		}
	}
	for _, name := range absent {
		if names[name] {
			t.Fatalf("root %q should not be visible in %#v", name, names)
		}
	}
}

func assertRootStatus(t *testing.T, serverURL, apiKey, rootID string, expectedStatus int) {
	t.Helper()
	status, body := jsonRequest(t, http.MethodGet, serverURL+"/roots/"+url.PathEscape(rootID), apiKey, nil, nil)
	if status != expectedStatus {
		t.Fatalf("GET root %s: HTTP %d, want %d: %s", rootID, status, expectedStatus, string(body))
	}
}

func jsonRequest(t *testing.T, method, requestURL, bearer string, payload any, out any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshaling request payload: %v", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, requestURL, body)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, requestURL, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(respBody, out); err != nil {
			t.Fatalf("decoding response %s: %v", string(respBody), err)
		}
	}
	return resp.StatusCode, respBody
}

func createUserAndAPIKey(t *testing.T, orgID, userID, rawKey string) {
	t.Helper()

	sum := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(sum[:])
	email := userID + "@example.com"
	slug := strings.ReplaceAll(orgID, "_", "-")

	sql := fmt.Sprintf(`
		INSERT INTO users (id, email, name, avatar_url, provider, provider_id, created_at)
		VALUES ('%s', '%s', 'E2E Test', '', 'google', '%s', NOW())
		ON CONFLICT (id) DO NOTHING;

		INSERT INTO organizations (id, name, slug, created_at)
		VALUES ('%s', 'E2E Test Org', '%s', NOW())
		ON CONFLICT (id) DO NOTHING;

		INSERT INTO org_members (org_id, user_id, role, joined_at)
		VALUES ('%s', '%s', 'owner', NOW())
		ON CONFLICT (org_id, user_id) DO NOTHING;

		INSERT INTO api_keys (id, org_id, user_id, name, key_hash, scopes, created_at)
		VALUES ('key-%s', '%s', '%s', 'e2e-key', '%s', '{"read","write"}', NOW())
		ON CONFLICT DO NOTHING;
	`, userID, email, userID, orgID, slug, orgID, userID, userID, orgID, userID, keyHash)

	cmd := exec.Command("docker", "exec", "-i", e2ePgContainer, "psql", "-U", e2eDBUser, "-d", e2eDBName)
	cmd.Stdin = strings.NewReader(sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inserting test user/API key: %v\n%s", err, out)
	}
}

func runPufferfs(t *testing.T, homeDir, serverURL, apiKey string, args ...string) (string, string, error) {
	t.Helper()

	cmd := exec.Command(e2eCLIBinPath, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"PUFFERFS_SERVER_URL="+serverURL,
		"PUFFERFS_API_KEY="+apiKey,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func initPufferFS(t *testing.T, env *e2eEnv, homeDir string) {
	t.Helper()

	stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "init", "--api-key", env.apiKey)
	if err != nil {
		t.Fatalf("init failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	requireOutputContains(t, stdout, "Config written to")
	configPath := filepath.Join(homeDir, ".tpfs", "config.toml")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config not created at %s: %v", configPath, err)
	}
}

type fixtureSet struct {
	textPaths         []string
	mdPaths           []string
	largeMarkdownPath string
	pdfs              []pdfFixture
	officeDocs        []officeDocumentFixture
	modifyPath        string
	watchPath         string
	movePath          string
	moveTargetPath    string
	removePath        string
	stablePaths       []string
}

type pdfFixture struct {
	source         string
	relPath        string
	expectedChunks int
}

type officeDocumentFixture struct {
	relPath        string
	fileType       string
	expectedChunks int
}

func createNestedProject(t *testing.T, projectDir string) fixtureSet {
	t.Helper()

	fixtures := fixtureSet{
		textPaths: []string{
			"docs/finance/bank/notes.txt",
			"ops/runbooks/deploy/rollback.txt",
		},
		mdPaths: []string{
			"README.md",
			"docs/product/strategy/roadmap.md",
			"docs/research/wiki/llm-wiki.md",
			"src/services/billing/reconciliation.md",
		},
		largeMarkdownPath: "docs/research/wiki/llm-wiki.md",
		officeDocs:        []officeDocumentFixture{
			// R2/user-provided DOCX/PPTX fixtures are appended dynamically.
		},
		modifyPath:     "docs/product/strategy/roadmap.md",
		watchPath:      "ops/runbooks/deploy/rollback.txt",
		movePath:       "src/services/billing/reconciliation.md",
		moveTargetPath: "src/services/payments/settlement-reconciliation.md",
		removePath:     "docs/finance/bank/notes.txt",
	}

	writeFile(t, projectDir, "README.md", "# Workspace\n\nThis workspace combines product, finance, research, source, and operations notes.\n")
	writeFile(t, projectDir, "docs/product/strategy/roadmap.md", "# Product Roadmap\n\nQ1: launch usage-based billing.\nQ2: publish KPI dashboard.\n")
	writeFile(t, projectDir, "docs/finance/bank/notes.txt", "Chase bank transaction notes for reconciliation and refund review.\n")
	writeFile(t, projectDir, "src/services/billing/reconciliation.md", "# Billing Reconciliation\n\nMatch bank deposits to invoices and subscription renewals.\n")
	writeFile(t, projectDir, "ops/runbooks/deploy/rollback.txt", "Rollback runbook\n\n1. Freeze deploys.\n2. Restore the previous artifact.\n")
	writeDownloadedTextFixture(t, projectDir, fixtures.largeMarkdownPath, llmWikiURL)

	fixtures.pdfs = copyPDFFixtures(t, projectDir)
	applyR2WorkspaceFixtures(t, projectDir, &fixtures)
	applyDiscoveredWorkspaceFixtures(t, projectDir, &fixtures)
	fixtures.stablePaths = stableFixturePaths(fixtures)
	return fixtures
}

func copyPDFFixtures(t *testing.T, projectDir string) []pdfFixture {
	t.Helper()

	var sources []string
	if list := os.Getenv("PUFFERFS_CLI_PDF_FIXTURES"); list != "" {
		sources = append(sources, filepath.SplitList(list)...)
	}
	if dir := os.Getenv("PUFFERFS_CLI_PDF_FIXTURE_DIR"); dir != "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading PDF fixture dir: %v", err)
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".pdf" {
				continue
			}
			sources = append(sources, filepath.Join(dir, entry.Name()))
		}
	}
	expected := expectedPDFChunksFromEnv(t)

	var copied []pdfFixture
	for _, source := range sources {
		source = strings.TrimSpace(source)
		if source == "" || strings.ToLower(filepath.Ext(source)) != ".pdf" {
			continue
		}
		base := filepath.Base(source)
		relPath := targetPDFPath(base)
		exp, ok := expected[relPath]
		if !ok {
			exp, ok = expected[base]
		}
		if !ok {
			exp, ok = defaultExpectedPDFChunks(base)
		}
		if !ok {
			t.Fatalf("missing expected PDF chunk count for %s; set PUFFERFS_CLI_PDF_EXPECTED_CHUNKS", base)
		}

		dstPath := filepath.Join(projectDir, filepath.FromSlash(relPath))
		mkdirAll(t, filepath.Dir(dstPath))
		src, err := os.Open(source)
		if err != nil {
			t.Fatalf("opening PDF fixture %s: %v", source, err)
		}
		dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			_ = src.Close()
			t.Fatalf("creating PDF fixture copy %s: %v", dstPath, err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			_ = src.Close()
			_ = dst.Close()
			t.Fatalf("copying PDF fixture %s: %v", source, err)
		}
		_ = src.Close()
		if err := dst.Close(); err != nil {
			t.Fatalf("closing PDF fixture copy %s: %v", dstPath, err)
		}
		copied = append(copied, pdfFixture{source: source, relPath: relPath, expectedChunks: exp})
	}
	return copied
}

func expectedPDFChunksFromEnv(t *testing.T) map[string]int {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv("PUFFERFS_CLI_PDF_EXPECTED_CHUNKS"))
	expected := make(map[string]int)
	if raw == "" {
		return expected
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid PUFFERFS_CLI_PDF_EXPECTED_CHUNKS item %q, want path=count", item)
		}
		count, err := strconv.Atoi(parts[1])
		if err != nil || count < 0 {
			t.Fatalf("invalid expected chunk count %q in %q", parts[1], item)
		}
		expected[filepath.ToSlash(parts[0])] = count
	}
	return expected
}

func targetPDFPath(base string) string {
	switch base {
	case "business-model-and-kpis.pdf":
		return "docs/product/business-model-and-kpis.pdf"
	case "Transaction_details_-_chase.com.pdf":
		return "docs/finance/bank/Transaction_details_-_chase.com.pdf"
	case "asdick_et_al_2018-2018-07-08T19_44_34.432Z.pdf":
		return "docs/research/papers/asdick_et_al_2018-2018-07-08T19_44_34.432Z.pdf"
	default:
		return filepath.ToSlash(filepath.Join("docs", "attachments", base))
	}
}

func defaultExpectedPDFChunks(base string) (int, bool) {
	switch base {
	case "business-model-and-kpis.pdf":
		return 4, true
	case "Transaction_details_-_chase.com.pdf":
		return 1, true
	case "asdick_et_al_2018-2018-07-08T19_44_34.432Z.pdf":
		return 0, true
	default:
		return 0, false
	}
}

func writeFile(t *testing.T, root, relPath, content string) {
	t.Helper()

	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	mkdirAll(t, filepath.Dir(fullPath))
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", relPath, err)
	}
}

func writePDF(t *testing.T, root, relPath, text string) {
	t.Helper()

	escaped := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(text)
	stream := fmt.Sprintf("BT /F1 18 Tf 72 720 Td (%s) Tj ET", escaped)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		objectNumber := i + 1
		offsets[objectNumber] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", objectNumber, obj)
	}
	xrefOffset := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xrefOffset)

	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	mkdirAll(t, filepath.Dir(fullPath))
	if err := os.WriteFile(fullPath, pdf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing PDF %s: %v", relPath, err)
	}
}

func appendFile(t *testing.T, root, relPath, content string) {
	t.Helper()

	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	f, err := os.OpenFile(fullPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening %s for append: %v", relPath, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("appending to %s: %v", relPath, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing %s after append: %v", relPath, err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
}

func writeDownloadedTextFixture(t *testing.T, root, relPath, sourceURL string) {
	t.Helper()

	body := downloadFixtureBytes(t, sourceURL)
	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	mkdirAll(t, filepath.Dir(fullPath))
	if err := os.WriteFile(fullPath, body, 0o644); err != nil {
		t.Fatalf("writing downloaded fixture %s: %v", relPath, err)
	}
}

type fixtureSource struct {
	URL            string   `json:"url"`
	Path           string   `json:"path"`
	SHA256         string   `json:"sha256,omitempty"`
	FileType       string   `json:"file_type,omitempty"`
	ExpectedChunks *int     `json:"expected_chunks,omitempty"`
	MinChunks      int      `json:"min_chunks,omitempty"`
	Roles          []string `json:"roles,omitempty"`
	MoveTargetPath string   `json:"move_target_path,omitempty"`
}

func applyR2WorkspaceFixtures(t *testing.T, projectDir string, fixtures *fixtureSet) {
	t.Helper()

	cfg := r2FixtureConfigFromEnv()
	if cfg == nil {
		return
	}

	ctx := context.Background()
	client := newR2S3Client(t, *cfg)
	paginator := s3sdk.NewListObjectsV2Paginator(client, &s3sdk.ListObjectsV2Input{
		Bucket: aws.String(cfg.bucket),
		Prefix: aws.String(cfg.prefix),
	})

	listStart := time.Now()
	var objects []r2FixtureObject
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			t.Fatalf("listing R2 fixtures in s3://%s/%s: %v", cfg.bucket, cfg.prefix, err)
		}
		for _, object := range page.Contents {
			if object.Key == nil || strings.HasSuffix(*object.Key, "/") {
				continue
			}
			var size int64
			if object.Size != nil {
				size = *object.Size
			}
			objects = append(objects, r2FixtureObject{key: *object.Key, size: size})
		}
	}
	t.Logf("timing stage=r2_list bucket=%s prefix=%s objects=%d elapsed=%s", cfg.bucket, cfg.prefix, len(objects), time.Since(listStart))
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].key < objects[j].key
	})
	if len(objects) == 0 {
		t.Fatalf("no R2 fixtures found in s3://%s/%s", cfg.bucket, cfg.prefix)
	}
	objects = selectR2FixtureObjects(objects)
	t.Logf("timing stage=r2_select objects=%d full_corpus=%t", len(objects), strings.EqualFold(strings.TrimSpace(os.Getenv("PUFFERFS_E2E_R2_FULL_CORPUS")), "1"))

	for _, object := range objects {
		relPath := r2FixtureRelPath(t, cfg.prefix, object.key)
		downloadStart := time.Now()
		resp, err := client.GetObject(ctx, &s3sdk.GetObjectInput{
			Bucket: aws.String(cfg.bucket),
			Key:    aws.String(object.key),
		})
		if err != nil {
			t.Fatalf("downloading R2 fixture s3://%s/%s: %v", cfg.bucket, object.key, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading R2 fixture s3://%s/%s: %v", cfg.bucket, object.key, err)
		}
		fullPath := filepath.Join(projectDir, filepath.FromSlash(relPath))
		mkdirAll(t, filepath.Dir(fullPath))
		if err := os.WriteFile(fullPath, body, 0o644); err != nil {
			t.Fatalf("writing R2 fixture %s: %v", relPath, err)
		}
		t.Logf("timing stage=r2_download key=%s bytes=%d elapsed=%s", object.key, len(body), time.Since(downloadStart))
		registerFixtureSource(t, fixtures, r2FixtureSource(cfg, object.key, relPath), relPath)
	}
}

type r2FixtureObject struct {
	key  string
	size int64
}

type r2FixtureConfig struct {
	accountID string
	bucket    string
	prefix    string
	publicURL string
	accessKey string
	secretKey string
}

func r2FixtureConfigFromEnv() *r2FixtureConfig {
	accountID := strings.TrimSpace(os.Getenv("PUFFERFS_E2E_R2_ACCOUNT_ID"))
	bucket := strings.TrimSpace(os.Getenv("PUFFERFS_E2E_R2_BUCKET"))
	prefix := strings.TrimSpace(os.Getenv("PUFFERFS_E2E_R2_PREFIX"))
	accessKey := strings.TrimSpace(os.Getenv("R2_ACCESS_KEY_ID"))
	secretKey := strings.TrimSpace(os.Getenv("R2_SECRET_ACCESS_KEY"))
	if accountID == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil
	}
	if prefix == "" {
		prefix = "workspace/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &r2FixtureConfig{
		accountID: accountID,
		bucket:    bucket,
		prefix:    prefix,
		publicURL: strings.TrimRight(strings.TrimSpace(os.Getenv("PUFFERFS_E2E_R2_PUBLIC_BASE_URL")), "/"),
		accessKey: accessKey,
		secretKey: secretKey,
	}
}

func newR2S3Client(t *testing.T, cfg r2FixtureConfig) *s3sdk.Client {
	t.Helper()

	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.accountID)
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider(cfg.accessKey, cfg.secretKey, "")),
	)
	if err != nil {
		t.Fatalf("loading R2 AWS config: %v", err)
	}
	return s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
}

func r2FixtureRelPath(t *testing.T, prefix, key string) string {
	t.Helper()
	if !strings.HasPrefix(key, prefix) {
		t.Fatalf("R2 fixture key %q is outside prefix %q", key, prefix)
	}
	return cleanFixturePath(t, strings.TrimPrefix(key, prefix))
}

func r2FixtureSource(cfg *r2FixtureConfig, key, relPath string) fixtureSource {
	file := fixtureSource{
		Path:     relPath,
		FileType: fixtureFileType(relPath),
	}
	if cfg.publicURL != "" {
		file.URL = cfg.publicURL + "/" + strings.TrimPrefix(key, cfg.prefix)
	} else {
		file.URL = "s3://" + cfg.bucket + "/" + key
	}
	if file.FileType == "pdf" {
		if expected, ok := defaultExpectedPDFChunks(filepath.Base(relPath)); ok {
			file.ExpectedChunks = &expected
		}
	}
	if file.FileType == "markdown" && strings.EqualFold(filepath.Base(relPath), "llm-wiki.md") {
		file.Roles = append(file.Roles, "large_markdown", "stable")
		file.MinChunks = 2
	}
	return file
}

func selectR2FixtureObjects(objects []r2FixtureObject) []r2FixtureObject {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("PUFFERFS_E2E_R2_FULL_CORPUS")), "1") {
		return objects
	}

	selected := make(map[string]r2FixtureObject)
	bestByType := make(map[string]r2FixtureObject)
	for _, object := range objects {
		rel := object.key
		fileType := fixtureFileType(rel)
		if fileType == "pptx" && object.size > r2SmokeMaxBytes("PUFFERFS_E2E_R2_SMOKE_MAX_PPTX_BYTES", 250_000) {
			continue
		}
		base := strings.ToLower(filepath.Base(rel))
		if base == "readme.md" || base == "llm-wiki.md" {
			selected[object.key] = object
			continue
		}
		if _, ok := bestByType[fileType]; !ok || object.size < bestByType[fileType].size {
			bestByType[fileType] = object
		}
	}
	for _, fileType := range []string{"pdf", "docx", "pptx", "markdown", "text"} {
		if object, ok := bestByType[fileType]; ok {
			selected[object.key] = object
		}
	}

	var subset []r2FixtureObject
	for _, object := range selected {
		subset = append(subset, object)
	}
	sort.Slice(subset, func(i, j int) bool {
		return subset[i].key < subset[j].key
	})
	return subset
}

func r2SmokeMaxBytes(envName string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func applyDiscoveredWorkspaceFixtures(t *testing.T, projectDir string, fixtures *fixtureSet) {
	t.Helper()

	seedURL := strings.TrimSpace(os.Getenv("PUFFERFS_E2E_WORKSPACE_URL"))
	if seedURL == "" {
		return
	}
	seed, err := url.Parse(seedURL)
	if err != nil {
		t.Fatalf("parsing PUFFERFS_E2E_WORKSPACE_URL: %v", err)
	}
	rootPath := path.Dir(seed.Path) + "/"
	seen := make(map[string]bool)
	queue := []*url.URL{seed}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		relPath := discoveredRelPath(t, current, rootPath)
		if seen[relPath] {
			continue
		}
		seen[relPath] = true

		body := downloadFixtureBytes(t, current.String())
		fullPath := filepath.Join(projectDir, filepath.FromSlash(relPath))
		mkdirAll(t, filepath.Dir(fullPath))
		if err := os.WriteFile(fullPath, body, 0o644); err != nil {
			t.Fatalf("writing discovered fixture %s: %v", relPath, err)
		}
		registerFixtureSource(t, fixtures, fixtureSource{
			URL:      current.String(),
			Path:     relPath,
			FileType: fixtureFileType(relPath),
		}, relPath)

		if fixtureFileType(relPath) != "markdown" {
			continue
		}
		for _, href := range discoverWorkspaceLinks(string(body)) {
			parsed, err := url.Parse(href)
			if err != nil {
				t.Fatalf("parsing discovered link %q in %s: %v", href, relPath, err)
			}
			next := current.ResolveReference(parsed)
			if next.Scheme != seed.Scheme || next.Host != seed.Host || !strings.HasPrefix(next.Path, rootPath) {
				continue
			}
			nextRelPath := discoveredRelPath(t, next, rootPath)
			if !seen[nextRelPath] && publicFixtureExists(next.String()) {
				queue = append(queue, next)
			}
		}
	}
}

func publicFixtureExists(sourceURL string) bool {
	req, err := http.NewRequest(http.MethodHead, sourceURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "pufferfs-e2e-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func discoveredRelPath(t *testing.T, u *url.URL, rootPath string) string {
	t.Helper()

	decodedPath, err := url.PathUnescape(u.Path)
	if err != nil {
		t.Fatalf("unescaping discovered URL path %s: %v", u.Path, err)
	}
	decodedRoot, err := url.PathUnescape(rootPath)
	if err != nil {
		t.Fatalf("unescaping workspace root path %s: %v", rootPath, err)
	}
	if !strings.HasPrefix(decodedPath, decodedRoot) {
		t.Fatalf("discovered URL %s is outside workspace root %s", u.String(), decodedRoot)
	}
	return cleanFixturePath(t, strings.TrimPrefix(decodedPath, decodedRoot))
}

func discoverWorkspaceLinks(markdown string) []string {
	seen := make(map[string]bool)
	var links []string
	add := func(raw string) {
		raw = strings.TrimSpace(strings.Trim(raw, "<>"))
		if raw == "" || strings.HasPrefix(raw, "#") || strings.Contains(raw, "://") || strings.HasPrefix(raw, "mailto:") {
			return
		}
		raw = strings.Split(raw, "#")[0]
		raw = strings.Split(raw, "?")[0]
		if raw == "" || strings.HasPrefix(raw, "/") || seen[raw] || fixtureFileType(raw) == "text" && filepath.Ext(raw) == "" {
			return
		}
		seen[raw] = true
		links = append(links, raw)
	}

	for _, match := range markdownLinkRE.FindAllStringSubmatch(markdown, -1) {
		add(match[1])
	}
	for _, line := range strings.Split(markdown, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		line = strings.TrimSpace(strings.Trim(line, "`"))
		if looksLikeFixtureFile(line) {
			add(url.PathEscape(line))
		}
	}
	return links
}

var markdownLinkRE = regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)

func looksLikeFixtureFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".pdf", ".docx", ".doc", ".pptx", ".ppt", ".md", ".txt", ".eml", ".msg", ".vcf", ".ics", ".mp3", ".wav", ".mp4", ".mov":
		return true
	default:
		return false
	}
}

func registerFixtureSource(t *testing.T, fixtures *fixtureSet, file fixtureSource, relPath string) {
	t.Helper()

	fileType := strings.TrimSpace(file.FileType)
	if fileType == "" {
		fileType = fixtureFileType(relPath)
	}
	switch fileType {
	case "pdf":
		expected := -1
		if file.ExpectedChunks != nil {
			expected = *file.ExpectedChunks
		}
		fixtures.pdfs = append(fixtures.pdfs, pdfFixture{source: file.URL, relPath: relPath, expectedChunks: expected})
	case "docx", "pptx":
		expected := -1
		if file.ExpectedChunks != nil {
			expected = *file.ExpectedChunks
		}
		fixtures.officeDocs = append(fixtures.officeDocs, officeDocumentFixture{relPath: relPath, fileType: fileType, expectedChunks: expected})
	case "markdown":
		fixtures.mdPaths = append(fixtures.mdPaths, relPath)
	case "text":
		fixtures.textPaths = append(fixtures.textPaths, relPath)
	default:
		if strings.HasPrefix(fileType, "text") {
			fixtures.textPaths = append(fixtures.textPaths, relPath)
		}
	}

	for _, role := range file.Roles {
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "modify":
			fixtures.modifyPath = relPath
		case "watch":
			fixtures.watchPath = relPath
		case "move":
			fixtures.movePath = relPath
			if file.MoveTargetPath != "" {
				fixtures.moveTargetPath = cleanFixturePath(t, file.MoveTargetPath)
			} else {
				fixtures.moveTargetPath = filepath.ToSlash(filepath.Join("moved", filepath.Base(relPath)))
			}
		case "remove":
			fixtures.removePath = relPath
		case "large_markdown":
			fixtures.largeMarkdownPath = relPath
		case "stable":
			fixtures.stablePaths = append(fixtures.stablePaths, relPath)
		}
	}
	if file.MinChunks > 0 && fileType == "markdown" {
		fixtures.largeMarkdownPath = relPath
	}
}

func cleanFixturePath(t *testing.T, relPath string) string {
	t.Helper()

	relPath = filepath.ToSlash(strings.TrimSpace(relPath))
	if relPath == "" || strings.HasPrefix(relPath, "/") || strings.Contains(relPath, "\x00") {
		t.Fatalf("invalid fixture path %q", relPath)
	}
	cleaned := path.Clean(relPath)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		t.Fatalf("invalid fixture path %q", relPath)
	}
	return cleaned
}

func fixtureFileType(relPath string) string {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".pdf":
		return "pdf"
	case ".docx", ".doc":
		return "docx"
	case ".pptx", ".ppt":
		return "pptx"
	case ".md", ".markdown", ".rst":
		return "markdown"
	case ".txt":
		return "text"
	case ".eml":
		return "eml"
	case ".msg":
		return "msg"
	case ".vcf":
		return "vcf"
	case ".ics":
		return "ics"
	case ".mp3", ".wav":
		return "audio"
	case ".mp4", ".mov":
		return "video"
	default:
		return "text"
	}
}

func downloadFixtureBytes(t *testing.T, sourceURL string) []byte {
	t.Helper()

	if strings.HasPrefix(sourceURL, "file://") {
		body, err := os.ReadFile(strings.TrimPrefix(sourceURL, "file://"))
		if err != nil {
			t.Fatalf("reading local fixture %s: %v", sourceURL, err)
		}
		return body
	}

	req, err := http.NewRequest(http.MethodGet, sourceURL, nil)
	if err != nil {
		t.Fatalf("creating fixture download request for %s: %v", sourceURL, err)
	}
	req.Header.Set("User-Agent", "pufferfs-e2e-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("downloading fixture %s: %v", sourceURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", sourceURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("downloading fixture %s: HTTP %d: %s", sourceURL, resp.StatusCode, string(body))
	}
	return body
}

func stableFixturePaths(fixtures fixtureSet) []string {
	seen := make(map[string]bool)
	var paths []string
	add := func(relPath string) {
		if relPath == "" || seen[relPath] {
			return
		}
		if relPath == fixtures.modifyPath || relPath == fixtures.movePath || relPath == fixtures.removePath || relPath == fixtures.watchPath {
			return
		}
		seen[relPath] = true
		paths = append(paths, relPath)
	}
	for _, relPath := range fixtures.stablePaths {
		add(relPath)
	}
	for _, relPath := range fixtures.textPaths {
		add(relPath)
	}
	for _, relPath := range fixtures.mdPaths {
		add(relPath)
	}
	for _, pdf := range fixtures.pdfs {
		if pdf.expectedChunks != 0 {
			add(pdf.relPath)
		}
	}
	for _, doc := range fixtures.officeDocs {
		if doc.expectedChunks != 0 {
			add(doc.relPath)
		}
	}
	return paths
}

func assertNestedRowsIndexed(t *testing.T, services realServices, namespaces []string, fixtures fixtureSet) {
	t.Helper()

	for _, relPath := range append(append([]string{}, fixtures.textPaths...), fixtures.mdPaths...) {
		assertHasTPRows(t, services, namespaces, relPath)
	}
	for _, pdf := range fixtures.pdfs {
		if pdf.expectedChunks == 0 {
			assertNoTPRows(t, services, namespaces, pdf.relPath)
			continue
		}
		assertHasTPRows(t, services, namespaces, pdf.relPath)
	}
	for _, doc := range fixtures.officeDocs {
		if doc.expectedChunks == 0 {
			assertNoTPRows(t, services, namespaces, doc.relPath)
			continue
		}
		assertHasTPRows(t, services, namespaces, doc.relPath)
	}
}

func assertPDFPageRows(t *testing.T, services realServices, namespaces []string, pdfs []pdfFixture) {
	t.Helper()

	if len(pdfs) == 0 {
		t.Log("PUFFERFS_CLI_PDF_FIXTURES not set; PDF page-count assertions skipped")
		return
	}

	for _, pdf := range pdfs {
		rows := queryTPRowsForPath(t, services, namespaces, pdf.relPath)
		if pdf.expectedChunks >= 0 && len(rows) != pdf.expectedChunks {
			t.Fatalf("expected %d Turbopuffer rows for %s, got %d: %#v", pdf.expectedChunks, pdf.relPath, len(rows), rows)
		}
		if pdf.expectedChunks < 0 && len(rows) == 0 {
			t.Fatalf("expected Turbopuffer rows for %s, got none", pdf.relPath)
		}
		seenIndexes := make(map[int]bool)
		for _, row := range rows {
			if row["file_path"] != pdf.relPath {
				t.Fatalf("expected file_path %s, got %#v", pdf.relPath, row["file_path"])
			}
			if row["file_type"] != "pdf" {
				t.Fatalf("expected file_type pdf for %s, got %#v", pdf.relPath, row["file_type"])
			}
			index, ok := numericAttr(row["chunk_index"])
			if !ok {
				t.Fatalf("expected numeric chunk_index for %s, got %#v", pdf.relPath, row["chunk_index"])
			}
			page, ok := numericAttr(row["page_number"])
			if !ok {
				t.Fatalf("expected numeric page_number for %s, got %#v", pdf.relPath, row["page_number"])
			}
			if index != page {
				t.Fatalf("expected chunk_index == page_number for %s, got %d and %d", pdf.relPath, index, page)
			}
			seenIndexes[index] = true
		}
		if pdf.expectedChunks >= 0 {
			for i := 0; i < pdf.expectedChunks; i++ {
				if !seenIndexes[i] {
					t.Fatalf("expected %s to contain chunk/page index %d, got indexes %#v", pdf.relPath, i, seenIndexes)
				}
			}
		}
	}
}

func assertOfficeDocumentPageRows(t *testing.T, services realServices, namespaces []string, docs []officeDocumentFixture) {
	t.Helper()

	for _, doc := range docs {
		rows := queryTPRowsForPath(t, services, namespaces, doc.relPath)
		if doc.expectedChunks >= 0 && len(rows) != doc.expectedChunks {
			t.Fatalf("expected %d Turbopuffer rows for %s, got %d: %#v", doc.expectedChunks, doc.relPath, len(rows), rows)
		}
		if doc.expectedChunks < 0 && len(rows) == 0 {
			t.Fatalf("expected Turbopuffer rows for %s, got none", doc.relPath)
		}
		for _, row := range rows {
			if row["file_path"] != doc.relPath {
				t.Fatalf("expected file_path %s, got %#v", doc.relPath, row["file_path"])
			}
			if row["file_type"] != doc.fileType {
				t.Fatalf("expected file_type %s for %s, got %#v", doc.fileType, doc.relPath, row["file_type"])
			}
			if _, ok := numericAttr(row["page_number"]); !ok {
				t.Fatalf("expected page_number for %s after Office→PDF→image chunking, got %#v", doc.relPath, row["page_number"])
			}
			if strings.TrimSpace(fmt.Sprint(row["image_path"])) == "" {
				t.Fatalf("expected image_path for %s after Office→PDF→image chunking, got %#v", doc.relPath, row["image_path"])
			}
		}
	}
}

func assertLargeMarkdownRows(t *testing.T, services realServices, namespaces []string, relPath string, minRows int) {
	t.Helper()
	if relPath == "" {
		return
	}
	rows := queryTPRowsForPath(t, services, namespaces, relPath)
	if len(rows) < minRows {
		t.Fatalf("expected at least %d Turbopuffer rows for large markdown %s, got %d: %#v", minRows, relPath, len(rows), rows)
	}
	for _, row := range rows {
		if row["file_type"] != "markdown" {
			t.Fatalf("expected file_type markdown for %s, got %#v", relPath, row["file_type"])
		}
	}
}

func hasIndexedPDF(pdfs []pdfFixture) bool {
	for _, pdf := range pdfs {
		if pdf.expectedChunks != 0 {
			return true
		}
	}
	return false
}

func assertCLIQuery(t *testing.T, homeDir string, env *e2eEnv, query, rootName, mode, glob, expectedPath string) {
	t.Helper()

	args := []string{"query", query, "--root", rootName, "--mode", mode, "--top-k", "50"}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, args...)
	if err != nil {
		t.Fatalf("query %q failed: %v\nstdout: %s\nstderr: %s", query, err, stdout, stderr)
	}
	if strings.Contains(stdout, "No results found") {
		t.Fatalf("expected query results for %q, got none", query)
	}
	if expectedPath != "" && !strings.Contains(stdout, expectedPath) {
		t.Fatalf("expected query output to contain %s, got: %s", expectedPath, stdout)
	}
}

func resolveRootID(t *testing.T, serverURL, apiKey, name string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, serverURL+"/roots", nil)
	if err != nil {
		t.Fatalf("creating roots request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("listing roots: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading roots response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing roots: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var roots []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &roots); err != nil {
		t.Fatalf("decoding roots response: %v", err)
	}
	for _, root := range roots {
		if root.Name == name {
			return root.ID
		}
	}
	t.Fatalf("root %q not found in %s", name, string(body))
	return ""
}

func visibleGenerationID(t *testing.T, serverURL, apiKey, rootID string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, serverURL+"/roots/"+url.PathEscape(rootID), nil)
	if err != nil {
		t.Fatalf("creating root request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("getting root: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading root response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getting root: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var root struct {
		VisibleGenerationID string `json:"visible_generation_id"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decoding root response: %v", err)
	}
	if root.VisibleGenerationID == "" {
		t.Fatalf("root has empty visible generation: %s", string(body))
	}
	return root.VisibleGenerationID
}

func rootIndexNamespaces(t *testing.T, rootID string) []string {
	t.Helper()

	out := psqlOutput(t, `SELECT namespace FROM root_index_namespaces WHERE root_id = `+sqlQuote(rootID)+` AND retired_at IS NULL ORDER BY shard_index`)
	var namespaces []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			namespaces = append(namespaces, line)
		}
	}
	if len(namespaces) == 0 {
		t.Fatalf("root %s has no root_index_namespaces rows", rootID)
	}
	return namespaces
}

func assertRootIndexNamespaceCount(t *testing.T, namespaces []string, want int) {
	t.Helper()
	if len(namespaces) != want {
		t.Fatalf("root index namespaces = %#v, want %d namespaces", namespaces, want)
	}
}

func tpBaseURL(services realServices) string {
	if services.turbopufferAPIURL != "" {
		return strings.TrimRight(services.turbopufferAPIURL, "/")
	}
	return "https://api.turbopuffer.com"
}

func queryTPRowsForPath(t *testing.T, services realServices, namespaces []string, relPath string) []map[string]any {
	t.Helper()
	return queryTPRowsForPathWithFilter(t, services, namespaces, []any{"And", []any{
		[]any{"file_path", "Eq", relPath},
		[]any{"valid_to_generation_seq", "Eq", 0},
	}})
}

func queryTPAllRowsForPath(t *testing.T, services realServices, namespaces []string, relPath string) []map[string]any {
	t.Helper()
	return queryTPRowsForPathWithFilter(t, services, namespaces, []any{"file_path", "Eq", relPath})
}

func queryTPRowsForPathWithFilter(t *testing.T, services realServices, namespaces []string, filter any) []map[string]any {
	t.Helper()

	var allRows []map[string]any
	for _, namespace := range namespaces {
		allRows = append(allRows, queryTPNamespaceRowsWithFilter(t, services, namespace, filter)...)
	}
	return allRows
}

func queryTPNamespaceRowsWithFilter(t *testing.T, services realServices, namespace string, filter any) []map[string]any {
	t.Helper()

	body := map[string]any{
		"rank_by": []any{"id", "asc"},
		"limit":   1000,
		"filters": filter,
		"include_attributes": []string{
			"content", "file_path", "absolute_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path", "valid_from_generation", "valid_from_generation_seq", "valid_to_generation", "valid_to_generation_seq",
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding TP query: %v", err)
	}

	endpoint := fmt.Sprintf("%s/v2/namespaces/%s/query", tpBaseURL(services), url.PathEscape(namespace))
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("creating TP query: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+services.turbopufferAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("querying TP rows: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading TP response: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		t.Fatalf("querying TP rows: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("decoding TP rows: %v", err)
	}
	return result.Rows
}

func deleteTPNamespaces(t *testing.T, services realServices, namespaces []string) {
	t.Helper()
	for _, namespace := range namespaces {
		deleteTPNamespace(t, services, namespace)
	}
}

func deleteTPNamespace(t *testing.T, services realServices, namespace string) {
	t.Helper()

	endpoint := fmt.Sprintf("%s/v2/namespaces/%s", tpBaseURL(services), url.PathEscape(namespace))
	req, err := http.NewRequest(http.MethodDelete, endpoint, nil)
	if err != nil {
		t.Logf("creating TP cleanup request for %s: %v", namespace, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+services.turbopufferAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("cleaning TP namespace %s: %v", namespace, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		t.Logf("cleaning TP namespace %s: HTTP %d: %s", namespace, resp.StatusCode, string(body))
	}
}

func assertHasTPRows(t *testing.T, services realServices, namespaces []string, relPath string) {
	t.Helper()
	rows := queryTPRowsForPath(t, services, namespaces, relPath)
	if len(rows) == 0 {
		t.Fatalf("expected TP rows for %s", relPath)
	}
}

func assertNoTPRows(t *testing.T, services realServices, namespaces []string, relPath string) {
	t.Helper()
	rows := queryTPRowsForPath(t, services, namespaces, relPath)
	if len(rows) != 0 {
		t.Fatalf("expected no TP rows for %s, got %#v", relPath, rows)
	}
}

func assertClosedTPRows(t *testing.T, services realServices, namespaces []string, relPath string) {
	t.Helper()
	rows := queryTPAllRowsForPath(t, services, namespaces, relPath)
	if len(rows) == 0 {
		t.Fatalf("expected inactive TP rows for %s", relPath)
	}
	for _, row := range rows {
		attrs := rowAttributes(row)
		if validToSeq, ok := attrs["valid_to_generation_seq"].(float64); !ok || validToSeq <= 0 {
			t.Fatalf("expected %s row to be closed, got %#v", relPath, row)
		}
	}
}

func rowAttributes(row map[string]any) map[string]any {
	if attrs, ok := row["attributes"].(map[string]any); ok {
		return attrs
	}
	return row
}

type rowDigest []string

func rowsDigest(rows []map[string]any) rowDigest {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		id := fmt.Sprint(row["id"])
		fileHash := fmt.Sprint(row["file_hash"])
		contentHash := fmt.Sprint(row["content_hash"])
		values = append(values, id+"|"+fileHash+"|"+contentHash)
	}
	sort.Strings(values)
	return rowDigest(values)
}

func (d rowDigest) equal(other rowDigest) bool {
	if len(d) != len(other) {
		return false
	}
	for i := range d {
		if d[i] != other[i] {
			return false
		}
	}
	return true
}

func waitForRowDigestChange(t *testing.T, services realServices, namespaces []string, relPath string, before rowDigest, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		after := rowsDigest(queryTPRowsForPath(t, services, namespaces, relPath))
		if !before.equal(after) {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("TP rows for %s did not change within %v", relPath, timeout)
}

func numericAttr(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), v == float64(int(v))
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		i, err := v.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

type runningWatch struct {
	cmd    *exec.Cmd
	output *safeBuffer
}

func startPufferfsWatch(t *testing.T, homeDir string, env *e2eEnv, projectDir, rootName string, debounce time.Duration) *runningWatch {
	t.Helper()

	cmd := exec.Command(e2eCLIBinPath, "watch", projectDir, "--name", rootName, "--debounce", debounce.String())
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"PUFFERFS_SERVER_URL="+env.serverURL,
		"PUFFERFS_API_KEY="+env.apiKey,
	)
	out := &safeBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting watch: %v", err)
	}
	w := &runningWatch{cmd: cmd, output: out}
	t.Cleanup(func() {
		w.stop(t)
	})
	return w
}

func (w *runningWatch) waitForOutput(t *testing.T, needle string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(w.output.String(), needle) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("watch output did not contain %q within %v; output:\n%s", needle, timeout, w.output.String())
}

func (w *runningWatch) stop(t *testing.T) {
	t.Helper()
	if w == nil || w.cmd == nil || w.cmd.Process == nil {
		return
	}
	_ = w.cmd.Process.Kill()
	_ = w.cmd.Wait()
	w.cmd = nil
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func requireOutputContains(t *testing.T, output, needle string) {
	t.Helper()
	if !strings.Contains(output, needle) {
		t.Fatalf("expected output to contain %q, got:\n%s", needle, output)
	}
}

func assertFileMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s to be absent", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking %s: %v", path, err)
	}
}

// acmeCorpConfig holds parsed credentials from PUFFERFS_INTEGRATION_TEST_S3_ENV.
type acmeCorpConfig struct {
	accessKeyID     string
	secretAccessKey string
	endpointURL     string
	bucketName      string
}

type objectEntry struct {
	key  string
	size int64
}

// parseIntegrationTestS3Env parses the PUFFERFS_INTEGRATION_TEST_S3_ENV env var,
// which is a space-separated set of KEY=VALUE pairs.
func parseIntegrationTestS3Env(t *testing.T) *acmeCorpConfig {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv("PUFFERFS_INTEGRATION_TEST_S3_ENV"))
	if raw == "" {
		return nil
	}

	env := make(map[string]string)
	// The env var may be newline-separated or space-separated KEY=VALUE pairs.
	// Try newline-separated first; fall back to splitting on " AWS_".
	var parts []string
	if strings.Contains(raw, "\n") {
		parts = strings.Split(raw, "\n")
	} else {
		// Space-separated: split on " AWS_" boundaries, keeping the key prefix.
		parts = splitOnKeyBoundary(raw)
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		idx := strings.Index(part, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(part[:idx])
		val := strings.TrimSpace(part[idx+1:])
		env[key] = val
	}

	cfg := &acmeCorpConfig{
		accessKeyID:     env["AWS_ACCESS_KEY_ID"],
		secretAccessKey: env["AWS_SECRET_ACCESS_KEY"],
		endpointURL:     env["AWS_ENDPOINT_URL"],
		bucketName:      env["AWS_BUCKET_NAME"],
	}
	if cfg.accessKeyID == "" || cfg.secretAccessKey == "" || cfg.endpointURL == "" || cfg.bucketName == "" {
		return nil
	}
	return cfg
}

func newAcmeCorpS3Client(t *testing.T, cfg *acmeCorpConfig) *s3sdk.Client {
	t.Helper()

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider(cfg.accessKeyID, cfg.secretAccessKey, "")),
	)
	if err != nil {
		t.Fatalf("loading acme-corp S3 config: %v", err)
	}
	return s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(cfg.endpointURL)
	})
}

const (
	acmeCorpWithGitignoreDir    = "with-gitignore"
	acmeCorpWithoutGitignoreDir = "without-gitignore"
)

// downloadAcmeCorp downloads selected objects under "acme-corp/" from the integration
// test bucket into a local directory, returning the list of relative paths downloaded.
func downloadAcmeCorp(t *testing.T, cfg *acmeCorpConfig, destDir string, includeGitignore bool) []string {
	t.Helper()

	ctx := context.Background()
	client := newAcmeCorpS3Client(t, cfg)

	prefix := "acme-corp/"
	paginator := s3sdk.NewListObjectsV2Paginator(client, &s3sdk.ListObjectsV2Input{
		Bucket: aws.String(cfg.bucketName),
		Prefix: aws.String(prefix),
	})

	listStart := time.Now()
	var objects []objectEntry
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			t.Fatalf("listing acme-corp objects in s3://%s/%s: %v", cfg.bucketName, prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || strings.HasSuffix(*obj.Key, "/") {
				continue
			}
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			objects = append(objects, objectEntry{key: *obj.Key, size: size})
		}
	}
	t.Logf("timing stage=acme_corp_list bucket=%s prefix=%s objects=%d elapsed=%s",
		cfg.bucketName, prefix, len(objects), time.Since(listStart))

	if len(objects) == 0 {
		t.Fatalf("no objects found under s3://%s/%s", cfg.bucketName, prefix)
	}

	sort.Slice(objects, func(i, j int) bool { return objects[i].key < objects[j].key })
	objects = selectAcmeCorpObjects(objects, includeGitignore)
	t.Logf("timing stage=acme_corp_select objects=%d include_gitignore=%t full_corpus=%t",
		len(objects), includeGitignore, strings.EqualFold(strings.TrimSpace(os.Getenv("PUFFERFS_E2E_ACME_CORP_FULL_CORPUS")), "1"))
	if len(objects) == 0 {
		t.Fatalf("no selected acme-corp objects under s3://%s/%s", cfg.bucketName, prefix)
	}

	var relPaths []string
	downloadStart := time.Now()
	var totalBytes int64
	for _, obj := range objects {
		relPath := strings.TrimPrefix(obj.key, prefix)
		fullPath := filepath.Join(destDir, filepath.FromSlash(relPath))
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating dir %s: %v", dir, err)
		}

		resp, err := client.GetObject(ctx, &s3sdk.GetObjectInput{
			Bucket: aws.String(cfg.bucketName),
			Key:    aws.String(obj.key),
		})
		if err != nil {
			t.Fatalf("downloading s3://%s/%s: %v", cfg.bucketName, obj.key, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading s3://%s/%s: %v", cfg.bucketName, obj.key, err)
		}
		if err := os.WriteFile(fullPath, body, 0o644); err != nil {
			t.Fatalf("writing %s: %v", fullPath, err)
		}
		totalBytes += int64(len(body))
		relPaths = append(relPaths, relPath)
	}
	t.Logf("timing stage=acme_corp_download files=%d total_bytes=%d elapsed=%s",
		len(relPaths), totalBytes, time.Since(downloadStart))

	return relPaths
}

func prefixAcmeCorpPaths(prefix string, relPaths []string) []string {
	prefixed := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		prefixed = append(prefixed, acmeCorpPath(prefix, relPath))
	}
	return prefixed
}

func acmeCorpPath(prefix, relPath string) string {
	return path.Join(prefix, relPath)
}

func selectAcmeCorpObjects(objects []objectEntry, includeGitignore bool) []objectEntry {
	fullCorpus := strings.EqualFold(strings.TrimSpace(os.Getenv("PUFFERFS_E2E_ACME_CORP_FULL_CORPUS")), "1")
	var selected []objectEntry
	for _, object := range objects {
		if !includeGitignore && strings.EqualFold(filepath.Base(object.key), ".gitignore") {
			continue
		}
		if fullCorpus || acmeCorpSmokeFixture(object.key, includeGitignore) {
			selected = append(selected, object)
		}
	}
	return selected
}

func acmeCorpSmokeFixture(key string, includeGitignore bool) bool {
	if includeGitignore && strings.EqualFold(filepath.Base(key), ".gitignore") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(key))
	switch ext {
	case ".md", ".txt", ".csv", ".tsv", ".html", ".eml", ".msg", ".ics", ".vcf", ".mp3", ".wav", ".mp4", ".mov":
		return true
	default:
		return false
	}
}

// runAcmeCorpSync downloads the acme-corp test directory from the
// pufferfs-integration-test R2 bucket and syncs it through the full pipeline:
// local MinIO (storage) + Postgres (DB) + dev Modal (embedding/query embedding).
// By default it syncs two ACME copies: one with ACME's .gitignore and one without
// it, so the test can assert gitignored video fixtures are skipped in only the
// scoped directory. Set PUFFERFS_E2E_ACME_CORP_FULL_CORPUS=1 to include PDFs,
// Office documents, images, and other large binary files when Modal's S3 secret
// is configured for writable storage.
//
// Required env vars:
//   - PUFFERFS_INTEGRATION_TEST_S3_ENV: R2 credentials for the integration test bucket
//   - MODAL_CHUNK_ENDPOINT, MODAL_EMBED_ENDPOINT, MODAL_QUERY_EMBED_ENDPOINT: Modal dev endpoints
//   - TURBOPUFFER_API_KEY: for indexing and querying
func runAcmeCorpSync(t *testing.T, services realServices) {
	acmeCfg := parseIntegrationTestS3Env(t)
	if acmeCfg == nil {
		t.Skip("PUFFERFS_INTEGRATION_TEST_S3_ENV not set or incomplete; skipping acme-corp integration test")
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	env := newE2EEnv(t, services, "")
	homeDir := t.TempDir()
	initPufferFS(t, env, homeDir)

	projectDir := filepath.Join(homeDir, "acme-corp-"+suffix)
	withGitignoreRelPaths := prefixAcmeCorpPaths(acmeCorpWithGitignoreDir, downloadAcmeCorp(t, acmeCfg, filepath.Join(projectDir, acmeCorpWithGitignoreDir), true))
	withoutGitignoreRelPaths := prefixAcmeCorpPaths(acmeCorpWithoutGitignoreDir, downloadAcmeCorp(t, acmeCfg, filepath.Join(projectDir, acmeCorpWithoutGitignoreDir), false))
	t.Logf("downloaded acme-corp fixture with_gitignore=%d without_gitignore=%d to %s", len(withGitignoreRelPaths), len(withoutGitignoreRelPaths), projectDir)

	// Sync the downloaded directory.
	syncStart := time.Now()
	stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
	t.Logf("timing stage=acme_corp_sync elapsed=%s", time.Since(syncStart))
	if err != nil {
		t.Fatalf("acme-corp sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	requireOutputContains(t, stdout, "Sync complete")

	rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
	namespaces := rootIndexNamespaces(t, rootID)
	t.Logf("root %s has %d namespace shards", rootID, len(namespaces))
	cleanupDone := false
	t.Cleanup(func() {
		if !cleanupDone {
			adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
			deleteTPNamespaces(t, services, namespaces)
		}
	})

	// Verify storage artifacts exist.
	assertMinioHasPrefix(t, fmt.Sprintf("bundles/%s/", rootID))
	assertMinioHasPrefix(t, "syncs/")

	// Verify indexing completed for a sample of known files.
	samplePaths := []string{
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "README.md"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "documentation/engineering/deployment_runbook.md"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "documentation/process/onboarding_process.md"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "documentation/process/sales_process.md"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "data/raw/customers.csv"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "data/raw/data_dictionary.txt"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "web/reports/monthly_report.html"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "communications/email/welcome_email.eml"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "communications/calendar/all_hands_meeting.ics"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "communications/contacts/john_smith.vcf"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "shared/style_guide.md"),
		acmeCorpPath(acmeCorpWithoutGitignoreDir, "archives/2024_q1/q1_summary.md"),
	}
	for _, p := range samplePaths {
		assertHasTPRows(t, services, namespaces, p)
	}
	assertAcmeCorpMediaRows(t, services, namespaces, withoutGitignoreRelPaths)
	assertAcmeCorpGitignoreMediaDiff(t, services, namespaces, withGitignoreRelPaths)

	assertAcmeCorpQueries(t, homeDir, env, acmeCorpWithoutGitignoreDir)

	// Cleanup.
	deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
	cleanupDone = true
}

func assertAcmeCorpQueries(t *testing.T, homeDir string, env *e2eEnv, prefix string) {
	t.Helper()

	cases := []struct {
		name         string
		query        string
		mode         string
		glob         string
		expectedPath string
	}{
		{
			name:         "readme role navigation",
			query:        "Sales Team Sales Dashboard Customer Data Engineering Team Deployment Guide",
			mode:         "fts",
			expectedPath: "README.md",
		},
		{
			name:         "engineering runbook",
			query:        "production deployment rollback procedures database backup kubectl smoke tests",
			mode:         "hybrid",
			expectedPath: "documentation/engineering/deployment_runbook.md",
		},
		{
			name:         "onboarding process",
			query:        "30 60 90 day goals buddy assigned pre boarding IT provisions equipment accounts",
			mode:         "fts",
			glob:         "documentation/process/**",
			expectedPath: "documentation/process/onboarding_process.md",
		},
		{
			name:         "sales process",
			query:        "BANT criteria discovery call demo proposal negotiation closed won Salesforce",
			mode:         "fts",
			glob:         "documentation/process/**",
			expectedPath: "documentation/process/sales_process.md",
		},
		{
			name:         "customer csv",
			query:        "Acme Manufacturing John Smith Manufacturing annual revenue active customer",
			mode:         "fts",
			glob:         "data/**",
			expectedPath: "data/raw/customers.csv",
		},
		{
			name:         "data dictionary",
			query:        "Salesforce CRM export account status product catalog TSV transactions salesperson region",
			mode:         "fts",
			glob:         "data/**",
			expectedPath: "data/raw/data_dictionary.txt",
		},
		{
			name:         "monthly html report",
			query:        "monthly revenue churn rate NPS score enterprise deals customer acquisition cost",
			mode:         "fts",
			glob:         "web/**",
			expectedPath: "web/reports/monthly_report.html",
		},
		{
			name:         "email fixture",
			query:        "welcome aboard laptop equipment pickup photo ID signed offer letter",
			mode:         "fts",
			glob:         "communications/**",
			expectedPath: "communications/email/welcome_email.eml",
		},
		{
			name:         "calendar fixture",
			query:        "Q3 All-Hands Meeting company-wide quarterly update 2025 goals Q&A",
			mode:         "fts",
			glob:         "communications/**",
			expectedPath: "communications/calendar/all_hands_meeting.ics",
		},
		{
			name:         "contact fixture",
			query:        "John Smith Senior Account Executive enterprise accounts sales",
			mode:         "fts",
			glob:         "communications/**",
			expectedPath: "communications/contacts/john_smith.vcf",
		},
		{
			name:         "style guide",
			query:        "Primary Navy Secondary Teal Accent Orange writing voice concise friendly",
			mode:         "fts",
			glob:         "shared/**",
			expectedPath: "shared/style_guide.md",
		},
		{
			name:         "archive summary",
			query:        "January March 2024 net income opened East region sales office historical record",
			mode:         "fts",
			glob:         "archives/**",
			expectedPath: "archives/2024_q1/q1_summary.md",
		},
		{
			name:         "semantic hybrid",
			query:        "how should I deploy production safely and roll back if the release fails",
			mode:         "hybrid",
			glob:         "documentation/engineering/**",
			expectedPath: "documentation/engineering/deployment_runbook.md",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			glob := tc.glob
			if glob == "" {
				glob = acmeCorpPath(prefix, "**")
			} else {
				glob = acmeCorpPath(prefix, glob)
			}
			assertCLIQuery(t, homeDir, env, tc.query, env.rootName, tc.mode, glob, acmeCorpPath(prefix, tc.expectedPath))
		})
	}
}

func assertAcmeCorpMediaRows(t *testing.T, services realServices, namespaces []string, relPaths []string) {
	t.Helper()

	checked := 0
	for _, relPath := range relPaths {
		fileType, ok := mediaFixtureFileType(relPath)
		if !ok {
			continue
		}
		rows := queryTPRowsForPath(t, services, namespaces, relPath)
		if len(rows) == 0 {
			t.Fatalf("expected Turbopuffer rows for ACME media fixture %s, got none", relPath)
		}
		for _, row := range rows {
			if row["file_path"] != relPath {
				t.Fatalf("expected file_path %s, got %#v", relPath, row["file_path"])
			}
			if row["file_type"] != fileType {
				t.Fatalf("expected file_type %s for %s, got %#v", fileType, relPath, row["file_type"])
			}
			content, ok := row["content"].(string)
			if !ok || !strings.HasPrefix(content, "[") {
				t.Fatalf("expected timestamped media content for %s, got %#v", relPath, row["content"])
			}
		}
		checked++
	}
	if checked == 0 {
		t.Log("no ACME media fixtures selected; media row assertions skipped")
	}
}

func assertAcmeCorpGitignoreMediaDiff(t *testing.T, services realServices, namespaces []string, relPaths []string) {
	t.Helper()

	checkedIgnored := 0
	checkedIndexed := 0
	for _, relPath := range relPaths {
		fileType, ok := mediaFixtureFileType(relPath)
		if !ok {
			continue
		}
		rows := queryTPRowsForPath(t, services, namespaces, relPath)
		if gitignoredAcmeMediaFixture(relPath) {
			if len(rows) != 0 {
				t.Fatalf("expected ACME gitignore to skip media fixture %s, got rows %#v", relPath, rows)
			}
			checkedIgnored++
			continue
		}
		if len(rows) == 0 {
			t.Fatalf("expected non-gitignored ACME media fixture %s to be indexed", relPath)
		}
		for _, row := range rows {
			if row["file_type"] != fileType {
				t.Fatalf("expected file_type %s for %s, got %#v", fileType, relPath, row["file_type"])
			}
		}
		checkedIndexed++
	}
	if checkedIgnored == 0 {
		t.Fatal("no ACME gitignored media fixtures selected")
	}
	if checkedIndexed == 0 {
		t.Fatal("no ACME non-gitignored media fixtures selected")
	}
}

func mediaFixtureFileType(relPath string) (string, bool) {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".mp3", ".wav":
		return "audio", true
	case ".mp4", ".mov":
		return "video", true
	default:
		return "", false
	}
}

func gitignoredAcmeMediaFixture(relPath string) bool {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".mp4", ".mov":
		return true
	default:
		return false
	}
}

// splitOnKeyBoundary splits a single-line "KEY=val KEY2=val2" string on spaces
// that precede an uppercase KEY= token. Go's regexp doesn't support lookaheads,
// so we do it manually.
func splitOnKeyBoundary(s string) []string {
	var parts []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i-1] == ' ' && i < len(s) && isUpperOrUnderscore(s[i]) {
			// Check if this starts a KEY= token
			eqIdx := strings.Index(s[i:], "=")
			if eqIdx > 0 && eqIdx < 40 {
				allUpper := true
				for _, c := range s[i : i+eqIdx] {
					if !isUpperOrUnderscore(byte(c)) {
						allUpper = false
						break
					}
				}
				if allUpper {
					parts = append(parts, s[start:i-1])
					start = i
				}
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func isUpperOrUnderscore(b byte) bool {
	return (b >= 'A' && b <= 'Z') || b == '_'
}

func repoRoot(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return repoRoot
}
