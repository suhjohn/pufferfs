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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
// Mock Modal + Turbopuffer
// ---------------------------------------------------------------------------

func cliStartMockModal(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/chunk", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FilePath string `json:"file_path"`
			FileType string `json:"file_type"`
			RootID   string `json:"root_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		resp := map[string]any{
			"count": 2,
			"chunks": []map[string]any{
				{
					"id":           fmt.Sprintf("chunk-%s-0", req.FilePath),
					"content":      fmt.Sprintf("Chunk 0 of %s: content here", req.FilePath),
					"file_path":    req.FilePath,
					"chunk_index":  0,
					"content_hash": fmt.Sprintf("sha256:hash0_%s", req.FilePath),
					"file_type":    req.FileType,
					"root_id":      req.RootID,
				},
				{
					"id":           fmt.Sprintf("chunk-%s-1", req.FilePath),
					"content":      fmt.Sprintf("Chunk 1 of %s: more content", req.FilePath),
					"file_path":    req.FilePath,
					"chunk_index":  1,
					"content_hash": fmt.Sprintf("sha256:hash1_%s", req.FilePath),
					"file_type":    req.FileType,
					"root_id":      req.RootID,
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/embed", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Chunks []map[string]any `json:"chunks"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		results := make([]map[string]any, len(req.Chunks))
		for i, chunk := range req.Chunks {
			emb := make([]any, 768)
			for j := range emb {
				emb[j] = float64(i*768+j) * 0.001
			}
			results[i] = map[string]any{"chunk": chunk, "embedding": emb}
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results, "count": len(results)})
	})

	mux.HandleFunc("/embed-query", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Texts []string `json:"texts"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		embeddings := make([][]float64, len(req.Texts))
		for i := range req.Texts {
			emb := make([]float64, 768)
			for j := range emb {
				emb[j] = float64(j) * 0.001
			}
			embeddings[i] = emb
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func cliStartMockTP(t *testing.T) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	namespaces := make(map[string][]map[string]any)

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/namespaces/"), "/")
		ns := parts[0]
		isQuery := len(parts) > 1 && parts[1] == "query"

		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)

		mu.Lock()
		defer mu.Unlock()

		if isQuery {
			docs := namespaces[ns]
			limit := 10
			if l, ok := req["limit"].(float64); ok {
				limit = int(l)
			}
			var rows []map[string]any
			for i, doc := range docs {
				if i >= limit {
					break
				}
				row := make(map[string]any)
				for k, v := range doc {
					if k != "vector" {
						row[k] = v
					}
				}
				row["$dist"] = float64(i) * 0.1
				rows = append(rows, row)
			}
			if _, ok := req["queries"]; ok {
				json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"rows": rows}, {"rows": rows}}})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"rows": rows})
			}
			return
		}

		if upsertRows, ok := req["upsert_rows"]; ok {
			rowSlice, _ := upsertRows.([]any)
			for _, r := range rowSlice {
				row, _ := r.(map[string]any)
				if id, ok := row["id"]; ok {
					docs := namespaces[ns]
					for i, d := range docs {
						if d["id"] == id {
							namespaces[ns] = append(docs[:i], docs[i+1:]...)
							break
						}
					}
				}
				namespaces[ns] = append(namespaces[ns], row)
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		if deleteFilter, ok := req["delete_by_filter"]; ok {
			filterArr, _ := deleteFilter.([]any)
			if len(filterArr) >= 3 {
				field, _ := filterArr[0].(string)
				value := filterArr[2]
				var remaining []map[string]any
				for _, doc := range namespaces[ns] {
					if doc[field] != value {
						remaining = append(remaining, doc)
					}
				}
				namespaces[ns] = remaining
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		if deletes, ok := req["deletes"]; ok {
			idSlice, _ := deletes.([]any)
			idSet := make(map[string]bool)
			for _, id := range idSlice {
				if s, ok := id.(string); ok {
					idSet[s] = true
				}
			}
			var remaining []map[string]any
			for _, doc := range namespaces[ns] {
				if !idSet[fmt.Sprintf("%v", doc["id"])] {
					remaining = append(remaining, doc)
				}
			}
			namespaces[ns] = remaining
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// Server process
// ---------------------------------------------------------------------------

type cliServerProcess struct {
	cmd  *exec.Cmd
	addr string
}

func cliStartServer(t *testing.T, modalURL, tpURL string) *cliServerProcess {
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
		"MODAL_CHUNK_ENDPOINT="+modalURL+"/chunk",
		"MODAL_EMBED_ENDPOINT="+modalURL+"/embed",
		"MODAL_QUERY_EMBED_ENDPOINT="+modalURL+"/embed-query",
		"TURBOPUFFER_API_KEY=test-key",
		"TURBOPUFFER_API_URL="+tpURL,
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

	modalSrv := cliStartMockModal(t)
	tpSrv := cliStartMockTP(t)
	srv := cliStartServer(t, modalSrv.URL, tpSrv.URL)
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
			stdout, stderr, err := runPufferfs(t, homeDir, serverURL, apiKey, "query", "pdf", "--root", "my-project", "--mode", "fts", "--glob", "*.pdf", "--top-k", "50")
			if err != nil {
				t.Fatalf("query PDF fixtures failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
			}
			if strings.Contains(stdout, "No results found") {
				t.Fatalf("expected PDF fixture results, got none")
			}
			for _, relPath := range pdfFixtures {
				if !strings.Contains(stdout, relPath) {
					t.Fatalf("expected PDF result for %s, got: %s", relPath, stdout)
				}
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
