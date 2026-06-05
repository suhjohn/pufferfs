package auth

import (
	"net/http"
	"strings"
	"time"
)

// SessionCookieName is the cookie that carries the browser session JWT set by
// the OAuth callback. API clients (CLI) keep using the Authorization header.
const SessionCookieName = "pf_session"

// CookieConfig controls how the session cookie is written. Domain is set to the
// registrable domain (e.g. ".pufferfs.com") so the cookie is shared between the
// app and api subdomains; Secure must be true whenever the site is served over
// HTTPS.
type CookieConfig struct {
	Domain string
	Secure bool
}

// SetSessionCookie writes the session JWT as an httpOnly cookie.
func SetSessionCookie(w http.ResponseWriter, cfg CookieConfig, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Domain:   cfg.Domain,
		Expires:  time.Now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(w http.ResponseWriter, cfg CookieConfig) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Domain:   cfg.Domain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// CORS returns middleware that allows the given comma-separated origins to make
// credentialed (cookie-bearing) requests. With an empty origin list it is a
// no-op, which is the right behaviour for same-origin / API-key-only setups.
func CORS(origins string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for o := range strings.SplitSeq(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
