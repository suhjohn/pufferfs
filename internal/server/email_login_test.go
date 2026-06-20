package server

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeLoginEmail(t *testing.T) {
	got, err := normalizeLoginEmail(" USER@example.COM ")
	if err != nil {
		t.Fatalf("normalizeLoginEmail returned error: %v", err)
	}
	if got != "user@example.com" {
		t.Fatalf("email = %q, want user@example.com", got)
	}
	if _, err := normalizeLoginEmail("not-an-email"); err == nil {
		t.Fatal("normalizeLoginEmail accepted invalid email")
	}
}

func TestNormalizeLoginCodeKeepsDigits(t *testing.T) {
	if got := normalizeLoginCode(" 123 456-78 "); got != "12345678" {
		t.Fatalf("code = %q, want 12345678", got)
	}
}

func TestRandomNumericCode(t *testing.T) {
	code, err := randomNumericCode(8)
	if err != nil {
		t.Fatalf("randomNumericCode returned error: %v", err)
	}
	if len(code) != 8 {
		t.Fatalf("code length = %d, want 8", len(code))
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			t.Fatalf("code contains non-digit rune %q", r)
		}
	}
}

func TestRenderLoginCodeEmail(t *testing.T) {
	sender := &SESTransactionalEmailSender{cfg: SESTransactionalEmailConfig{AppURL: "https://app.example.com"}}
	subject, textBody, htmlBody := sender.renderLoginCode(LoginCodeEmail{
		To:        "user@example.com",
		Code:      "12345678",
		ExpiresIn: 10 * time.Minute,
	})
	if subject != "Your PufferFS login code" {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{"12345678", "10 minutes", "https://app.example.com/login"} {
		if !strings.Contains(textBody, want) {
			t.Fatalf("text body missing %q:\n%s", want, textBody)
		}
	}
	if !strings.Contains(htmlBody, "12345678") || !strings.Contains(htmlBody, `href="https://app.example.com/login"`) {
		t.Fatalf("html body missing code or link:\n%s", htmlBody)
	}
}
