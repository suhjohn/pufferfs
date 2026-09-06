package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
)

// apiClient handles HTTP communication with the PufferFs server.
type apiClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

type apiError struct {
	StatusCode int
	Body       []byte
}

var apiHTTPTransport = func() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	clone := base.Clone()
	if clone.MaxIdleConnsPerHost < maxUploadConcurrency {
		clone.MaxIdleConnsPerHost = maxUploadConcurrency
	}
	return clone
}()

func (e *apiError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, string(e.Body))
}

func newAPIClient(cfg *appconfig.Config) *apiClient {
	return &apiClient{
		baseURL:    cfg.Server.URL,
		apiKey:     cfg.Server.APIKey,
		httpClient: &http.Client{Transport: apiHTTPTransport, Timeout: 3600 * time.Second},
	}
}

func (c *apiClient) post(path string, body any) ([]byte, error) {
	return c.request("POST", path, body)
}

func (c *apiClient) postContext(ctx context.Context, path string, body any) ([]byte, error) {
	return c.requestWithContext(ctx, "POST", path, body)
}

func (c *apiClient) put(path string, body any) ([]byte, error) {
	return c.request("PUT", path, body)
}

func (c *apiClient) get(path string) ([]byte, error) {
	return c.request("GET", path, nil)
}

func (c *apiClient) delete(path string) ([]byte, error) {
	return c.request("DELETE", path, nil)
}

func (c *apiClient) request(method, path string, body any) ([]byte, error) {
	return c.requestWithContext(context.Background(), method, path, body)
}

func (c *apiClient) requestWithContext(ctx context.Context, method, path string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{StatusCode: resp.StatusCode, Body: respBody}
	}

	return respBody, nil
}
