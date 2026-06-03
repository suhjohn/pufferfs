package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// TPClient talks to the Turbopuffer API.
type TPClient struct {
	apiKey     string
	region     string
	httpClient *http.Client
}

// NewTPClient creates a Turbopuffer client.
func NewTPClient(apiKey, region string) *TPClient {
	if region == "" {
		region = "gcp-us-central1"
	}
	return &TPClient{
		apiKey:     apiKey,
		region:     region,
		httpClient: &http.Client{},
	}
}

func (t *TPClient) baseURL() string {
	return "https://api.turbopuffer.com"
}

// namespaceName returns the tp namespace for a root.
func namespaceName(rootID string) string {
	return "root-" + rootID
}

// UpsertRows writes documents to a namespace.
func (t *TPClient) UpsertRows(rootID string, rows []map[string]any, distanceMetric string) error {
	ns := namespaceName(rootID)
	body := map[string]any{
		"upsert_rows":    rows,
		"distance_metric": distanceMetric,
		"schema": map[string]any{
			"content": map[string]any{
				"type":             "string",
				"full_text_search": true,
			},
			"file_path":    map[string]any{"type": "string"},
			"chunk_index":  map[string]any{"type": "uint"},
			"content_hash": map[string]any{"type": "string"},
			"file_type":    map[string]any{"type": "string"},
			"page_number":  map[string]any{"type": "uint"},
			"image_path":   map[string]any{"type": "string"},
			"root_id":      map[string]any{"type": "string"},
		},
	}
	_, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s", ns), body)
	return err
}

// DeleteByFilter deletes documents matching a filter.
func (t *TPClient) DeleteByFilter(rootID string, filter any) error {
	ns := namespaceName(rootID)
	body := map[string]any{
		"delete_by_filter": filter,
	}
	_, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s", ns), body)
	return err
}

// DeleteIDs deletes documents by their IDs.
func (t *TPClient) DeleteIDs(rootID string, ids []string) error {
	ns := namespaceName(rootID)
	body := map[string]any{
		"deletes": ids,
	}
	_, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s", ns), body)
	return err
}

// Query performs a search query.
func (t *TPClient) Query(rootID string, rankBy any, limit int, filters any, includeAttrs []string) ([]map[string]any, error) {
	ns := namespaceName(rootID)
	body := map[string]any{
		"rank_by":            rankBy,
		"limit":              limit,
		"include_attributes": includeAttrs,
	}
	if filters != nil {
		body["filters"] = filters
	}
	resp, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s/query", ns), body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parsing query response: %w", err)
	}
	return result.Rows, nil
}

// MultiQuery performs multiple queries (for hybrid search) via the /query endpoint.
func (t *TPClient) MultiQuery(rootID string, queries []map[string]any) ([][]map[string]any, error) {
	ns := namespaceName(rootID)
	body := map[string]any{
		"queries": queries,
	}
	resp, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s/query", ns), body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Results []struct {
			Rows []map[string]any `json:"rows"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parsing multi_query response: %w", err)
	}

	var allRows [][]map[string]any
	for _, r := range result.Results {
		allRows = append(allRows, r.Rows)
	}
	return allRows, nil
}

func (t *TPClient) request(method, path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := t.baseURL() + path
	req, err := http.NewRequest(method, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("turbopuffer HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// HybridSearch performs a hybrid BM25+vector search with reciprocal rank fusion.
func (t *TPClient) HybridSearch(rootID string, queryText string, queryVector []float64, topK int, globFilter string) ([]map[string]any, error) {
	includeAttrs := []string{"content", "file_path", "chunk_index", "file_type", "page_number", "image_path"}

	queries := []map[string]any{
		{
			"rank_by":            []any{"vector", "ANN", queryVector},
			"limit":              topK,
			"include_attributes": includeAttrs,
		},
		{
			"rank_by":            []any{"content", "BM25", queryText},
			"limit":              topK,
			"include_attributes": includeAttrs,
		},
	}

	// Add glob filter if specified
	if globFilter != "" {
		for i := range queries {
			queries[i]["filters"] = []any{"file_path", "Glob", globFilter}
		}
	}

	resultSets, err := t.MultiQuery(rootID, queries)
	if err != nil {
		return nil, err
	}

	return reciprocalRankFusion(resultSets, 60), nil
}

// reciprocalRankFusion merges multiple ranked result lists.
func reciprocalRankFusion(resultSets [][]map[string]any, k int) []map[string]any {
	scores := make(map[string]float64)
	docs := make(map[string]map[string]any)

	for _, results := range resultSets {
		for rank, doc := range results {
			id := fmt.Sprintf("%v", doc["id"])
			scores[id] += 1.0 / float64(k+rank+1)
			docs[id] = doc
		}
	}

	// Sort by score descending
	type scored struct {
		id    string
		score float64
	}
	var sorted []scored
	for id, score := range scores {
		sorted = append(sorted, scored{id, score})
	}
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].score > sorted[i].score {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	var results []map[string]any
	for _, s := range sorted {
		doc := docs[s.id]
		doc["$dist"] = s.score
		results = append(results, doc)
	}
	return results
}

func init() {
	// Ensure we have the env vars
	_ = os.Getenv("TURBOPUFFER_API_KEY")
}
