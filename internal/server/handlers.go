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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	productanalytics "github.com/pufferfs/pufferfs/internal/analytics"
	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/internal/ignore"
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
	db          *DB
	s3          objectStore
	modal       *ModalClient
	tp          *TPClient
	queue       queue.Queue
	billing     *StripeClient
	emails      TransactionalEmailSender
	jwtSecret   []byte
	cookie      auth.CookieConfig
	frontend    string
	emailLogin  bool
	googleLogin bool
	analytics   productanalytics.Capturer
	mux         *http.ServeMux
}

// New creates a new Server with all dependencies.
func New(db *DB, s3 *storage.Client, modal *ModalClient, tp *TPClient) *Server {
	return NewWithStore(db, s3, modal, tp)
}

func NewWithStore(db *DB, s3 objectStore, modal *ModalClient, tp *TPClient) *Server {
	s := &Server{
		db:         db,
		s3:         s3,
		modal:      modal,
		tp:         tp,
		emailLogin: true,
		analytics:  productanalytics.Noop{},
		mux:        http.NewServeMux(),
	}
	s.routes()
	return s
}

// SetQueue enables the JetStream-backed sync path. Without a queue the server
// keeps using the legacy in-process sync pipeline.
func (s *Server) SetQueue(q queue.Queue) {
	s.queue = q
}

// SetInviteEmailSender enables best-effort email notifications for org invites.
func (s *Server) SetInviteEmailSender(sender InviteEmailSender) {
	s.emails = sender
}

// SetTransactionalEmailSender enables best-effort transactional product emails.
func (s *Server) SetTransactionalEmailSender(sender TransactionalEmailSender) {
	s.emails = sender
}

// SetSessionAuth configures browser/CLI session issuance for non-OAuth login
// providers such as email-code.
func (s *Server) SetSessionAuth(jwtSecret []byte, cookie auth.CookieConfig, frontendURL string) {
	s.jwtSecret = jwtSecret
	s.cookie = cookie
	s.frontend = strings.TrimRight(strings.TrimSpace(frontendURL), "/")
}

func (s *Server) SetEmailLoginEnabled(enabled bool) {
	s.emailLogin = enabled
}

func (s *Server) SetGoogleLoginEnabled(enabled bool) {
	s.googleLogin = enabled
}

