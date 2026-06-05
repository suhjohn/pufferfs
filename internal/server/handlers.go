package server

import (
	"bytes"
	"compress/gzip"
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
	UploadStream(ctx context.Context, key string, body io.Reader, contentType string) error
	UploadCAS(ctx context.Context, key string, data []byte, contentType, ifMatch, ifNoneMatch string) (string, error)
	Download(ctx context.Context, key string) ([]byte, error)
	DownloadWithETag(ctx context.Context, key string) ([]byte, string, error)
	DownloadRange(ctx context.Context, key string, offset, length int64) ([]byte, error)
	DeleteMany(ctx context.Context, keys []string) error
	DeletePrefix(ctx context.Context, prefix string) (int, error)
}

// Server holds the dependencies for HTTP handlers.
type Server struct {
	db      *DB
	s3      objectStore
	modal   *ModalClient
	tp      *TPClient
	queue   queue.Queue
	billing *StripeClient
	mux     *http.ServeMux
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
	s.mux.HandleFunc("GET /cli/version", s.handleCLIVersion)

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

	// Platform admin
	s.mux.HandleFunc("POST /admin/orgs", s.handleAdminProvisionOrg)
	s.mux.HandleFunc("POST /admin/users", s.handleAdminProvisionUser)
	s.mux.HandleFunc("PUT /admin/orgs/{orgId}/members/{userId}", s.handleAdminUpsertMember)
	s.mux.HandleFunc("POST /admin/orgs/{orgId}/users/{userId}/api-keys", s.handleAdminCreateAPIKey)
	s.mux.HandleFunc("POST /admin/orgs/{orgId}/roots", s.handleAdminCreateRoot)
	s.mux.HandleFunc("DELETE /admin/roots/{id}", s.handleAdminDeleteRoot)
	s.mux.HandleFunc("DELETE /admin/orgs/{id}", s.handleAdminDeleteOrg)
	s.mux.HandleFunc("DELETE /admin/users/{id}", s.handleAdminDeleteUser)

	// Roots (org-scoped)
	s.mux.HandleFunc("POST /roots", s.handleCreateRoot)
	s.mux.HandleFunc("GET /roots", s.handleListRoots)
	s.mux.HandleFunc("GET /roots/{id}", s.handleGetRoot)
	s.mux.HandleFunc("DELETE /roots/{id}", s.handleDeleteRoot)
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

	// Billing (active only when Stripe is configured via SetBilling; otherwise
	// these return 404). The webhook is left unauthenticated in auth.Middleware.
	s.mux.HandleFunc("GET /billing", s.handleGetBilling)
	s.mux.HandleFunc("POST /billing/checkout-session", s.handleCreateCheckoutSession)
	s.mux.HandleFunc("POST /billing/webhook", s.handleStripeWebhook)
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

func (s *Server) handleCLIVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, cliReleaseManifestFromEnv())
}

func cliReleaseManifestFromEnv() models.CLIReleaseManifest {
	latest := cleanVersionEnv("PUFFERFS_CLI_LATEST_VERSION")
	minimum := cleanVersionEnv("PUFFERFS_CLI_MIN_VERSION")
	if latest == "" {
		latest = minimum
	}
	if latest == "" {
		latest = "dev"
	}

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PUFFERFS_CLI_DOWNLOAD_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = "https://github.com/suhjohn/pufferfs/releases/download"
	}
	downloadVersion := latest
	if downloadVersion != "dev" && !strings.HasPrefix(downloadVersion, "v") {
		downloadVersion = "v" + downloadVersion
	}

	manifest := models.CLIReleaseManifest{
		Latest:      latest,
		Minimum:     minimum,
		ProtocolMin: models.SyncProtocolVersion,
		ProtocolMax: models.SyncProtocolVersion,
		Downloads:   make(map[string]models.CLIDownload),
	}
	if downloadVersion != "dev" {
		for _, platform := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"} {
			assetOS, assetArch, _ := strings.Cut(platform, "-")
			assetName := fmt.Sprintf("pufferfs_%s_%s_%s.tar.gz", strings.TrimPrefix(downloadVersion, "v"), assetOS, assetArch)
			manifest.Downloads[platform] = models.CLIDownload{
				URL:    fmt.Sprintf("%s/%s/%s", baseURL, downloadVersion, assetName),
				SHA256: cleanSHAEnv(platform),
			}
		}
		manifest.NotesURL = fmt.Sprintf("%s/%s", baseURL, downloadVersion)
	}
	return manifest
}

func cleanVersionEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return ""
	}
	return strings.TrimPrefix(value, "v")
}

func cleanSHAEnv(platform string) string {
	key := "PUFFERFS_CLI_SHA256_" + strings.ToUpper(strings.ReplaceAll(platform, "-", "_"))
	return strings.TrimSpace(os.Getenv(key))
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
	if !auth.HasScope(id, "api_keys:write", "admin", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "api key write scope required"})
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
		req.Scopes = []string{"sync", "query", "root:delete"}
	}

	rawKey, err := s.db.CreateAPIKey(r.Context(), id.OrgID, id.UserID, req.Name, req.Scopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"key": rawKey,
	})
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "api_keys:read", "api_keys:write", "admin", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "api key read scope required"})
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
	if !auth.HasScope(id, "api_keys:write", "admin", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "api key write scope required"})
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
	if !auth.HasScope(id, "org:admin", "admin", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "org admin scope required"})
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
	if !auth.HasScope(id, "org:admin", "admin", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "org admin scope required"})
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
// Platform admin
// ---------------------------------------------------------------------------

func (s *Server) handleAdminProvisionOrg(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}

	var req struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Slug       string `json:"slug"`
		ExternalID string `json:"external_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = cleanSlug(req.Slug)
	req.ExternalID = strings.TrimSpace(req.ExternalID)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if req.Slug == "" {
		req.Slug = cleanSlug(req.Name)
	}
	if req.Slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slug is required"})
		return
	}

	org, err := s.db.ProvisionOrganization(r.Context(), strings.TrimSpace(req.ID), req.Name, req.Slug, req.ExternalID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, org)
}

func (s *Server) handleAdminProvisionUser(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}

	var req struct {
		ID         string `json:"id"`
		Email      string `json:"email"`
		Name       string `json:"name"`
		AvatarURL  string `json:"avatar_url"`
		Provider   string `json:"provider"`
		ProviderID string `json:"provider_id"`
		ExternalID string `json:"external_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	req.Name = strings.TrimSpace(req.Name)
	req.Provider = strings.TrimSpace(req.Provider)
	req.ProviderID = strings.TrimSpace(req.ProviderID)
	req.ExternalID = strings.TrimSpace(req.ExternalID)
	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email is required"})
		return
	}

	user, err := s.db.ProvisionUser(r.Context(), strings.TrimSpace(req.ID), req.Email, req.Name, strings.TrimSpace(req.AvatarURL), req.Provider, req.ProviderID, req.ExternalID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleAdminUpsertMember(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}

	orgID := r.PathValue("orgId")
	userID := r.PathValue("userId")
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	role, err := parseRole(req.Role, auth.RoleViewer)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.db.GetOrganization(r.Context(), orgID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	if _, err := s.db.GetUser(r.Context(), userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}
	if err := s.db.AddOrgMember(r.Context(), orgID, userID, role); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	member, err := s.db.GetOrgMember(r.Context(), orgID, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, member)
}

func (s *Server) handleAdminCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}

	orgID := r.PathValue("orgId")
	userID := r.PathValue("userId")
	var req struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		req.Name = "provisioned-key"
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{"query"}
	}
	if _, err := s.db.GetOrgMember(r.Context(), orgID, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member not found"})
		return
	}
	rawKey, err := s.db.CreateAPIKey(r.Context(), orgID, userID, req.Name, req.Scopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":     rawKey,
		"org_id":  orgID,
		"user_id": userID,
		"scopes":  req.Scopes,
	})
}

