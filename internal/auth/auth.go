// Package auth provides authentication and authorization middleware.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Role represents a user's role within an organization.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Identity represents the authenticated user and their org context.
type Identity struct {
	UserID string
	OrgID  string
	Role   Role
	Email  string
	Scopes []string
}

type contextKey string

const identityKey contextKey = "identity"
const adminKey contextKey = "admin"

// IdentityFromContext extracts the authenticated identity from the request context.
func IdentityFromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityKey).(*Identity)
	return id
}

// WithIdentity attaches an Identity to the context.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

func IsAdmin(ctx context.Context) bool {
	ok, _ := ctx.Value(adminKey).(bool)
	return ok
}

func WithAdmin(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminKey, true)
}

// HasScope reports whether an identity is allowed to perform an API-key scoped
// action. JWT identities and legacy keys with no scopes are treated as
// unrestricted; scoped API keys must include the exact scope, an alias, or "*".
func HasScope(id *Identity, required string, aliases ...string) bool {
	if id == nil {
		return false
	}
	if len(id.Scopes) == 0 {
		return true
	}
	allowed := map[string]struct{}{
		required: {},
		"*":      {},
	}
	for _, alias := range aliases {
		allowed[alias] = struct{}{}
	}
	for _, scope := range id.Scopes {
		if _, ok := allowed[scope]; ok {
			return true
		}
	}
	return false
}

// JWTClaims are the custom claims in a PufferFs JWT.
type JWTClaims struct {
	UserID string `json:"user_id"`
	OrgID  string `json:"org_id"`
	Role   string `json:"role"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

// GenerateJWT creates a signed JWT for a user.
func GenerateJWT(secret []byte, userID, orgID string, role Role, email string, expiry time.Duration) (string, error) {
	claims := JWTClaims{
		UserID: userID,
		OrgID:  orgID,
		Role:   string(role),
		Email:  email,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "pufferfs",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(secret)
}

// ValidateJWT parses and validates a JWT string.
func ValidateJWT(secret []byte, tokenStr string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &JWTClaims{}, func(t *jwt.Token) (any, error) {
		return secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*JWTClaims)
	if !ok || !token.Valid {
		return nil, jwt.ErrSignatureInvalid
	}
	return claims, nil
}

// HashAPIKey returns the SHA-256 hex digest of a raw API key.
func HashAPIKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// APIKeyResolver is called to look up an API key hash → Identity.
type APIKeyResolver func(ctx context.Context, keyHash string) (*Identity, error)

// tokenFromRequest extracts the bearer token from the Authorization header, or
// falls back to the session cookie set by the OAuth callback (browser clients).
func tokenFromRequest(r *http.Request) (string, bool) {
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == authHeader {
			return "", false
		}
		return token, true
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}

// Middleware creates auth middleware that supports JWT (header or session
// cookie) and API key auth. Unauthenticated paths (health, readyz, OAuth
// endpoints, the CLI version manifest, the Stripe webhook) are skipped.
func Middleware(jwtSecret []byte, resolveAPIKey APIKeyResolver) func(http.Handler) http.Handler {
	unauthPaths := map[string]bool{
		"/healthz":         true,
		"/readyz":          true,
		"/health":          true,
		"/cli/version":     true,
		"/auth/google":     true,
		"/auth/callback":   true,
		"/auth/logout":     true,
		"/billing/webhook": true,
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if unauthPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			token, ok := tokenFromRequest(r)
			if !ok {
				http.Error(w, `{"error":"missing or malformed credentials"}`, http.StatusUnauthorized)
				return
			}

			// Try JWT first
			if claims, err := ValidateJWT(jwtSecret, token); err == nil {
				id := &Identity{
					UserID: claims.UserID,
					OrgID:  claims.OrgID,
					Role:   Role(claims.Role),
					Email:  claims.Email,
				}
				ctx := WithIdentity(r.Context(), id)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Try API key
			keyHash := HashAPIKey(token)
			id, err := resolveAPIKey(r.Context(), keyHash)
			if err != nil {
				http.Error(w, `{"error":"invalid or expired API key"}`, http.StatusUnauthorized)
				return
			}
			ctx := WithIdentity(r.Context(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func AdminMiddleware(adminKeyHash string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.TrimSpace(adminKeyHash) == "" {
				http.Error(w, `{"error":"admin API key is not configured"}`, http.StatusForbidden)
				return
			}
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, `{"error":"missing Authorization header"}`, http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == authHeader {
				http.Error(w, `{"error":"invalid Authorization format, expected Bearer token"}`, http.StatusUnauthorized)
				return
			}
			if subtle.ConstantTimeCompare([]byte(HashAPIKey(token)), []byte(adminKeyHash)) != 1 {
				http.Error(w, `{"error":"invalid admin API key"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithAdmin(r.Context())))
		})
	}
}

// RequireRole returns middleware that checks the user has at least the given role.
func RequireRole(minRole Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := IdentityFromContext(r.Context())
			if id == nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if !hasMinRole(id.Role, minRole) {
				http.Error(w, `{"error":"insufficient permissions"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// roleLevel returns a numeric level for role comparison.
func roleLevel(r Role) int {
	switch r {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// HasMinRole checks if userRole meets or exceeds minRole.
func HasMinRole(userRole, minRole Role) bool {
	return roleLevel(userRole) >= roleLevel(minRole)
}

func hasMinRole(userRole, minRole Role) bool {
	return HasMinRole(userRole, minRole)
}
