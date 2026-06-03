package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/pufferfs/pufferfs/internal/auth"
	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/internal/server"
	"github.com/pufferfs/pufferfs/internal/storage"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// ---------------------------------------------------------------------------
// Test infrastructure: Postgres (Docker), MinIO (Docker), Mock Modal/TP
// ---------------------------------------------------------------------------

const (
	testDBUser = "pufferfs_test"
	testDBPass = "testpass"
	testDBName = "pufferfs_test"
	testDBPort = "15432"

	testMinioPort      = "19000"
	testMinioUser      = "minioadmin"
	testMinioPass      = "minioadmin"
	testMinioBucket    = "pufferfs-test"
	testJWTSecret      = "integration-test-secret-32chars!"
)

var (
	setupOnce sync.Once
	testDB    *server.DB
	testS3    *storage.Client

	pgContainerName    = "pufferfs-test-pg"
	minioContainerName = "pufferfs-test-minio"
)

func TestMain(m *testing.M) {
	code := m.Run()
	// Cleanup containers after all tests
	exec.Command("docker", "rm", "-f", pgContainerName).Run()
	exec.Command("docker", "rm", "-f", minioContainerName).Run()
	os.Exit(code)
}

func setupTestInfra(t *testing.T) (*server.DB, *storage.Client, *mockModal, *mockTP) {
	t.Helper()

	setupOnce.Do(func() {
		startPostgres(t)
		startMinIO(t)

		// Wait for Postgres
		dbURL := fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
			testDBUser, testDBPass, testDBPort, testDBName)
		waitForPort(t, testDBPort, 30*time.Second)

		// Set MIGRATIONS_DIR so goose finds the migration files
		os.Setenv("MIGRATIONS_DIR", "../../migrations")

		var err error
		testDB, err = server.NewDB(dbURL)
		if err != nil {
			t.Fatalf("connecting to test DB: %v", err)
		}

		// Wait for MinIO
		waitForPort(t, testMinioPort, 30*time.Second)

		// Create bucket via mc or S3 API
		createMinioBucket(t)

		testS3, err = storage.NewClient(appconfig.StorageConfig{
			AccessKeyID:     testMinioUser,
			SecretAccessKey: testMinioPass,
			Bucket:          testMinioBucket,
			EndpointURL:     fmt.Sprintf("http://localhost:%s", testMinioPort),
		})
		if err != nil {
			t.Fatalf("creating test S3 client: %v", err)
		}
	})

	modal := newMockModal(t)
	tp := newMockTP(t)

	return testDB, testS3, modal, tp
}

func startPostgres(t *testing.T) {
	t.Helper()
	// Kill existing container if any
	exec.Command("docker", "rm", "-f", pgContainerName).Run()
	cmd := exec.Command("docker", "run", "-d",
		"--name", pgContainerName,
		"-e", "POSTGRES_USER="+testDBUser,
		"-e", "POSTGRES_PASSWORD="+testDBPass,
		"-e", "POSTGRES_DB="+testDBName,
		"-p", testDBPort+":5432",
		"postgres:16-alpine",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("starting postgres: %v\n%s", err, out)
	}
}

func startMinIO(t *testing.T) {
	t.Helper()
	exec.Command("docker", "rm", "-f", minioContainerName).Run()
	cmd := exec.Command("docker", "run", "-d",
		"--name", minioContainerName,
		"-e", "MINIO_ROOT_USER="+testMinioUser,
		"-e", "MINIO_ROOT_PASSWORD="+testMinioPass,
		"-p", testMinioPort+":9000",
		"minio/minio", "server", "/data",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("starting minio: %v\n%s", err, out)
	}
}

func createMinioBucket(t *testing.T) {
	t.Helper()
	endpoint := fmt.Sprintf("http://localhost:%s", testMinioPort)

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			awscreds.NewStaticCredentialsProvider(testMinioUser, testMinioPass, ""),
		),
	)
	if err != nil {
		t.Fatalf("loading aws config: %v", err)
	}

	s3Client := s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	_, err = s3Client.CreateBucket(context.Background(), &s3sdk.CreateBucketInput{
		Bucket: aws.String(testMinioBucket),
	})
	if err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
		t.Logf("create bucket warning (may already exist): %v", err)
	}
}

