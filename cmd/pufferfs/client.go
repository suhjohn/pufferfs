package main

import (
	"bytes"
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

func (e *apiError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, string(e.Body))
}

func newAPIClient(cfg *appconfig.Config) *apiClient {
	return &apiClient{
		baseURL:    cfg.Server.URL,
		apiKey:     cfg.Server.APIKey,
		httpClient: &http.Client{Timeout: 3600 * time.Second},
	}
}

func (c *apiClient) post(path string, body any) ([]byte, error) {
	return c.request("POST", path, body)
}

func (c *apiClient) get(path string) ([]byte, error) {
	return c.request("GET", path, nil)
}

func (c *apiClient) request(method, path string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	url := c.baseURL + path
	req, err := http.NewRequest(method, url, bodyReader)
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

func (c *apiClient) postRaw(path string, data []byte, contentType string) ([]byte, error) {
	return c.postStream(path, bytes.NewReader(data), contentType)
}

func (c *apiClient) postStream(path string, body io.Reader, contentType string) ([]byte, error) {
	url := c.baseURL + path
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Content-Type", contentType)

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
