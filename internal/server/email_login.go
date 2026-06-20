package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	productanalytics "github.com/pufferfs/pufferfs/internal/analytics"
	"github.com/pufferfs/pufferfs/internal/auth"
)

const (
	emailLoginCodeDigits   = 8
	emailLoginTTL          = 10 * time.Minute
	emailLoginResendAfter  = 30 * time.Second
	emailLoginMaxAttempts  = 5
	emailLoginRateLimitMax = 12
)

type emailLoginStartRequest struct {
	Email          string `json:"email"`
	Flow           string `json:"flow"`
	CLIRedirectURI string `json:"cli_redirect_uri"`
}

type emailLoginStartResponse struct {
	ChallengeID string `json:"challenge_id"`
	ExpiresIn   int    `json:"expires_in"`
	ResendAfter int    `json:"resend_after"`
}

type emailLoginVerifyRequest struct {
	ChallengeID string `json:"challenge_id"`
	Code        string `json:"code"`
}

func (s *Server) handleEmailLoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.emailLogin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "email login is not enabled"})
		return
	}
	if s.emails == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "email login is not configured"})
		return
	}
	var req emailLoginStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	email, err := normalizeLoginEmail(req.Email)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid email required"})
		return
	}
	flow := normalizeEmailLoginFlow(req.Flow)
	if flow == "cli" && !isLoopbackLoginRedirectURI(req.CLIRedirectURI) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid CLI redirect URI"})
		return
	}

	ipHash := s.requestFingerprint(r.Context(), requestIP(r))
	if count, err := s.db.CountRecentEmailLoginChallenges(r.Context(), email, ipHash, time.Now().Add(-emailLoginResendAfter)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "checking login cooldown: " + err.Error()})
		return
	} else if count > 0 {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "wait before requesting another login code"})
		return
	}
	if count, err := s.db.CountRecentEmailLoginChallenges(r.Context(), email, ipHash, time.Now().Add(-time.Hour)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "checking login rate limit: " + err.Error()})
		return
	} else if count >= emailLoginRateLimitMax {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many login codes requested; try again later"})
		return
	}

	challengeID := "elc_" + randomID()
	code, err := randomNumericCode(emailLoginCodeDigits)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "creating login code"})
		return
	}
	challenge := EmailLoginChallenge{
		ID:             challengeID,
		Email:          email,
		CodeHash:       s.emailLoginCodeHash(challengeID, code),
		Flow:           flow,
		CLIRedirectURI: strings.TrimSpace(req.CLIRedirectURI),
		MaxAttempts:    emailLoginMaxAttempts,
		RequestIPHash:  ipHash,
		UserAgentHash:  s.requestFingerprint(r.Context(), r.UserAgent()),
		ExpiresAt:      time.Now().Add(emailLoginTTL),
	}
	if err := s.db.CreateEmailLoginChallenge(r.Context(), challenge); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "creating login challenge: " + err.Error()})
		return
	}
	if err := s.emails.SendLoginCode(r.Context(), LoginCodeEmail{
		To:        email,
		Code:      code,
		ExpiresIn: emailLoginTTL,
		AppURL:    s.frontend,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sending login code: " + err.Error()})
		return
	}
	_ = s.db.DeleteExpiredEmailLoginChallenges(context.WithoutCancel(r.Context()))
	s.analytics.Capture(r.Context(), productanalytics.Event{
		DistinctID: email,
		Name:       "email_login_code_requested",
		Properties: map[string]any{
			"event_source": "backend",
			"flow":         flow,
			"email_domain": emailDomain(email),
		},
	})
	writeJSON(w, http.StatusOK, emailLoginStartResponse{
		ChallengeID: challengeID,
		ExpiresIn:   int(emailLoginTTL.Seconds()),
		ResendAfter: int(emailLoginResendAfter.Seconds()),
	})
}