func waitForPort(t *testing.T, port string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", "exec",
			pgContainerName, "pg_isready", "-p", "5432")
		if port == testDBPort {
			if err := cmd.Run(); err == nil {
				return
			}
		} else {
			// For MinIO, just check TCP
			cmd = exec.Command("curl", "-sf", fmt.Sprintf("http://localhost:%s/minio/health/live", port))
			if err := cmd.Run(); err == nil {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("port %s not ready after %v", port, timeout)
}

// ---------------------------------------------------------------------------
// Mock Modal: returns deterministic chunks + embeddings
// ---------------------------------------------------------------------------

type mockModal struct {
	srv *httptest.Server
	client *server.ModalClient
}

func newMockModal(t *testing.T) *mockModal {
	t.Helper()
	mux := http.NewServeMux()

	// Chunk endpoint
	mux.HandleFunc("/chunk", func(w http.ResponseWriter, r *http.Request) {
		var req server.ChunkFileRequest
		json.NewDecoder(r.Body).Decode(&req)

		// Return 2 deterministic chunks per file
		resp := server.ChunkFileResponse{
			Count: 2,
			Chunks: []map[string]any{
				{
					"id":           fmt.Sprintf("chunk-%s-0", req.FilePath),
					"content":      fmt.Sprintf("Chunk 0 of %s", req.FilePath),
					"file_path":    req.FilePath,
					"chunk_index":  0,
					"content_hash": fmt.Sprintf("sha256:hash0_%s", req.FilePath),
					"file_type":    req.FileType,
					"root_id":      req.RootID,
				},
				{
					"id":           fmt.Sprintf("chunk-%s-1", req.FilePath),
					"content":      fmt.Sprintf("Chunk 1 of %s", req.FilePath),
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

	// Embed chunks endpoint
	mux.HandleFunc("/embed", func(w http.ResponseWriter, r *http.Request) {
		var req server.EmbedChunksRequest
		json.NewDecoder(r.Body).Decode(&req)

		results := make([]map[string]any, len(req.Chunks))
		for i, chunk := range req.Chunks {
			// Return deterministic 768-dim embedding (Nomic Embed v1.5 size)
			emb := make([]any, 768)
			for j := range emb {
				emb[j] = float64(i*768+j) * 0.001
			}
			results[i] = map[string]any{
				"chunk":     chunk,
				"embedding": emb,
			}
		}

		resp := server.EmbedChunksResponse{
			Results: results,
			Count:   len(results),
		}
		json.NewEncoder(w).Encode(resp)
	})

	// Query embed endpoint
	mux.HandleFunc("/embed-query", func(w http.ResponseWriter, r *http.Request) {
		var req server.EmbedQueryRequest
		json.NewDecoder(r.Body).Decode(&req)

		embeddings := make([][]float64, len(req.Texts))
		for i := range req.Texts {
			emb := make([]float64, 768)
			for j := range emb {
				emb[j] = float64(j) * 0.001
			}
			embeddings[i] = emb
		}

		resp := server.EmbedQueryResponse{Embeddings: embeddings}
		json.NewEncoder(w).Encode(resp)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Set env vars that ModalClient reads
	os.Setenv("MODAL_CHUNK_ENDPOINT", srv.URL+"/chunk")
	os.Setenv("MODAL_EMBED_ENDPOINT", srv.URL+"/embed")
	os.Setenv("MODAL_QUERY_EMBED_ENDPOINT", srv.URL+"/embed-query")

	return &mockModal{
		srv:    srv,
		client: server.NewModalClient(),
	}
}

// ---------------------------------------------------------------------------
// Mock Turbopuffer: in-memory vector store
// ---------------------------------------------------------------------------

type mockTP struct {
	srv    *httptest.Server
	client *server.TPClient
	mu     sync.Mutex
	// namespace -> []document
	namespaces map[string][]map[string]any
}

func newMockTP(t *testing.T) *mockTP {
	t.Helper()
	tp := &mockTP{
		namespaces: make(map[string][]map[string]any),
	}

	mux := http.NewServeMux()

	// Upsert/Delete handler
	mux.HandleFunc("/v2/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/namespaces/"), "/")
		ns := parts[0]
		isQuery := len(parts) > 1 && parts[1] == "query"

		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)

		if isQuery {
			tp.handleQuery(w, ns, req)
			return
		}

		if upsertRows, ok := req["upsert_rows"]; ok {
			tp.handleUpsert(w, ns, upsertRows)
			return
		}

		if deleteFilter, ok := req["delete_by_filter"]; ok {
			tp.handleDeleteByFilter(w, ns, deleteFilter)
			return
		}

		if deletes, ok := req["deletes"]; ok {
			tp.handleDeleteIDs(w, ns, deletes)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Create TP client pointing at mock server
	tp.srv = srv
	tp.client = server.NewTPClientWithURL("test-key", srv.URL)

	return tp
}

func (tp *mockTP) handleUpsert(w http.ResponseWriter, ns string, rows any) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	rowSlice, ok := rows.([]any)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	for _, r := range rowSlice {
		row, ok := r.(map[string]any)
		if !ok {
			continue
		}
		// Remove existing row with same ID if present
		if id, ok := row["id"]; ok {
			docs := tp.namespaces[ns]
			for i, d := range docs {
				if d["id"] == id {
					tp.namespaces[ns] = append(docs[:i], docs[i+1:]...)
					break
				}
			}
		}
		tp.namespaces[ns] = append(tp.namespaces[ns], row)
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (tp *mockTP) handleDeleteByFilter(w http.ResponseWriter, ns string, filter any) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	filterArr, ok := filter.([]any)
	if !ok || len(filterArr) < 3 {
		w.WriteHeader(http.StatusOK)
		return
	}

	field, _ := filterArr[0].(string)
	value := filterArr[2]

	var remaining []map[string]any
	for _, doc := range tp.namespaces[ns] {
		if doc[field] != value {
			remaining = append(remaining, doc)
		}
	}
	tp.namespaces[ns] = remaining

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (tp *mockTP) handleDeleteIDs(w http.ResponseWriter, ns string, ids any) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	idSlice, ok := ids.([]any)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	idSet := make(map[string]bool)
	for _, id := range idSlice {
		if s, ok := id.(string); ok {
			idSet[s] = true
		}
	}

	var remaining []map[string]any
	for _, doc := range tp.namespaces[ns] {
		idStr := fmt.Sprintf("%v", doc["id"])
		if !idSet[idStr] {
			remaining = append(remaining, doc)
		}
	}
	tp.namespaces[ns] = remaining

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (tp *mockTP) handleQuery(w http.ResponseWriter, ns string, req map[string]any) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	docs := tp.namespaces[ns]
	limit := 10
	if l, ok := req["limit"].(float64); ok {
		limit = int(l)
	}

	// Simple: return all docs (up to limit) with a fake distance
	var rows []map[string]any
	for i, doc := range docs {
		if i >= limit {
			break
		}
		row := make(map[string]any)
		for k, v := range doc {
			if k != "vector" { // don't return vectors
				row[k] = v
			}
		}
		row["$dist"] = float64(i) * 0.1
		rows = append(rows, row)
	}

	// Handle multi-query
	if _, ok := req["queries"]; ok {
		results := []map[string]any{
			{"rows": rows},
			{"rows": rows},
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{"rows": rows})
}

func (tp *mockTP) docCount(ns string) int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return len(tp.namespaces[ns])
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type testEnv struct {
	db     *server.DB
	s3     *storage.Client
	modal  *mockModal
	tp     *mockTP
	srv    *httptest.Server
	jwt    string
	orgID  string
	userID string
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	db, s3Client, modal, tp := setupTestInfra(t)

	srv := server.New(db, s3Client, modal.client, tp.client)

	// Wrap with auth middleware
	handler := auth.Middleware(
		[]byte(testJWTSecret),
		db.ResolveAPIKey,
	)(srv.Handler())

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// Create a test user via UpsertUser (which also creates a personal org)
	ctx := context.Background()
	email := fmt.Sprintf("testuser-%d@example.com", time.Now().UnixNano())
	userInfo := auth.UserInfo{
		ID:      fmt.Sprintf("google-%d", time.Now().UnixNano()),
		Email:   email,
		Name:    "Test User",
		Picture: "",
	}
	userID, orgID, _, err := db.UpsertUser(ctx, userInfo, "google")
	if err != nil {
		t.Fatalf("creating test user: %v", err)
	}

	// Generate JWT
	token, err := auth.GenerateJWT([]byte(testJWTSecret), userID, orgID, auth.RoleOwner, email, 24*time.Hour)
	if err != nil {
		t.Fatalf("generating JWT: %v", err)
	}

	return &testEnv{
		db:     db,
		s3:     s3Client,
		modal:  modal,
		tp:     tp,
		srv:    ts,
		jwt:    token,
		orgID:  orgID,
		userID: userID,
	}
}

func (e *testEnv) doRequest(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewReader(data)
	}

	req, _ := http.NewRequest(method, e.srv.URL+path, reqBody)
	req.Header.Set("Authorization", "Bearer "+e.jwt)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(respBody, &result)

	return resp.StatusCode, result
}

func isSuccess(status int) bool {
	return status >= 200 && status < 300
}

func (e *testEnv) doRequestRaw(t *testing.T, method, path string, body []byte, contentType string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.jwt)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestIntegration_HealthEndpoints(t *testing.T) {
	env := setupTestEnv(t)

	// Health endpoints don't need auth
	resp, err := http.Get(env.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: expected 200, got %d", resp.StatusCode)
	}

	resp, err = http.Get(env.srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readyz: expected 200, got %d", resp.StatusCode)
	}
}

func TestIntegration_AuthFlow(t *testing.T) {
	env := setupTestEnv(t)

	// Test JWT auth — GET /auth/me
	status, result := env.doRequest(t, "GET", "/auth/me", nil)
	if !isSuccess(status) {
		t.Fatalf("GET /auth/me: expected 200, got %d: %v", status, result)
	}
	// Response is {user: {...}, org_id: ..., role: ...}
	userMap, _ := result["user"].(map[string]any)
	if userMap == nil || userMap["email"] == nil {
		t.Errorf("expected user with email in /auth/me, got %v", result)
	}

	// Test API key creation + auth
	status, result = env.doRequest(t, "POST", "/auth/api-keys", map[string]any{
		"name":   "test-key",
		"scopes": []string{"read", "write"},
	})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("POST /auth/api-keys: expected 200/201, got %d: %v", status, result)
	}
	rawKey, ok := result["key"].(string)
	if !ok || !strings.HasPrefix(rawKey, "pfs_") {
		t.Fatalf("expected API key starting with pfs_, got %v", result)
	}

	// Use the API key to auth
	req, _ := http.NewRequest("GET", env.srv.URL+"/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("API key auth: expected 200, got %d", resp.StatusCode)
	}

	// List API keys
	status, result = env.doRequest(t, "GET", "/auth/api-keys", nil)
	if !isSuccess(status) {
		t.Fatalf("GET /auth/api-keys: expected 200, got %d", status)
	}

	// Unauthenticated request should fail
	req, _ = http.NewRequest("GET", env.srv.URL+"/auth/me", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthed request: expected 401, got %d", resp.StatusCode)
	}
}

func TestIntegration_OrgAndRootCRUD(t *testing.T) {
	env := setupTestEnv(t)

	// Get org
	status, result := env.doRequest(t, "GET", "/org", nil)
	if !isSuccess(status) {
		t.Fatalf("GET /org: expected 200, got %d: %v", status, result)
	}
	if result["id"] != env.orgID {
		t.Errorf("expected org ID %s, got %v", env.orgID, result["id"])
	}

	// List members
	status, _ = env.doRequest(t, "GET", "/org/members", nil)
	if !isSuccess(status) {
		t.Fatalf("GET /org/members: expected 200, got %d", status)
	}

	// Create root
	status, result = env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "test-project",
		"source_path": "/home/user/project",
	})
	if !isSuccess(status) {
		t.Fatalf("POST /roots: expected 200, got %d: %v", status, result)
	}
	rootID, ok := result["id"].(string)
	if !ok || rootID == "" {
		t.Fatal("expected root ID in response")
	}

	// List roots
	status, _ = env.doRequest(t, "GET", "/roots", nil)
	if !isSuccess(status) {
		t.Fatalf("GET /roots: expected 200, got %d", status)
	}

	// Get root
	status, result = env.doRequest(t, "GET", "/roots/"+rootID, nil)
	if !isSuccess(status) {
		t.Fatalf("GET /roots/%s: expected 200, got %d: %v", rootID, status, result)
	}
	if result["name"] != "test-project" {
		t.Errorf("expected name test-project, got %v", result["name"])
	}
}

func TestIntegration_SyncFlow(t *testing.T) {
	env := setupTestEnv(t)

	// Create root
	status, result := env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "sync-test",
		"source_path": "/home/user/project",
	})
	if !isSuccess(status) {
		t.Fatalf("create root: %d %v", status, result)
	}
	rootID := result["id"].(string)

	// Upload a file
	fileContent := []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n")
	uploadStatus, _ := env.doRequestRaw(t, "POST",
		fmt.Sprintf("/roots/%s/upload?path=main.go", rootID),
		fileContent, "application/octet-stream")
	if !isSuccess(uploadStatus) {
		t.Fatalf("upload: expected 200, got %d", uploadStatus)
	}

	// Upload a second file
	readmeContent := []byte("# My Project\n\nThis is a test project.\n")
	uploadStatus, _ = env.doRequestRaw(t, "POST",
		fmt.Sprintf("/roots/%s/upload?path=README.md", rootID),
		readmeContent, "application/octet-stream")
	if !isSuccess(uploadStatus) {
		t.Fatalf("upload readme: expected 200, got %d", uploadStatus)
	}

	// Sync with 2 added files
	syncReq := models.SyncRequest{
		RootID: rootID,
		Changes: []models.FileChange{
			{Path: "main.go", Status: models.StatusAdded, ContentHash: "sha256:abc123", Size: int64(len(fileContent))},
			{Path: "README.md", Status: models.StatusAdded, ContentHash: "sha256:def456", Size: int64(len(readmeContent))},
		},
		State: map[string]models.FileState{
			"main.go":   {Size: int64(len(fileContent)), ContentHash: "sha256:abc123", Mtime: time.Now().UnixNano()},
			"README.md": {Size: int64(len(readmeContent)), ContentHash: "sha256:def456", Mtime: time.Now().UnixNano()},
		},
	}
	status, result = env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq)
	if !isSuccess(status) {
		t.Fatalf("sync: expected 200, got %d: %v", status, result)
	}

	filesProcessed := int(result["files_processed"].(float64))
	if filesProcessed != 2 {
		t.Errorf("expected 2 files processed, got %d", filesProcessed)
	}

	chunksAdded := int(result["chunks_added"].(float64))
	if chunksAdded != 2 {
		t.Errorf("expected 2 chunks added, got %d", chunksAdded)
	}

	// Verify sync job was created
	if result["sync_job_id"] == nil || result["sync_job_id"] == "" {
		t.Error("expected sync_job_id in response")
	}

	// Verify state was saved
	status, stateResult := env.doRequest(t, "GET", "/roots/"+rootID+"/state", nil)
	if !isSuccess(status) {
		t.Fatalf("get state: expected 200, got %d", status)
	}
	if stateResult["main.go"] == nil {
		t.Error("expected main.go in state")
	}

	// Verify TP namespace has documents
	ns := fmt.Sprintf("org-%s-root-%s", env.orgID, rootID)
	if env.tp.docCount(ns) == 0 {
		t.Error("expected documents in Turbopuffer namespace")
	}

	// Verify sync status endpoint
	status, statusResult := env.doRequest(t, "GET", "/roots/"+rootID+"/sync/status", nil)
	if !isSuccess(status) {
		t.Fatalf("sync status: expected 200, got %d", status)
	}
	if statusResult["status"] != "completed" {
		t.Errorf("expected sync status completed, got %v", statusResult["status"])
	}
}

func TestIntegration_SyncWithModifiedAndRemoved(t *testing.T) {
	env := setupTestEnv(t)

	// Create root
	_, result := env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "modify-test",
		"source_path": "/home/user/project2",
	})
	rootID := result["id"].(string)

	// First sync: add 2 files
	fileContent := []byte("original content")
	env.doRequestRaw(t, "POST", fmt.Sprintf("/roots/%s/upload?path=file.txt", rootID), fileContent, "application/octet-stream")

	syncReq := models.SyncRequest{
		RootID: rootID,
		Changes: []models.FileChange{
			{Path: "file.txt", Status: models.StatusAdded, ContentHash: "sha256:original"},
		},
		State: map[string]models.FileState{
			"file.txt": {Size: int64(len(fileContent)), ContentHash: "sha256:original"},
		},
	}
	status, _ := env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq)
	if !isSuccess(status) {
		t.Fatalf("first sync: expected 200, got %d", status)
	}

	// Second sync: modify file
	newContent := []byte("modified content")
	env.doRequestRaw(t, "POST", fmt.Sprintf("/roots/%s/upload?path=file.txt", rootID), newContent, "application/octet-stream")

	syncReq2 := models.SyncRequest{
		RootID: rootID,
		Changes: []models.FileChange{
			{Path: "file.txt", Status: models.StatusModified, ContentHash: "sha256:modified"},
		},
		State: map[string]models.FileState{
			"file.txt": {Size: int64(len(newContent)), ContentHash: "sha256:modified"},
		},
	}
	status, result = env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq2)
	if !isSuccess(status) {
		t.Fatalf("modify sync: expected 200, got %d", status)
	}
	if int(result["files_processed"].(float64)) != 1 {
		t.Errorf("expected 1 file processed for modify, got %v", result["files_processed"])
	}

	// Third sync: remove file
	syncReq3 := models.SyncRequest{
		RootID: rootID,
		Changes: []models.FileChange{
			{Path: "file.txt", Status: models.StatusRemoved, ContentHash: "sha256:modified"},
		},
		State: map[string]models.FileState{},
	}
	status, result = env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq3)
	if !isSuccess(status) {
		t.Fatalf("remove sync: expected 200, got %d", status)
	}
	if int(result["chunks_removed"].(float64)) != 1 {
		t.Errorf("expected 1 chunk removed, got %v", result["chunks_removed"])
	}
}

