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
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	productanalytics "github.com/pufferfs/pufferfs/internal/analytics"
	"github.com/pufferfs/pufferfs/internal/auth"
	"github.com/pufferfs/pufferfs/internal/queue"
	"github.com/pufferfs/pufferfs/internal/storage"
	"github.com/pufferfs/pufferfs/pkg/models"
)

// Server holds the dependencies for HTTP handlers.
type Server struct {
	db          *DB
	s3          *storage.Client
	modal       *ModalClient
	tp          *TPClient
	queue       *queue.SQSQueue
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

// SetQueue connects source registration to SQS delivery.
func (s *Server) SetQueue(q *queue.SQSQueue) {
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
	s.mux.HandleFunc("POST /roots/{id}/sources/init", s.handleSourcePackInit)
	s.mux.HandleFunc("POST /roots/{id}/sources/complete", s.handleSourcePackComplete)
	s.mux.HandleFunc("POST /roots/{id}/sources/multipart/init", s.handleCaptureMultipartInit)
	s.mux.HandleFunc("POST /roots/{id}/sources/multipart/part", s.handleCaptureMultipartPart)
	s.mux.HandleFunc("POST /roots/{id}/sources/multipart/complete", s.handleCaptureMultipartComplete)
	s.mux.HandleFunc("POST /roots/{id}/versions", s.handleRegisterFileVersions)
	s.mux.HandleFunc("GET /roots/{id}/captured-files", s.handleListCapturedFiles)
	s.mux.HandleFunc("POST /roots/{id}/captured-proofs", s.handleCapturedProofs)
	s.mux.HandleFunc("GET /roots/{id}/state", s.handleGetState)
	s.mux.HandleFunc("POST /roots/{id}/read", s.handleReadFile)

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
	scopes := append([]string{}, id.Scopes...)
	writeJSON(w, http.StatusOK, models.AuthMeResponse{
		User:   *user,
		OrgID:  id.OrgID,
		Role:   string(id.Role),
		Scopes: scopes,
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
		"acl:read":       {},
		"acl:write":      {},
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
	cancelled, err := s.db.PrepareRootDeletion(r.Context(), root.OrgID, root.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "preparing root deletion: " + err.Error()})
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
		"sync_jobs_cancelled":    cancelled,
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
	cancelled, err := s.db.PrepareRootDeletion(r.Context(), id.OrgID, rootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "preparing root deletion: " + err.Error()})
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
		"sync_jobs_cancelled":    cancelled,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                 "deleted",
		"root_id":                root.ID,
		"name":                   root.Name,
		"turbopuffer_ns":         result.TurbopufferNamespace,
		"turbopuffer_namespaces": result.TurbopufferNamespaces,
		"s3_objects_deleted":     result.S3ObjectsDeleted,
		"sync_jobs_cancelled":    cancelled,
	})
}

type rootArtifactDeleteResult struct {
	TurbopufferNamespace  string
	TurbopufferNamespaces []string
	S3ObjectsDeleted      int
}

func (s *Server) deleteRootArtifacts(ctx context.Context, orgID, rootID string) (rootArtifactDeleteResult, error) {
	generationIDs, err := s.db.ListSyncGenerationIDs(ctx, orgID, rootID)
	if err != nil {
		return rootArtifactDeleteResult{}, fmt.Errorf("listing sync generations: %w", err)
	}
	indexNamespaces, err := s.db.ListRootIndexNamespaces(ctx, orgID, rootID)
	if err != nil {
		return rootArtifactDeleteResult{}, fmt.Errorf("listing root index namespaces: %w", err)
	}
	namespaces := make([]string, 0, len(indexNamespaces))
	for _, ns := range indexNamespaces {
		namespaces = append(namespaces, ns.Namespace)
	}
	return s.deleteKnownRootArtifacts(ctx, orgID, rootID, generationIDs, namespaces)
}

