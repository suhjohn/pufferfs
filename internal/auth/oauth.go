package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// OAuthConfig holds OAuth2 settings.
type OAuthConfig struct {
	GoogleClientID     string
	GoogleClientSecret string
	RedirectURL        string // e.g. "https://api.pufferfs.com/auth/callback"
	JWTSecret          []byte
}

// UserInfo is the profile returned by Google's userinfo endpoint.
type UserInfo struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// UserUpsertFunc is called to create-or-update a user after OAuth, returning
// (userID, orgID, role).
type UserUpsertFunc func(ctx context.Context, info UserInfo, provider string) (userID, orgID string, role Role, err error)

// OAuthHandler handles the Google OAuth2 flow.
type OAuthHandler struct {
	oauthCfg   *oauth2.Config
	jwtSecret  []byte
	upsertUser UserUpsertFunc
}

// NewOAuthHandler creates a handler for Google OAuth2.
func NewOAuthHandler(cfg OAuthConfig, upsertUser UserUpsertFunc) *OAuthHandler {
	return &OAuthHandler{
		oauthCfg: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     google.Endpoint,
		},
		jwtSecret:  cfg.JWTSecret,
		upsertUser: upsertUser,
	}
}

// HandleLogin redirects the user to Google's consent page.
func (h *OAuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// In production, use a random state + store in cookie for CSRF protection
	state := "pufferfs-oauth-state"
	url := h.oauthCfg.AuthCodeURL(state, oauth2.AccessTypeOffline)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// HandleCallback exchanges the auth code for a token, fetches user info,
// upserts the user, and returns a JWT.
func (h *OAuthHandler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, `{"error":"missing code parameter"}`, http.StatusBadRequest)
		return
	}

	// Exchange code for token
	token, err := h.oauthCfg.Exchange(r.Context(), code)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"token exchange failed: %s"}`, err), http.StatusBadRequest)
		return
	}

	// Fetch user info from Google
	client := h.oauthCfg.Client(r.Context(), token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"fetching user info: %s"}`, err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var info UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		http.Error(w, `{"error":"decoding user info"}`, http.StatusInternalServerError)
		return
	}

	// Upsert user + resolve org
	userID, orgID, role, err := h.upsertUser(r.Context(), info, "google")
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"user setup failed: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Generate JWT
	jwtToken, err := GenerateJWT(h.jwtSecret, userID, orgID, role, info.Email, 24*time.Hour)
	if err != nil {
		http.Error(w, `{"error":"generating token"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"token":   jwtToken,
		"user_id": userID,
		"org_id":  orgID,
		"email":   info.Email,
		"name":    info.Name,
	})
}
