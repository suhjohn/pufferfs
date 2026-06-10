// Package analytics emits best-effort product events.
package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// Event is a sanitized analytics event. DistinctID should be the authenticated
// user ID when available, or a stable org-level ID for system events.
type Event struct {
	DistinctID string
	Name       string
	Properties map[string]any
	Timestamp  time.Time
}

// Capturer is the interface used by the rest of the application.
type Capturer interface {
	Capture(ctx context.Context, event Event)
}

// Config controls PostHog capture.
type Config struct {
	Enabled    bool
	ProjectKey string
	Host       string
	HTTPClient *http.Client
	Logger     *log.Logger
}

// Client sends events to PostHog's capture API.
type Client struct {
	enabled    bool
	projectKey string
	host       string
	http       *http.Client
	logger     *log.Logger
}

// New returns a PostHog analytics client. A disabled client is a no-op.
func New(cfg Config) *Client {
	host := strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	if host == "" {
		host = "https://us.i.posthog.com"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Second}
	}
	return &Client{
		enabled:    cfg.Enabled && strings.TrimSpace(cfg.ProjectKey) != "",
		projectKey: strings.TrimSpace(cfg.ProjectKey),
		host:       host,
		http:       httpClient,
		logger:     cfg.Logger,
	}
}

// Capture queues one event for best-effort delivery. It never returns an error
// to product code.
func (c *Client) Capture(ctx context.Context, event Event) {
	if c == nil || !c.enabled || event.Name == "" || event.DistinctID == "" {
		return
	}
	go c.capture(ctx, event)
}

func (c *Client) capture(parent context.Context, event Event) {
	ctx := context.WithoutCancel(parent)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	payload, err := json.Marshal(CapturePayload(c.projectKey, event))
	if err != nil {
		c.logf("analytics: marshal event %q: %v", event.Name, err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/i/v0/e/", bytes.NewReader(payload))
	if err != nil {
		c.logf("analytics: create request for event %q: %v", event.Name, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		c.logf("analytics: send event %q: %v", event.Name, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		c.logf("analytics: event %q returned HTTP %d", event.Name, resp.StatusCode)
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// CapturePayload builds the PostHog capture API request body.
func CapturePayload(projectKey string, event Event) map[string]any {
	timestamp := event.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	properties := map[string]any{}
	for key, value := range event.Properties {
		if value == nil {
			continue
		}
		properties[key] = value
	}
	return map[string]any{
		"api_key":     projectKey,
		"event":       event.Name,
		"distinct_id": event.DistinctID,
		"properties":  properties,
		"timestamp":   timestamp.UTC().Format(time.RFC3339Nano),
	}
}

// Noop is an analytics implementation for tests and disabled environments.
type Noop struct{}

// Capture implements Capturer.
func (Noop) Capture(context.Context, Event) {}
