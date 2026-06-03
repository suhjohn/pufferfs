//go:build cli_integration
// +build cli_integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	cliTestDBUser = "pufferfs_cli_test"
	cliTestDBPass = "testpass"
	cliTestDBName = "pufferfs_cli_test"
	cliTestDBPort = "25432"

	cliTestMinioPort   = "29000"
	cliTestMinioUser   = "minioadmin"
	cliTestMinioPass   = "minioadmin"
	cliTestMinioBucket = "pufferfs-cli-test"

	cliTestJWTSecret = "cli-integration-test-jwt-secret!"
)

var (
	cliSetupOnce      sync.Once
	cliBinPath        string
	serverBinPath     string
	cliPgContainer    = "pufferfs-cli-test-pg"
	cliMinioContainer = "pufferfs-cli-test-minio"
)

func TestMain(m *testing.M) {
	code := m.Run()
	exec.Command("docker", "rm", "-f", cliPgContainer).Run()
	exec.Command("docker", "rm", "-f", cliMinioContainer).Run()
	os.Exit(code)
}

func cliBuildBinaries(t *testing.T) {
	t.Helper()
	repoRoot, _ := filepath.Abs("..")

	tmpDir := t.TempDir()
	cliBinPath = filepath.Join(tmpDir, "pufferfs")
	cmd := exec.Command("go", "build", "-o", cliBinPath, "./cmd/pufferfs")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building CLI: %v\n%s", err, out)
	}

	serverBinPath = filepath.Join(tmpDir, "pufferfs-server")
	cmd = exec.Command("go", "build", "-o", serverBinPath, "./cmd/server")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building server: %v\n%s", err, out)
	}
}

func cliStartPostgres(t *testing.T) {
	t.Helper()
	exec.Command("docker", "rm", "-f", cliPgContainer).Run()
	cmd := exec.Command("docker", "run", "-d",
		"--name", cliPgContainer,
		"-e", "POSTGRES_USER="+cliTestDBUser,
		"-e", "POSTGRES_PASSWORD="+cliTestDBPass,
		"-e", "POSTGRES_DB="+cliTestDBName,
		"-p", cliTestDBPort+":5432",
		"postgres:16-alpine",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("starting postgres: %v\n%s", err, out)
	}
}

func cliStartMinIO(t *testing.T) {
	t.Helper()
	exec.Command("docker", "rm", "-f", cliMinioContainer).Run()
	cmd := exec.Command("docker", "run", "-d",
		"--name", cliMinioContainer,
		"-e", "MINIO_ROOT_USER="+cliTestMinioUser,
		"-e", "MINIO_ROOT_PASSWORD="+cliTestMinioPass,
		"-p", cliTestMinioPort+":9000",
		"minio/minio", "server", "/data",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("starting minio: %v\n%s", err, out)
	}
}

func cliWaitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", addr)
}

func cliCreateMinioBucket(t *testing.T) {
	t.Helper()
	endpoint := fmt.Sprintf("http://localhost:%s", cliTestMinioPort)

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider(cliTestMinioUser, cliTestMinioPass, "")),
	)
	if err != nil {
		t.Fatalf("loading aws config: %v", err)
	}

	s3Client := s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	_, err = s3Client.CreateBucket(context.Background(), &s3sdk.CreateBucketInput{
		Bucket: aws.String(cliTestMinioBucket),
	})
	if err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
		t.Logf("create bucket warning (may already exist): %v", err)
	}
}

// ---------------------------------------------------------------------------
// Real Modal + Turbopuffer services
// ---------------------------------------------------------------------------

type cliRealServices struct {
	modalChunkURL      string
	modalEmbedURL      string
	modalQueryEmbedURL string
	turbopufferAPIKey  string
	turbopufferAPIURL  string
}

