package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	sleep      func(time.Duration)
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

func (c *apiClient) postRaw(path string, data []byte, contentType string) ([]byte, error) {
	return c.postStream(path, bytes.NewReader(data), contentType)
}

func (c *apiClient) postStream(path string, body io.Reader, contentType string) ([]byte, error) {
	const maxAttempts = 3
	const baseDelay = 250 * time.Millisecond

	seeker, seekable := body.(io.ReadSeeker)
	readerAt, canRetry := body.(io.ReaderAt)
	startOffset, contentLength := int64(0), int64(-1)
	if seekable {
		var err error
		startOffset, err = seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			canRetry = false
		} else {
			endOffset, seekErr := seeker.Seek(0, io.SeekEnd)
			if seekErr == nil && endOffset >= startOffset {
				contentLength = endOffset - startOffset
			} else {
				canRetry = false
			}
			if _, restoreErr := seeker.Seek(startOffset, io.SeekStart); restoreErr != nil {
				return nil, fmt.Errorf("rewinding upload body: %w", restoreErr)
			}
		}
	} else {
		canRetry = false
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := baseDelay * time.Duration(1<<(attempt-1))
			if c.sleep != nil {
				c.sleep(delay)
			} else {
				time.Sleep(delay)
			}
		}

		attemptBody := body
		if canRetry {
			// A SectionReader has an independent offset, avoiding a race with a
			// transport that may finish closing the previous request body after
			// Do has returned.
			attemptBody = io.NewSectionReader(readerAt, startOffset, contentLength)
		}
		respBody, err := c.postStreamOnce(path, attemptBody, contentType, contentLength)
		if err == nil {
			return respBody, nil
		}
		lastErr = err
		if !canRetry || !isRetryableUploadError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *apiClient) postStreamOnce(path string, body io.Reader, contentType string, contentLength int64) ([]byte, error) {
	url := c.baseURL + path
	// The transport closes Request.Body after every attempt. Wrap the caller's
	// reader so seekable files remain open for a retry and ownership stays with
	// the caller.
	reqBody := io.NopCloser(struct{ io.Reader }{body})
	req, err := http.NewRequest("POST", url, reqBody)
	if err != nil {
		return nil, err
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
		if contentLength == 0 {
			req.Body = http.NoBody
		}
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

func isRetryableUploadError(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		// Transport failures and truncated responses are safe to retry because
		// buffered bodies are replayable and generation-scoped standalone upload
		// attempts are stored under distinct immutable object keys.
		return true
	}
	return apiErr.StatusCode == http.StatusRequestTimeout ||
		apiErr.StatusCode == http.StatusTooManyRequests ||
		apiErr.StatusCode >= http.StatusInternalServerError
}