// SetAnalytics enables best-effort product analytics.
func (s *Server) SetAnalytics(c productanalytics.Capturer) {
	if c == nil {
		s.analytics = productanalytics.Noop{}
		return
	}
	s.analytics = c
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
	s.mux.HandleFunc("GET /auth/providers", s.handleAuthProviders)
	s.mux.HandleFunc("POST /auth/email/start", s.handleEmailLoginStart)
	s.mux.HandleFunc("POST /auth/email/resend", s.handleEmailLoginStart)
	s.mux.HandleFunc("POST /auth/email/verify", s.handleEmailLoginVerify)
	s.mux.HandleFunc("GET /auth/me", s.handleMe)
	s.mux.HandleFunc("POST /auth/api-keys", s.handleCreateAPIKey)
	s.mux.HandleFunc("GET /auth/api-keys", s.handleListAPIKeys)
	s.mux.HandleFunc("DELETE /auth/api-keys/{id}", s.handleDeleteAPIKey)

	// Org management
	s.mux.HandleFunc("GET /org", s.handleGetOrg)
	s.mux.HandleFunc("GET /org/members", s.handleListMembers)
	s.mux.HandleFunc("POST /org/members", s.handleAddMember)
	s.mux.HandleFunc("PUT /org/members/{userId}", s.handleUpdateMemberRole)
	s.mux.HandleFunc("DELETE /org/members/{userId}", s.handleRemoveMember)
	s.mux.HandleFunc("GET /org/invites", s.handleListInvites)
	s.mux.HandleFunc("POST /org/invites", s.handleCreateInvite)
	s.mux.HandleFunc("DELETE /org/invites/{id}", s.handleDeleteInvite)
	s.mux.HandleFunc("GET /ignore-policy", s.handleGetEffectiveIgnorePolicy)
	s.mux.HandleFunc("GET /ignore-policy/user", s.handleGetUserIgnorePolicy)
	s.mux.HandleFunc("PUT /ignore-policy/user", s.handleSetUserIgnorePolicy)
	s.mux.HandleFunc("GET /ignore-policy/org", s.handleGetOrgIgnorePolicy)
	s.mux.HandleFunc("PUT /ignore-policy/org", s.handleSetOrgIgnorePolicy)

	// Platform admin
	s.mux.HandleFunc("POST /admin/orgs", s.handleAdminProvisionOrg)
	s.mux.HandleFunc("POST /admin/users", s.handleAdminProvisionUser)
	s.mux.HandleFunc("PUT /admin/orgs/{orgId}/members/{userId}", s.handleAdminUpsertMember)
	s.mux.HandleFunc("POST /admin/orgs/{orgId}/groups", s.handleAdminCreateGroup)
	s.mux.HandleFunc("GET /admin/orgs/{orgId}/groups", s.handleAdminListGroups)
	s.mux.HandleFunc("GET /admin/orgs/{orgId}/groups/{groupId}/members", s.handleAdminListGroupMembers)
	s.mux.HandleFunc("PUT /admin/orgs/{orgId}/groups/{groupId}/members/{userId}", s.handleAdminAddGroupMember)
	s.mux.HandleFunc("DELETE /admin/orgs/{orgId}/groups/{groupId}/members/{userId}", s.handleAdminDeleteGroupMember)
	s.mux.HandleFunc("POST /admin/orgs/{orgId}/users/{userId}/api-keys", s.handleAdminCreateAPIKey)
	s.mux.HandleFunc("POST /admin/orgs/{orgId}/roots", s.handleAdminCreateRoot)
	s.mux.HandleFunc("POST /admin/orgs/{orgId}/roots/{rootId}/grants", s.handleAdminCreateRootGrant)
	s.mux.HandleFunc("GET /admin/orgs/{orgId}/roots/{rootId}/grants", s.handleAdminListRootGrants)
	s.mux.HandleFunc("DELETE /admin/orgs/{orgId}/roots/{rootId}/grants/{grantId}", s.handleAdminDeleteRootGrant)
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
	s.mux.HandleFunc("POST /roots/{id}/sync/{generation_id}/upload", s.handleSyncArtifactUpload)
	s.mux.HandleFunc("DELETE /roots/{id}/sync/{generation_id}", s.handleSyncAbort)
	s.mux.HandleFunc("GET /roots/{id}/state", s.handleGetState)
	s.mux.HandleFunc("POST /roots/{id}/read", s.handleReadFile)
	s.mux.HandleFunc("GET /roots/{id}/assets", s.handleGetRootAsset)
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

func (s *Server) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"email_code": s.emailLogin && s.emails != nil,
		"google":     s.googleLogin,
	})
}

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
	scopes, err := normalizeExplicitAPIKeyScopes(req.Scopes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	rawKey, err := s.db.CreateAPIKey(r.Context(), id.OrgID, id.UserID, req.Name, scopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.captureBackendEvent(r.Context(), id, "api_key_created", map[string]any{
		"scope_count":      len(scopes),
		"has_query_scope":  hasScope(scopes, "query"),
		"has_sync_scope":   hasScope(scopes, "sync"),
		"has_delete_scope": hasScope(scopes, "root:delete"),
		"has_admin_scope":  hasScope(scopes, "admin") || hasScope(scopes, "org:admin"),
	})
	writeJSON(w, http.StatusCreated, map[string]string{
		"key": rawKey,
	})
}

func normalizeExplicitAPIKeyScopes(scopes []string) ([]string, error) {
	allowedScopes := map[string]struct{}{
		"query":          {},
		"sync":           {},
		"root:delete":    {},
		"api_keys:read":  {},
		"api_keys:write": {},
		"org:admin":      {},
		"read":           {},
		"write":          {},
		"admin":          {},
		"*":              {},
	}
	normalized := make([]string, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, ok := allowedScopes[scope]; !ok {
			return nil, fmt.Errorf("unsupported scope %q", scope)
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		normalized = append(normalized, scope)
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("explicit scopes required; use [\"query\"] for read-only keys or [\"sync\", \"query\"] for sync keys")
	}
	return normalized, nil
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
	s.captureBackendEvent(r.Context(), id, "api_key_revoked", nil)
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
	id, ok := requireOrgAdmin(w, r)
	if !ok {
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
	role, err := parseRole(req.Role, auth.RoleViewer)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !canAssignRole(id.Role, role) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot assign that role"})
		return
	}
	if strings.TrimSpace(req.UserID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id required"})
		return
	}
	if err := s.db.AddOrgMember(r.Context(), id.OrgID, strings.TrimSpace(req.UserID), role); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

func (s *Server) handleUpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	id, ok := requireOrgAdmin(w, r)
	if !ok {
		return
	}
	userID := r.PathValue("userId")
	if userID == id.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot change your own role"})
		return
	}
	member, err := s.db.GetOrgMember(r.Context(), id.OrgID, userID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member not found"})
		return
	}

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
	if !canAssignRole(id.Role, role) || !canManageMemberRole(id.Role, auth.Role(member.Role)) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot change that member role"})
		return
	}
	if auth.Role(member.Role) == auth.RoleOwner && role != auth.RoleOwner {
		if ok, err := s.canRemoveOwner(r.Context(), id.OrgID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		} else if !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "organization must keep at least one owner"})
			return
		}
	}
	if err := s.db.UpdateOrgMemberRole(r.Context(), id.OrgID, userID, role); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	updated, err := s.db.GetOrgMember(r.Context(), id.OrgID, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.captureBackendEvent(r.Context(), id, "org_member_role_updated", map[string]any{
		"target_role": updated.Role,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	id, ok := requireOrgAdmin(w, r)
	if !ok {
		return
	}
	userID := r.PathValue("userId")
	if userID == id.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot remove yourself"})
		return
	}
	member, err := s.db.GetOrgMember(r.Context(), id.OrgID, userID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member not found"})
		return
	}
	memberRole := auth.Role(member.Role)
	if !canManageMemberRole(id.Role, memberRole) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot remove that member"})
		return
	}
	if memberRole == auth.RoleOwner {
		if ok, err := s.canRemoveOwner(r.Context(), id.OrgID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		} else if !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "organization must keep at least one owner"})
			return
		}
	}
	if err := s.db.RemoveOrgMember(r.Context(), id.OrgID, userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.captureBackendEvent(r.Context(), id, "org_member_removed", map[string]any{
		"target_role": string(memberRole),
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	invites, err := s.db.ListOrgInvites(r.Context(), id.OrgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, invites)
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := requireOrgAdmin(w, r)
	if !ok {
		return
	}
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	email := normalizeEmail(req.Email)
	if email == "" || !strings.Contains(email, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid email required"})
		return
	}
	role, err := parseRole(req.Role, auth.RoleViewer)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !canAssignRole(id.Role, role) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot invite that role"})
		return
	}
	invite, err := s.db.InviteOrgMember(r.Context(), id.OrgID, email, role, id.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp := orgInviteResponse{OrgInvite: *invite}
	if s.emails != nil {
		org, err := s.db.GetOrganization(r.Context(), id.OrgID)
		if err != nil {
			resp.EmailError = "invite email was not sent: " + err.Error()
		} else if err := s.emails.SendOrgInvite(r.Context(), OrgInviteEmail{
			To:           invite.Email,
			Role:         invite.Role,
			OrgName:      org.Name,
			InviterID:    id.UserID,
			InviterEmail: id.Email,
		}); err != nil {
			resp.EmailError = "invite email was not sent: " + err.Error()
		} else {
			resp.EmailSent = true
		}
	}
	if resp.EmailError != "" {
		log.Printf("org invite email failed for org=%s invite=%s email=%s: %s", id.OrgID, invite.ID, invite.Email, resp.EmailError)
	}
	s.captureBackendEvent(r.Context(), id, "org_invite_created", map[string]any{
		"target_role":  invite.Role,
		"email_domain": emailDomain(invite.Email),
		"email_sent":   resp.EmailSent,
	})
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleDeleteInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := requireOrgAdmin(w, r)
	if !ok {
		return
	}
	inviteID := r.PathValue("id")
	invite, err := s.db.GetOrgInvite(r.Context(), id.OrgID, inviteID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "invite not found"})
		return
	}
	if !canManageMemberRole(id.Role, auth.Role(invite.Role)) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot revoke that invite"})
		return
	}
	if err := s.db.DeleteOrgInvite(r.Context(), id.OrgID, inviteID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.captureBackendEvent(r.Context(), id, "org_invite_revoked", map[string]any{
		"target_role":  invite.Role,
		"email_domain": emailDomain(invite.Email),
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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

func (s *Server) handleAdminCreateGroup(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("orgId")
	var req struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ExternalID string `json:"external_id"`
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
	group, err := s.db.CreateGroup(r.Context(), orgID, strings.TrimSpace(req.ID), req.Name, strings.TrimSpace(req.ExternalID))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, group)
}

func (s *Server) handleAdminListGroups(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("orgId")
	if _, err := s.db.GetOrganization(r.Context(), orgID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "org not found"})
		return
	}
	groups, err := s.db.ListGroups(r.Context(), orgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) handleAdminListGroupMembers(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("orgId")
	groupID := r.PathValue("groupId")
	if _, err := s.db.GetGroup(r.Context(), orgID, groupID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group not found"})
		return
	}
	members, err := s.db.ListGroupMembers(r.Context(), orgID, groupID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, members)
}

func (s *Server) handleAdminAddGroupMember(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("orgId")
	groupID := r.PathValue("groupId")
	userID := r.PathValue("userId")
	if _, err := s.db.GetGroup(r.Context(), orgID, groupID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group not found"})
		return
	}
	if _, err := s.db.GetOrgMember(r.Context(), orgID, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member not found"})
		return
	}
	member, err := s.db.AddGroupMember(r.Context(), orgID, groupID, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, member)
}

func (s *Server) handleAdminDeleteGroupMember(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("orgId")
	groupID := r.PathValue("groupId")
	userID := r.PathValue("userId")
	if err := s.db.DeleteGroupMember(r.Context(), orgID, groupID, userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "group member not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
	scopes, err := normalizeExplicitAPIKeyScopes(req.Scopes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.db.GetOrgMember(r.Context(), orgID, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member not found"})
		return
	}
	rawKey, err := s.db.CreateAPIKey(r.Context(), orgID, userID, req.Name, scopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":     rawKey,
		"org_id":  orgID,
		"user_id": userID,
		"scopes":  scopes,
	})
}

func (s *Server) handleAdminCreateRoot(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}

	orgID := r.PathValue("orgId")
	var req struct {
		Name           string `json:"name"`
		SourcePath     string `json:"source_path"`
		Scope          string `json:"scope"`
		OwnerUserID    string `json:"owner_user_id"`
		VectorDisabled bool   `json:"vector_disabled"`
		DisableVector  bool   `json:"disable_vector"`
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

	root, err := s.db.CreateRootWithScopeAndFeatures(r.Context(), orgID, req.Name, req.SourcePath, scope, ownerUserID, req.VectorDisabled || req.DisableVector)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, root)
}

func (s *Server) handleAdminCreateRootGrant(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("orgId")
	rootID := r.PathValue("rootId")
	root, err := s.db.GetRoot(r.Context(), orgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	var req struct {
		PrincipalType string   `json:"principal_type"`
		PrincipalID   string   `json:"principal_id"`
		Permissions   []string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	principalType, principalID, ok := s.validateRootGrantPrincipal(w, r, orgID, req.PrincipalType, req.PrincipalID)
	if !ok {
		return
	}
	permissions, err := normalizeRootGrantPermissions(req.Permissions)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	grant, err := s.db.CreateRootGrant(r.Context(), orgID, root.ID, principalType, principalID, permissions)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}

func (s *Server) handleAdminListRootGrants(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("orgId")
	rootID := r.PathValue("rootId")
	if _, err := s.db.GetRoot(r.Context(), orgID, rootID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	grants, err := s.db.ListRootGrants(r.Context(), orgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) handleAdminDeleteRootGrant(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API key required"})
		return
	}
	orgID := r.PathValue("orgId")
	rootID := r.PathValue("rootId")
	grantID := r.PathValue("grantId")
	if err := s.db.DeleteRootGrant(r.Context(), orgID, rootID, grantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "grant not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
		Name           string `json:"name"`
		SourcePath     string `json:"source_path"`
		Scope          string `json:"scope"`
		OwnerUserID    string `json:"owner_user_id"`
		VectorDisabled bool   `json:"vector_disabled"`
		DisableVector  bool   `json:"disable_vector"`
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
	case models.RootScopeRestricted:
		if !auth.HasMinRole(id.Role, auth.RoleAdmin) || !auth.HasScope(id, "org:admin", "admin", "write") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "org admin scope required for restricted roots"})
			return
		}
		ownerUserID = ""
	}

	root, err := s.db.CreateRootWithScopeAndFeatures(r.Context(), id.OrgID, req.Name, req.SourcePath, scope, ownerUserID, req.VectorDisabled || req.DisableVector)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.captureBackendEvent(r.Context(), id, "root_created", map[string]any{
		"root_scope":      rootScopeProperty(root),
		"owned_by_actor":  root.OwnerUserID == "" || root.OwnerUserID == id.UserID,
		"vector_disabled": root.VectorDisabled,
	})
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
	root, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead)
	if err != nil || !ok {
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
	root, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionDelete)
	if err != nil || !ok {
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

	s.captureBackendEvent(r.Context(), id, "root_deleted", map[string]any{
		"root_scope":             rootScopeProperty(root),
		"had_visible_generation": root.VisibleGenerationID != "" || root.VisibleGenerationSeq > 0,
		"turbopuffer_namespaces": result.TurbopufferNamespaces,
		"s3_objects_deleted":     result.S3ObjectsDeleted,
	})
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

	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionSync)
	if err != nil || !ok {
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

	generationID := strings.TrimSpace(r.URL.Query().Get("generation_id"))
	s3Key := fmt.Sprintf("files/%s/%s", rootID, filePath)
	if generationID != "" {
		if _, err := s.db.GetSyncGeneration(r.Context(), id.OrgID, rootID, generationID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "sync generation not found"})
			return
		}
		s3Key = syncSourceFileKey(generationID, filePath)
	}
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
	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionSync)
	if err != nil || !ok {
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
	generationID := strings.TrimSpace(r.URL.Query().Get("generation_id"))
	s3Key := fmt.Sprintf("bundles/%s/%s", rootID, bundleID)
	if generationID != "" {
		if _, err := s.db.GetSyncGeneration(r.Context(), id.OrgID, rootID, generationID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "sync generation not found"})
			return
		}
		s3Key = syncSourceBundleKey(generationID, bundleID)
	}
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