func cliRequireRealServices(t *testing.T) cliRealServices {
	t.Helper()

	cfg := cliRealServices{
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

// ---------------------------------------------------------------------------
// Server process
// ---------------------------------------------------------------------------

type cliServerProcess struct {
	cmd  *exec.Cmd
	addr string
}

func cliStartServer(t *testing.T, services cliRealServices) *cliServerProcess {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	dbURL := fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		cliTestDBUser, cliTestDBPass, cliTestDBPort, cliTestDBName)

	repoRoot, _ := filepath.Abs("..")

	cmd := exec.Command(serverBinPath)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"LISTEN_ADDR="+addr,
		"JWT_SECRET="+cliTestJWTSecret,
		"MIGRATIONS_DIR="+filepath.Join(repoRoot, "migrations"),
		"MODAL_CHUNK_ENDPOINT="+services.modalChunkURL,
		"MODAL_EMBED_ENDPOINT="+services.modalEmbedURL,
		"MODAL_QUERY_EMBED_ENDPOINT="+services.modalQueryEmbedURL,
		"TURBOPUFFER_API_KEY="+services.turbopufferAPIKey,
		"TURBOPUFFER_API_URL="+services.turbopufferAPIURL,
		"AWS_ENDPOINT_URL="+fmt.Sprintf("http://localhost:%s", cliTestMinioPort),
		"AWS_BUCKET_NAME="+cliTestMinioBucket,
		"AWS_ACCESS_KEY_ID="+cliTestMinioUser,
		"AWS_SECRET_ACCESS_KEY="+cliTestMinioPass,
	)
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stderr // redirect to test output
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	// Wait for health check
	cliWaitForTCP(t, addr, 30*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}

	return &cliServerProcess{cmd: cmd, addr: addr}
}

// ---------------------------------------------------------------------------
// CLI runner
// ---------------------------------------------------------------------------

