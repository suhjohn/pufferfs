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

	e2eJWTSecret = "e2e-integration-test-jwt-secret!"

	llmWikiURL = "https://gist.githubusercontent.com/karpathy/442a6bf555914893e9891c11519de94f/raw/ac46de1ad27f92b28ac95459c782c07f6b8c964a/llm-wiki.md"
)

var (
	e2eSetupOnce      sync.Once
	e2eCLIBinPath     string
	e2eServerBinPath  string
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
		namespace := tpNamespace(env.orgID, rootID)
		t.Cleanup(func() {
			deleteTPNamespace(t, services, namespace)
		})

		assertNestedRowsIndexed(t, services, namespace, fixtures)
		assertPDFPageRows(t, services, namespace, fixtures.pdfs)
		assertOfficeDocumentPageRows(t, services, namespace, fixtures.officeDocs)
		assertLargeMarkdownRows(t, services, namespace, fixtures.largeMarkdownPath, 2)
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
			stableBefore[relPath] = rowsDigest(queryTPRowsForPath(t, services, namespace, relPath))
		}
		modifyBefore := rowsDigest(queryTPRowsForPath(t, services, namespace, modifyPath))

		appendFile(t, projectDir, modifyPath, "\n\nRuntime update: add enterprise audit exports and tighten retention controls.\n")
		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("modify sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Merkle diff found 1 changed files")
		requireOutputContains(t, stdout, "Syncing 1 changes")

		modifyAfter := rowsDigest(queryTPRowsForPath(t, services, namespace, modifyPath))
		if modifyBefore.equal(modifyAfter) {
			t.Fatalf("modified %s kept identical indexed row digest: %#v", modifyPath, modifyAfter)
		}
		for relPath, before := range stableBefore {
			after := rowsDigest(queryTPRowsForPath(t, services, namespace, relPath))
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
		assertNoTPRows(t, services, namespace, oldMovePath)
		assertHasTPRows(t, services, namespace, newMovePath)

		removedPath := fixtures.removePath
		if err := os.Remove(filepath.Join(projectDir, filepath.FromSlash(removedPath))); err != nil {
			t.Fatalf("removing nested txt file: %v", err)
		}
		stdout, stderr, err = runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
		if err != nil {
			t.Fatalf("remove sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		requireOutputContains(t, stdout, "Sync complete")
		assertNoTPRows(t, services, namespace, removedPath)

		watchPath := fixtures.watchPath
		beforeWatch := rowsDigest(queryTPRowsForPath(t, services, namespace, watchPath))
		watch := startPufferfsWatch(t, homeDir, env, projectDir, env.rootName, 300*time.Millisecond)
		watch.waitForOutput(t, "Watching", 30*time.Second)
		appendFile(t, projectDir, watchPath, "\n3. Verify invoice webhooks and search indexing.\n")
		waitForRowDigestChange(t, services, namespace, watchPath, beforeWatch, 90*time.Second)
		watch.stop(t)
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

		namespace := tpNamespace(env.orgID, rootID)
		t.Cleanup(func() {
			deleteTPNamespace(t, services, namespace)
		})
		assertHasTPRows(t, services, namespace, "docs/deep/retry/incident.md")
		assertHasTPRows(t, services, namespace, "docs/deep/retry/evidence.txt")
		assertCLIQuery(t, homeDir, env, "failed indexing retry", env.rootName, "hybrid", "", "docs/deep/retry/incident.md")
	})
}

type realServices struct {
	modalChunkURL      string
	modalEmbedURL      string
	modalQueryEmbedURL string
	turbopufferAPIKey  string
	turbopufferAPIURL  string
}

func requireRealServices(t *testing.T) realServices {
	t.Helper()

	cfg := realServices{
		modalChunkURL:      os.Getenv("MODAL_CHUNK_ENDPOINT"),
		modalEmbedURL:      os.Getenv("MODAL_EMBED_ENDPOINT"),
		modalQueryEmbedURL: os.Getenv("MODAL_QUERY_EMBED_ENDPOINT"),
		turbopufferAPIKey:  os.Getenv("TURBOPUFFER_API_KEY"),
		turbopufferAPIURL:  os.Getenv("TURBOPUFFER_API_URL"),
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
	if len(missing) > 0 {
		t.Skipf("real Modal/Turbopuffer integration requires env vars: %s", strings.Join(missing, ", "))
	}
	return cfg
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
		"MIGRATIONS_DIR="+filepath.Join(repoRoot(t), "migrations"),
		"MODAL_CHUNK_ENDPOINT="+services.modalChunkURL,
		"MODAL_EMBED_ENDPOINT="+services.modalEmbedURL,
		"MODAL_QUERY_EMBED_ENDPOINT="+services.modalQueryEmbedURL,
		"TURBOPUFFER_API_KEY="+tpKey,
		"TURBOPUFFER_API_URL="+services.turbopufferAPIURL,
		"AWS_ENDPOINT_URL="+fmt.Sprintf("http://localhost:%s", e2eMinioPort),
		"AWS_BUCKET_NAME="+e2eMinioBucket,
		"AWS_ACCESS_KEY_ID="+e2eMinioUser,
		"AWS_SECRET_ACCESS_KEY="+e2eMinioPass,
	)
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

	stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "init")
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
			// R2/user-provided DOCX/PPTX fixtures are appended dynamically from the manifest.
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
	applyExternalFixtureManifest(t, projectDir, &fixtures)
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

type fixtureManifest struct {
	Files []manifestFile `json:"files"`
}