func (s *Server) handleSyncArtifactUpload(w http.ResponseWriter, r *http.Request) {
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
	generationID := r.PathValue("generation_id")
	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionSync)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	if _, err := s.db.GetSyncGeneration(r.Context(), id.OrgID, rootID, generationID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sync generation not found"})
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	name := safeObjectName(strings.TrimSpace(r.URL.Query().Get("name")))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name query param required"})
		return
	}
	var s3Key string
	switch kind {
	case "manifest":
		s3Key = fmt.Sprintf("syncs/%s/manifests/%s", generationID, name)
	case "proof":
		s3Key = fmt.Sprintf("syncs/%s/proofs/%s", generationID, name)
	case "state":
		s3Key = stateObjectKey(rootID, generationID)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be manifest, proof, or state"})
		return
	}
	const maxUploadSize = 1024 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := s.s3.UploadStream(r.Context(), s3Key, r.Body, contentType); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": s3Key})
}

func (s *Server) handleSyncAbort(w http.ResponseWriter, r *http.Request) {
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
	generationID := r.PathValue("generation_id")
	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionSync)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	generation, err := s.db.GetSyncGeneration(r.Context(), id.OrgID, rootID, generationID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sync generation not found"})
		return
	}
	_ = s.db.MarkSyncGenerationFailed(r.Context(), generation.ID)
	if generation.SyncJobID != "" {
		_ = s.db.CompleteSyncJob(r.Context(), generation.SyncJobID, "failed", []map[string]string{{"error": "sync aborted"}})
	}
	if err := s.cleanupTerminalSyncObjects(r.Context(), rootID, generation.ID, nil); err != nil {
		log.Printf("warning: failed aborted sync object cleanup for root %s generation %s: %v", rootID, generation.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
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

	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead)
	if err != nil || !ok {
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
	root, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionSync)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	if r.Body == nil || r.ContentLength == 0 {
		writeJSON(w, http.StatusOK, map[string]bool{"can_reuse": false})
		return
	}

	var req models.SyncInitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.ProtocolVersion != models.SyncProtocolVersion {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":            fmt.Sprintf("unsupported sync protocol_version %d", req.ProtocolVersion),
			"protocol_version": req.ProtocolVersion,
			"required_version": models.SyncProtocolVersion,
		})
		return
	}
	if err := validateSyncBase(req.BaseGenerationID, req.BaseGenerationSeq, root.VisibleGenerationID, root.VisibleGenerationSeq); err != nil {
		writeSyncConflict(w, err, &models.SyncRequest{BaseGenerationID: req.BaseGenerationID, BaseGenerationSeq: req.BaseGenerationSeq}, root.VisibleGenerationID, root.VisibleGenerationSeq)
		return
	}
	job, err := s.db.CreateSyncJob(r.Context(), id.OrgID, rootID, id.UserID, req.TotalFiles)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "creating sync job: " + err.Error()})
		return
	}
	generation, err := s.db.CreateSyncGeneration(r.Context(), id.OrgID, rootID, job.ID, "", req.BaseGenerationID, req.BaseGenerationSeq)
	if err != nil {
		_ = s.db.CompleteSyncJob(r.Context(), job.ID, "failed", []map[string]string{{"error": err.Error()}})
		if errors.Is(err, errStaleSyncBase) {
			if currentRoot, rootErr := s.db.GetRoot(r.Context(), id.OrgID, rootID); rootErr == nil {
				writeSyncConflict(w, err, &models.SyncRequest{BaseGenerationID: req.BaseGenerationID, BaseGenerationSeq: req.BaseGenerationSeq}, currentRoot.VisibleGenerationID, currentRoot.VisibleGenerationSeq)
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
	s.captureSyncStarted(r.Context(), id, root, &models.SyncRequest{
		BaseGenerationID:  req.BaseGenerationID,
		BaseGenerationSeq: req.BaseGenerationSeq,
		ProtocolVersion:   req.ProtocolVersion,
	}, job, req.TotalFiles)
	writeJSON(w, http.StatusOK, models.SyncInitResponse{
		RootID:            rootID,
		SyncJobID:         job.ID,
		GenerationID:      generation.ID,
		GenerationSeq:     generation.Seq,
		BaseGenerationID:  generation.BaseGenerationID,
		BaseGenerationSeq: generation.BaseGenerationSeq,
		ManifestPrefix:    fmt.Sprintf("syncs/%s/manifests/", generation.ID),
	})
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

	root, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionSync)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	var req models.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.RootID = rootID
	req.DisableVector = root.VectorDisabled
	if req.ProtocolVersion != models.SyncProtocolVersion {
		s.cleanupRejectedSyncRequest(r.Context(), id.OrgID, rootID, &req, fmt.Sprintf("unsupported sync protocol_version %d", req.ProtocolVersion))
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":            fmt.Sprintf("unsupported sync protocol_version %d", req.ProtocolVersion),
			"protocol_version": req.ProtocolVersion,
			"required_version": models.SyncProtocolVersion,
		})
		return
	}
	if err := normalizeSyncRequest(&req); err != nil {
		s.cleanupRejectedSyncRequest(r.Context(), id.OrgID, rootID, &req, err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.State == nil && req.StateRef == "" {
		s.cleanupRejectedSyncRequest(r.Context(), id.OrgID, rootID, &req, "state or state_ref is required")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state or state_ref is required"})
		return
	}
	if len(req.Changes) == 0 && len(req.ChangeRefs) == 0 {
		s.cleanupRejectedSyncRequest(r.Context(), id.OrgID, rootID, &req, "changes or change_refs is required")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "changes or change_refs is required"})
		return
	}
	if err := s.validateSyncIgnorePolicy(r.Context(), id, &req); err != nil {
		s.cleanupRejectedSyncRequest(r.Context(), id.OrgID, rootID, &req, err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.checkSyncWriteACL(r.Context(), id, rootID, &req); err != nil {
		s.cleanupRejectedSyncRequest(r.Context(), id.OrgID, rootID, &req, err.Error())
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if err := validateSyncBase(req.BaseGenerationID, req.BaseGenerationSeq, root.VisibleGenerationID, root.VisibleGenerationSeq); err != nil {
		s.cleanupRejectedSyncRequest(r.Context(), id.OrgID, rootID, &req, err.Error())
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
	if actionableFiles == 0 && req.ChangeCount > 0 {
		actionableFiles = req.ChangeCount
	}

	var job *models.SyncJob
	var generation *SyncGeneration
	if req.GenerationID != "" {
		generation, err = s.db.GetSyncGeneration(r.Context(), id.OrgID, rootID, req.GenerationID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "sync generation not found"})
			return
		}
		if generation.SyncJobID != "" {
			job, err = s.db.GetSyncJob(r.Context(), id.OrgID, generation.SyncJobID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "loading sync job: " + err.Error()})
				return
			}
		}
	} else {
		job, err = s.db.CreateSyncJob(r.Context(), id.OrgID, rootID, id.UserID, actionableFiles)
		if err != nil {
			log.Printf("warning: failed to create sync job: %v", err)
		}
		syncJobID := ""
		if job != nil {
			syncJobID = job.ID
		}
		generation, err = s.db.CreateSyncGeneration(r.Context(), id.OrgID, rootID, syncJobID, req.ManifestRef, req.BaseGenerationID, req.BaseGenerationSeq)
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
		s.captureSyncStarted(r.Context(), id, root, &req, job, actionableFiles)
	}
	if err := s.ensureSyncStateRef(r.Context(), rootID, generation.ID, &req); err != nil {
		_ = s.db.MarkSyncGenerationFailed(r.Context(), generation.ID)
		if job != nil {
			_ = s.db.CompleteSyncJob(r.Context(), job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		if cleanupErr := s.cleanupTerminalSyncObjects(r.Context(), rootID, generation.ID, &req); cleanupErr != nil {
			log.Printf("warning: failed sync object cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
		}
		s.captureSyncFailed(r.Context(), id.OrgID, id.UserID, root, &req, job, "prepare_state")
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

func (s *Server) cleanupRejectedSyncRequest(ctx context.Context, orgID, rootID string, req *models.SyncRequest, reason string) {
	if req == nil || strings.TrimSpace(req.GenerationID) == "" {
		return
	}
	generation, err := s.db.GetSyncGeneration(ctx, orgID, rootID, strings.TrimSpace(req.GenerationID))
	if err != nil {
		return
	}
	_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
	if generation.SyncJobID != "" {
		_ = s.db.CompleteSyncJob(ctx, generation.SyncJobID, "failed", []map[string]string{{"error": reason}})
	}
	if cleanupErr := s.cleanupTerminalSyncObjects(ctx, rootID, generation.ID, nil); cleanupErr != nil {
		log.Printf("warning: failed rejected sync object cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
	}
}

func (s *Server) validateSyncIgnorePolicy(ctx context.Context, id *auth.Identity, req *models.SyncRequest) error {
	if id == nil || req == nil {
		return nil
	}
	policy, err := s.db.GetEffectiveIgnorePolicy(ctx, id.OrgID, id.UserID)
	if err != nil {
		return fmt.Errorf("loading ignore policy: %w", err)
	}
	if strings.TrimSpace(policy.OrgPatterns) == "" && strings.TrimSpace(policy.UserPatterns) == "" {
		return nil
	}
	matcher := ignore.NewPolicyMatcher(ignore.PolicyPatternSet{
		OrgPatterns:  policy.OrgPatterns,
		UserPatterns: policy.UserPatterns,
	})
	changes, err := s.syncRequestChangesForACL(ctx, req)
	if err != nil {
		return err
	}
	for _, change := range changes {
		for _, path := range syncChangePolicyPaths(change) {
			if matcher.ShouldIgnore(path, false) {
				return fmt.Errorf("path %q is ignored by org/user policy", path)
			}
		}
	}
	return nil
}

func syncChangePolicyPaths(change models.FileChange) []string {
	switch change.Status {
	case models.StatusRemoved:
		return nil
	case models.StatusMoved, models.StatusRenamed:
		if strings.TrimSpace(change.Path) == "" {
			return nil
		}
		return []string{change.Path}
	}
	paths := make([]string, 0, 2)
	if strings.TrimSpace(change.Path) != "" {
		paths = append(paths, change.Path)
	}
	return paths
}

func writeSyncConflict(w http.ResponseWriter, err error, req *models.SyncRequest, currentGenerationID string, currentGenerationSeq int64) {
	var clientBaseGenerationID string
	var clientBaseGenerationSeq int64
	if req != nil {
		clientBaseGenerationID = req.BaseGenerationID
		clientBaseGenerationSeq = req.BaseGenerationSeq
	}
	writeJSON(w, http.StatusConflict, models.SyncConflictResponse{
		Error:                   err.Error(),
		ClientBaseGenerationID:  clientBaseGenerationID,
		ClientBaseGenerationSeq: clientBaseGenerationSeq,
		CurrentGenerationID:     currentGenerationID,
		CurrentGenerationSeq:    currentGenerationSeq,
	})
}

func (s *Server) runSyncJob(ctx context.Context, orgID, userID, rootID string, generation *SyncGeneration, req *models.SyncRequest, job *models.SyncJob) (*models.SyncResponse, error) {
	root, _ := s.db.GetRoot(ctx, orgID, rootID)
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
		if cleanupObjErr := s.cleanupTerminalSyncObjects(ctx, rootID, generation.ID, req); cleanupObjErr != nil {
			log.Printf("warning: failed sync object cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupObjErr)
		}
		s.captureSyncFailed(ctx, orgID, userID, root, req, job, "pipeline")
		return nil, err
	}
	if s.queue != nil {
		return resp, nil
	}

	if err := s.storeSyncContentProof(ctx, orgID, userID, rootID, req); err != nil {
		_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
		if cleanupErr := s.cleanupFailedGenerationRows(ctx, orgID, rootID, generation.ID); cleanupErr != nil {
			log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
		}
		if job != nil {
			_ = s.db.CompleteSyncJob(ctx, job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		if cleanupObjErr := s.cleanupTerminalSyncObjects(ctx, rootID, generation.ID, req); cleanupObjErr != nil {
			log.Printf("warning: failed sync object cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupObjErr)
		}
		s.captureSyncFailed(ctx, orgID, userID, root, req, job, "content_proof")
		return nil, fmt.Errorf("storing content proof: %w", err)
	}
	if err := s.cleanupFailedGenerationRowsForRoot(ctx, orgID, rootID); err != nil {
		_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
		if cleanupErr := s.cleanupFailedGenerationRows(ctx, orgID, rootID, generation.ID); cleanupErr != nil {
			log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
		}
		if job != nil {
			_ = s.db.CompleteSyncJob(ctx, job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		if cleanupObjErr := s.cleanupTerminalSyncObjects(ctx, rootID, generation.ID, req); cleanupObjErr != nil {
			log.Printf("warning: failed sync object cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupObjErr)
		}
		s.captureSyncFailed(ctx, orgID, userID, root, req, job, "cleanup")
		return nil, fmt.Errorf("cleaning failed generations before commit: %w", err)
	}

	if job != nil {
		_ = s.db.UpdateSyncJobStatus(ctx, job.ID, "committing")
	}
	if err := s.db.CommitSyncGeneration(ctx, generation, req.State, req.StateRef); err != nil {
		_ = s.db.MarkSyncGenerationFailed(ctx, generation.ID)
		if cleanupErr := s.cleanupFailedGenerationRows(ctx, orgID, rootID, generation.ID); cleanupErr != nil {
			log.Printf("warning: failed generation row cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupErr)
		}
		if job != nil {
			_ = s.db.CompleteSyncJob(ctx, job.ID, "failed", []map[string]string{{"error": err.Error()}})
		}
		if cleanupObjErr := s.cleanupTerminalSyncObjects(ctx, rootID, generation.ID, req); cleanupObjErr != nil {
			log.Printf("warning: failed sync object cleanup for root %s generation %s: %v", rootID, generation.ID, cleanupObjErr)
		}
		s.captureSyncFailed(ctx, orgID, userID, root, req, job, "commit")
		return nil, fmt.Errorf("committing generation: %w", err)
	}
	if job != nil {
		if err := s.db.CompleteSyncJob(ctx, job.ID, "completed", nil); err != nil {
			log.Printf("error completing sync job %s: %v", job.ID, err)
		}
		resp.SyncJobID = job.ID
	}
	s.captureSyncCompleted(ctx, orgID, userID, root, req, job, resp)
	if err := s.cleanupCommittedSyncObjects(ctx, rootID, generation, req); err != nil {
		log.Printf("warning: failed committed sync object cleanup for root %s generation %s: %v", rootID, generation.ID, err)
	}
	return resp, nil
}

func (s *Server) runSyncPipeline(ctx context.Context, orgID, userID string, generation *SyncGeneration, req *models.SyncRequest, job *models.SyncJob) (*models.SyncResponse, error) {
	if s.queue != nil {
		return s.enqueueSync(ctx, orgID, userID, generation, req, job)
	}
	return s.processSync(ctx, orgID, generation, req, job)
}

func (s *Server) storeSyncContentProof(ctx context.Context, orgID, userID, rootID string, req *models.SyncRequest) error {
	if req == nil {
		return nil
	}
	proof := req.ContentProof
	var proofBytes []byte
	if proof != nil {
		proofBytes, _ = json.Marshal(proof)
	} else if req.ContentProofRef != "" {
		data, err := s.s3.Download(ctx, req.ContentProofRef)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", req.ContentProofRef, err)
		}
		var parsed models.ContentProofData
		if err := json.Unmarshal(data, &parsed); err != nil {
			return fmt.Errorf("parsing %s: %w", req.ContentProofRef, err)
		}
		proof = &parsed
		proofBytes = data
	}
	if proof == nil {
		return nil
	}
	return s.db.UpsertContentProof(ctx, orgID, userID, rootID, proof.RootHash, proofBytes)
}

func (s *Server) cleanupCommittedSyncObjects(ctx context.Context, rootID string, generation *SyncGeneration, req *models.SyncRequest) error {
	if generation == nil {
		return nil
	}
	return s.cleanupTerminalSyncObjects(ctx, rootID, generation.ID, req)
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
	if !auth.HasScope(id, "query", "sync", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query or sync scope required"})
		return
	}
	rootID := r.PathValue("id")
	if _, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead); err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

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
	if generation, err := s.db.GetSyncGenerationForJob(ctx, job.OrgID, job.RootID, job.ID); err == nil {
		req := s.syncRequestForCleanup(ctx, generation.ID)
		if cleanupErr := s.cleanupTerminalSyncObjects(ctx, job.RootID, generation.ID, req); cleanupErr != nil {
			log.Printf("warning: failed expired sync object cleanup for root %s generation %s: %v", job.RootID, generation.ID, cleanupErr)
		}
	} else {
		log.Printf("warning: failed to load expired sync generation for job %s: %v", job.ID, err)
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
	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead)
	if err != nil || !ok {
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
// Ignore policies
// ---------------------------------------------------------------------------

func (s *Server) handleGetEffectiveIgnorePolicy(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "query", "sync", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read or sync scope required"})
		return
	}
	policy, err := s.db.GetEffectiveIgnorePolicy(r.Context(), id.OrgID, id.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handleGetOrgIgnorePolicy(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "query", "sync", "read", "write", "org:admin", "admin") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read or sync scope required"})
		return
	}
	policy, err := s.db.GetOrgIgnorePolicy(r.Context(), id.OrgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handleSetOrgIgnorePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := requireOrgAdmin(w, r)
	if !ok {
		return
	}
	patterns, ok := decodeIgnorePolicyUpdate(w, r)
	if !ok {
		return
	}
	policy, err := s.db.SetOrgIgnorePolicy(r.Context(), id.OrgID, id.UserID, patterns)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handleGetUserIgnorePolicy(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "query", "sync", "read", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read or sync scope required"})
		return
	}
	policy, err := s.db.GetUserIgnorePolicy(r.Context(), id.OrgID, id.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handleSetUserIgnorePolicy(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "sync", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sync scope required"})
		return
	}
	patterns, ok := decodeIgnorePolicyUpdate(w, r)
	if !ok {
		return
	}
	policy, err := s.db.SetUserIgnorePolicy(r.Context(), id.OrgID, id.UserID, patterns)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func decodeIgnorePolicyUpdate(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req models.IgnorePolicyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return "", false
	}
	patterns, err := normalizeIgnorePolicyPatterns(req.Patterns)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return "", false
	}
	return patterns, true
}

func normalizeIgnorePolicyPatterns(patterns string) (string, error) {
	const maxIgnorePolicyBytes = 256 << 10
	patterns = strings.ReplaceAll(patterns, "\r\n", "\n")
	patterns = strings.ReplaceAll(patterns, "\r", "\n")
	if len(patterns) > maxIgnorePolicyBytes {
		return "", fmt.Errorf("ignore policy is too large; max %d bytes", maxIgnorePolicyBytes)
	}
	if strings.Contains(patterns, "\x00") {
		return "", fmt.Errorf("ignore policy contains NUL byte")
	}
	return patterns, nil
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
	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead)
	if err != nil || !ok {
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
	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead)
	if err != nil || !ok {
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
	_, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead)
	if err != nil || !ok {
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
// Deterministic file reads
// ---------------------------------------------------------------------------

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "query", "read") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query scope required"})
		return
	}

	rootID := r.PathValue("id")
	root, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}

	var req models.ReadFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Path, err = cleanFilePath(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if (req.Pages == nil) == (req.Lines == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "exactly one of pages or lines is required"})
		return
	}
	if !s.checkReadACL(r.Context(), id, rootID, req.Path) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	if req.Pages != nil {
		resp, err := s.readFilePages(r.Context(), id, root, &req, r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp, err := s.readFileLines(r.Context(), id, root, &req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetRootAsset(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasScope(id, "query", "read") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query scope required"})
		return
	}
	rootID := r.PathValue("id")
	root, ok, err := s.rootForPermission(r.Context(), id, rootID, models.RootPermissionRead)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key query param required"})
		return
	}
	if !strings.HasPrefix(key, fmt.Sprintf("chunks/%s/", rootID)) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "asset not found"})
		return
	}
	rows, err := s.readRows(r.Context(), root, []any{"image_path", "Eq", key}, 10, []string{"file_path", "file_hash", "image_path"})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rows = filterDeniedQueryRows(rows, s.buildACLFilter(r.Context(), id, root.ID))
	if root.Scope == models.RootScopeUser && !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		rows = s.filterByContentProof(r.Context(), id.OrgID, id.UserID, root.ID, rows)
	}
	if len(rows) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "asset not found"})
		return
	}
	data, err := s.s3.Download(r.Context(), key)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "asset not found"})
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (s *Server) readFilePages(ctx context.Context, id *auth.Identity, root *models.RootMetadata, req *models.ReadFileRequest, r *http.Request) (models.ReadFileResponse, error) {
	if err := validateReadRange(req.Pages); err != nil {
		return models.ReadFileResponse{}, err
	}
	startPage := req.Pages.Start - 1
	endPage := req.Pages.End - 1
	pageFilters := []any{
		[]any{"file_path", "Eq", req.Path},
		[]any{"page_number", "Lte", endPage},
	}
	if startPage > 0 {
		pageFilters = append(pageFilters, []any{"page_number", "Gt", startPage - 1})
	}
	filters := []any{"And", pageFilters}
	rows, err := s.readRows(ctx, root, filters, req.Pages.End-req.Pages.Start+1, readIncludeAttrs())
	if err != nil {
		return models.ReadFileResponse{}, err
	}
	rows = s.filterReadRows(ctx, id, root, rows)
	sort.SliceStable(rows, func(i, j int) bool {
		return intFromAny(rows[i]["page_number"], 0) < intFromAny(rows[j]["page_number"], 0)
	})
	resp := models.ReadFileResponse{
		RootID:   root.ID,
		RootName: root.Name,
		FilePath: req.Path,
		Mode:     "pages",
		Pages:    []models.ReadPageResult{},
	}
	for _, row := range rows {
		pageNumber := intFromAny(row["page_number"], -1)
		if pageNumber < startPage || pageNumber > endPage {
			continue
		}
		if resp.AbsolutePath == "" {
			resp.AbsolutePath = strVal(row, "absolute_path")
		}
		page := models.ReadPageResult{
			Page:         pageNumber + 1,
			PageNumber:   pageNumber,
			ChunkIndex:   intFromAny(row["chunk_index"], 0),
			Content:      strVal(row, "content"),
			AbsolutePath: strVal(row, "absolute_path"),
			FileType:     strVal(row, "file_type"),
		}
		if imagePath := strVal(row, "image_path"); imagePath != "" {
			page.ImagePath = &imagePath
			if req.IncludeImages {
				imageURL := rootAssetURL(r, root.ID, imagePath)
				page.ImageURL = &imageURL
			}
		}
		resp.Pages = append(resp.Pages, page)
	}
	return resp, nil
}

func (s *Server) readFileLines(ctx context.Context, id *auth.Identity, root *models.RootMetadata, req *models.ReadFileRequest) (models.ReadFileResponse, error) {
	if err := validateReadRange(req.Lines); err != nil {
		return models.ReadFileResponse{}, err
	}
	filters := []any{"And", []any{
		[]any{"file_path", "Eq", req.Path},
		[]any{"line_end", "Gt", req.Lines.Start - 1},
		[]any{"line_start", "Lte", req.Lines.End},
	}}
	rows, err := s.readRows(ctx, root, filters, 1000, readIncludeAttrs())
	if err != nil {
		return models.ReadFileResponse{}, err
	}
	rows = s.filterReadRows(ctx, id, root, rows)
	if len(rows) == 0 {
		metadataRows, metaErr := s.readRows(ctx, root, []any{"file_path", "Eq", req.Path}, 1000, readIncludeAttrs())
		if metaErr != nil {
			return models.ReadFileResponse{}, fmt.Errorf("line range %d:%d unavailable for %s; could not inspect indexed file metadata: %w", req.Lines.Start, req.Lines.End, req.Path, metaErr)
		}
		metadataRows = s.filterReadRows(ctx, id, root, metadataRows)
		return models.ReadFileResponse{}, readLineRangeUnavailableError(req.Path, req.Lines, metadataRows)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return intFromAny(rows[i]["line_start"], 0) < intFromAny(rows[j]["line_start"], 0)
	})

	byLine := make(map[int]string)
	resp := models.ReadFileResponse{
		RootID:   root.ID,
		RootName: root.Name,
		FilePath: req.Path,
		Mode:     "lines",
		Lines:    []models.ReadLineResult{},
	}
	for _, row := range rows {
		lineStart := intFromAny(row["line_start"], 0)
		lineEnd := intFromAny(row["line_end"], 0)
		if lineStart == 0 || lineEnd == 0 {
			continue
		}
		if resp.AbsolutePath == "" {
			resp.AbsolutePath = strVal(row, "absolute_path")
		}
		lines := strings.SplitAfter(strVal(row, "content"), "\n")
		for i, line := range lines {
			lineNumber := lineStart + i
			if lineNumber > lineEnd {
				break
			}
			if lineNumber < req.Lines.Start || lineNumber > req.Lines.End {
				continue
			}
			if _, exists := byLine[lineNumber]; !exists {
				byLine[lineNumber] = strings.TrimSuffix(line, "\n")
			}
		}
	}
	for lineNumber := req.Lines.Start; lineNumber <= req.Lines.End; lineNumber++ {
		line, ok := byLine[lineNumber]
		if !ok {
			continue
		}
		resp.Lines = append(resp.Lines, models.ReadLineResult{LineNumber: lineNumber, Content: line})
	}
	if len(resp.Lines) == 0 {
		return models.ReadFileResponse{}, readLineRangeUnavailableError(req.Path, req.Lines, rows)
	}
	return resp, nil
}

func readLineRangeUnavailableError(filePath string, requested *models.ReadRange, rows []map[string]any) error {
	if requested == nil {
		return fmt.Errorf("line range unavailable for %s", filePath)
	}
	if len(rows) == 0 {
		return fmt.Errorf("line range %d:%d unavailable for %s; no indexed chunks found for this file", requested.Start, requested.End, filePath)
	}

	fileType := ""
	hasPages := false
	minPage, maxPage := 0, 0
	hasLines := false
	minLine, maxLine := 0, 0
	for _, row := range rows {
		if fileType == "" {
			fileType = strVal(row, "file_type")
		}
		if raw, ok := row["page_number"]; ok && raw != nil {
			page := intFromAny(raw, 0) + 1
			if !hasPages || page < minPage {
				minPage = page
			}
			if !hasPages || page > maxPage {
				maxPage = page
			}
			hasPages = true
		}
		lineStartRaw, hasLineStart := row["line_start"]
		lineEndRaw, hasLineEnd := row["line_end"]
		if hasLineStart && hasLineEnd && lineStartRaw != nil && lineEndRaw != nil {
			lineStart := intFromAny(lineStartRaw, 0)
			lineEnd := intFromAny(lineEndRaw, 0)
			if lineStart <= 0 || lineEnd <= 0 {
				continue
			}
			if !hasLines || lineStart < minLine {
				minLine = lineStart
			}
			if !hasLines || lineEnd > maxLine {
				maxLine = lineEnd
			}
			hasLines = true
		}
	}

	fileTypePart := ""
	if fileType != "" {
		fileTypePart = " file_type=" + fileType + ";"
	}
	if hasLines {
		return fmt.Errorf("line range %d:%d unavailable for %s;%s indexed line range is %d:%d", requested.Start, requested.End, filePath, fileTypePart, minLine, maxLine)
	}
	if hasPages {
		return fmt.Errorf("line ranges unavailable for %s;%s supports page reads instead (available pages %d:%d, use --pages %d:%d)", filePath, fileTypePart, minPage, maxPage, minPage, maxPage)
	}
	if fileType != "" {
		return fmt.Errorf("line ranges unavailable for %s; file_type=%s was indexed without line metadata; resync this file or root and retry --lines", filePath, fileType)
	}
	return fmt.Errorf("line ranges unavailable for %s; indexed chunks do not include line metadata; resync this file or root and retry --lines", filePath)
}

func (s *Server) readRows(ctx context.Context, root *models.RootMetadata, filters any, limit int, includeAttrs []string) ([]map[string]any, error) {
	indexNamespaces, err := s.db.ListRootIndexNamespaces(ctx, root.OrgID, root.ID)
	if err != nil {
		return nil, fmt.Errorf("listing root index namespaces: %w", err)
	}
	visibleSeq, err := s.db.GetVisibleGenerationSeq(ctx, root.ID)
	if err != nil {
		return nil, fmt.Errorf("resolving visible generation: %w", err)
	}
	combinedFilters := []any{filters, activeGenerationFilter(visibleSeq)}
	return queryRootIndexNamespaces(indexNamespaces, limit, func(namespace string) ([]map[string]any, error) {
		return s.tp.Query(namespace, []any{"file_path", "asc"}, limit, tpAndFilter(combinedFilters), includeAttrs)
	})
}

func (s *Server) filterReadRows(ctx context.Context, id *auth.Identity, root *models.RootMetadata, rows []map[string]any) []map[string]any {
	rows = filterDeniedQueryRows(rows, s.buildACLFilter(ctx, id, root.ID))
	if root.Scope == models.RootScopeUser && !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		rows = s.filterByContentProof(ctx, id.OrgID, id.UserID, root.ID, rows)
	}
	return rows
}

