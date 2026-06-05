package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pufferfs/pufferfs/internal/auth"
)

// Subscription is the billing state for an org.
type Subscription struct {
	OrgID            string     `json:"org_id"`
	Plan             string     `json:"plan"`
	Status           string     `json:"status"`
	CurrentPeriodEnd *time.Time `json:"current_period_end,omitempty"`
}

// StripeConfig configures the Stripe integration. Billing is only wired up when
// this is provided (see main.go / ENABLE_BILLING).
type StripeConfig struct {
	SecretKey     string
	WebhookSecret string
	PriceID       string
	FrontendURL   string
	// BaseURL overrides the Stripe API base (used in tests). Defaults to the
	// live Stripe API.
	BaseURL    string
	HTTPClient *http.Client
}

// StripeClient is a tiny, dependency-free Stripe client covering the two calls
// we need: creating a Checkout session and verifying webhook signatures.
type StripeClient struct {
	secretKey     string
	webhookSecret string
	priceID       string
	frontendURL   string
	baseURL       string
	http          *http.Client
}

// NewStripeClient builds a StripeClient from config.
func NewStripeClient(cfg StripeConfig) *StripeClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.stripe.com"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &StripeClient{
		secretKey:     cfg.SecretKey,
		webhookSecret: cfg.WebhookSecret,
		priceID:       cfg.PriceID,
		frontendURL:   strings.TrimRight(cfg.FrontendURL, "/"),
		baseURL:       strings.TrimRight(baseURL, "/"),
		http:          httpClient,
	}
}

// SetBilling enables the billing endpoints. Without it they return 404.
func (s *Server) SetBilling(client *StripeClient) {
	s.billing = client
}

// CreateCheckoutSession creates a Stripe Checkout session and returns its hosted
// URL. client_reference_id and metadata carry the org so the webhook can map
// the completed session back to a subscription.
func (c *StripeClient) CreateCheckoutSession(ctx context.Context, orgID, customerEmail string) (string, error) {
	if c.priceID == "" {
		return "", fmt.Errorf("STRIPE_PRICE_ID is not configured")
	}
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("line_items[0][price]", c.priceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("client_reference_id", orgID)
	form.Set("metadata[org_id]", orgID)
	form.Set("subscription_data[metadata][org_id]", orgID)
	form.Set("success_url", c.frontendURL+"/billing?status=success")
	form.Set("cancel_url", c.frontendURL+"/billing?status=cancelled")
	if customerEmail != "" {
		form.Set("customer_email", customerEmail)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("stripe checkout session failed (%d): %s", resp.StatusCode, string(body))
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decoding stripe response: %w", err)
	}
	if out.URL == "" {
		return "", fmt.Errorf("stripe returned an empty checkout url")
	}
	return out.URL, nil
}

// stripeEvent is the minimal shape we read from webhook payloads.
type stripeEvent struct {
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// VerifyWebhook validates the Stripe-Signature header against the raw payload
// and returns the parsed event. Implements Stripe's scheme: signed_payload =
// "<timestamp>.<body>", HMAC-SHA256 with the webhook secret, compared to a v1
// signature, within a 5-minute tolerance.
func (c *StripeClient) VerifyWebhook(payload []byte, sigHeader string) (*stripeEvent, error) {
	var timestamp string
	var signatures []string
	for part := range strings.SplitSeq(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			signatures = append(signatures, kv[1])
		}
	}
	if timestamp == "" || len(signatures) == 0 {
		return nil, fmt.Errorf("missing timestamp or signature")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp: %w", err)
	}
	if time.Since(time.Unix(ts, 0)) > 5*time.Minute {
		return nil, fmt.Errorf("timestamp outside tolerance")
	}

	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	matched := false
	for _, sig := range signatures {
		if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) == 1 {
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("signature mismatch")
	}

	var event stripeEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("decoding event: %w", err)
	}
	return &event, nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleGetBilling(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "billing is not enabled"})
		return
	}
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	sub, err := s.db.GetSubscription(r.Context(), id.OrgID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (s *Server) handleCreateCheckoutSession(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "billing is not enabled"})
		return
	}
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !auth.HasMinRole(id.Role, auth.RoleAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required to manage billing"})
		return
	}
	checkoutURL, err := s.billing.CreateCheckoutSession(r.Context(), id.OrgID, id.Email)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": checkoutURL})
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "billing is not enabled"})
		return
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body"})
		return
	}
	event, err := s.billing.VerifyWebhook(payload, r.Header.Get("Stripe-Signature"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "webhook verification failed: " + err.Error()})
		return
	}

	if err := s.applyStripeEvent(r.Context(), event); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"received": "true"})
}

