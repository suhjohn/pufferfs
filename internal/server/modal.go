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

// defaultEmbeddingModelVersion identifies the embedding model whose vectors are
// stored in the embedding cache. It MUST match EMBEDDING_MODEL in modal/app.py.
// Bump it (or set PUFFERFS_EMBEDDING_MODEL_VERSION) whenever the Modal embedding
// model changes so cached vectors from the old model are never reused.
const defaultEmbeddingModelVersion = "nomic-ai/nomic-embed-text-v1.5"

// ModalClient calls Modal web endpoints for chunking and embedding.
type ModalClient struct {
	chunkURL      string
	embedURL      string
	queryEmbedURL string
	modelVersion  string
	httpClient    *http.Client
}

// NewModalClient creates a client for calling Modal endpoints.
func NewModalClient() *ModalClient {
	return &ModalClient{
		chunkURL:      os.Getenv("MODAL_CHUNK_ENDPOINT"),
		embedURL:      os.Getenv("MODAL_EMBED_ENDPOINT"),
		queryEmbedURL: os.Getenv("MODAL_QUERY_EMBED_ENDPOINT"),
		modelVersion:  embeddingModelVersion(),
		httpClient:    &http.Client{Timeout: 900 * time.Second},
	}
}

// EmbeddingModelVersion returns the identifier of the active embedding model.
// It is used as part of the embedding cache key so vectors are never reused
// across model versions.
func (m *ModalClient) EmbeddingModelVersion() string {
	if m.modelVersion == "" {
		return defaultEmbeddingModelVersion
	}
	return m.modelVersion
}

func embeddingModelVersion() string {
	if v := strings.TrimSpace(os.Getenv("PUFFERFS_EMBEDDING_MODEL_VERSION")); v != "" {
		return v
	}
	return defaultEmbeddingModelVersion
}

// ChunkFileRequest is the payload sent to the Modal chunk endpoint.
type ChunkFileRequest struct {
	S3Key      string `json:"s3_key"`
	FilePath   string `json:"file_path"`
	FileType   string `json:"file_type"`
	RootID     string `json:"root_id"`
	ContentB64 string `json:"content_b64,omitempty"`
}

// ChunkFileResponse is returned from Modal.
type ChunkFileResponse struct {
	Chunks []map[string]any `json:"chunks"`
	Count  int              `json:"count"`
}

// EmbedChunksRequest is sent to the Modal embed endpoint.
type EmbedChunksRequest struct {
	Chunks []map[string]any `json:"chunks"`
}

// EmbedChunksResponse is returned from Modal.
type EmbedChunksResponse struct {
	Results []map[string]any `json:"results"`
	Count   int              `json:"count"`
}

// EmbedQueryRequest is sent to the Modal query embed endpoint.
type EmbedQueryRequest struct {
	Texts []string `json:"texts"`
}

// EmbedQueryResponse is returned from Modal.
type EmbedQueryResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// ChunkFile calls the Modal chunking function.
func (m *ModalClient) ChunkFile(req ChunkFileRequest) (*ChunkFileResponse, error) {
	var resp ChunkFileResponse
	if err := m.post(m.chunkURL, req, &resp); err != nil {
		return nil, fmt.Errorf("modal chunk: %w", err)
	}
	return &resp, nil
}

// EmbedChunks calls the Modal embedding function.
func (m *ModalClient) EmbedChunks(chunks []map[string]any) (*EmbedChunksResponse, error) {
	req := EmbedChunksRequest{Chunks: chunks}
	var resp EmbedChunksResponse
	if err := m.post(m.embedURL, req, &resp); err != nil {
		return nil, fmt.Errorf("modal embed: %w", err)
	}
	return &resp, nil
}

// EmbedQuery embeds a single query string.
func (m *ModalClient) EmbedQuery(text string) ([]float64, error) {
	req := EmbedQueryRequest{Texts: []string{text}}
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