func validateReadRange(r *models.ReadRange) error {
	if r == nil || r.Start <= 0 || r.End <= 0 {
		return fmt.Errorf("range start and end must be positive")
	}
	if r.End < r.Start {
		return fmt.Errorf("range end must be greater than or equal to start")
	}
	if r.End-r.Start > 999 {
		return fmt.Errorf("range cannot include more than 1000 items")
	}
	return nil
}

func readIncludeAttrs() []string {
	return []string{"content", "file_path", "absolute_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path", "line_start", "line_end", "generation_id", "valid_from_generation", "valid_from_generation_seq", "valid_to_generation", "valid_to_generation_seq"}
}

func rootAssetURL(r *http.Request, rootID, key string) string {
	return fmt.Sprintf("/roots/%s/assets?key=%s", url.PathEscape(rootID), url.QueryEscape(key))
}

// ---------------------------------------------------------------------------
// ACL helpers
// ---------------------------------------------------------------------------

// checkWriteACL checks if a user has write permission for a path in a root.
// If no ACLs are configured for the root, all org editors+ have access.
func (s *Server) checkWriteACL(ctx context.Context, id *auth.Identity, rootID, filePath string) bool {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil || len(acls) == 0 {
		_, ok, rootErr := s.rootForPermission(ctx, id, rootID, models.RootPermissionSync)
		return rootErr == nil && ok
	}

	return checkPermission(acls, filePath, "write")
}