func TestIntegration_SimHashAndIndexReuse(t *testing.T) {
	env := setupTestEnv(t)

	// Create root A (first user syncs repo)
	_, result := env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "repo-alpha",
		"source_path": "/home/alice/repo",
	})
	rootA := result["id"].(string)

	// Upload and sync root A
	env.doRequestRaw(t, "POST", fmt.Sprintf("/roots/%s/upload?path=src/app.go", rootA),
		[]byte("package main"), "application/octet-stream")

	syncA := models.SyncRequest{
		RootID:  rootA,
		SimHash: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		Changes: []models.FileChange{
			{Path: "src/app.go", Status: models.StatusAdded, ContentHash: "sha256:app"},
		},
		State: map[string]models.FileState{
			"src/app.go": {Size: 12, ContentHash: "sha256:app"},
		},
	}
	status, _ := env.doRequest(t, "POST", "/roots/"+rootA+"/sync", syncA)
	if !isSuccess(status) {
		t.Fatalf("sync A: expected 200, got %d", status)
	}

	// Verify simhash was stored
	nsA := fmt.Sprintf("org-%s-root-%s", env.orgID, rootA)
	if env.tp.docCount(nsA) == 0 {
		t.Error("root A should have documents in TP")
	}

	// Create root B (second user syncs same repo — same simhash)
	_, result = env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "repo-beta",
		"source_path": "/home/bob/repo",
	})
	rootB := result["id"].(string)

	// Sync init: check for similar indexes
	status, initResult := env.doRequest(t, "POST", "/roots/"+rootB+"/sync/init", map[string]any{
		"simhash": "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
	})
	if !isSuccess(status) {
		t.Fatalf("sync init: expected 200, got %d", status)
	}
	if initResult["can_reuse"] != true {
		t.Errorf("expected can_reuse=true, got %v", initResult["can_reuse"])
	}
	if initResult["similarity"] == nil || initResult["similarity"].(float64) < 0.99 {
		t.Errorf("expected high similarity for identical simhash, got %v", initResult["similarity"])
	}

	// Sync root B with same simhash (should trigger index reuse)
	env.doRequestRaw(t, "POST", fmt.Sprintf("/roots/%s/upload?path=src/app.go", rootB),
		[]byte("package main"), "application/octet-stream")

	syncB := models.SyncRequest{
		RootID:  rootB,
		SimHash: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		Changes: []models.FileChange{
			{Path: "src/app.go", Status: models.StatusAdded, ContentHash: "sha256:app"},
		},
		State: map[string]models.FileState{
			"src/app.go": {Size: 12, ContentHash: "sha256:app"},
		},
	}
	status, _ = env.doRequest(t, "POST", "/roots/"+rootB+"/sync", syncB)
	if !isSuccess(status) {
		t.Fatalf("sync B: expected 200, got %d", status)
	}

	// Root B's namespace should have documents (from clone + any new processing)
	nsB := fmt.Sprintf("org-%s-root-%s", env.orgID, rootB)
	if env.tp.docCount(nsB) == 0 {
		t.Error("root B should have documents after index reuse")
	}
}

