package server

import (
	"context"
	"strings"
	"testing"
)

func TestInviteEmailSenderFromEnvDisabledWithoutSender(t *testing.T) {
	t.Setenv("INVITE_EMAIL_FROM", "")
	sender, err := NewInviteEmailSenderFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewInviteEmailSenderFromEnv returned error: %v", err)
	}
	if sender != nil {
		t.Fatalf("expected nil sender when INVITE_EMAIL_FROM is unset")
	}
}

func TestSESInviteEmailSenderRequiresAppURL(t *testing.T) {
	t.Setenv("INVITE_EMAIL_FROM", "team@example.com")
	t.Setenv("INVITE_EMAIL_APP_URL", "")
	t.Setenv("FRONTEND_URL", "")
	sender, err := NewInviteEmailSenderFromEnv(context.Background())
	if err == nil {
		t.Fatalf("expected missing app URL error, sender=%v", sender)
	}
	if !strings.Contains(err.Error(), "INVITE_EMAIL_APP_URL") {
		t.Fatalf("expected app URL error, got %v", err)
	}
}

func TestRenderInviteEmail(t *testing.T) {
	sender := &SESInviteEmailSender{cfg: SESInviteEmailConfig{AppURL: "https://app.example.com"}}
	subject, textBody, htmlBody := sender.renderInvite(OrgInviteEmail{
		To:           "teammate@example.com",
		Role:         "editor",
		OrgName:      "Acme <Corp>",
		InviterEmail: "owner@example.com",
	})

	if subject != "You are invited to join Acme <Corp>" {
		t.Fatalf("unexpected subject: %q", subject)
	}
	for _, want := range []string{
		"owner@example.com invited you",
		"Acme <Corp>",
		"editor",
		"https://app.example.com/login",
	} {
		if !strings.Contains(textBody, want) {
			t.Fatalf("text body missing %q:\n%s", want, textBody)
		}
	}
	if !strings.Contains(htmlBody, "Acme &lt;Corp&gt;") {
		t.Fatalf("html body did not escape org name:\n%s", htmlBody)
	}
	if !strings.Contains(htmlBody, `href="https://app.example.com/login"`) {
		t.Fatalf("html body missing login link:\n%s", htmlBody)
	}
}