func (s *Server) handleAdminCreateRoot(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}

	orgID := r.PathValue("orgId")
	var req struct {
		Name        string `json:"name"`
		SourcePath  string `json:"source_path"`
		Scope       string `json:"scope"`
		OwnerUserID string `json:"owner_user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if _, err := s.db.GetOrganization(r.Context(), orgID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	scope, err := parseRootScope(req.Scope)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ownerUserID := strings.TrimSpace(req.OwnerUserID)
	if scope == models.RootScopeUser {
		if ownerUserID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner_user_id is required for user roots"})
			return
		}
		if _, err := s.db.GetOrgMember(r.Context(), orgID, ownerUserID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner must be a member of the org"})
			return
		}
	} else {
		ownerUserID = ""
	}

	root, err := s.db.CreateRootWithScope(r.Context(), orgID, req.Name, req.SourcePath, scope, ownerUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, root)
}

func (s *Server) handleAdminDeleteRoot(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	rootID := r.PathValue("id")
	root, err := s.db.GetRootAnyOrg(r.Context(), rootID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	active, err := s.db.RootHasActiveSync(r.Context(), root.OrgID, root.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "checking active syncs: " + err.Error()})
		return
	}
	if active {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "root has active sync jobs; wait for them to finish before deleting"})
		return
	}
	result, err := s.deleteRootArtifacts(r.Context(), root.OrgID, root.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.db.DeleteRoot(r.Context(), root.OrgID, root.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "deleting root metadata: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                 "deleted",
		"org_id":                 root.OrgID,
		"root_id":                root.ID,
		"name":                   root.Name,
		"turbopuffer_ns":         result.TurbopufferNamespace,
		"turbopuffer_namespaces": result.TurbopufferNamespaces,
		"s3_objects_deleted":     result.S3ObjectsDeleted,
	})
}

func (s *Server) handleAdminDeleteOrg(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("id")
	org, err := s.db.GetOrganization(r.Context(), orgID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	active, err := s.db.OrgHasActiveSync(r.Context(), orgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "checking active syncs: " + err.Error()})
		return
	}
	if active {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "org has active sync jobs; wait for them to finish before deleting"})
		return
	}
	roots, err := s.db.ListRoots(r.Context(), orgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing roots: " + err.Error()})
		return
	}
	deletedObjects := 0
	namespaces := []string{}
	for _, root := range roots {
		result, err := s.deleteRootArtifacts(r.Context(), orgID, root.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		deletedObjects += result.S3ObjectsDeleted
		namespaces = append(namespaces, result.TurbopufferNamespaces...)
	}
	if err := s.db.DeleteOrganization(r.Context(), orgID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "deleting org metadata: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                 "deleted",
		"org_id":                 org.ID,
		"name":                   org.Name,
		"roots_deleted":          len(roots),
		"turbopuffer_namespaces": namespaces,
		"s3_objects_deleted":     deletedObjects,
	})
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	userID := r.PathValue("id")
	user, err := s.db.GetUser(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}
	active, err := s.db.UserHasActiveSync(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "checking active syncs: " + err.Error()})
		return
	}
	if active {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "user has active sync jobs; wait for them to finish before deleting"})
		return
	}
	roots, err := s.db.ListRootsOwnedByUser(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing user roots: " + err.Error()})
		return
	}
	for _, root := range roots {
		active, err := s.db.RootHasActiveSync(r.Context(), root.OrgID, root.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "checking active syncs: " + err.Error()})
			return
		}
		if active {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "user owns roots with active sync jobs; wait for them to finish before deleting"})
			return
		}
	}
	deletedObjects := 0
	namespaces := []string{}
	for _, root := range roots {
		result, err := s.deleteRootArtifacts(r.Context(), root.OrgID, root.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		deletedObjects += result.S3ObjectsDeleted
		namespaces = append(namespaces, result.TurbopufferNamespaces...)
		if err := s.db.DeleteRoot(r.Context(), root.OrgID, root.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "deleting user root metadata: " + err.Error()})
			return
		}
	}
	if err := s.db.DeleteUser(r.Context(), userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "deleting user metadata: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                 "deleted",
		"user_id":                user.ID,
		"email":                  user.Email,
		"roots_deleted":          len(roots),
		"turbopuffer_namespaces": namespaces,
		"s3_objects_deleted":     deletedObjects,
	})
}

// ---------------------------------------------------------------------------
// Roots
// ---------------------------------------------------------------------------

func (s *Server) handleCreateRoot(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "sync", "root:create", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sync scope required"})
		return
	}

	var req struct {
		Name        string `json:"name"`
		SourcePath  string `json:"source_path"`
		Scope       string `json:"scope"`
		OwnerUserID string `json:"owner_user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	scope, err := parseRootScope(req.Scope)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ownerUserID := strings.TrimSpace(req.OwnerUserID)
	switch scope {
	case models.RootScopeOrg:
		if !auth.HasMinRole(id.Role, auth.RoleEditor) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "editor role required for org roots"})
			return
		}
		ownerUserID = ""
	case models.RootScopeUser:
		if ownerUserID == "" {
			ownerUserID = id.UserID
		}
		if ownerUserID != id.UserID && !auth.HasMinRole(id.Role, auth.RoleAdmin) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required to create roots for another user"})
			return
		}
		if _, err := s.db.GetOrgMember(r.Context(), id.OrgID, ownerUserID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner must be a member of the org"})
			return
		}
	}

	root, err := s.db.CreateRootWithScope(r.Context(), id.OrgID, req.Name, req.SourcePath, scope, ownerUserID)
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
	if !auth.HasScope(id, "query", "sync", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query or sync scope required"})
		return
	}
	roots, err := s.db.ListAccessibleRoots(r.Context(), id.OrgID, id.UserID, id.Role)
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
	if err != nil || !canReadRoot(id, root) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	if !auth.HasScope(id, "query", "sync", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query or sync scope required"})
		return
	}
	writeJSON(w, http.StatusOK, root)
}