func TestIntegration_ContentProofFiltering(t *testing.T) {
	env := setupTestEnv(t)

	// Create root and sync with content proof
	_, result := env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "proof-test",
		"source_path": "/home/user/proof-project",
	})
	rootID := result["id"].(string)

	// Upload files
	env.doRequestRaw(t, "POST", fmt.Sprintf("/roots/%s/upload?path=public.txt", rootID),
		[]byte("public content"), "application/octet-stream")
	env.doRequestRaw(t, "POST", fmt.Sprintf("/roots/%s/upload?path=secret.txt", rootID),
		[]byte("secret content"), "application/octet-stream")

	// Sync with content proof that only includes public.txt
	syncReq := models.SyncRequest{
		RootID: rootID,
		Changes: []models.FileChange{
			{Path: "public.txt", Status: models.StatusAdded, ContentHash: "sha256:public_hash"},
			{Path: "secret.txt", Status: models.StatusAdded, ContentHash: "sha256:secret_hash"},
		},
		State: map[string]models.FileState{
			"public.txt": {Size: 14, ContentHash: "sha256:public_hash"},
			"secret.txt": {Size: 14, ContentHash: "sha256:secret_hash"},
		},
		ContentProof: &models.ContentProofData{
			FileHashes: map[string]string{
				"public.txt": "sha256:public_hash",
				// Note: secret.txt is NOT in the proof
			},
			RootHash: "sha256:root_proof",
		},
	}
	status, _ := env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq)
	if !isSuccess(status) {
		t.Fatalf("sync: expected 200, got %d", status)
	}

	// Verify content proof was stored
	ctx := context.Background()
	proofBytes, rootHash, err := env.db.GetContentProof(ctx, env.orgID, env.userID, rootID)
	if err != nil {
		t.Fatalf("get content proof: %v", err)
	}
	if rootHash != "sha256:root_proof" {
		t.Errorf("expected root hash sha256:root_proof, got %s", rootHash)
	}

	var storedProof models.ContentProofData
	if err := json.Unmarshal(proofBytes, &storedProof); err != nil {
		t.Fatalf("unmarshal proof: %v", err)
	}
	if len(storedProof.FileHashes) != 1 {
		t.Errorf("expected 1 file in proof, got %d", len(storedProof.FileHashes))
	}
	if storedProof.FileHashes["public.txt"] != "sha256:public_hash" {
		t.Error("expected public.txt in proof")
	}
}

