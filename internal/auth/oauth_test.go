package auth

import (
	"strings"
	"testing"
	"time"
)

func TestOAuthStateRoundTrip(t *testing.T) {
	h := NewOAuthHandler(OAuthConfig{JWTSecret: []byte("test-secret")}, nil)
	state := oauthState{
		Flow:        "cli",
		RedirectURI: "http://127.0.0.1:49152/callback",
		IssuedAt:    time.Now().Unix(),
	}

	raw := h.signOAuthState(state)
	got, err := h.parseOAuthState(raw)
	if err != nil {
		t.Fatalf("parseOAuthState returned error: %v", err)
	}
	if got.Flow != state.Flow || got.RedirectURI != state.RedirectURI {
		t.Fatalf("state = %+v, want %+v", got, state)
	}
}

func TestOAuthStateRejectsTampering(t *testing.T) {
	h := NewOAuthHandler(OAuthConfig{JWTSecret: []byte("test-secret")}, nil)
	raw := h.signOAuthState(oauthState{
		Flow:        "cli",
		RedirectURI: "http://127.0.0.1:49152/callback",
		IssuedAt:    time.Now().Unix(),
	})

	payload, sig, ok := strings.Cut(raw, ".")
	if !ok {
		t.Fatalf("signed state is malformed: %q", raw)
	}
	replacement := "A"
	if strings.HasPrefix(payload, replacement) {
		replacement = "B"
	}
	tampered := replacement + payload[1:] + "." + sig
	if _, err := h.parseOAuthState(tampered); err == nil {
		t.Fatal("parseOAuthState accepted a tampered state")
	}
}

func TestLoopbackRedirectURIValidation(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:49152/callback",
		"http://localhost:49152/callback",
		"http://[::1]:49152/callback",
	}
	for _, uri := range valid {
		if !isLoopbackRedirectURI(uri) {
			t.Fatalf("isLoopbackRedirectURI(%q) = false, want true", uri)
		}
	}

	invalid := []string{
		"https://127.0.0.1:49152/callback",
		"http://example.com:49152/callback",
		"http://127.0.0.1/callback",
		"http://127.0.0.1:49152/callback#fragment",
	}
	for _, uri := range invalid {
		if isLoopbackRedirectURI(uri) {
			t.Fatalf("isLoopbackRedirectURI(%q) = true, want false", uri)
		}
	}
}