// deleteKnownRootArtifacts does not consult root metadata, so a late worker
// can repeat the cleanup after the root and its generation rows are gone.
func (s *Server) deleteKnownRootArtifacts(ctx context.Context, orgID, rootID string, generationIDs, namespaces []string) (rootArtifactDeleteResult, error) {
	result := rootArtifactDeleteResult{}
	seenNamespaces := make(map[string]bool, len(namespaces)+1)
	for _, namespace := range namespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" || seenNamespaces[namespace] {
			continue
		}
		seenNamespaces[namespace] = true
		result.TurbopufferNamespaces = append(result.TurbopufferNamespaces, namespace)
	}
	if len(result.TurbopufferNamespaces) == 0 {
		result.TurbopufferNamespaces = []string{tpNamespace(orgID, rootID)}
	}
	result.TurbopufferNamespace = result.TurbopufferNamespaces[0]

	var cleanupErr error
	if s.tp != nil {
		for _, namespace := range result.TurbopufferNamespaces {
			if err := s.tp.DeleteNamespace(namespace); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("deleting turbopuffer namespace %s: %w", namespace, err))
			}
		}
	}

	prefixes := []string{
		fmt.Sprintf("sources/%s/%s/", orgID, rootID),
		fmt.Sprintf("extractions/%s/%s/", orgID, rootID),
		fmt.Sprintf("mutations/%s/%s/", orgID, rootID),
		fmt.Sprintf("files/%s/", rootID),
		fmt.Sprintf("bundles/%s/", rootID),
		fmt.Sprintf("states/%s/", rootID),
		fmt.Sprintf("chunks/%s/", rootID),
	}
	for _, generationID := range generationIDs {
		if generationID = strings.TrimSpace(generationID); generationID != "" {
			prefixes = append(prefixes, fmt.Sprintf("syncs/%s/", generationID))
		}
	}
	if s.s3 == nil {
		return result, cleanupErr
	}
	for _, prefix := range prefixes {
		count, err := s.s3.DeletePrefix(ctx, prefix)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("deleting storage prefix %s: %w", prefix, err))
			continue
		}
		result.S3ObjectsDeleted += count
	}
	return result, cleanupErr
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
	allowed, err := s.checkReadACL(r.Context(), id, rootID, req.Path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !allowed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	if req.Pages != nil {
		resp, err := s.readFilePages(r.Context(), id, root, &req, r)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errFilePermissionsUnavailable) {
				status = http.StatusInternalServerError
			} else if errors.Is(err, errQueryRootNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp, err := s.readFileLines(r.Context(), id, root, &req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errFilePermissionsUnavailable) {
			status = http.StatusInternalServerError
		} else if errors.Is(err, errQueryRootNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
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
	rows, err := s.readFileRows(ctx, root, req.Path, filters)
	if err != nil {
		return models.ReadFileResponse{}, err
	}
	rows, err = s.filterReadRows(ctx, id, root, rows)
	if err != nil {
		return models.ReadFileResponse{}, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		pi, pj := intFromAny(rows[i]["page_number"], 0), intFromAny(rows[j]["page_number"], 0)
		if pi != pj {
			return pi < pj
		}
		return intFromAny(rows[i]["chunk_index"], 0) < intFromAny(rows[j]["chunk_index"], 0)
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
		if len(resp.Pages) > 0 && resp.Pages[len(resp.Pages)-1].PageNumber == pageNumber {
			resp.Pages[len(resp.Pages)-1].Content += strVal(row, "content")
			continue
		}
		page := models.ReadPageResult{
			Page:         pageNumber + 1,
			PageNumber:   pageNumber,
			ChunkIndex:   intFromAny(row["chunk_index"], 0),
			Content:      strVal(row, "content"),
			AbsolutePath: strVal(row, "absolute_path"),
			FileType:     strVal(row, "file_type"),
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
	rows, err := s.readFileRows(ctx, root, req.Path, filters)
	if err != nil {
		return models.ReadFileResponse{}, err
	}
	rows, err = s.filterReadRows(ctx, id, root, rows)
	if err != nil {
		return models.ReadFileResponse{}, err
	}
	if len(rows) == 0 {
		metadataRows, metaErr := s.readFileMetadataRows(ctx, root, req.Path)
		if metaErr != nil {
			return models.ReadFileResponse{}, fmt.Errorf("line range %d:%d unavailable for %s; could not inspect indexed file metadata: %w", req.Lines.Start, req.Lines.End, req.Path, metaErr)
		}
		metadataRows, metaErr = s.filterReadRows(ctx, id, root, metadataRows)
		if metaErr != nil {
			return models.ReadFileResponse{}, metaErr
		}
		return models.ReadFileResponse{}, readLineRangeUnavailableError(req.Path, req.Lines, metadataRows)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		li, lj := intFromAny(rows[i]["line_start"], 0), intFromAny(rows[j]["line_start"], 0)
		if li != lj {
			return li < lj
		}
		return intFromAny(rows[i]["chunk_index"], 0) < intFromAny(rows[j]["chunk_index"], 0)
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
			if strVal(row, "extraction_id") != "" {
				byLine[lineNumber] += strings.TrimSuffix(line, "\n")
			} else if _, exists := byLine[lineNumber]; !exists {
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

func (s *Server) readFileMetadataRows(ctx context.Context, root *models.RootMetadata, path string) ([]map[string]any, error) {
	indexNamespaces, err := s.db.ListRootIndexNamespaces(ctx, root.OrgID, root.ID)
	if err != nil {
		return nil, fmt.Errorf("listing root index namespaces: %w", err)
	}
	if len(activeRootIndexNamespaces(indexNamespaces)) == 0 {
		return nil, nil
	}
	ns, err := rootIndexNamespaceForPath(indexNamespaces, path)
	if err != nil {
		return nil, err
	}
	visibility, err := s.catalogVisibilitySnapshot(ctx, root.OrgID, root.ID, path)
	if err != nil {
		return nil, fmt.Errorf("resolving file publication: %w", err)
	}
	return s.tp.Query(ctx, ns.Namespace, TPQuery{RankBy: []any{"chunk_index", "asc"}, Limit: 1000,
		Filters: tpAndFilter([]any{[]any{"file_path", "Eq", path}, visibility}), ExcludeAttributes: readExcludedAttrs()})
}

func (s *Server) filterReadRows(ctx context.Context, id *auth.Identity, root *models.RootMetadata, rows []map[string]any) ([]map[string]any, error) {
	denied, err := s.buildACLFilter(ctx, id, root.ID)
	if err != nil {
		return nil, err
	}
	rows = filterDeniedQueryRows(rows, denied)
	if root.Scope == models.RootScopeUser && !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		rows = s.filterByContentProof(ctx, id.OrgID, id.UserID, root.ID, rows)
	}
	return rows, nil
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

func readExcludedAttrs() []string {
	// Missing excluded fields are ignored by Turbopuffer, unlike missing included
	// fields. Preserve extraction_id for split-line reads, but never transfer
	// vectors or internal artifact/generation metadata for read/search results.
	return []string{
		"vector", "image_path", "generation_id", "valid_from_generation", "valid_from_generation_seq",
		"valid_to_generation", "valid_to_generation_seq", "root_id", "file_id", "version_id",
		"version_sequence", "extraction_sequence", "source_manifest_ref", "location_json",
	}
}

// ---------------------------------------------------------------------------
// ACL helpers
// ---------------------------------------------------------------------------

var errFilePermissionsUnavailable = errors.New("file permissions unavailable")

// checkWriteACL checks if a user has write permission for a path in a root.
// If no ACLs are configured for the root, all org editors+ have access.
func (s *Server) checkWriteACL(ctx context.Context, id *auth.Identity, rootID, filePath string) bool {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil {
		return false
	}
	if len(acls) == 0 {
		_, ok, rootErr := s.rootForPermission(ctx, id, rootID, models.RootPermissionSync)
		return rootErr == nil && ok
	}

	return checkPermission(acls, filePath, "write")
}

// checkReadACL checks if a user has read permission for a path in a root.
func (s *Server) checkReadACL(ctx context.Context, id *auth.Identity, rootID, filePath string) (bool, error) {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil {
		return false, errFilePermissionsUnavailable
	}
	return checkPermission(acls, filePath, "read"), nil
}

// buildACLFilter returns denied literal path prefixes for post-filtering.
// An empty successful result means unrestricted; a lookup error never does.
func (s *Server) buildACLFilter(ctx context.Context, id *auth.Identity, rootID string) ([]string, error) {
	acls, err := s.db.GetACLsForUser(ctx, id.OrgID, rootID, id.UserID, id.Role)
	if err != nil {
		return nil, errFilePermissionsUnavailable
	}

	// Collect denied path prefixes
	var denied []string
	for _, acl := range acls {
		if acl.Permission == "none" {
			denied = append(denied, acl.PathPrefix)
		}
	}
	return denied, nil
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
// Historical root inventory (read-only migration audit)
// ---------------------------------------------------------------------------

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
			if errors.Is(err, errQueryRootNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "root not found"})
				return
			}
			if errors.Is(err, errSearchPublicationBusy) {
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "search_publication_busy", "error": errSearchPublicationBusy.Error()})
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "index search timed out"})
				return
			}
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	indexNamespaces, err := s.db.ListRootIndexNamespaces(ctx, id.OrgID, root.ID)
	if err != nil {
		return nil, queryRootStats{}, fmt.Errorf("listing root index namespaces: %w", err)
	}
	activeNamespaces := activeRootIndexNamespaces(indexNamespaces)
	stats := queryRootStats{namespaceCount: len(activeNamespaces)}
	if len(activeNamespaces) == 0 {
		return nil, stats, nil
	}

	fts, ann := []any{"content", "BM25", req.Query}, []any{"vector", "ANN", embedding}
	var rankings []any
	switch req.Mode {
	case "fts":
		rankings = []any{fts}
	case "vector":
		if root.VectorDisabled {
			return nil, stats, fmt.Errorf("root %s has vector search disabled", root.ID)
		}
		rankings = []any{ann}
	case "hybrid":
		rankings = []any{ann, fts}
		if root.VectorDisabled {
			rankings = []any{fts}
		}
	default:
		return nil, stats, fmt.Errorf("mode must be fts, vector, or hybrid")
	}
	rows, err := queryRootIndexNamespaces(activeNamespaces, queryLimit, func(namespace string) ([]map[string]any, error) {
		sets, err := s.queryPublishedRows(ctx, root.OrgID, root.ID, func(visibility any) ([][]map[string]any, error) {
			filters := []any{visibility}
			if req.Glob != "" {
				filters = append(filters, []any{"file_path", "Glob", req.Glob})
			}
			queries := make([]TPQuery, len(rankings))
			for i, rank := range rankings {
				queries[i] = TPQuery{RankBy: rank, Limit: queryLimit, Filters: tpAndFilter(filters), ExcludeAttributes: readExcludedAttrs()}
			}
			if len(queries) == 1 {
				rows, err := s.tp.Query(ctx, namespace, queries[0])
				return [][]map[string]any{rows}, err
			}
			return s.tp.MultiQuery(ctx, namespace, queries)
		})
		if err != nil {
			return nil, err
		}
		if len(sets) == 1 {
			return sets[0], nil
		}
		return reciprocalRankFusion(sets, 60), nil
	})
	if err != nil {
		return nil, stats, err
	}
	stats.rawResultCount = len(rows)

	deniedPrefixes, err := s.buildACLFilter(ctx, id, root.ID)
	if err != nil {
		return nil, stats, err
	}
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