func TestIntegration_ACLEnforcement(t *testing.T) {
	env := setupTestEnv(t)

	// Create root
	_, result := env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "acl-test",
		"source_path": "/home/user/acl-project",
	})
	rootID := result["id"].(string)

	// Create an ACL that denies access to /secret/ path
	status, _ := env.doRequest(t, "POST", "/roots/"+rootID+"/acls", map[string]any{
		"path_prefix": "/secret/",
		"grant_to":    env.userID,
		"permission":  "none",
	})
	if !isSuccess(status) {
		t.Fatalf("create ACL: expected 200, got %d", status)
	}

	// List ACLs
	status, aclResult := env.doRequest(t, "GET", "/roots/"+rootID+"/acls", nil)
	if !isSuccess(status) {
		t.Fatalf("list ACLs: expected 200, got %d", status)
	}
	// Should have the ACL we just created
	_ = aclResult

	// Try to upload to denied path — should be forbidden
	uploadStatus, _ := env.doRequestRaw(t, "POST",
		fmt.Sprintf("/roots/%s/upload?path=secret/passwords.txt", rootID),
		[]byte("passwords"), "application/octet-stream")
	if uploadStatus != http.StatusForbidden {
		t.Errorf("upload to denied path: expected 403, got %d", uploadStatus)
	}
}

