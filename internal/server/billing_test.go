package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func signPayload(secret, payload string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(fmt.Appendf(nil, "%d.%s", ts, payload))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyWebhook(t *testing.T) {
	const secret = "whsec_test"
	client := NewStripeClient(StripeConfig{WebhookSecret: secret})
	payload := `{"type":"checkout.session.completed","data":{"object":{"client_reference_id":"org-1"}}}`

	t.Run("valid", func(t *testing.T) {
		sig := signPayload(secret, payload, time.Now().Unix())
		event, err := client.VerifyWebhook([]byte(payload), sig)
		if err != nil {
			t.Fatalf("expected valid signature, got %v", err)
		}
		if event.Type != "checkout.session.completed" {
			t.Fatalf("unexpected event type %q", event.Type)
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		sig := signPayload(secret, payload, time.Now().Unix())
		if _, err := client.VerifyWebhook([]byte(payload+"x"), sig); err == nil {
			t.Fatal("expected signature mismatch for tampered payload")
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		sig := signPayload("whsec_other", payload, time.Now().Unix())
		if _, err := client.VerifyWebhook([]byte(payload), sig); err == nil {
			t.Fatal("expected signature mismatch for wrong secret")
		}
	})

	t.Run("expired timestamp", func(t *testing.T) {
		sig := signPayload(secret, payload, time.Now().Add(-10*time.Minute).Unix())
		if _, err := client.VerifyWebhook([]byte(payload), sig); err == nil {
			t.Fatal("expected error for timestamp outside tolerance")
		}
	})
}