func (s *Server) checkSyncWriteACL(ctx context.Context, id *auth.Identity, rootID string, req *models.SyncRequest) error {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil || len(acls) == 0 {
		_, ok, rootErr := s.rootForPermission(ctx, id, rootID, models.RootPermissionSync)
		if rootErr == nil && ok {
			return nil
		}
		return fmt.Errorf("write permission required")
	}

	canWrite := func(filePath string) bool {
		return checkPermission(acls, filePath, "write")
	}

	changes, err := s.syncRequestChangesForACL(ctx, req)
	if err != nil {
		return err
	}
	for _, change := range changes {
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

func (s *Server) syncRequestChangesForACL(ctx context.Context, req *models.SyncRequest) ([]models.FileChange, error) {
	if req == nil {
		return nil, nil
	}
	changes := append([]models.FileChange(nil), req.Changes...)
	if len(req.ChangeRefs) == 0 {
		return changes, nil
	}
	for _, ref := range req.ChangeRefs {
		data, err := s.s3.Download(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("downloading change ref %s: %w", ref, err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		for {
			var change models.FileChange
			if err := dec.Decode(&change); err != nil {
				if err == io.EOF {
					break
				}
				return nil, fmt.Errorf("parsing change ref %s: %w", ref, err)
			}
			path, err := cleanFilePath(change.Path)
			if err != nil {
				return nil, fmt.Errorf("invalid change path %q in %s: %w", change.Path, ref, err)
			}
			change.Path = path
			if change.OldPath != "" {
				oldPath, err := cleanFilePath(change.OldPath)
				if err != nil {
					return nil, fmt.Errorf("invalid old path %q in %s: %w", change.OldPath, ref, err)
				}
				change.OldPath = oldPath
			}
			if err := validateSourceRef(req.RootID, req.GenerationID, &change); err != nil {
				return nil, fmt.Errorf("invalid source for %q in %s: %w", change.Path, ref, err)
			}
			changes = append(changes, change)
		}
	}
	return changes, nil
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
	if req == nil {
		return nil
	}
	if req.StateRef != "" {
		if generationID == "" || !strings.HasPrefix(req.StateRef, fmt.Sprintf("syncs/%s/state/", generationID)) {
			return nil
		}
		data, err := s.s3.Download(ctx, req.StateRef)
		if err != nil {
			return fmt.Errorf("downloading sync state object %s: %w", req.StateRef, err)
		}
		key := stateObjectKey(rootID, generationID)
		if err := s.s3.Upload(ctx, key, data, "application/gzip"); err != nil {
			return fmt.Errorf("uploading root state object %s: %w", key, err)
		}
		req.StateRef = key
		return nil
	}
	if req.State == nil {
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

func (s *Server) upsertRowsInBatches(ns string, rows []map[string]any, distanceMetric string) error {
	batchSize := tpWriteBatchSize()
	for start := 0; start < len(rows); start += batchSize {
		end := start + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		writeStart := time.Now()
		if err := s.tp.UpsertRows(ns, rows[start:end], distanceMetric); err != nil {
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
	if req.TopK <= 0 {
		req.TopK = 10
	}
	if req.Mode == "" {
		req.Mode = "hybrid"
	}
	queryLimit := filteredQueryLimit(req.TopK)

	selection, err := s.resolveQueryRoots(r.Context(), id, &req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errQueryRootNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	switch req.Mode {
	case "fts", "vector", "hybrid":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be fts, vector, or hybrid"})
		return
	}
	if len(selection.roots) == 0 {
		s.captureBackendEvent(r.Context(), id, "query_submitted", map[string]any{
			"mode":             req.Mode,
			"top_k":            req.TopK,
			"has_glob":         req.Glob != "",
			"query_scope":      selection.scope,
			"roots_searched":   0,
			"namespace_count":  0,
			"raw_result_count": 0,
			"result_count":     0,
		})
		writeJSON(w, http.StatusOK, models.QueryResponse{
			Results:       []models.QueryResult{},
			Query:         req.Query,
			Mode:          req.Mode,
			RootsSearched: 0,
		})
		return
	}

	if req.Mode == "vector" {
		for _, root := range selection.roots {
			if root.VectorDisabled {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("root %s has vector search disabled", root.ID)})
				return
			}
		}
	}

	var needsEmbedding bool
	if req.Mode == "vector" || req.Mode == "hybrid" {
		for _, root := range selection.roots {
			if !root.VectorDisabled {
				needsEmbedding = true
				break
			}
		}
	}

	var embedding []float64
	if needsEmbedding {
		var embedErr error
		embedding, embedErr = s.modal.EmbedQuery(req.Query)
		if embedErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding query: " + embedErr.Error()})
			return
		}
	}

	allResults := make([]models.QueryResult, 0)
	totalNamespaces := 0
	rawResultCount := 0
	for _, root := range selection.roots {
		rootResults, stats, err := s.queryOneRoot(r.Context(), id, &req, root, embedding, queryLimit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		totalNamespaces += stats.namespaceCount
		rawResultCount += stats.rawResultCount
		allResults = append(allResults, rootResults...)
	}

	if len(selection.roots) > 1 {
		sort.SliceStable(allResults, func(i, j int) bool {
			return allResults[i].Score > allResults[j].Score
		})
	}
	if len(allResults) > req.TopK {
		allResults = allResults[:req.TopK]
	}

	s.captureBackendEvent(r.Context(), id, "query_submitted", map[string]any{
		"mode":             req.Mode,
		"top_k":            req.TopK,
		"has_glob":         req.Glob != "",
		"query_scope":      selection.scope,
		"roots_searched":   len(selection.roots),
		"namespace_count":  totalNamespaces,
		"raw_result_count": rawResultCount,
		"result_count":     len(allResults),
	})

	writeJSON(w, http.StatusOK, models.QueryResponse{
		Results:       allResults,
		Query:         req.Query,
		Mode:          req.Mode,
		RootsSearched: len(selection.roots),
	})
}

var errQueryRootNotFound = errors.New("root not found")

type queryRootSelection struct {
	roots []models.RootMetadata
	scope string
}

func (s *Server) resolveQueryRoots(ctx context.Context, id *auth.Identity, req *models.QueryRequest) (queryRootSelection, error) {
	selectorCount := 0
	if strings.TrimSpace(req.RootID) != "" {
		selectorCount++
	}
	if len(req.RootIDs) > 0 {
		selectorCount++
	}
	if req.AllRoots {
		selectorCount++
	}
	if selectorCount == 0 {
		return queryRootSelection{}, fmt.Errorf("root_id, root_ids, or all_roots is required")
	}
	if selectorCount > 1 {
		return queryRootSelection{}, fmt.Errorf("use exactly one of root_id, root_ids, or all_roots")
	}

	if req.AllRoots {
		roots, err := s.db.ListAccessibleRoots(ctx, id.OrgID, id.UserID, id.Role)
		if err != nil {
			return queryRootSelection{}, fmt.Errorf("listing accessible roots: %w", err)
		}
		return queryRootSelection{roots: roots, scope: "all_roots"}, nil
	}

	if strings.TrimSpace(req.RootID) != "" {
		root, ok, err := s.rootForPermission(ctx, id, strings.TrimSpace(req.RootID), models.RootPermissionRead)
		if err != nil || !ok {
			return queryRootSelection{}, errQueryRootNotFound
		}
		return queryRootSelection{roots: []models.RootMetadata{*root}, scope: "single_root"}, nil
	}

	seen := make(map[string]struct{}, len(req.RootIDs))
	roots := make([]models.RootMetadata, 0, len(req.RootIDs))
	for _, rawRootID := range req.RootIDs {
		rootID := strings.TrimSpace(rawRootID)
		if rootID == "" {
			continue
		}
		if _, ok := seen[rootID]; ok {
			continue
		}
		seen[rootID] = struct{}{}
		root, ok, err := s.rootForPermission(ctx, id, rootID, models.RootPermissionRead)
		if err != nil || !ok {
			return queryRootSelection{}, errQueryRootNotFound
		}
		roots = append(roots, *root)
	}
	if len(roots) == 0 {
		return queryRootSelection{}, fmt.Errorf("root_ids must include at least one non-empty root id")
	}
	return queryRootSelection{roots: roots, scope: "selected_roots"}, nil
}

type queryRootStats struct {
	namespaceCount int
	rawResultCount int
}

func (s *Server) queryOneRoot(ctx context.Context, id *auth.Identity, req *models.QueryRequest, root models.RootMetadata, embedding []float64, queryLimit int) ([]models.QueryResult, queryRootStats, error) {
	indexNamespaces, err := s.db.ListRootIndexNamespaces(ctx, id.OrgID, root.ID)
	if err != nil {
		return nil, queryRootStats{}, fmt.Errorf("listing root index namespaces: %w", err)
	}
	activeNamespaces := activeRootIndexNamespaces(indexNamespaces)
	stats := queryRootStats{namespaceCount: len(activeNamespaces)}
	if len(activeNamespaces) == 0 {
		return nil, stats, nil
	}

	includeAttrs := []string{"content", "file_path", "absolute_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path", "line_start", "line_end", "generation_id", "valid_from_generation", "valid_from_generation_seq", "valid_to_generation", "valid_to_generation_seq"}

	var filters []any
	if req.Glob != "" {
		filters = append(filters, []any{"file_path", "Glob", req.Glob})
	}
	visibleSeq, err := s.db.GetVisibleGenerationSeq(ctx, root.ID)
	if err != nil {
		return nil, stats, fmt.Errorf("resolving visible generation: %w", err)
	}
	filters = append(filters, activeGenerationFilter(visibleSeq))

	var rows []map[string]any
	switch req.Mode {
	case "fts":
		rankBy := []any{"content", "BM25", req.Query}
		rows, err = queryRootIndexNamespaces(activeNamespaces, queryLimit, func(namespace string) ([]map[string]any, error) {
			return s.tp.Query(namespace, rankBy, queryLimit, tpAndFilter(filters), includeAttrs)
		})
	case "vector":
		if root.VectorDisabled {
			return nil, stats, fmt.Errorf("root %s has vector search disabled", root.ID)
		}
		rankBy := []any{"vector", "ANN", embedding}
		rows, err = queryRootIndexNamespaces(activeNamespaces, queryLimit, func(namespace string) ([]map[string]any, error) {
			return s.tp.Query(namespace, rankBy, queryLimit, tpAndFilter(filters), includeAttrs)
		})
	case "hybrid":
		if root.VectorDisabled {
			rankBy := []any{"content", "BM25", req.Query}
			rows, err = queryRootIndexNamespaces(activeNamespaces, queryLimit, func(namespace string) ([]map[string]any, error) {
				return s.tp.Query(namespace, rankBy, queryLimit, tpAndFilter(filters), includeAttrs)
			})
		} else {
			rows, err = queryRootIndexNamespaces(activeNamespaces, queryLimit, func(namespace string) ([]map[string]any, error) {
				return s.tp.HybridSearch(namespace, req.Query, embedding, queryLimit, tpAndFilter(filters))
			})
		}
	default:
		return nil, stats, fmt.Errorf("mode must be fts, vector, or hybrid")
	}
	if err != nil {
		return nil, stats, err
	}
	stats.rawResultCount = len(rows)

	deniedPrefixes := s.buildACLFilter(ctx, id, root.ID)
	filteredRows := filterDeniedQueryRows(rows, deniedPrefixes)
	if root.Scope == models.RootScopeUser && !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		filteredRows = s.filterByContentProof(ctx, id.OrgID, id.UserID, root.ID, filteredRows)
	}
	if len(filteredRows) > req.TopK {
		filteredRows = filteredRows[:req.TopK]
	}
	return queryResultsFromRows(root, filteredRows), stats, nil
}

func filterDeniedQueryRows(rows []map[string]any, deniedPrefixes []string) []map[string]any {
	if len(deniedPrefixes) == 0 {
		return rows
	}
	filteredRows := make([]map[string]any, 0, len(rows))
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
	return filteredRows
}

func queryResultsFromRows(root models.RootMetadata, rows []map[string]any) []models.QueryResult {
	results := make([]models.QueryResult, len(rows))
	for i, row := range rows {
		results[i] = models.QueryResult{
			RootID:       root.ID,
			RootName:     root.Name,
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
	return results
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

func requireOrgAdmin(w http.ResponseWriter, r *http.Request) (*auth.Identity, bool) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil || !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return nil, false
	}
	if !auth.HasScope(id, "org:admin", "admin", "write") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "org admin scope required"})
		return nil, false
	}
	return id, true
}

func canAssignRole(actorRole, targetRole auth.Role) bool {
	switch actorRole {
	case auth.RoleOwner:
		return targetRole == auth.RoleOwner ||
			targetRole == auth.RoleAdmin ||
			targetRole == auth.RoleEditor ||
			targetRole == auth.RoleViewer
	case auth.RoleAdmin:
		return targetRole == auth.RoleEditor || targetRole == auth.RoleViewer
	default:
		return false
	}
}

func canManageMemberRole(actorRole, targetRole auth.Role) bool {
	switch actorRole {
	case auth.RoleOwner:
		return true
	case auth.RoleAdmin:
		return targetRole == auth.RoleEditor || targetRole == auth.RoleViewer
	default:
		return false
	}
}

func (s *Server) canRemoveOwner(ctx context.Context, orgID string) (bool, error) {
	count, err := s.db.CountOrgMembersByRole(ctx, orgID, auth.RoleOwner)
	if err != nil {
		return false, err
	}
	return count > 1, nil
}

func parseRootScope(raw string) (string, error) {
	scope := strings.TrimSpace(raw)
	if scope == "" {
		scope = models.RootScopeOrg
	}
	switch scope {
	case models.RootScopeOrg, models.RootScopeUser, models.RootScopeRestricted:
		return scope, nil
	default:
		return "", fmt.Errorf("scope must be org, user, or restricted")
	}
}

func normalizeRootGrantPermissions(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("permissions are required")
	}
	seen := map[string]bool{}
	for _, value := range raw {
		permission := strings.TrimSpace(value)
		switch permission {
		case models.RootPermissionRead, models.RootPermissionSync, models.RootPermissionDelete, models.RootPermissionAdmin:
			seen[permission] = true
		case "":
		default:
			return nil, fmt.Errorf("unsupported root permission %q", permission)
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("permissions are required")
	}
	return sortedRootPermissions(seen), nil
}

func (s *Server) validateRootGrantPrincipal(w http.ResponseWriter, r *http.Request, orgID, rawType, rawID string) (string, string, bool) {
	principalType := strings.TrimSpace(rawType)
	principalID := strings.TrimSpace(rawID)
	if principalType == "" || principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "principal_type and principal_id are required"})
		return "", "", false
	}
	switch principalType {
	case "org":
		if principalID != orgID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "org principal_id must match orgId"})
			return "", "", false
		}
	case "user":
		if _, err := s.db.GetOrgMember(r.Context(), orgID, principalID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user principal must be an org member"})
			return "", "", false
		}
	case "group":
		if _, err := s.db.GetGroup(r.Context(), orgID, principalID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "group principal not found"})
			return "", "", false
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "principal_type must be org, user, or group"})
		return "", "", false
	}
	return principalType, principalID, true
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

func (s *Server) rootForPermission(ctx context.Context, id *auth.Identity, rootID, permission string) (*models.RootMetadata, bool, error) {
	if id == nil {
		return nil, false, nil
	}
	root, err := s.db.GetRoot(ctx, id.OrgID, rootID)
	if err != nil {
		return nil, false, err
	}
	perms, source, err := s.db.RootPermissions(ctx, root, id.UserID, id.Role)
	if err != nil {
		return nil, false, err
	}
	root.Access = perms
	root.AccessSource = source
	return root, rootPermissionAllowed(perms, permission), nil
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
