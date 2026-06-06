package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	// FrontendURL, when set, switches the callback from returning the JWT in the
	// response body (legacy CLI flow) to setting an httpOnly session cookie and
	// redirecting the browser to FrontendURL + "/auth/callback".
	FrontendURL  string
	Cookie       CookieConfig
	CreateAPIKey APIKeyCreateFunc
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

// APIKeyCreateFunc creates a raw API key for a user. It is used by the CLI
// browser login flow after OAuth succeeds.
type APIKeyCreateFunc func(ctx context.Context, orgID, userID, name string, scopes []string) (rawKey string, err error)

// OAuthHandler handles the Google OAuth2 flow.
type OAuthHandler struct {
	oauthCfg     *oauth2.Config
	jwtSecret    []byte
	upsertUser   UserUpsertFunc
	createAPIKey APIKeyCreateFunc
	frontendURL  string
	cookie       CookieConfig
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
		jwtSecret:    cfg.JWTSecret,
		upsertUser:   upsertUser,
		createAPIKey: cfg.CreateAPIKey,
		frontendURL:  strings.TrimRight(cfg.FrontendURL, "/"),
		cookie:       cfg.Cookie,
	}
}

// HandleLogin redirects the user to Google's consent page.
func (h *OAuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state := oauthState{
		Flow:     "web",
		IssuedAt: time.Now().Unix(),
	}
	if redirectURI := strings.TrimSpace(r.URL.Query().Get("cli_redirect_uri")); redirectURI != "" {
		if !isLoopbackRedirectURI(redirectURI) {
			http.Error(w, `{"error":"invalid CLI redirect URI"}`, http.StatusBadRequest)
			return
		}
		state.Flow = "cli"
		state.RedirectURI = redirectURI
	}
	url := h.oauthCfg.AuthCodeURL(h.signOAuthState(state), oauth2.AccessTypeOffline)
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
	state, err := h.parseOAuthState(r.URL.Query().Get("state"))
	if err != nil {
		http.Error(w, `{"error":"invalid OAuth state"}`, http.StatusBadRequest)
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
	const sessionTTL = 24 * time.Hour
	jwtToken, err := GenerateJWT(h.jwtSecret, userID, orgID, role, info.Email, sessionTTL)
	if err != nil {
		http.Error(w, `{"error":"generating token"}`, http.StatusInternalServerError)
		return
	}

	if state.Flow == "cli" {
		h.finishCLILogin(w, r, state, orgID, userID, info.Email)
		return
	}

	// Browser flow: set an httpOnly session cookie and bounce back to the app.
	if h.frontendURL != "" {
		SetSessionCookie(w, h.cookie, jwtToken, sessionTTL)
		http.Redirect(w, r, h.frontendURL+"/auth/callback", http.StatusFound)
		return
	}

	// Legacy flow (CLI): return the token in the response body.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"token":   jwtToken,
		"user_id": userID,
		"org_id":  orgID,
		"email":   info.Email,
		"name":    info.Name,
	})
}

// HandleLogout clears the session cookie.
func (h *OAuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	ClearSessionCookie(w, h.cookie)
	w.WriteHeader(http.StatusNoContent)
}

type oauthState struct {
	Flow        string `json:"flow"`
	RedirectURI string `json:"redirect_uri,omitempty"`
	IssuedAt    int64  `json:"iat"`
}

func (h *OAuthHandler) signOAuthState(state oauthState) string {
	payload, _ := json.Marshal(state)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, h.jwtSecret)
	mac.Write([]byte(payloadB64))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payloadB64 + "." + sig
}

func (h *OAuthHandler) parseOAuthState(raw string) (oauthState, error) {
	if raw == "" || raw == "pufferfs-oauth-state" {
		return oauthState{Flow: "web", IssuedAt: time.Now().Unix()}, nil
	}
	payloadB64, sigB64, ok := strings.Cut(raw, ".")
	if !ok {
		return oauthState{}, fmt.Errorf("malformed state")
	}
	mac := hmac.New(sha256.New, h.jwtSecret)
	mac.Write([]byte(payloadB64))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !hmac.Equal(got, want) {
		return oauthState{}, fmt.Errorf("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return oauthState{}, err
	}
	var state oauthState
	if err := json.Unmarshal(payload, &state); err != nil {
		return oauthState{}, err
	}
	if state.IssuedAt == 0 || time.Since(time.Unix(state.IssuedAt, 0)) > 10*time.Minute {
		return oauthState{}, fmt.Errorf("expired state")
	}
	switch state.Flow {
	case "web":
		return state, nil
	case "cli":
		if !isLoopbackRedirectURI(state.RedirectURI) {
			return oauthState{}, fmt.Errorf("invalid redirect")
		}
		return state, nil
	default:
		return oauthState{}, fmt.Errorf("invalid flow")
	}
}

func (h *OAuthHandler) finishCLILogin(w http.ResponseWriter, r *http.Request, state oauthState, orgID, userID, email string) {
	callback, err := url.Parse(state.RedirectURI)
	if err != nil || !isLoopbackRedirectURI(state.RedirectURI) {
		http.Error(w, `{"error":"invalid CLI redirect URI"}`, http.StatusBadRequest)
		return
	}
	query := callback.Query()
	if h.createAPIKey == nil {
		query.Set("error", "CLI login is not enabled on this server")
		callback.RawQuery = query.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
		return
	}
	rawKey, err := h.createAPIKey(r.Context(), orgID, userID, "CLI key from pufferfs init", []string{"sync", "query", "root:delete"})
	if err != nil {
		query.Set("error", "creating CLI key failed")
		callback.RawQuery = query.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
		return
	}
	query.Set("api_key", rawKey)
	query.Set("email", email)
	callback.RawQuery = query.Encode()
	http.Redirect(w, r, callback.String(), http.StatusFound)
}

func isLoopbackRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	if host == "" || u.Port() == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