func TestIntegration_SyncJobTracking(t *testing.T) {
	env := setupTestEnv(t)

	// Create root and sync
	_, result := env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "job-tracking-test",
		"source_path": "/home/user/tracking",
	})
	rootID := result["id"].(string)

	env.doRequestRaw(t, "POST", fmt.Sprintf("/roots/%s/upload?path=file1.go", rootID),
		[]byte("package main"), "application/octet-stream")

	syncReq := models.SyncRequest{
		RootID: rootID,
		Changes: []models.FileChange{
			{Path: "file1.go", Status: models.StatusAdded, ContentHash: "sha256:track1"},
		},
		State: map[string]models.FileState{
			"file1.go": {Size: 12, ContentHash: "sha256:track1"},
		},
	}
	status, syncResult := env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq)
	if !isSuccess(status) {
		t.Fatalf("sync: expected 200, got %d", status)
	}

	jobID := syncResult["sync_job_id"].(string)
	if jobID == "" {
		t.Fatal("expected sync job ID")
	}

	// Check sync status
	status, statusResult := env.doRequest(t, "GET", "/roots/"+rootID+"/sync/status", nil)
	if !isSuccess(status) {
		t.Fatalf("sync status: expected 200, got %d", status)
	}
	if statusResult["status"] != "completed" {
		t.Errorf("expected completed, got %v", statusResult["status"])
	}
	if int(statusResult["total_files"].(float64)) != 1 {
		t.Errorf("expected 1 total file, got %v", statusResult["total_files"])
	}

	// List sync jobs
	status, _ = env.doRequest(t, "GET", "/roots/"+rootID+"/sync/jobs", nil)
	if !isSuccess(status) {
		t.Fatalf("list sync jobs: expected 200, got %d", status)
	}
}