// applyStripeEvent reconciles our subscriptions table with the events we care
// about. Unknown event types are acknowledged and ignored.
func (s *Server) applyStripeEvent(ctx context.Context, event *stripeEvent) error {
	switch event.Type {
	case "checkout.session.completed":
		var obj struct {
			ClientReferenceID string `json:"client_reference_id"`
			Customer          string `json:"customer"`
			Subscription      string `json:"subscription"`
			Metadata          struct {
				OrgID string `json:"org_id"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(event.Data.Object, &obj); err != nil {
			return fmt.Errorf("decoding checkout session: %w", err)
		}
		orgID := obj.ClientReferenceID
		if orgID == "" {
			orgID = obj.Metadata.OrgID
		}
		if orgID == "" {
			return nil
		}
		return s.db.UpsertSubscription(ctx, Subscription{
			OrgID:  orgID,
			Plan:   "pro",
			Status: "active",
		}, obj.Customer, obj.Subscription, nil)

	case "customer.subscription.updated", "customer.subscription.deleted":
		var obj struct {
			Customer         string `json:"customer"`
			Status           string `json:"status"`
			CurrentPeriodEnd int64  `json:"current_period_end"`
			Metadata         struct {
				OrgID string `json:"org_id"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(event.Data.Object, &obj); err != nil {
			return fmt.Errorf("decoding subscription: %w", err)
		}
		var periodEnd *time.Time
		if obj.CurrentPeriodEnd > 0 {
			t := time.Unix(obj.CurrentPeriodEnd, 0).UTC()
			periodEnd = &t
		}
		status := obj.Status
		plan := "pro"
		if event.Type == "customer.subscription.deleted" {
			status = "canceled"
			plan = "free"
		}
		return s.db.UpdateSubscriptionByCustomer(ctx, obj.Customer, obj.Metadata.OrgID, plan, status, periodEnd)
	}
	return nil
}

// ---------------------------------------------------------------------------
// DB
// ---------------------------------------------------------------------------

// GetSubscription returns the org's subscription, defaulting to the free plan.
func (db *DB) GetSubscription(ctx context.Context, orgID string) (*Subscription, error) {
	sub := &Subscription{OrgID: orgID, Plan: "free", Status: "none"}
	err := db.pool.QueryRow(ctx,
		`SELECT plan, status, current_period_end FROM subscriptions WHERE org_id = $1`, orgID,
	).Scan(&sub.Plan, &sub.Status, &sub.CurrentPeriodEnd)
	if err != nil {
		// No row yet → free plan default.
		return &Subscription{OrgID: orgID, Plan: "free", Status: "none"}, nil
	}
	return sub, nil
}

// UpsertSubscription writes the subscription row for an org.
func (db *DB) UpsertSubscription(ctx context.Context, sub Subscription, customerID, subscriptionID string, periodEnd *time.Time) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO subscriptions (org_id, stripe_customer_id, stripe_subscription_id, plan, status, current_period_end, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())
		 ON CONFLICT (org_id) DO UPDATE SET
		     stripe_customer_id = EXCLUDED.stripe_customer_id,
		     stripe_subscription_id = EXCLUDED.stripe_subscription_id,
		     plan = EXCLUDED.plan,
		     status = EXCLUDED.status,
		     current_period_end = EXCLUDED.current_period_end,
		     updated_at = NOW()`,
		sub.OrgID, customerID, subscriptionID, sub.Plan, sub.Status, periodEnd,
	)
	return err
}

// UpdateSubscriptionByCustomer updates the subscription identified by Stripe
// customer id (falling back to org id from metadata when present).
func (db *DB) UpdateSubscriptionByCustomer(ctx context.Context, customerID, orgID, plan, status string, periodEnd *time.Time) error {
	if customerID != "" {
		tag, err := db.pool.Exec(ctx,
			`UPDATE subscriptions
			 SET plan = $1, status = $2, current_period_end = $3, updated_at = NOW()
			 WHERE stripe_customer_id = $4`,
			plan, status, periodEnd, customerID,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
	}
	if orgID == "" {
		return nil
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE subscriptions
		 SET plan = $1, status = $2, current_period_end = $3, updated_at = NOW()
		 WHERE org_id = $4`,
		plan, status, periodEnd, orgID,
	)
	return err
}
