// Package server implements the PufferFs API server.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ModalClient calls independently deployed transformation, indexing, and query roles.
type ModalClient struct {
	transformURL    string
	fileIndexURL    string
	fileCPUIndexURL string
	queryEmbedURL   string
	secretKey       string
	httpClient      *http.Client
}

// NewModalClient creates a client for calling Modal endpoints.
func NewModalClient() *ModalClient {
	return &ModalClient{
		transformURL:    os.Getenv("MODAL_TRANSFORM_ENDPOINT"),
		fileIndexURL:    os.Getenv("MODAL_FILE_INDEX_ENDPOINT"),
		fileCPUIndexURL: os.Getenv("MODAL_FILE_CPU_INDEX_ENDPOINT"),
		queryEmbedURL:   os.Getenv("MODAL_QUERY_EMBED_ENDPOINT"),
		secretKey:       os.Getenv("MODAL_SECRET_KEY"),
		httpClient:      &http.Client{Timeout: time.Hour},
	}
}

// EmbedQueryRequest is sent to the Modal query embed endpoint.
type EmbedQueryRequest struct {
	SecretKey string   `json:"secret_key"`
	Texts     []string `json:"texts"`
}

// EmbedQueryResponse is returned from Modal.
type EmbedQueryResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// EmbedQuery embeds a single query string.
func (m *ModalClient) EmbedQuery(text string) ([]float64, error) {
	if strings.TrimSpace(m.secretKey) == "" {
		return nil, fmt.Errorf("MODAL_SECRET_KEY is required for Modal query embedding")
	}
	req := EmbedQueryRequest{SecretKey: m.secretKey, Texts: []string{text}}
	var resp EmbedQueryResponse
	if err := m.post(m.queryEmbedURL, req, &resp); err != nil {
		return nil, fmt.Errorf("modal embed query: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return resp.Embeddings[0], nil
}

func (m *ModalClient) post(url string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := m.httpClient.Post(url, "application/json", bytes.NewReader(data))
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			return json.Unmarshal(respBody, out)
		}
		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		if resp.StatusCode < 500 || attempt == 2 {
			return lastErr
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}

	return lastErr
}