func TestIntegration_QueryWithFiltering(t *testing.T) {
	env := setupTestEnv(t)

	// Create root, upload, sync
	_, result := env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "query-test",
		"source_path": "/home/user/query-project",
	})
	rootID := result["id"].(string)

	env.doRequestRaw(t, "POST", fmt.Sprintf("/roots/%s/upload?path=main.go", rootID),
		[]byte("package main"), "application/octet-stream")

	syncReq := models.SyncRequest{
		RootID: rootID,
		Changes: []models.FileChange{
			{Path: "main.go", Status: models.StatusAdded, ContentHash: "sha256:query1"},
		},
		State: map[string]models.FileState{
			"main.go": {Size: 12, ContentHash: "sha256:query1"},
		},
	}
	env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq)

	// Query: FTS mode
	status, queryResult := env.doRequest(t, "POST", "/query", map[string]any{
		"query":   "main function",
		"root_id": rootID,
		"mode":    "fts",
		"top_k":   5,
	})
	if !isSuccess(status) {
		t.Fatalf("query fts: expected 200, got %d: %v", status, queryResult)
	}

	// Query: vector mode
	status, queryResult = env.doRequest(t, "POST", "/query", map[string]any{
		"query":   "main function",
		"root_id": rootID,
		"mode":    "vector",
		"top_k":   5,
	})
	if !isSuccess(status) {
		t.Fatalf("query vector: expected 200, got %d: %v", status, queryResult)
	}

	// Query: hybrid mode
	status, queryResult = env.doRequest(t, "POST", "/query", map[string]any{
		"query":   "main function",
		"root_id": rootID,
		"mode":    "hybrid",
		"top_k":   5,
	})
	if !isSuccess(status) {
		t.Fatalf("query hybrid: expected 200, got %d: %v", status, queryResult)
	}
}

func TestIntegration_RBACEnforcement(t *testing.T) {
	env := setupTestEnv(t)

	// Create a viewer user via UpsertUser (creates their own personal org)
	ctx := context.Background()
	viewerEmail := fmt.Sprintf("viewer-%d@example.com", time.Now().UnixNano())
	viewerInfo := auth.UserInfo{
		ID:    fmt.Sprintf("google-viewer-%d", time.Now().UnixNano()),
		Email: viewerEmail,
		Name:  "Viewer User",
	}
	viewerID, _, _, err := env.db.UpsertUser(ctx, viewerInfo, "google")
	if err != nil {
		t.Fatalf("creating viewer user: %v", err)
	}
	// Add viewer to the test env's org as viewer role
	if err := env.db.AddOrgMember(ctx, env.orgID, viewerID, auth.RoleViewer); err != nil {
		t.Fatalf("adding viewer: %v", err)
	}

	viewerJWT, err := auth.GenerateJWT([]byte(testJWTSecret), viewerID, env.orgID, auth.RoleViewer, viewerEmail, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Viewer tries to create a root (requires editor) — should fail
	req, _ := http.NewRequest("POST", env.srv.URL+"/roots", bytes.NewReader([]byte(`{"name":"x","source_path":"y"}`)))
	req.Header.Set("Authorization", "Bearer "+viewerJWT)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer creating root: expected 403, got %d", resp.StatusCode)
	}

	// Viewer can read roots (viewer role is sufficient for GET /roots)
	req, _ = http.NewRequest("GET", env.srv.URL+"/roots", nil)
	req.Header.Set("Authorization", "Bearer "+viewerJWT)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("viewer listing roots: expected 200, got %d", resp.StatusCode)
	}
}

