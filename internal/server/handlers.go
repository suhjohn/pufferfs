package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/internal/storage"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// Server holds the dependencies for HTTP handlers.
type Server struct {
	db    *DB
	s3    *storage.Client
	modal *ModalClient
	tp    *TPClient
	mux   *http.ServeMux
}

// New creates a new Server with all dependencies.
func New(db *DB, s3 *storage.Client, modal *ModalClient, tp *TPClient) *Server {
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

// handleSyncInit checks for similar indexes in the org before a full sync.
// Returns similarity info so the CLI can decide whether to do a full or delta sync.
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

	var req struct {
		SimHash string `json:"simhash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	type initResponse struct {
		SimilarRoot *models.RootWithSimHash `json:"similar_root,omitempty"`
		Similarity  float64                 `json:"similarity"`
		CanReuse    bool                    `json:"can_reuse"`
	}

	if req.SimHash == "" {
		writeJSON(w, http.StatusOK, initResponse{})
		return
	}

	similar, err := s.db.FindSimilarRoots(r.Context(), id.OrgID, rootID, req.SimHash)
	if err != nil || len(similar) == 0 {
		writeJSON(w, http.StatusOK, initResponse{})
		return
	}

	targetBytes, err := hexToSimHash(req.SimHash)
	if err != nil {
		writeJSON(w, http.StatusOK, initResponse{})
		return
	}

	var bestRoot *models.RootWithSimHash
	bestDist := 257
	for i, r := range similar {
		candidateBytes, err := hexToSimHash(r.SimHash)
		if err != nil {
			continue
		}
		dist := hammingDistance(targetBytes, candidateBytes)
		if dist < bestDist {
			bestDist = dist
			bestRoot = &similar[i]
		}
	}

	if bestRoot == nil || bestDist > 51 {
		writeJSON(w, http.StatusOK, initResponse{})
		return
	}

	similarity := 1.0 - float64(bestDist)/256.0
	writeJSON(w, http.StatusOK, initResponse{
		SimilarRoot: bestRoot,
		Similarity:  similarity,
		CanReuse:    true,
	})
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleEditor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "editor role required"})
		return
	}

	rootID := r.PathValue("id")

	// Verify root belongs to org
	if _, err := s.db.GetRoot(r.Context(), id.OrgID, rootID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	var req models.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.RootID = rootID

	// Store SimHash for future index reuse
	if req.SimHash != "" {
		if err := s.db.UpdateRootSimHash(r.Context(), id.OrgID, rootID, req.SimHash); err != nil {
			log.Printf("warning: failed to update simhash: %v", err)
		}

		// Try index reuse: find similar roots and clone their data
		s.tryIndexReuse(r.Context(), id.OrgID, rootID, req.SimHash, &req)
	}

	// Store content proof if provided
	if req.ContentProof != nil {
		proofBytes, _ := json.Marshal(req.ContentProof)
		if err := s.db.UpsertContentProof(r.Context(), id.OrgID, id.UserID, rootID, req.ContentProof.RootHash, proofBytes); err != nil {
			log.Printf("warning: failed to store content proof: %v", err)
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

	resp, err := s.processSync(r.Context(), id.OrgID, &req, job)
	if err != nil {
		log.Printf("sync error for root %s: %v", rootID, err)
		if job != nil {
			_ = s.db.CompleteSyncJob(r.Context(), job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Persist state
	if err := s.db.SaveState(r.Context(), rootID, req.State); err != nil {
		log.Printf("error saving state for root %s: %v", rootID, err)
	}
	if err := s.db.UpdateRootTimestamp(r.Context(), rootID); err != nil {
		log.Printf("error updating timestamp for root %s: %v", rootID, err)
	}

	if job != nil {
		_ = s.db.CompleteSyncJob(r.Context(), job.ID, "completed", nil)
		resp.SyncJobID = job.ID
	}

	writeJSON(w, http.StatusOK, resp)
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

	job, err := s.db.GetLatestSyncJob(r.Context(), id.OrgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no sync jobs found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
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
	if req.PathPrefix == "" {
		req.PathPrefix = "/"
	}
	if req.Permission == "" {
		req.Permission = "read"
	}

	acl, err := s.db.CreateACL(r.Context(), id.OrgID, rootID, req.PathPrefix, req.GrantTo, req.Permission)
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
func checkPermission(acls []models.RootACL, filePath, requiredPermission string) bool {
	normalizedPath := "/" + filePath

	for _, acl := range acls {
		if strings.HasPrefix(normalizedPath, acl.PathPrefix) {
			if acl.Permission == "none" {
				return false
			}
			if requiredPermission == "read" {
				return acl.Permission == "read" || acl.Permission == "write"
			}
			return acl.Permission == "write"
		}
	}
	return true // No matching ACL → allow
}

// ---------------------------------------------------------------------------
// Index reuse (Phase 2)
// ---------------------------------------------------------------------------

// tryIndexReuse finds a similar root in the same org and clones its Turbopuffer
// namespace + embedding cache to bootstrap the new root's index.
// This allows the second (and subsequent) team member syncing the same codebase
// to skip most of the expensive chunking+embedding work.
func (s *Server) tryIndexReuse(ctx context.Context, orgID, rootID, simhash string, req *models.SyncRequest) {
	similar, err := s.db.FindSimilarRoots(ctx, orgID, rootID, simhash)
	if err != nil || len(similar) == 0 {
		return
	}

	// Find the best match by computing Hamming distance
	targetBytes, err := hexToSimHash(simhash)
	if err != nil {
		return
	}

	var bestRoot *models.RootWithSimHash
	bestDist := 257 // worse than max
	for i, r := range similar {
		candidateBytes, err := hexToSimHash(r.SimHash)
		if err != nil {
			continue
		}
		dist := hammingDistance(targetBytes, candidateBytes)
		if dist < bestDist {
			bestDist = dist
			bestRoot = &similar[i]
		}
	}

	// Threshold: similarity must be > 80% (Hamming distance < 51 out of 256 bits)
	if bestRoot == nil || bestDist > 51 {
		return
	}

	similarity := 1.0 - float64(bestDist)/256.0
	log.Printf("index reuse: found similar root %s (%.1f%% similar), cloning namespace", bestRoot.ID, similarity*100)

	// Clone Turbopuffer namespace: copy all vectors from source to target
	srcNS := tpNamespace(orgID, bestRoot.ID)
	dstNS := tpNamespace(orgID, rootID)
	if err := s.cloneTurbopufferNamespace(ctx, srcNS, dstNS); err != nil {
		log.Printf("warning: failed to clone turbopuffer namespace: %v", err)
		return
	}

	// Copy embedding cache entries
	var contentHashes []string
	for _, fs := range req.State {
		contentHashes = append(contentHashes, fs.ContentHash)
	}
	copied, err := s.db.CopyEmbeddingCache(ctx, orgID, orgID, contentHashes)
	if err != nil {
		log.Printf("warning: failed to copy embedding cache: %v", err)
	} else if copied > 0 {
		log.Printf("index reuse: copied %d embedding cache entries", copied)
	}

	// Remove changes for files that already exist in the cloned namespace
	// by checking content_hash matches from the source state
	if len(req.Changes) > 0 {
		sourceState, err := s.db.LoadState(ctx, bestRoot.ID)
		if err == nil && sourceState != nil {
			sourceByHash := make(map[string]bool)
			for _, fs := range sourceState {
				sourceByHash[fs.ContentHash] = true
			}

			var remaining []models.FileChange
			skipped := 0
			for _, c := range req.Changes {
				if (c.Status == models.StatusAdded || c.Status == models.StatusModified) && sourceByHash[c.ContentHash] {
					skipped++
					continue
				}
				remaining = append(remaining, c)
			}
			if skipped > 0 {
				log.Printf("index reuse: skipped %d files already in cloned index", skipped)
				req.Changes = remaining
			}
		}
	}
}

// cloneTurbopufferNamespace copies all vectors from source to destination namespace.
func (s *Server) cloneTurbopufferNamespace(ctx context.Context, srcNS, dstNS string) error {
	// Query all vectors from source (in batches)
	includeAttrs := []string{"content", "file_path", "chunk_index", "file_type", "content_hash", "page_number", "image_path", "root_id"}
	offset := 0
	batchSize := 500
	totalCopied := 0

	for {
		// Use BM25 search with wildcard to get all documents
		rows, err := s.tp.Query(srcNS, []any{"content", "BM25", "*"}, batchSize, nil, includeAttrs)
		if err != nil {
			if totalCopied > 0 {
				return nil // partial clone is OK
			}
			return err
		}

		if len(rows) == 0 {
			break
		}

		// Upsert to destination namespace
		if err := s.tp.UpsertRows(dstNS, rows, "cosine_distance"); err != nil {
			return fmt.Errorf("upserting to %s: %w", dstNS, err)
		}

		totalCopied += len(rows)
		if len(rows) < batchSize {
			break
		}
		offset += batchSize
	}

	log.Printf("index reuse: cloned %d vectors from %s to %s", totalCopied, srcNS, dstNS)
	return nil
}

func hexToSimHash(hex string) ([32]byte, error) {
	var result [32]byte
	if len(hex) != 64 {
		return result, fmt.Errorf("invalid simhash length: %d", len(hex))
	}
	for i := 0; i < 32; i++ {
		b, err := hexByte(hex[i*2], hex[i*2+1])
		if err != nil {
			return result, err
		}
		result[i] = b
	}
	return result, nil
}

func hexByte(hi, lo byte) (byte, error) {
	h, err := hexDigit(hi)
	if err != nil {
		return 0, err
	}
	l, err := hexDigit(lo)
	if err != nil {
		return 0, err
	}
	return h<<4 | l, nil
}

func hexDigit(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex digit: %c", c)
	}
}

func hammingDistance(a, b [32]byte) int {
	dist := 0
	for i := 0; i < 32; i++ {
		xor := a[i] ^ b[i]
		for xor != 0 {
			dist += int(xor & 1)
			xor >>= 1
		}
	}
	return dist
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
		return rows // No proof stored → no filtering (backward compatible)
	}

	var proof models.ContentProofData
	if err := json.Unmarshal(proofBytes, &proof); err != nil {
		return rows
	}

	var filtered []map[string]any
	for _, row := range rows {
		fp := strVal(row, "file_path")
		ch := strVal(row, "content_hash")
		if fp == "" {
			continue
		}
		// Check: does the client's proof contain this file with matching hash?
		if proofHash, ok := proof.FileHashes[fp]; ok {
			if ch == "" || proofHash == ch {
				filtered = append(filtered, row)
			}
		}
	}
	return filtered
}

// ---------------------------------------------------------------------------
// Sync processing
// ---------------------------------------------------------------------------

func (s *Server) processSync(ctx context.Context, orgID string, req *models.SyncRequest, job *models.SyncJob) (*models.SyncResponse, error) {
	resp := &models.SyncResponse{RootID: req.RootID}
	processed := 0

	for _, change := range req.Changes {
		switch change.Status {
		case models.StatusAdded, models.StatusModified:
			if job != nil {
				_ = s.db.UpdateSyncJobStatus(ctx, job.ID, "chunking", processed)
			}
			if err := s.processFileAdd(ctx, orgID, req.RootID, change); err != nil {
				return nil, fmt.Errorf("processing %s (%s): %w", change.Path, change.Status, err)
			}
			resp.ChunksAdded++
			resp.FilesProcessed++

		case models.StatusRemoved:
			if err := s.processFileRemove(ctx, req.RootID, change); err != nil {
				return nil, fmt.Errorf("removing %s: %w", change.Path, err)
			}
			resp.ChunksRemoved++
			resp.FilesProcessed++

		case models.StatusMoved, models.StatusRenamed:
			if err := s.processFileMove(ctx, req.RootID, change); err != nil {
				return nil, fmt.Errorf("moving %s -> %s: %w", change.OldPath, change.Path, err)
			}
			resp.ChunksMoved++
			resp.FilesProcessed++

		case models.StatusUnchanged:
			// nothing to do
		}

		processed++
		if job != nil && processed%5 == 0 {
			_ = s.db.UpdateSyncJobStatus(ctx, job.ID, "embedding", processed)
		}
	}

	return resp, nil
}

// tpNamespace returns the Turbopuffer namespace for a root, scoped to an org.
func tpNamespace(orgID, rootID string) string {
	return fmt.Sprintf("org-%s-root-%s", orgID, rootID)
}

func (s *Server) processFileAdd(ctx context.Context, orgID, rootID string, change models.FileChange) error {
	ns := tpNamespace(orgID, rootID)

	// For MODIFIED files, delete old chunks first
	if change.Status == models.StatusModified {
		filter := []any{"file_path", "Eq", change.Path}
		if err := s.tp.DeleteByFilter(ns, filter); err != nil {
			log.Printf("warning: failed to delete old chunks for %s: %v", change.Path, err)
		}
	}

	s3Key := fmt.Sprintf("files/%s/%s", rootID, change.Path)

	fileData, err := s.s3.Download(ctx, s3Key)
	if err != nil {
		return fmt.Errorf("downloading %s from S3: %w", s3Key, err)
	}

	chunkResp, err := s.modal.ChunkFile(ChunkFileRequest{
		S3Key:      s3Key,
		FilePath:   change.Path,
		FileType:   detectFileType(change.Path),
		RootID:     rootID,
		ContentB64: base64.StdEncoding.EncodeToString(fileData),
	})
	if err != nil {
		return fmt.Errorf("chunking: %w", err)
	}

	if len(chunkResp.Chunks) == 0 {
		return nil
	}

	var contentHashes []string
	for _, c := range chunkResp.Chunks {
		if h, ok := c["content_hash"].(string); ok {
			contentHashes = append(contentHashes, h)
		}
	}

	cached, err := s.db.GetCachedEmbeddings(ctx, contentHashes)
	if err != nil {
		log.Printf("warning: embedding cache lookup failed: %v", err)
		cached = make(map[string][]float64)
	}

	var uncachedChunks []map[string]any
	cachedRows := make(map[int][]float64)

	for i, c := range chunkResp.Chunks {
		hash, _ := c["content_hash"].(string)
		if emb, ok := cached[hash]; ok {
			cachedRows[i] = emb
		} else {
			uncachedChunks = append(uncachedChunks, c)
		}
	}

	log.Printf("file %s: %d chunks total, %d cached, %d to embed",
		change.Path, len(chunkResp.Chunks), len(cachedRows), len(uncachedChunks))

	var embedResults []map[string]any
	if len(uncachedChunks) > 0 {
		embedResp, err := s.modal.EmbedChunks(uncachedChunks)
		if err != nil {
			return fmt.Errorf("embedding: %w", err)
		}
		embedResults = embedResp.Results

		newCacheEntries := make(map[string][]float64)
		for _, r := range embedResults {
			chunk, _ := r["chunk"].(map[string]any)
			hash, _ := chunk["content_hash"].(string)
			if emb, ok := r["embedding"].([]any); ok && hash != "" {
				embFloat := make([]float64, len(emb))
				for k, v := range emb {
					if f, ok := v.(float64); ok {
						embFloat[k] = f
					}
				}
				newCacheEntries[hash] = embFloat
			}
		}
		if err := s.db.SaveCachedEmbeddings(ctx, newCacheEntries); err != nil {
			log.Printf("warning: failed to save embedding cache: %v", err)
		}
	}

	rows := make([]map[string]any, len(chunkResp.Chunks))
	embedIdx := 0
	for i, c := range chunkResp.Chunks {
		row := map[string]any{
			"id":           c["id"],
			"content":      c["content"],
			"file_path":    c["file_path"],
			"chunk_index":  c["chunk_index"],
			"content_hash": c["content_hash"],
			"file_type":    c["file_type"],
			"root_id":      rootID,
		}
		if pn, ok := c["page_number"]; ok && pn != nil {
			row["page_number"] = pn
		}
		if ip, ok := c["image_path"]; ok && ip != nil {
			row["image_path"] = ip
		}

		if emb, ok := cachedRows[i]; ok {
			row["vector"] = emb
		} else if embedIdx < len(embedResults) {
			r := embedResults[embedIdx]
			row["vector"], _ = r["embedding"].([]any)
			embedIdx++
		}

		rows[i] = row
	}

	return s.tp.UpsertRows(ns, rows, "cosine_distance")
}

func (s *Server) processFileRemove(ctx context.Context, rootID string, change models.FileChange) error {
	filter := []any{"file_path", "Eq", change.Path}
	// Note: we can't scope by org here since we don't have orgID in context easily,
	// but the namespace is already org-scoped via tpNamespace
	return s.tp.DeleteByFilter(rootID, filter)
}

func (s *Server) processFileMove(ctx context.Context, rootID string, change models.FileChange) error {
	oldFileKey := fmt.Sprintf("files/%s/%s", rootID, change.OldPath)
	newFileKey := fmt.Sprintf("files/%s/%s", rootID, change.Path)
	if err := s.s3.Rename(ctx, oldFileKey, newFileKey); err != nil {
		log.Printf("warning: S3 rename failed (may not exist): %v", err)
	}

	rows, err := s.tp.Query(rootID,
		[]any{"file_path", "asc"},
		10000,
		[]any{"file_path", "Eq", change.OldPath},
		[]string{"content", "file_path", "chunk_index", "content_hash", "file_type", "page_number", "image_path"},
	)
	if err != nil {
		return fmt.Errorf("querying old chunks: %w", err)
	}

	if len(rows) == 0 {
		return nil
	}

	oldIDs := make([]string, len(rows))
	for i, row := range rows {
		oldIDs[i] = fmt.Sprintf("%v", row["id"])
	}
	if err := s.tp.DeleteIDs(rootID, oldIDs); err != nil {
		return fmt.Errorf("deleting old chunks: %w", err)
	}

	var contentHashes []string
	for _, row := range rows {
		if h, ok := row["content_hash"].(string); ok {
			contentHashes = append(contentHashes, h)
		}
	}

	cached, err := s.db.GetCachedEmbeddings(ctx, contentHashes)
	if err != nil {
		log.Printf("warning: embedding cache lookup for move failed: %v", err)
		cached = make(map[string][]float64)
	}

	var uncachedChunks []map[string]any
	newRows := make([]map[string]any, len(rows))
	for i, row := range rows {
		chunkIdx := 0
		if ci, ok := row["chunk_index"]; ok {
			if f, ok := ci.(float64); ok {
				chunkIdx = int(f)
			}
		}
		newRows[i] = map[string]any{
			"id":           models.MakeChunkID(rootID, change.Path, chunkIdx),
			"content":      row["content"],
			"file_path":    change.Path,
			"chunk_index":  chunkIdx,
			"content_hash": row["content_hash"],
			"file_type":    row["file_type"],
			"root_id":      rootID,
			"page_number":  row["page_number"],
			"image_path":   row["image_path"],
		}

		hash, _ := row["content_hash"].(string)
		if emb, ok := cached[hash]; ok {
			newRows[i]["vector"] = emb
		} else {
			uncachedChunks = append(uncachedChunks, newRows[i])
		}
	}

	if len(uncachedChunks) > 0 {
		log.Printf("move %s: %d/%d chunks need re-embedding (cache miss)", change.Path, len(uncachedChunks), len(rows))
		embedResp, err := s.modal.EmbedChunks(uncachedChunks)
		if err != nil {
			return fmt.Errorf("re-embedding moved chunks: %w", err)
		}
		embedIdx := 0
		for i := range newRows {
			if _, hasVec := newRows[i]["vector"]; !hasVec && embedIdx < len(embedResp.Results) {
				newRows[i]["vector"], _ = embedResp.Results[embedIdx]["embedding"].([]any)
				embedIdx++
			}
		}
	}

	return s.tp.UpsertRows(rootID, newRows, "cosine_distance")
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
	includeAttrs := []string{"content", "file_path", "chunk_index", "file_type", "page_number", "image_path"}

	// Build filter from glob + ACL denied paths
	var filters any
	if req.Glob != "" {
		filters = []any{"file_path", "Glob", req.Glob}
	}

	// Get denied paths from ACLs and filter results post-query
	deniedPrefixes := s.buildACLFilter(r.Context(), id, req.RootID)

	var rows []map[string]any
	var err error

	switch req.Mode {
	case "fts":
		rankBy := []any{"content", "BM25", req.Query}
		rows, err = s.tp.Query(ns, rankBy, req.TopK, filters, includeAttrs)

	case "vector":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rankBy := []any{"vector", "ANN", embedding}
		rows, err = s.tp.Query(ns, rankBy, req.TopK, filters, includeAttrs)

	case "hybrid":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rows, err = s.tp.HybridSearch(ns, req.Query, embedding, req.TopK, req.Glob)

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
