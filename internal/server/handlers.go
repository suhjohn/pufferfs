package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/internal/storage"
	"github.com/pufferfs/pufferfs/pkg/models"
)

type objectStore interface {
	Upload(ctx context.Context, key string, data []byte, contentType string) error
	UploadCAS(ctx context.Context, key string, data []byte, contentType, ifMatch, ifNoneMatch string) (string, error)
	Download(ctx context.Context, key string) ([]byte, error)
	DownloadWithETag(ctx context.Context, key string) ([]byte, string, error)
	DownloadRange(ctx context.Context, key string, offset, length int64) ([]byte, error)
	DeleteMany(ctx context.Context, keys []string) error
}

// Server holds the dependencies for HTTP handlers.
type Server struct {
	db    *DB
	s3    objectStore
	modal *ModalClient
	tp    *TPClient
	queue queue.Queue
	mux   *http.ServeMux
}

// New creates a new Server with all dependencies.
func New(db *DB, s3 *storage.Client, modal *ModalClient, tp *TPClient) *Server {
	return NewWithStore(db, s3, modal, tp)
}

func NewWithStore(db *DB, s3 objectStore, modal *ModalClient, tp *TPClient) *Server {
	s := &Server{
		db:    db,
		s3:    s3,
		modal: modal,
		tp:    tp,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

// SetQueue enables the JetStream-backed sync path. Without a queue the server
// keeps using the legacy in-process sync pipeline.
func (s *Server) SetQueue(q queue.Queue) {
	s.queue = q
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	// Health
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /health", s.handleHealthz) // backward compat

	// Auth
	s.mux.HandleFunc("GET /auth/me", s.handleMe)
	s.mux.HandleFunc("POST /auth/api-keys", s.handleCreateAPIKey)
	s.mux.HandleFunc("GET /auth/api-keys", s.handleListAPIKeys)
	s.mux.HandleFunc("DELETE /auth/api-keys/{id}", s.handleDeleteAPIKey)

	// Org management
	s.mux.HandleFunc("GET /org", s.handleGetOrg)
	s.mux.HandleFunc("GET /org/members", s.handleListMembers)
	s.mux.HandleFunc("POST /org/members", s.handleAddMember)
	s.mux.HandleFunc("DELETE /org/members/{userId}", s.handleRemoveMember)

	// Roots (org-scoped)
	s.mux.HandleFunc("POST /roots", s.handleCreateRoot)
	s.mux.HandleFunc("GET /roots", s.handleListRoots)
	s.mux.HandleFunc("GET /roots/{id}", s.handleGetRoot)
	s.mux.HandleFunc("POST /roots/{id}/upload", s.handleUpload)
	s.mux.HandleFunc("POST /roots/{id}/upload-bundle", s.handleUploadBundle)
	s.mux.HandleFunc("POST /roots/{id}/sync", s.handleSync)
	s.mux.HandleFunc("POST /roots/{id}/sync/init", s.handleSyncInit)
	s.mux.HandleFunc("GET /roots/{id}/state", s.handleGetState)
	s.mux.HandleFunc("GET /roots/{id}/sync/status", s.handleSyncStatus)
	s.mux.HandleFunc("GET /roots/{id}/sync/jobs", s.handleListSyncJobs)

	// ACLs
	s.mux.HandleFunc("POST /roots/{id}/acls", s.handleCreateACL)
	s.mux.HandleFunc("GET /roots/{id}/acls", s.handleListACLs)
	s.mux.HandleFunc("DELETE /roots/{id}/acls/{aclId}", s.handleDeleteACL)

	// Query
	s.mux.HandleFunc("POST /query", s.handleQuery)
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": "database: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	user, err := s.db.GetUser(r.Context(), id.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":   user,
		"org_id": id.OrgID,
		"role":   id.Role,
	})
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		req.Name = "CLI Key"
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{"sync", "query"}
	}

	rawKey, err := s.db.CreateAPIKey(r.Context(), id.OrgID, id.UserID, req.Name, req.Scopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"key":  rawKey,
		"note": "Store this key securely. It will not be shown again.",
	})
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	keys, err := s.db.ListAPIKeys(r.Context(), id.OrgID, id.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	keyID := r.PathValue("id")
	if err := s.db.DeleteAPIKey(r.Context(), id.OrgID, keyID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------------
// Org management
// ---------------------------------------------------------------------------

func (s *Server) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	org, err := s.db.GetOrganization(r.Context(), id.OrgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, org)
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	members, err := s.db.ListOrgMembers(r.Context(), id.OrgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, members)
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}

	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := s.db.AddOrgMember(r.Context(), id.OrgID, req.UserID, auth.Role(req.Role)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}
	userID := r.PathValue("userId")
	if err := s.db.RemoveOrgMember(r.Context(), id.OrgID, userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// ---------------------------------------------------------------------------
// Roots (org-scoped)
// ---------------------------------------------------------------------------

func (s *Server) handleCreateRoot(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleEditor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "editor role required"})
		return
	}

	var req struct {
		Name       string `json:"name"`
		SourcePath string `json:"source_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	root, err := s.db.CreateRoot(r.Context(), id.OrgID, req.Name, req.SourcePath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, root)
}

func (s *Server) handleListRoots(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	roots, err := s.db.ListRoots(r.Context(), id.OrgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, roots)
}

func (s *Server) handleGetRoot(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rootID := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	writeJSON(w, http.StatusOK, root)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleEditor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "editor role required"})
		return
	}

	rootID := r.PathValue("id")
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path query param required"})
		return
	}
	filePath, err := cleanFilePath(filePath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Verify root belongs to org
	if _, err := s.db.GetRoot(r.Context(), id.OrgID, rootID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	// Check write ACL for this path
	if !s.checkWriteACL(r.Context(), id, rootID, filePath) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no write permission for this path"})
		return
	}

	const maxUploadSize = 512 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body: " + err.Error()})
		return
	}

	s3Key := fmt.Sprintf("files/%s/%s", rootID, filePath)
	if err := s.s3.Upload(r.Context(), s3Key, data, "application/octet-stream"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"key": s3Key})
}

func (s *Server) handleUploadBundle(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleEditor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "editor role required"})
		return
	}

	rootID := r.PathValue("id")
	if _, err := s.db.GetRoot(r.Context(), id.OrgID, rootID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	bundleID := strings.TrimSpace(r.URL.Query().Get("bundle_id"))
	if bundleID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bundle_id query param required"})
		return
	}
	bundleID = safeObjectName(bundleID)
	if bundleID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bundle_id"})
		return
	}

	const maxUploadSize = 1024 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body: " + err.Error()})
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	s3Key := fmt.Sprintf("bundles/%s/%s", rootID, bundleID)
	if err := s.s3.Upload(r.Context(), s3Key, data, contentType); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"key": s3Key})
}

func safeObjectName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rootID := r.PathValue("id")

	// Verify root belongs to org
	if _, err := s.db.GetRoot(r.Context(), id.OrgID, rootID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	state, err := s.db.LoadState(r.Context(), rootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// handleSyncInit is kept for old clients. Namespace cloning is disabled.
func (s *Server) handleSyncInit(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleEditor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "editor role required"})
		return
	}

	rootID := r.PathValue("id")
	if _, err := s.db.GetRoot(r.Context(), id.OrgID, rootID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	type initResponse struct {
		CanReuse bool `json:"can_reuse"`
	}
	writeJSON(w, http.StatusOK, initResponse{})
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleEditor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "editor role required"})
		return
	}

	rootID := r.PathValue("id")

	// Verify root belongs to org
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	var req models.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.RootID = rootID
	if req.ProtocolVersion != models.SyncProtocolVersion {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":            fmt.Sprintf("unsupported sync protocol_version %d", req.ProtocolVersion),
			"protocol_version": req.ProtocolVersion,
			"required_version": models.SyncProtocolVersion,
		})
		return
	}
	if err := normalizeSyncRequest(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.checkSyncWriteACL(r.Context(), id, rootID, &req); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if err := validateSyncBase(req.BaseGenerationID, req.BaseGenerationSeq, root.VisibleGenerationID, root.VisibleGenerationSeq); err != nil {
		writeSyncConflict(w, err, &req, root.VisibleGenerationID, root.VisibleGenerationSeq)
		return
	}

	// Store SimHash for future index reuse
	if req.SimHash != "" {
		if err := s.db.UpdateRootSimHash(r.Context(), id.OrgID, rootID, req.SimHash); err != nil {
			log.Printf("warning: failed to update simhash: %v", err)
		}

	}

	// Count actionable files
	actionableFiles := 0
	for _, c := range req.Changes {
		if c.Status != models.StatusUnchanged {
			actionableFiles++
		}
	}

	// Create sync job
	job, err := s.db.CreateSyncJob(r.Context(), id.OrgID, rootID, id.UserID, actionableFiles)
	if err != nil {
		log.Printf("warning: failed to create sync job: %v", err)
	}
	syncJobID := ""
	if job != nil {
		syncJobID = job.ID
	}
	generation, err := s.db.CreateSyncGeneration(r.Context(), id.OrgID, rootID, syncJobID, req.ManifestRef, req.BaseGenerationID, req.BaseGenerationSeq)
	if err != nil {
		if job != nil {
			_ = s.db.CompleteSyncJob(r.Context(), job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		if errors.Is(err, errStaleSyncBase) {
			if currentRoot, rootErr := s.db.GetRoot(r.Context(), id.OrgID, rootID); rootErr == nil {
				writeSyncConflict(w, err, &req, currentRoot.VisibleGenerationID, currentRoot.VisibleGenerationSeq)
				return
			}
		}
		status := http.StatusInternalServerError
		if errors.Is(err, errSyncInProgress) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": "creating sync generation: " + err.Error()})
		return
	}
	if r.URL.Query().Get("async") == "true" {
		go func(req models.SyncRequest, generation *SyncGeneration, job *models.SyncJob) {
			ctx, cancel := context.WithTimeout(context.Background(), syncJobTimeout())
			defer cancel()
			if _, err := s.runSyncJob(ctx, id.OrgID, id.UserID, rootID, generation, &req, job); err != nil {
				log.Printf("async sync error for root %s: %v", rootID, err)
			}
		}(req, generation, job)
		writeJSON(w, http.StatusAccepted, models.SyncResponse{RootID: rootID, SyncJobID: syncJobIdentifier(job), GenerationID: generation.ID, GenerationSeq: generation.Seq})
		return
	}

	resp, err := s.runSyncJob(r.Context(), id.OrgID, id.UserID, rootID, generation, &req, job)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeSyncConflict(w http.ResponseWriter, err error, req *models.SyncRequest, currentGenerationID string, currentGenerationSeq int64) {
	writeJSON(w, http.StatusConflict, models.SyncConflictResponse{
		Error:                   err.Error(),
		ClientBaseGenerationID:  req.BaseGenerationID,
		ClientBaseGenerationSeq: req.BaseGenerationSeq,
		CurrentGenerationID:     currentGenerationID,
		CurrentGenerationSeq:    currentGenerationSeq,
	})
}

func (s *Server) runSyncJob(ctx context.Context, orgID, userID, rootID string, generation *SyncGeneration, req *models.SyncRequest, job *models.SyncJob) (*models.SyncResponse, error) {
	resp, err := s.runSyncPipeline(ctx, orgID, userID, generation, req, job)
	if err != nil {
		log.Printf("sync error for root %s: %v", rootID, err)
		_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
		if job != nil {
			_ = s.db.CompleteSyncJob(ctx, job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		return nil, err
	}
	if s.queue != nil {
		return resp, nil
	}

	if req.ContentProof != nil {
		proofBytes, _ := json.Marshal(req.ContentProof)
		if err := s.db.UpsertContentProof(ctx, orgID, userID, rootID, req.ContentProof.RootHash, proofBytes); err != nil {
			_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
			if job != nil {
				_ = s.db.CompleteSyncJob(ctx, job.ID, "failed", []map[string]string{{"error": err.Error()}})
			}
			return nil, fmt.Errorf("storing content proof: %w", err)
		}
	}

	if err := s.db.CommitSyncGeneration(ctx, generation, req.State); err != nil {
		_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
		if job != nil {
			_ = s.db.CompleteSyncJob(ctx, job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		return nil, fmt.Errorf("committing generation: %w", err)
	}
	if job != nil {
		if err := s.db.CompleteSyncJob(ctx, job.ID, "completed", nil); err != nil {
			log.Printf("error completing sync job %s: %v", job.ID, err)
		}
		resp.SyncJobID = job.ID
	}
	return resp, nil
}

func (s *Server) runSyncPipeline(ctx context.Context, orgID, userID string, generation *SyncGeneration, req *models.SyncRequest, job *models.SyncJob) (*models.SyncResponse, error) {
	if s.queue != nil {
		return s.enqueueSync(ctx, orgID, userID, generation, req, job)
	}
	return s.processSync(ctx, orgID, generation, req, job)
}

// ---------------------------------------------------------------------------
// Sync status
// ---------------------------------------------------------------------------

func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rootID := r.PathValue("id")

	var (
		job *models.SyncJob
		err error
	)
	if jobID := r.URL.Query().Get("job_id"); jobID != "" {
		job, err = s.db.GetSyncJob(r.Context(), id.OrgID, jobID)
	} else {
		job, err = s.db.GetLatestSyncJob(r.Context(), id.OrgID, rootID)
	}
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no sync jobs found"})
		return
	}
	if job.RootID != rootID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sync job not found"})
		return
	}
	job = s.failExpiredSyncJob(r.Context(), job)
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) failExpiredSyncJob(ctx context.Context, job *models.SyncJob) *models.SyncJob {
	if job == nil || job.Status == "completed" || job.Status == "failed" {
		return job
	}
	timeout := syncJobTimeout()
	if time.Since(job.StartedAt) <= timeout {
		return job
	}

	message := fmt.Sprintf("sync job expired after %s; background worker is no longer running", timeout)
	errors := []map[string]string{{"error": message}}
	if err := s.db.CompleteSyncJob(ctx, job.ID, "failed", errors); err != nil {
		log.Printf("warning: failed to expire sync job %s: %v", job.ID, err)
		return job
	}
	if err := s.db.MarkSyncGenerationFailedForJob(ctx, job.ID); err != nil {
		log.Printf("warning: failed to expire sync generation for job %s: %v", job.ID, err)
	}
	errBytes, _ := json.Marshal(errors)
	now := time.Now()
	job.Status = "failed"
	job.Errors = errBytes
	job.FinishedAt = &now
	return job
}

func syncJobTimeout() time.Duration {
	const defaultTimeout = 30 * time.Minute
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_SYNC_JOB_TIMEOUT"))
	if raw == "" {
		return defaultTimeout
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < time.Second {
		return defaultTimeout
	}
	return timeout
}

func (s *Server) handleListSyncJobs(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rootID := r.PathValue("id")

	jobs, err := s.db.ListSyncJobs(r.Context(), id.OrgID, rootID, 20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// ---------------------------------------------------------------------------
// ACLs
// ---------------------------------------------------------------------------

func (s *Server) handleCreateACL(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}
	rootID := r.PathValue("id")

	var req struct {
		PathPrefix string `json:"path_prefix"`
		GrantTo    string `json:"grant_to"`
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	pathPrefix, err := cleanPathPrefix(req.PathPrefix)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.GrantTo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grant_to is required"})
		return
	}
	if req.Permission == "" {
		req.Permission = "none"
	}
	if req.Permission != "none" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "folder ACLs are deny-prefix rules; permission must be none"})
		return
	}

	acl, err := s.db.CreateACL(r.Context(), id.OrgID, rootID, pathPrefix, req.GrantTo, req.Permission)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, acl)
}

func (s *Server) handleListACLs(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}
	rootID := r.PathValue("id")

	acls, err := s.db.ListACLs(r.Context(), id.OrgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, acls)
}

func (s *Server) handleDeleteACL(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}
	aclID := r.PathValue("aclId")

	if err := s.db.DeleteACL(r.Context(), id.OrgID, aclID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------------
// ACL helpers
// ---------------------------------------------------------------------------

// checkWriteACL checks if a user has write permission for a path in a root.
// If no ACLs are configured for the root, all org editors+ have access.
func (s *Server) checkWriteACL(ctx context.Context, id *auth.Identity, rootID, filePath string) bool {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil || len(acls) == 0 {
		// No ACLs configured → default: editors+ can write
		return auth.HasMinRole(id.Role, auth.RoleEditor)
	}

	return checkPermission(acls, filePath, "write")
}

func (s *Server) checkSyncWriteACL(ctx context.Context, id *auth.Identity, rootID string, req *models.SyncRequest) error {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil || len(acls) == 0 {
		if auth.HasMinRole(id.Role, auth.RoleEditor) {
			return nil
		}
		return fmt.Errorf("editor role required")
	}

	canWrite := func(filePath string) bool {
		return checkPermission(acls, filePath, "write")
	}

	for _, change := range req.Changes {
		switch change.Status {
		case models.StatusAdded, models.StatusModified, models.StatusRemoved:
			if !canWrite(change.Path) {
				return fmt.Errorf("no write permission for %s", change.Path)
			}
		case models.StatusMoved, models.StatusRenamed:
			if !canWrite(change.OldPath) {
				return fmt.Errorf("no write permission for %s", change.OldPath)
			}
			if !canWrite(change.Path) {
				return fmt.Errorf("no write permission for %s", change.Path)
			}
		}
	}
	return nil
}

// checkReadACL checks if a user has read permission for a path in a root.
func (s *Server) checkReadACL(ctx context.Context, id *auth.Identity, rootID, filePath string) bool {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil || len(acls) == 0 {
		return true // No ACLs → all org members can read
	}

	return checkPermission(acls, filePath, "read")
}

// buildACLFilter returns Turbopuffer filter conditions based on user's ACLs.
// Returns nil if no filtering is needed (user has full access).
func (s *Server) buildACLFilter(ctx context.Context, id *auth.Identity, rootID string) []string {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil || len(acls) == 0 {
		return nil // No ACLs → full access
	}

	// Collect denied path prefixes
	var denied []string
	for _, acl := range acls {
		if acl.Permission == "none" {
			denied = append(denied, acl.PathPrefix)
		}
	}
	return denied
}

// checkPermission evaluates ACLs for a specific path and permission.
// ACLs are expected to be sorted by path_prefix length descending (most specific first).
func checkPermission(acls []models.RootACL, filePath, _ string) bool {
	normalizedPath := "/" + filePath

	for _, acl := range acls {
		if acl.Permission == "none" && strings.HasPrefix(normalizedPath, acl.PathPrefix) {
			return false
		}
	}
	return true // No matching ACL → allow
}

// ---------------------------------------------------------------------------
// Content proof filtering (Phase 3)
// ---------------------------------------------------------------------------

// filterByContentProof removes query results for files the user doesn't have locally.
// This provides zero-trust search: even with a shared/cloned index, users only see
// results for files they can prove they possess (via Merkle tree hashes).
func (s *Server) filterByContentProof(ctx context.Context, orgID, userID, rootID string, rows []map[string]any) []map[string]any {
	proofBytes, _, err := s.db.GetContentProof(ctx, orgID, userID, rootID)
	if err != nil {
		return rows
	}

	var proof models.ContentProofData
	if err := json.Unmarshal(proofBytes, &proof); err != nil {
		return rows
	}

	var filtered []map[string]any
	for _, row := range rows {
		fp := strVal(row, "file_path")
		fileHash := strVal(row, "file_hash")
		if fp == "" {
			continue
		}
		if proofHash, ok := proof.FileHashes[fp]; ok && proofHash == fileHash {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// ---------------------------------------------------------------------------
// Sync processing
// ---------------------------------------------------------------------------

type pendingEmbedding struct {
	chunk       map[string]any
	row         map[string]any
	contentHash string
}

type syncSourceCache struct {
	s3      objectStore
	mu      sync.Mutex
	objects map[string][]byte
}

func newSyncSourceCache(s3 objectStore) *syncSourceCache {
	return &syncSourceCache{s3: s3, objects: make(map[string][]byte)}
}

func (c *syncSourceCache) read(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("empty source key")
	}
	if !strings.HasPrefix(key, "bundles/") {
		return c.s3.DownloadRange(ctx, key, offset, length)
	}
	c.mu.Lock()
	data, ok := c.objects[key]
	if !ok {
		var err error
		data, err = c.s3.Download(ctx, key)
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.objects[key] = data
	}
	c.mu.Unlock()
	if length <= 0 {
		return data, nil
	}
	end := offset + length
	if offset < 0 || end < offset || end > int64(len(data)) {
		return nil, fmt.Errorf("invalid range offset=%d length=%d object_bytes=%d", offset, length, len(data))
	}
	out := make([]byte, length)
	copy(out, data[offset:end])
	return out, nil
}

func syncWorkerCount() int {
	const defaultWorkers = 64
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_SYNC_WORKERS"))
	if raw == "" {
		return defaultWorkers
	}
	workers, err := strconv.Atoi(raw)
	if err != nil || workers < 1 {
		return defaultWorkers
	}
	if workers > 64 {
		return 64
	}
	return workers
}

func (s *Server) writeIndexRowsArtifact(ctx context.Context, generationID, reason string, rows []map[string]any) error {
	if generationID == "" || len(rows) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return fmt.Errorf("encoding index row artifact: %w", err)
		}
	}
	key := fmt.Sprintf("syncs/%s/index_rows/%d-%s.jsonl", generationID, time.Now().UnixNano(), safeObjectName(reason))
	if err := s.s3.Upload(ctx, key, buf.Bytes(), "application/x-ndjson"); err != nil {
		return fmt.Errorf("uploading index row artifact %s: %w", key, err)
	}
	return nil
}

func (s *Server) resolvePendingEmbeddings(ctx context.Context, orgID string, pending []pendingEmbedding) error {
	chunks := make([]map[string]any, len(pending))
	for i, item := range pending {
		chunks[i] = item.chunk
	}
	embedStart := time.Now()
	embedResults, err := s.embedChunksInBatches(chunks)
	if err != nil {
		return fmt.Errorf("embedding sync chunks: %w", err)
	}
	log.Printf("timing stage=modal_embed_global chunks=%d elapsed=%s", len(chunks), time.Since(embedStart))

	cacheEntries := make(map[string][]float64)
	for i, result := range embedResults {
		if i >= len(pending) {
			break
		}
		embedding, ok := result["embedding"].([]any)
		if !ok {
			continue
		}
		pending[i].row["vector"] = embedding
		hash := pending[i].contentHash
		if hash == "" {
			chunk, _ := result["chunk"].(map[string]any)
			hash, _ = chunk["content_hash"].(string)
		}
		if hash == "" {
			continue
		}
		embFloat := make([]float64, len(embedding))
		for j, value := range embedding {
			if f, ok := value.(float64); ok {
				embFloat[j] = f
			}
		}
		cacheEntries[hash] = embFloat
	}

	cacheSaveStart := time.Now()
	if err := s.db.SaveCachedEmbeddings(ctx, orgID, s.modal.EmbeddingModelVersion(), cacheEntries); err != nil {
		log.Printf("warning: failed to save embedding cache: %v", err)
	}
	log.Printf("timing stage=embedding_cache_save_global entries=%d elapsed=%s", len(cacheEntries), time.Since(cacheSaveStart))
	return nil
}

func (s *Server) upsertRowsInBatches(ns string, rows []map[string]any) error {
	batchSize := tpWriteBatchSize()
	for start := 0; start < len(rows); start += batchSize {
		end := start + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		writeStart := time.Now()
		if err := s.tp.UpsertRows(ns, rows[start:end], "cosine_distance"); err != nil {
			return err
		}
		log.Printf("timing stage=tp_upsert_batch batch=%d/%d rows=%d elapsed=%s", start/batchSize+1, (len(rows)+batchSize-1)/batchSize, end-start, time.Since(writeStart))
	}
	return nil
}

func (s *Server) patchRowsInBatches(ns string, rows []map[string]any) error {
	batchSize := tpWriteBatchSize()
	for start := 0; start < len(rows); start += batchSize {
		end := start + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		writeStart := time.Now()
		if err := s.tp.PatchRows(ns, rows[start:end]); err != nil {
			return err
		}
		log.Printf("timing stage=tp_patch_batch batch=%d/%d rows=%d elapsed=%s", start/batchSize+1, (len(rows)+batchSize-1)/batchSize, end-start, time.Since(writeStart))
	}
	return nil
}

func tpWriteBatchSize() int {
	const defaultRows = 512
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_TP_WRITE_BATCH_ROWS"))
	if raw == "" {
		return defaultRows
	}
	rows, err := strconv.Atoi(raw)
	if err != nil || rows < 1 {
		return defaultRows
	}
	if rows > 5000 {
		return 5000
	}
	return rows
}

// tpNamespace returns the Turbopuffer namespace for a root, scoped to an org.
func tpNamespace(orgID, rootID string) string {
	return fmt.Sprintf("org-%s-root-%s", orgID, rootID)
}

func modalChunkPayload(row map[string]any) map[string]any {
	chunk := map[string]any{
		"id":           row["id"],
		"content":      row["content"],
		"file_path":    row["file_path"],
		"chunk_index":  row["chunk_index"],
		"content_hash": row["content_hash"],
		"file_type":    row["file_type"],
		"root_id":      row["root_id"],
	}
	if pageNumber, ok := row["page_number"]; ok && pageNumber != nil {
		chunk["page_number"] = pageNumber
	}
	if imagePath, ok := row["image_path"]; ok && imagePath != nil {
		chunk["image_path"] = imagePath
	}
	return chunk
}

func (s *Server) embedChunksInBatches(chunks []map[string]any) ([]map[string]any, error) {
	batchSize := embedBatchSize()
	batchCount := (len(chunks) + batchSize - 1) / batchSize
	concurrency := embedBatchConcurrency()
	if concurrency > batchCount {
		concurrency = batchCount
	}
	totalStart := time.Now()
	batchResults := make([][]map[string]any, batchCount)
	errs := make([]error, batchCount)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for start := 0; start < len(chunks); start += batchSize {
		end := start + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batchIndex := start / batchSize
		wg.Add(1)
		go func(batchIndex, start, end int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			batchStart := time.Now()
			resp, err := s.modal.EmbedChunks(chunks[start:end])
			if err != nil {
				errs[batchIndex] = err
				return
			}
			log.Printf("timing stage=modal_embed_batch batch=%d/%d chunks=%d elapsed=%s", batchIndex+1, batchCount, end-start, time.Since(batchStart))
			batchResults[batchIndex] = resp.Results
		}(batchIndex, start, end)
	}
	wg.Wait()

	results := make([]map[string]any, 0, len(chunks))
	for batchIndex, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("embedding batch %d/%d: %w", batchIndex+1, batchCount, err)
		}
		results = append(results, batchResults[batchIndex]...)
	}
	log.Printf("timing stage=modal_embed_batches_total chunks=%d batches=%d batch_size=%d concurrency=%d elapsed=%s", len(chunks), batchCount, batchSize, concurrency, time.Since(totalStart))
	return results, nil
}

func embedBatchSize() int {
	const defaultSize = 16
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_EMBED_BATCH_SIZE"))
	if raw == "" {
		return defaultSize
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 1 {
		return defaultSize
	}
	if size > 128 {
		return 128
	}
	return size
}

func embedBatchConcurrency() int {
	const defaultConcurrency = 4
	raw := strings.TrimSpace(os.Getenv("PUFFERFS_EMBED_BATCH_CONCURRENCY"))
	if raw == "" {
		return defaultConcurrency
	}
	concurrency, err := strconv.Atoi(raw)
	if err != nil || concurrency < 1 {
		return defaultConcurrency
	}
	if concurrency > 16 {
		return 16
	}
	return concurrency
}

// ---------------------------------------------------------------------------
// Query (with ACL filtering)
// ---------------------------------------------------------------------------

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req models.QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query is required"})
		return
	}
	if req.RootID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "root_id is required"})
		return
	}
	if req.TopK <= 0 {
		req.TopK = 10
	}
	if req.Mode == "" {
		req.Mode = "hybrid"
	}

	// Verify root belongs to org
	if _, err := s.db.GetRoot(r.Context(), id.OrgID, req.RootID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	ns := tpNamespace(id.OrgID, req.RootID)
	includeAttrs := []string{"content", "file_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path", "generation_id", "valid_from_generation", "valid_from_generation_seq", "valid_to_generation", "valid_to_generation_seq"}

	var filters []any
	if req.Glob != "" {
		filters = append(filters, []any{"file_path", "Glob", req.Glob})
	}
	// Always constrain results to the visible generation's active window. If we
	// cannot resolve it, fail closed (return an error) rather than returning
	// unfiltered rows. When the root has no committed generation yet
	// (visibleSeq == 0), activeGenerationFilter matches no rows, so
	// uncommitted/in-flight rows from a building or failed sync are never served.
	visibleSeq, vErr := s.db.GetVisibleGenerationSeq(r.Context(), req.RootID)
	if vErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "resolving visible generation: " + vErr.Error()})
		return
	}
	filters = append(filters, activeGenerationFilter(visibleSeq))

	// Get denied paths from ACLs and filter results post-query
	deniedPrefixes := s.buildACLFilter(r.Context(), id, req.RootID)

	var rows []map[string]any
	var err error

	switch req.Mode {
	case "fts":
		rankBy := []any{"content", "BM25", req.Query}
		rows, err = s.tp.Query(ns, rankBy, req.TopK, tpAndFilter(filters), includeAttrs)

	case "vector":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rankBy := []any{"vector", "ANN", embedding}
		rows, err = s.tp.Query(ns, rankBy, req.TopK, tpAndFilter(filters), includeAttrs)

	case "hybrid":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rows, err = s.tp.HybridSearch(ns, req.Query, embedding, req.TopK, tpAndFilter(filters))

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be fts, vector, or hybrid"})
		return
	}

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Filter out denied paths (ACL enforcement)
	var filteredRows []map[string]any
	for _, row := range rows {
		fp := strVal(row, "file_path")
		denied := false
		for _, prefix := range deniedPrefixes {
			if strings.HasPrefix("/"+fp, prefix) {
				denied = true
				break
			}
		}
		if !denied {
			filteredRows = append(filteredRows, row)
		}
	}

	// Content proof filtering: only return results for files the user can prove they have
	filteredRows = s.filterByContentProof(r.Context(), id.OrgID, id.UserID, req.RootID, filteredRows)

	results := make([]models.QueryResult, len(filteredRows))
	for i, row := range filteredRows {
		results[i] = models.QueryResult{
			FilePath: strVal(row, "file_path"),
			Content:  strVal(row, "content"),
			FileType: strVal(row, "file_type"),
			Score:    floatVal(row, "$dist"),
		}
		if ci, ok := row["chunk_index"]; ok {
			if f, ok := ci.(float64); ok {
				results[i].ChunkIndex = int(f)
			}
		}
		if pn, ok := row["page_number"]; ok && pn != nil {
			if f, ok := pn.(float64); ok {
				n := int(f)
				results[i].PageNumber = &n
			}
		}
		if ip, ok := row["image_path"]; ok && ip != nil {
			if s, ok := ip.(string); ok {
				results[i].ImagePath = &s
			}
		}
	}

	writeJSON(w, http.StatusOK, models.QueryResponse{
		Results: results,
		Query:   req.Query,
		Mode:    req.Mode,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func detectFileType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".pdf":
		return "pdf"
	case ".docx", ".doc":
		return "docx"
	case ".pptx", ".ppt":
		return "pptx"
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".bmp":
		return "image"
	default:
		return "auto"
	}
}

func tpAndFilter(filters []any) any {
	switch len(filters) {
	case 0:
		return nil
	case 1:
		return filters[0]
	default:
		return []any{"And", filters}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func floatVal(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}