type manifestFile struct {
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
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].key < objects[j].key
	})
	if len(objects) == 0 {
		t.Fatalf("no R2 fixtures found in s3://%s/%s", cfg.bucket, cfg.prefix)
	}
	objects = selectR2FixtureObjects(objects)

	for _, object := range objects {
		relPath := r2FixtureRelPath(t, cfg.prefix, object.key)
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
		registerManifestFixture(t, fixtures, r2FixtureManifestFile(cfg, object.key, relPath), relPath)
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

func r2FixtureManifestFile(cfg *r2FixtureConfig, key, relPath string) manifestFile {
	file := manifestFile{
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

func applyExternalFixtureManifest(t *testing.T, projectDir string, fixtures *fixtureSet) {
	t.Helper()

	manifest := loadFixtureManifest(t)
	if manifest == nil {
		return
	}
	for _, file := range manifest.Files {
		relPath := cleanFixturePath(t, file.Path)
		body := downloadFixtureBytes(t, file.URL)
		if file.SHA256 != "" {
			sum := sha256.Sum256(body)
			got := hex.EncodeToString(sum[:])
			if !strings.EqualFold(got, file.SHA256) {
				t.Fatalf("fixture %s sha256 mismatch: got %s want %s", relPath, got, file.SHA256)
			}
		}
		fullPath := filepath.Join(projectDir, filepath.FromSlash(relPath))
		mkdirAll(t, filepath.Dir(fullPath))
		if err := os.WriteFile(fullPath, body, 0o644); err != nil {
			t.Fatalf("writing external fixture %s: %v", relPath, err)
		}
		registerManifestFixture(t, fixtures, file, relPath)
	}
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
		registerManifestFixture(t, fixtures, manifestFile{
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
	case ".pdf", ".docx", ".doc", ".pptx", ".ppt", ".md", ".txt":
		return true
	default:
		return false
	}
}

func loadFixtureManifest(t *testing.T) *fixtureManifest {
	t.Helper()

	manifestURL := strings.TrimSpace(os.Getenv("PUFFERFS_E2E_FIXTURE_MANIFEST_URL"))
	if manifestURL == "" {
		return nil
	}
	raw := string(downloadFixtureBytes(t, manifestURL))

	var manifest fixtureManifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("decoding fixture manifest: %v", err)
	}
	return &manifest
}

func registerManifestFixture(t *testing.T, fixtures *fixtureSet, file manifestFile, relPath string) {
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

func assertNestedRowsIndexed(t *testing.T, services realServices, namespace string, fixtures fixtureSet) {
	t.Helper()

	for _, relPath := range append(append([]string{}, fixtures.textPaths...), fixtures.mdPaths...) {
		assertHasTPRows(t, services, namespace, relPath)
	}
	for _, pdf := range fixtures.pdfs {
		if pdf.expectedChunks == 0 {
			assertNoTPRows(t, services, namespace, pdf.relPath)
			continue
		}
		assertHasTPRows(t, services, namespace, pdf.relPath)
	}
	for _, doc := range fixtures.officeDocs {
		if doc.expectedChunks == 0 {
			assertNoTPRows(t, services, namespace, doc.relPath)
			continue
		}
		assertHasTPRows(t, services, namespace, doc.relPath)
	}
}

func assertPDFPageRows(t *testing.T, services realServices, namespace string, pdfs []pdfFixture) {
	t.Helper()

	if len(pdfs) == 0 {
		t.Log("PUFFERFS_CLI_PDF_FIXTURES not set; PDF page-count assertions skipped")
		return
	}

	for _, pdf := range pdfs {
		rows := queryTPRowsForPath(t, services, namespace, pdf.relPath)
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

func assertOfficeDocumentPageRows(t *testing.T, services realServices, namespace string, docs []officeDocumentFixture) {
	t.Helper()

	for _, doc := range docs {
		rows := queryTPRowsForPath(t, services, namespace, doc.relPath)
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

func assertLargeMarkdownRows(t *testing.T, services realServices, namespace, relPath string, minRows int) {
	t.Helper()
	if relPath == "" {
		return
	}
	rows := queryTPRowsForPath(t, services, namespace, relPath)
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

func tpNamespace(orgID, rootID string) string {
	return fmt.Sprintf("org-%s-root-%s", orgID, rootID)
}

func tpBaseURL(services realServices) string {
	if services.turbopufferAPIURL != "" {
		return strings.TrimRight(services.turbopufferAPIURL, "/")
	}
	return "https://api.turbopuffer.com"
}

func queryTPRowsForPath(t *testing.T, services realServices, namespace, relPath string) []map[string]any {
	t.Helper()

	body := map[string]any{
		"rank_by": []any{"id", "asc"},
		"limit":   1000,
		"filters": []any{"file_path", "Eq", relPath},
		"include_attributes": []string{
			"content", "file_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path",
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
		t.Fatalf("querying TP rows for %s: %v", relPath, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading TP response: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("querying TP rows for %s: HTTP %d: %s", relPath, resp.StatusCode, string(respBody))
	}

	var result struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("decoding TP rows: %v", err)
	}
	return result.Rows
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

func assertHasTPRows(t *testing.T, services realServices, namespace, relPath string) {
	t.Helper()
	rows := queryTPRowsForPath(t, services, namespace, relPath)
	if len(rows) == 0 {
		t.Fatalf("expected TP rows for %s", relPath)
	}
}

func assertNoTPRows(t *testing.T, services realServices, namespace, relPath string) {
	t.Helper()
	rows := queryTPRowsForPath(t, services, namespace, relPath)
	if len(rows) != 0 {
		t.Fatalf("expected no TP rows for %s, got %#v", relPath, rows)
	}
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

func waitForRowDigestChange(t *testing.T, services realServices, namespace, relPath string, before rowDigest, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		after := rowsDigest(queryTPRowsForPath(t, services, namespace, relPath))
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

func repoRoot(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return repoRoot
}