func runPufferfs(t *testing.T, homeDir, serverURL, apiKey string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(cliBinPath, args...)
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

func cliExpectedPDFChunks(t *testing.T) map[string]int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_CLI_PDF_EXPECTED_CHUNKS"))
	if raw == "" {
		return nil
	}

	expected := make(map[string]int)
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

func cliResolveRootID(t *testing.T, serverURL, apiKey, name string) string {
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

func cliTPBaseURL(services cliRealServices) string {
	if services.turbopufferAPIURL != "" {
		return strings.TrimRight(services.turbopufferAPIURL, "/")
	}
	return "https://api.turbopuffer.com"
}

func cliQueryTPRowsForPath(t *testing.T, services cliRealServices, namespace, relPath string) []map[string]any {
	t.Helper()

	body := map[string]any{
		"rank_by":            []any{"id", "asc"},
		"limit":              1000,
		"filters":            []any{"file_path", "Eq", relPath},
		"include_attributes": []string{"file_path", "chunk_index", "file_hash", "file_type", "page_number", "image_path"},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding TP query: %v", err)
	}

	endpoint := fmt.Sprintf("%s/v2/namespaces/%s/query", cliTPBaseURL(services), url.PathEscape(namespace))
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

func cliDeleteTPNamespace(t *testing.T, services cliRealServices, namespace string) {
	t.Helper()

	endpoint := fmt.Sprintf("%s/v2/namespaces/%s", cliTPBaseURL(services), url.PathEscape(namespace))
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

// ---------------------------------------------------------------------------
// Create test user + API key via psql
// ---------------------------------------------------------------------------

func cliCreateUser(t *testing.T) string {
	t.Helper()

	userID := "cli-test-user-1"
	orgID := "cli-test-org-1"
	email := "clitest@example.com"
	ts := time.Now().UnixNano()

	inserts := fmt.Sprintf(`
		INSERT INTO users (id, email, name, avatar_url, provider, provider_id, created_at)
		VALUES ('%s', '%s', 'CLI Test', '', 'google', 'goog-1', NOW())
		ON CONFLICT (id) DO NOTHING;

		INSERT INTO organizations (id, name, slug, created_at)
		VALUES ('%s', 'CLI Test Org', 'cli-test', NOW())
		ON CONFLICT (id) DO NOTHING;

		INSERT INTO org_members (org_id, user_id, role, joined_at)
		VALUES ('%s', '%s', 'owner', NOW())
		ON CONFLICT (org_id, user_id) DO NOTHING;
	`, userID, email, orgID, orgID, userID)

	cmd := exec.Command("docker", "exec", "-i", cliPgContainer, "psql", "-U", cliTestDBUser, "-d", cliTestDBName)
	cmd.Stdin = strings.NewReader(inserts)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inserting test user: %v\n%s", err, out)
	}

	// Create API key
	rawKey := fmt.Sprintf("pfs_clitestkey_%d", ts)
	hashCmd := exec.Command("sh", "-c", fmt.Sprintf(`printf '%%s' '%s' | sha256sum | cut -d' ' -f1`, rawKey))
	hashOut, err := hashCmd.Output()
	if err != nil {
		t.Fatalf("computing hash: %v", err)
	}
	keyHash := strings.TrimSpace(string(hashOut))

	keyInsert := fmt.Sprintf(`
		INSERT INTO api_keys (id, org_id, user_id, name, key_hash, scopes, created_at)
		VALUES ('key-cli-%d', '%s', '%s', 'cli-key', '%s', '{"read","write"}', NOW())
		ON CONFLICT DO NOTHING;
	`, ts, orgID, userID, keyHash)

	cmd = exec.Command("docker", "exec", "-i", cliPgContainer, "psql", "-U", cliTestDBUser, "-d", cliTestDBName)
	cmd.Stdin = strings.NewReader(keyInsert)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inserting API key: %v\n%s", err, out)
	}

	return rawKey
}

func cliCopyPDFFixtures(t *testing.T, projectDir string) []string {
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
	if len(sources) == 0 {
		return nil
	}

	docsDir := filepath.Join(projectDir, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("creating docs dir: %v", err)
	}

	var copied []string
	for _, source := range sources {
		if strings.TrimSpace(source) == "" {
			continue
		}
		if strings.ToLower(filepath.Ext(source)) != ".pdf" {
			continue
		}
		src, err := os.Open(source)
		if err != nil {
			t.Fatalf("opening PDF fixture %s: %v", source, err)
		}

		relPath := filepath.ToSlash(filepath.Join("docs", filepath.Base(source)))
		dstPath := filepath.Join(projectDir, filepath.FromSlash(relPath))
		dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			src.Close()
			t.Fatalf("creating PDF fixture copy %s: %v", dstPath, err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			src.Close()
			dst.Close()
			t.Fatalf("copying PDF fixture %s: %v", source, err)
		}
		src.Close()
		if err := dst.Close(); err != nil {
			t.Fatalf("closing PDF fixture copy %s: %v", dstPath, err)
		}
		copied = append(copied, relPath)
	}
	return copied
}

// ---------------------------------------------------------------------------
// Test: Full CLI user journey
// ---------------------------------------------------------------------------

func TestCLI_FullUserJourney(t *testing.T) {
	services := cliRequireRealServices(t)

	cliSetupOnce.Do(func() {
		cliBuildBinaries(t)
		cliStartPostgres(t)
		cliStartMinIO(t)

		cliWaitForTCP(t, fmt.Sprintf("localhost:%s", cliTestDBPort), 30*time.Second)

		// Wait for pg_isready
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			cmd := exec.Command("docker", "exec", cliPgContainer, "pg_isready", "-U", cliTestDBUser)
			if err := cmd.Run(); err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		cliWaitForTCP(t, fmt.Sprintf("localhost:%s", cliTestMinioPort), 30*time.Second)
		cliCreateMinioBucket(t)
	})

	srv := cliStartServer(t, services)
	serverURL := fmt.Sprintf("http://%s", srv.addr)
	apiKey := cliCreateUser(t)
	homeDir := t.TempDir()

	// ---- Scenario 1: pufferfs init ----
	t.Run("init", func(t *testing.T) {
		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "init")
		if err != nil {
			t.Fatalf("init failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if !strings.Contains(stdout, "Config written to") {
			t.Errorf("expected 'Config written to', got: %s", stdout)
		}
		configPath := filepath.Join(homeDir, ".tpfs", "config.toml")
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			t.Fatalf("config not created at %s", configPath)
		}
	})

	// ---- Create a project directory ----
	projectDir := filepath.Join(homeDir, "my-project")
	os.MkdirAll(filepath.Join(projectDir, "src"), 0o755)
	os.WriteFile(filepath.Join(projectDir, "README.md"),
		[]byte("# My Project\n\nA test project.\n"), 0o644)
	os.WriteFile(filepath.Join(projectDir, "src", "main.go"),
		[]byte("package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"), 0o644)
	os.WriteFile(filepath.Join(projectDir, "src", "utils.go"),
		[]byte("package main\n\nfunc add(a, b int) int { return a + b }\n"), 0o644)

	pdfFixtures := cliCopyPDFFixtures(t, projectDir)
	expectedPDFChunks := cliExpectedPDFChunks(t)
	if len(pdfFixtures) > 0 && len(expectedPDFChunks) == 0 {
		t.Fatalf("PUFFERFS_CLI_PDF_EXPECTED_CHUNKS is required when PUFFERFS_CLI_PDF_FIXTURES is set")
	}

	// ---- Scenario 2: First sync ----
	t.Run("first_sync", func(t *testing.T) {
		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "sync", projectDir, "--name", "my-project")
		if err != nil {
			t.Fatalf("sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if !strings.Contains(stdout, "Sync complete") {
			t.Errorf("expected 'Sync complete', got: %s", stdout)
		}
	})

	if len(expectedPDFChunks) > 0 {
		parentT := t
		t.Run("pdf_chunks_match_page_counts", func(t *testing.T) {
			rootID := cliResolveRootID(t, serverURL, apiKey, "my-project")
			namespace := fmt.Sprintf("org-%s-root-%s", "cli-test-org-1", rootID)
			parentT.Cleanup(func() {
				cliDeleteTPNamespace(parentT, services, namespace)
			})

			for _, relPath := range pdfFixtures {
				expected, ok := expectedPDFChunks[relPath]
				if !ok {
					t.Fatalf("missing expected chunk count for %s", relPath)
				}

				rows := cliQueryTPRowsForPath(t, services, namespace, relPath)
				if len(rows) != expected {
					t.Fatalf("expected %d Turbopuffer rows for %s, got %d: %#v", expected, relPath, len(rows), rows)
				}

				seenIndexes := make(map[int]bool)
				for _, row := range rows {
					if row["file_path"] != relPath {
						t.Fatalf("expected file_path %s, got %#v", relPath, row["file_path"])
					}
					if row["file_type"] != "pdf" {
						t.Fatalf("expected file_type pdf for %s, got %#v", relPath, row["file_type"])
					}
					index, ok := numericAttr(row["chunk_index"])
					if !ok {
						t.Fatalf("expected numeric chunk_index for %s, got %#v", relPath, row["chunk_index"])
					}
					page, ok := numericAttr(row["page_number"])
					if !ok {
						t.Fatalf("expected numeric page_number for %s, got %#v", relPath, row["page_number"])
					}
					if index != page {
						t.Fatalf("expected chunk_index == page_number for %s, got %d and %d", relPath, index, page)
					}
					seenIndexes[index] = true
				}
				for i := 0; i < expected; i++ {
					if !seenIndexes[i] {
						t.Fatalf("expected %s to contain chunk/page index %d, got indexes %#v", relPath, i, seenIndexes)
					}
				}
			}
		})
	}

	// ---- Scenario 3: Query (fts, vector, hybrid) ----
	t.Run("query_fts", func(t *testing.T) {
		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "query", "hello", "--root", "my-project", "--mode", "fts")
		if err != nil {
			t.Fatalf("query fts failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if strings.Contains(stdout, "No results found") {
			t.Error("expected results, got none")
		}
	})

	t.Run("query_vector", func(t *testing.T) {
		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "query", "main function", "--root", "my-project", "--mode", "vector")
		if err != nil {
			t.Fatalf("query vector failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if strings.Contains(stdout, "No results found") {
			t.Error("expected results, got none")
		}
	})

	t.Run("query_hybrid", func(t *testing.T) {
		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "query", "add function", "--root", "my-project", "--mode", "hybrid")
		if err != nil {
			t.Fatalf("query hybrid failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if strings.Contains(stdout, "No results found") {
			t.Error("expected results, got none")
		}
	})

	if len(pdfFixtures) > 0 {
		t.Run("query_pdf_fixtures", func(t *testing.T) {
			stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "query", "business", "--root", "my-project", "--mode", "hybrid", "--glob", "*.pdf", "--top-k", "50")
			if err != nil {
				t.Fatalf("query PDF fixtures failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
			}
			if strings.Contains(stdout, "No results found") {
				t.Fatalf("expected PDF fixture results, got none")
			}
			matched := 0
			for _, relPath := range pdfFixtures {
				if expectedPDFChunks[relPath] == 0 {
					continue
				}
				if strings.Contains(stdout, relPath) {
					matched++
				}
			}
			if matched == 0 {
				t.Fatalf("expected at least one indexed PDF result, got: %s", stdout)
			}
		})
	}

	// ---- Scenario 4: Modify file + re-sync ----
	t.Run("modify_and_resync", func(t *testing.T) {
		os.WriteFile(filepath.Join(projectDir, "src", "main.go"),
			[]byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello updated\")\n}\n"), 0o644)

		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "sync", projectDir, "--name", "my-project")
		if err != nil {
			t.Fatalf("sync after modify failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if !strings.Contains(stdout, "Sync complete") {
			t.Errorf("expected 'Sync complete', got: %s", stdout)
		}
	})

	// ---- Scenario 5: Add file + re-sync ----
	t.Run("add_file_and_resync", func(t *testing.T) {
		os.WriteFile(filepath.Join(projectDir, "src", "handler.go"),
			[]byte("package main\n\nfunc handleRequest() {}\n"), 0o644)

		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "sync", projectDir, "--name", "my-project")
		if err != nil {
			t.Fatalf("sync after add failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if !strings.Contains(stdout, "Sync complete") {
			t.Errorf("expected 'Sync complete', got: %s", stdout)
		}
	})

	// ---- Scenario 6: Remove file + re-sync ----
	t.Run("remove_file_and_resync", func(t *testing.T) {
		os.Remove(filepath.Join(projectDir, "src", "utils.go"))

		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "sync", projectDir, "--name", "my-project")
		if err != nil {
			t.Fatalf("sync after remove failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if !strings.Contains(stdout, "Sync complete") {
			t.Errorf("expected 'Sync complete', got: %s", stdout)
		}
	})

	// ---- Scenario 7: Dry-run ----
	t.Run("dry_run", func(t *testing.T) {
		os.WriteFile(filepath.Join(projectDir, "CHANGELOG.md"),
			[]byte("# Changelog\n\n## v0.1\n- Init\n"), 0o644)

		stdout, _, err := runPufferfs(t, homeDir, serverURL, apiKey, "sync", projectDir, "--name", "my-project", "--dry-run")
		if err != nil {
			t.Fatalf("dry-run failed: %v\nstdout: %s", err, stdout)
		}
		if strings.Contains(stdout, "Sync complete") {
			t.Error("dry-run should not complete a sync")
		}
	})

	// ---- Scenario 8: Idempotent re-sync (no changes) ----
	t.Run("idempotent_resync", func(t *testing.T) {
		// Sync the pending CHANGELOG first
		runPufferfs(t, homeDir, serverURL, apiKey, "sync", projectDir, "--name", "my-project")

		// Re-sync with no changes
		stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "sync", projectDir, "--name", "my-project")
		if err != nil {
			t.Fatalf("idempotent sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if !strings.Contains(stdout, "No changes detected") {
			t.Errorf("expected 'No changes detected', got: %s", stdout)
		}
	})
}