func (s *Server) handleDeleteRoot(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "root:delete", "delete", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "root delete scope required"})
		return
	}

	rootID := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canDeleteRoot(id, root) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	active, err := s.db.RootHasActiveSync(r.Context(), id.OrgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "checking active syncs: " + err.Error()})
		return
	}
	if active {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "root has active sync jobs; wait for them to finish before deleting"})
		return
	}
	result, err := s.deleteRootArtifacts(r.Context(), id.OrgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := s.db.DeleteRoot(r.Context(), id.OrgID, rootID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "deleting root metadata: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":                 "deleted",
		"root_id":                root.ID,
		"name":                   root.Name,
		"turbopuffer_ns":         result.TurbopufferNamespace,
		"turbopuffer_namespaces": result.TurbopufferNamespaces,
		"s3_objects_deleted":     result.S3ObjectsDeleted,
	})
}

type rootArtifactDeleteResult struct {
	TurbopufferNamespace  string
	TurbopufferNamespaces []string
	S3ObjectsDeleted      int
}

func (s *Server) deleteRootArtifacts(ctx context.Context, orgID, rootID string) (rootArtifactDeleteResult, error) {
	result := rootArtifactDeleteResult{}

	generationIDs, err := s.db.ListSyncGenerationIDs(ctx, orgID, rootID)
	if err != nil {
		return result, fmt.Errorf("listing sync generations: %w", err)
	}
	indexNamespaces, err := s.db.ListRootIndexNamespaces(ctx, orgID, rootID)
	if err != nil {
		return result, fmt.Errorf("listing root index namespaces: %w", err)
	}
	for _, ns := range activeRootIndexNamespaces(indexNamespaces) {
		result.TurbopufferNamespaces = append(result.TurbopufferNamespaces, ns.Namespace)
	}
	if len(result.TurbopufferNamespaces) == 0 {
		result.TurbopufferNamespaces = []string{tpNamespace(orgID, rootID)}
	}
	result.TurbopufferNamespace = result.TurbopufferNamespaces[0]
	if s.tp != nil {
		for _, namespace := range result.TurbopufferNamespaces {
			if err := s.tp.DeleteNamespace(namespace); err != nil {
				return result, fmt.Errorf("deleting turbopuffer namespace %s: %w", namespace, err)
			}
		}
	}

	prefixes := []string{
		fmt.Sprintf("files/%s/", rootID),
		fmt.Sprintf("bundles/%s/", rootID),
		fmt.Sprintf("states/%s/", rootID),
		fmt.Sprintf("chunks/%s/", rootID),
	}
	for _, generationID := range generationIDs {
		prefixes = append(prefixes, fmt.Sprintf("syncs/%s/", generationID))
	}
	if s.s3 == nil {
		return result, nil
	}
	for _, prefix := range prefixes {
		count, err := s.s3.DeletePrefix(ctx, prefix)
		if err != nil {
			return result, fmt.Errorf("deleting storage prefix %s: %w", prefix, err)
		}
		result.S3ObjectsDeleted += count
	}
	return result, nil
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "sync", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sync scope required"})
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

	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canWriteRoot(id, root) {
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

	s3Key := fmt.Sprintf("files/%s/%s", rootID, filePath)
	if err := s.s3.UploadStream(r.Context(), s3Key, r.Body, "application/octet-stream"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"key": s3Key})
}