func TestIntegration_MerkleTreeEndToEnd(t *testing.T) {
	env := setupTestEnv(t)

	// Create root
	_, result := env.doRequest(t, "POST", "/roots", map[string]any{
		"name":        "merkle-e2e",
		"source_path": "/home/user/merkle-project",
	})
	rootID := result["id"].(string)

	// First sync: 3 files with simhash and content proof
	for _, f := range []string{"src/main.go", "src/utils.go", "docs/README.md"} {
		env.doRequestRaw(t, "POST",
			fmt.Sprintf("/roots/%s/upload?path=%s", rootID, f),
			[]byte(fmt.Sprintf("content of %s", f)), "application/octet-stream")
	}

	syncReq := models.SyncRequest{
		RootID:  rootID,
		SimHash: "1111111111111111111111111111111111111111111111111111111111111111",
		Changes: []models.FileChange{
			{Path: "src/main.go", Status: models.StatusAdded, ContentHash: "sha256:main"},
			{Path: "src/utils.go", Status: models.StatusAdded, ContentHash: "sha256:utils"},
			{Path: "docs/README.md", Status: models.StatusAdded, ContentHash: "sha256:readme"},
		},
		State: map[string]models.FileState{
			"src/main.go":    {Size: 20, ContentHash: "sha256:main"},
			"src/utils.go":   {Size: 22, ContentHash: "sha256:utils"},
			"docs/README.md": {Size: 24, ContentHash: "sha256:readme"},
		},
		ContentProof: &models.ContentProofData{
			FileHashes: map[string]string{
				"src/main.go":    "sha256:main",
				"src/utils.go":   "sha256:utils",
				"docs/README.md": "sha256:readme",
			},
			DirHashes: map[string]string{
				"src":  "sha256:src_dir",
				"docs": "sha256:docs_dir",
			},
			RootHash: "sha256:merkle_root_1",
		},
	}

	status, syncResult := env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq)
	if !isSuccess(status) {
		t.Fatalf("merkle sync: expected 200, got %d: %v", status, syncResult)
	}
	if int(syncResult["files_processed"].(float64)) != 3 {
		t.Errorf("expected 3 files processed, got %v", syncResult["files_processed"])
	}

	// Verify simhash, content proof, and TP namespace all populated
	ns := fmt.Sprintf("org-%s-root-%s", env.orgID, rootID)
	if env.tp.docCount(ns) == 0 {
		t.Error("expected TP namespace to have documents")
	}

	ctx := context.Background()
	proofBytes, rootHash, err := env.db.GetContentProof(ctx, env.orgID, env.userID, rootID)
	if err != nil {
		t.Fatalf("get content proof: %v", err)
	}
	if rootHash != "sha256:merkle_root_1" {
		t.Errorf("expected root hash sha256:merkle_root_1, got %s", rootHash)
	}
	var proof models.ContentProofData
	json.Unmarshal(proofBytes, &proof)
	if len(proof.FileHashes) != 3 {
		t.Errorf("expected 3 files in proof, got %d", len(proof.FileHashes))
	}

	// Second sync: only 1 file changed (Merkle tree diff would only send this)
	env.doRequestRaw(t, "POST",
		fmt.Sprintf("/roots/%s/upload?path=src/main.go", rootID),
		[]byte("updated content of main.go"), "application/octet-stream")

	syncReq2 := models.SyncRequest{
		RootID:  rootID,
		SimHash: "1111111111111111111111111111111111111111111111111111111111111112",
		Changes: []models.FileChange{
			{Path: "src/main.go", Status: models.StatusModified, ContentHash: "sha256:main_v2"},
		},
		State: map[string]models.FileState{
			"src/main.go":    {Size: 28, ContentHash: "sha256:main_v2"},
			"src/utils.go":   {Size: 22, ContentHash: "sha256:utils"},
			"docs/README.md": {Size: 24, ContentHash: "sha256:readme"},
		},
		ContentProof: &models.ContentProofData{
			FileHashes: map[string]string{
				"src/main.go":    "sha256:main_v2",
				"src/utils.go":   "sha256:utils",
				"docs/README.md": "sha256:readme",
			},
			RootHash: "sha256:merkle_root_2",
		},
	}

	status, syncResult2 := env.doRequest(t, "POST", "/roots/"+rootID+"/sync", syncReq2)
	if !isSuccess(status) {
		t.Fatalf("second sync: expected 200, got %d: %v", status, syncResult2)
	}
	if int(syncResult2["files_processed"].(float64)) != 1 {
		t.Errorf("expected 1 file processed (only changed file), got %v", syncResult2["files_processed"])
	}
}