func (s *Server) handleEmailLoginVerify(w http.ResponseWriter, r *http.Request) {
	if !s.emailLogin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "email login is not enabled"})
		return
	}
	var req emailLoginVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	challengeID := strings.TrimSpace(req.ChallengeID)
	code := normalizeLoginCode(req.Code)
	if challengeID == "" || code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "challenge_id and code are required"})
		return
	}

	challenge, err := s.db.GetEmailLoginChallenge(r.Context(), challengeID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or expired login code"})
		return
	}
	if challenge.ConsumedAt != nil || time.Now().After(challenge.ExpiresAt) || challenge.Attempts >= challenge.MaxAttempts {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or expired login code"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(challenge.CodeHash), []byte(s.emailLoginCodeHash(challenge.ID, code))) != 1 {
		_ = s.db.IncrementEmailLoginChallengeAttempts(r.Context(), challenge.ID)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or expired login code"})
		return
	}
	if err := s.db.ConsumeEmailLoginChallenge(r.Context(), challenge.ID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or expired login code"})
		return
	}

	login, err := s.db.CompleteLogin(r.Context(), auth.VerifiedIdentity{
		Provider:      "email_code",
		Email:         challenge.Email,
		EmailVerified: true,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "user setup failed: " + err.Error()})
		return
	}

	s.analytics.Capture(r.Context(), productanalytics.Event{
		DistinctID: login.UserID,
		Name:       "user_signed_in",
		Properties: map[string]any{
			"event_source": "backend",
			"org_id":       login.OrgID,
			"user_id":      login.UserID,
			"role":         string(login.Role),
			"flow":         challenge.Flow,
			"provider":     "email_code",
			"email_domain": emailDomain(login.Email),
			"$groups":      map[string]string{"organization": login.OrgID},
		},
	})

	if challenge.Flow == "cli" {
		rawKey, err := s.db.CreateAPIKey(r.Context(), login.OrgID, login.UserID, "CLI key from pufferfs init", []string{"sync", "query", "root:delete"})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "creating CLI key: " + err.Error()})
			return
		}
		resp := map[string]string{
			"status":  "ok",
			"api_key": rawKey,
			"email":   login.Email,
			"org_id":  login.OrgID,
			"user_id": login.UserID,
		}
		if challenge.CLIRedirectURI != "" {
			callback, err := url.Parse(challenge.CLIRedirectURI)
			if err == nil {
				query := callback.Query()
				query.Set("api_key", rawKey)
				query.Set("email", login.Email)
				callback.RawQuery = query.Encode()
				resp["callback_url"] = callback.String()
			}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	const sessionTTL = 24 * time.Hour
	token, err := auth.GenerateJWT(s.jwtSecret, login.UserID, login.OrgID, login.Role, login.Email, sessionTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "generating token"})
		return
	}
	auth.SetSessionCookie(w, s.cookie, token, sessionTTL)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func normalizeLoginEmail(raw string) (string, error) {
	email := normalizeEmail(raw)
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return "", err
	}
	if normalizeEmail(addr.Address) != email || !strings.Contains(email, "@") {
		return "", fmt.Errorf("invalid email")
	}
	return email, nil
}

func normalizeEmailLoginFlow(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "cli":
		return "cli"
	default:
		return "web"
	}
}

func normalizeLoginCode(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func randomNumericCode(digits int) (string, error) {
	if digits <= 0 {
		digits = emailLoginCodeDigits
	}
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", digits, n.Int64()), nil
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) emailLoginCodeHash(challengeID, code string) string {
	mac := hmac.New(sha256.New, s.emailLoginSecret())
	mac.Write([]byte("email-login-code:"))
	mac.Write([]byte(challengeID))
	mac.Write([]byte(":"))
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) requestFingerprint(_ context.Context, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.emailLoginSecret())
	mac.Write([]byte("email-login-fingerprint:"))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) emailLoginSecret() []byte {
	if len(s.jwtSecret) > 0 {
		return s.jwtSecret
	}
	return []byte("pufferfs-dev-secret-change-in-production")
}

func requestIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func isLoopbackLoginRedirectURI(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