func (s *Server) handleUploadBundle(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "sync", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sync scope required"})
		return
	}

	rootID := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canWriteRoot(id, root) {
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

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	s3Key := fmt.Sprintf("bundles/%s/%s", rootID, bundleID)
	if err := s.s3.UploadStream(r.Context(), s3Key, r.Body, contentType); err != nil {
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
	if !auth.HasScope(id, "query", "sync", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query or sync scope required"})
		return
	}
	rootID := r.PathValue("id")

	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canReadRoot(id, root) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	state, err := s.loadRootState(r.Context(), rootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// handleSyncInit is kept for old clients. Namespace cloning is disabled.
func (s *Server) handleSyncInit(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "sync", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sync scope required"})
		return
	}

	rootID := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canWriteRoot(id, root) {
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
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "sync", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sync scope required"})
		return
	}

	rootID := r.PathValue("id")

	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canWriteRoot(id, root) {
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
	if req.State == nil && req.StateRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state or state_ref is required"})
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
	if err := s.ensureSyncStateRef(r.Context(), rootID, generation.ID, &req); err != nil {
		_ = s.db.MarkSyncGenerationFailed(r.Context(), generation.ID)
		if job != nil {
			_ = s.db.CompleteSyncJob(r.Context(), job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "preparing sync state: " + err.Error()})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.cleanupFailedGenerationRowsForRoot(ctx, id.OrgID, rootID); err != nil {
			log.Printf("warning: failed generation row cleanup for root %s: %v", rootID, err)
		}
	}()
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
		if cleanupErr := s.cleanupFailedGenerationRows(ctx, orgID, rootID, generation.ID); cleanupErr != nil {
			log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
		}
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
			if cleanupErr := s.cleanupFailedGenerationRows(ctx, orgID, rootID, generation.ID); cleanupErr != nil {
				log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
			}
			if job != nil {
				_ = s.db.CompleteSyncJob(ctx, job.ID, "failed", []map[string]string{{"error": err.Error()}})
			}
			return nil, fmt.Errorf("storing content proof: %w", err)
		}
	}
	if err := s.cleanupFailedGenerationRowsForRoot(ctx, orgID, rootID); err != nil {
		_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
		if cleanupErr := s.cleanupFailedGenerationRows(ctx, orgID, rootID, generation.ID); cleanupErr != nil {
			log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
		}
		if job != nil {
			_ = s.db.CompleteSyncJob(ctx, job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		return nil, fmt.Errorf("cleaning failed generations before commit: %w", err)
	}

	if err := s.db.CommitSyncGeneration(ctx, generation, req.State, req.StateRef); err != nil {
		_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
		if cleanupErr := s.cleanupFailedGenerationRows(ctx, orgID, rootID, generation.ID); cleanupErr != nil {
			log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
		}
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
	if !auth.HasScope(id, "query", "sync", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query or sync scope required"})
		return
	}
	rootID := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canReadRoot(id, root) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

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
	if !auth.HasScope(id, "acl:write", "admin", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ACL write scope required"})
		return
	}
	rootID := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canReadRoot(id, root) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

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
	if !auth.HasScope(id, "acl:read", "acl:write", "admin", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ACL read scope required"})
		return
	}
	rootID := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canReadRoot(id, root) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

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
	if !auth.HasScope(id, "acl:write", "admin", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ACL write scope required"})
		return
	}
	rootID := r.PathValue("id")
	root, err := s.db.GetRoot(r.Context(), id.OrgID, rootID)
	if err != nil || !canReadRoot(id, root) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
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
		root, rootErr := s.db.GetRoot(ctx, id.OrgID, rootID)
		return rootErr == nil && canWriteRoot(id, root)
	}

	return checkPermission(acls, filePath, "write")
}

func (s *Server) checkSyncWriteACL(ctx context.Context, id *auth.Identity, rootID string, req *models.SyncRequest) error {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil || len(acls) == 0 {
		root, rootErr := s.db.GetRoot(ctx, id.OrgID, rootID)
		if rootErr == nil && canWriteRoot(id, root) {
			return nil
		}
		return fmt.Errorf("write permission required")
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
		return nil
	}

	var proof models.ContentProofData
	if err := json.Unmarshal(proofBytes, &proof); err != nil {
		return nil
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

func stateObjectKey(rootID, generationID string) string {
	return fmt.Sprintf("states/%s/%s.json.gz", rootID, safeObjectName(generationID))
}

func (s *Server) ensureSyncStateRef(ctx context.Context, rootID, generationID string, req *models.SyncRequest) error {
	if req == nil || req.StateRef != "" || req.State == nil {
		return nil
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(req.State); err != nil {
		_ = gz.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	data := buf.Bytes()
	key := stateObjectKey(rootID, generationID)
	if err := s.s3.Upload(ctx, key, data, "application/gzip"); err != nil {
		return fmt.Errorf("uploading root state object %s: %w", key, err)
	}
	req.StateRef = key
	req.State = nil
	return nil
}

func (s *Server) loadRootState(ctx context.Context, rootID string) (map[string]models.FileState, error) {
	record, err := s.db.LoadStateRecord(ctx, rootID)
	if err != nil {
		return nil, err
	}
	if record.Ref == "" {
		if record.State == nil {
			return make(map[string]models.FileState), nil
		}
		return record.State, nil
	}
	if err := validateStateRef(rootID, record.Ref); err != nil {
		return nil, err
	}
	data, err := s.s3.Download(ctx, record.Ref)
	if err != nil {
		return nil, fmt.Errorf("downloading root state %s: %w", record.Ref, err)
	}
	state, err := decodeRootState(record.Ref, data)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func decodeRootState(ref string, data []byte) (map[string]models.FileState, error) {
	reader := io.Reader(bytes.NewReader(data))
	var gz *gzip.Reader
	var err error
	if strings.HasSuffix(ref, ".gz") {
		gz, err = gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("opening compressed root state %s: %w", ref, err)
		}
		defer gz.Close()
		reader = gz
	}
	var state map[string]models.FileState
	if err := json.NewDecoder(reader).Decode(&state); err != nil {
		return nil, fmt.Errorf("parsing root state %s: %w", ref, err)
	}
	if state == nil {
		state = make(map[string]models.FileState)
	}
	return state, nil
}

func newSyncSourceCache(s3 objectStore) *syncSourceCache {
	return &syncSourceCache{s3: s3, objects: make(map[string][]byte)}
}

func (c *syncSourceCache) read(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("empty source key")
	}
	if length > 0 {
		return c.s3.DownloadRange(ctx, key, offset, length)
	}
	if !strings.HasPrefix(key, "bundles/") {
		return c.s3.Download(ctx, key)
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
	if len(embedResults) != len(pending) {
		return fmt.Errorf("embedding sync chunks: got %d results for %d chunks", len(embedResults), len(pending))
	}
	log.Printf("timing stage=modal_embed_global chunks=%d elapsed=%s", len(chunks), time.Since(embedStart))

	cacheEntries := make(map[string][]float64)
	for i, result := range embedResults {
		embedding, ok := result["embedding"].([]any)
		if !ok {
			return fmt.Errorf("embedding result %d missing embedding vector", i)
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

func filteredQueryLimit(topK int) int {
	if topK < 1 {
		topK = 10
	}
	limit := topK * 10
	if limit < 50 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	return limit
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
	if absolutePath, ok := row["absolute_path"]; ok && absolutePath != nil {
		chunk["absolute_path"] = absolutePath
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
	if !auth.HasScope(id, "query", "read") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query scope required"})
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
	queryLimit := filteredQueryLimit(req.TopK)

	root, err := s.db.GetRoot(r.Context(), id.OrgID, req.RootID)
	if err != nil || !canReadRoot(id, root) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	indexNamespaces, err := s.db.ListRootIndexNamespaces(r.Context(), id.OrgID, req.RootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing root index namespaces: " + err.Error()})
		return
	}
	includeAttrs := []string{"content", "file_path", "absolute_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path", "generation_id", "valid_from_generation", "valid_from_generation_seq", "valid_to_generation", "valid_to_generation_seq"}

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
	var queryErr error

	switch req.Mode {
	case "fts":
		rankBy := []any{"content", "BM25", req.Query}
		rows, queryErr = queryRootIndexNamespaces(indexNamespaces, queryLimit, func(namespace string) ([]map[string]any, error) {
			return s.tp.Query(namespace, rankBy, queryLimit, tpAndFilter(filters), includeAttrs)
		})

	case "vector":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rankBy := []any{"vector", "ANN", embedding}
		rows, queryErr = queryRootIndexNamespaces(indexNamespaces, queryLimit, func(namespace string) ([]map[string]any, error) {
			return s.tp.Query(namespace, rankBy, queryLimit, tpAndFilter(filters), includeAttrs)
		})

	case "hybrid":
		embedding, embedErr := s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
		rows, queryErr = queryRootIndexNamespaces(indexNamespaces, queryLimit, func(namespace string) ([]map[string]any, error) {
			return s.tp.HybridSearch(namespace, req.Query, embedding, queryLimit, tpAndFilter(filters))
		})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be fts, vector, or hybrid"})
		return
	}

	if queryErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": queryErr.Error()})
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

	// Content proof filtering is only needed for user-owned roots. Org roots are
	// shared by membership plus ACL; applying per-user proofs there would hide
	// shared results from members who did not run the sync.
	if root.Scope == models.RootScopeUser && !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		filteredRows = s.filterByContentProof(r.Context(), id.OrgID, id.UserID, req.RootID, filteredRows)
	}
	if len(filteredRows) > req.TopK {
		filteredRows = filteredRows[:req.TopK]
	}

	results := make([]models.QueryResult, len(filteredRows))
	for i, row := range filteredRows {
		results[i] = models.QueryResult{
			FilePath:     strVal(row, "file_path"),
			AbsolutePath: strVal(row, "absolute_path"),
			Content:      strVal(row, "content"),
			FileType:     strVal(row, "file_type"),
			Score:        floatVal(row, "$dist"),
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

func parseRole(raw string, defaultRole auth.Role) (auth.Role, error) {
	role := auth.Role(strings.TrimSpace(raw))
	if role == "" {
		role = defaultRole
	}
	switch role {
	case auth.RoleOwner, auth.RoleAdmin, auth.RoleEditor, auth.RoleViewer:
		return role, nil
	default:
		return "", fmt.Errorf("role must be owner, admin, editor, or viewer")
	}
}

func parseRootScope(raw string) (string, error) {
	scope := strings.TrimSpace(raw)
	if scope == "" {
		scope = models.RootScopeOrg
	}
	switch scope {
	case models.RootScopeOrg, models.RootScopeUser:
		return scope, nil
	default:
		return "", fmt.Errorf("scope must be org or user")
	}
}

func cleanSlug(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	previousDash := false
	for _, r := range raw {
		writeDash := false
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			previousDash = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			previousDash = false
		case r == '-' || r == '_' || r == ' ' || r == '.':
			writeDash = true
		}
		if writeDash && !previousDash && b.Len() > 0 {
			b.WriteByte('-')
			previousDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func canReadRoot(id *auth.Identity, root *models.RootMetadata) bool {
	if id == nil || root == nil || id.OrgID != root.OrgID {
		return false
	}
	switch root.Scope {
	case "", models.RootScopeOrg:
		return true
	case models.RootScopeUser:
		return root.OwnerUserID == id.UserID || auth.HasMinRole(id.Role, auth.RoleAdmin)
	default:
		return false
	}
}

func canWriteRoot(id *auth.Identity, root *models.RootMetadata) bool {
	if !canReadRoot(id, root) {
		return false
	}
	switch root.Scope {
	case "", models.RootScopeOrg:
		return auth.HasMinRole(id.Role, auth.RoleEditor)
	case models.RootScopeUser:
		return root.OwnerUserID == id.UserID || auth.HasMinRole(id.Role, auth.RoleAdmin)
	default:
		return false
	}
}

func canDeleteRoot(id *auth.Identity, root *models.RootMetadata) bool {
	if !canReadRoot(id, root) {
		return false
	}
	switch root.Scope {
	case "", models.RootScopeOrg:
		return auth.HasMinRole(id.Role, auth.RoleAdmin)
	case models.RootScopeUser:
		return root.OwnerUserID == id.UserID || auth.HasMinRole(id.Role, auth.RoleAdmin)
	default:
		return false
	}
}

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
