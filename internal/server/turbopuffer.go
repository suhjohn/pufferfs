package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

// TPClient talks to the Turbopuffer API.
type TPClient struct {
	apiKey       string
	region       string
	httpClient   *http.Client
	baseOverride string
}

// NewTPClient creates a Turbopuffer client.
// If TURBOPUFFER_API_URL is set, it overrides the default API URL.
func NewTPClient(apiKey, region string) *TPClient {
	if region == "" {
		region = "gcp-us-central1"
	}
	c := &TPClient{
		apiKey:     apiKey,
		region:     region,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
	if u := os.Getenv("TURBOPUFFER_API_URL"); u != "" {
		c.baseOverride = u
	}
	return c
}

// NewTPClientWithURL creates a TP client with a custom base URL (for testing).
func NewTPClientWithURL(apiKey, baseURL string) *TPClient {
	return &TPClient{
		apiKey:       apiKey,
		region:       "test",
		httpClient:   &http.Client{Timeout: 120 * time.Second},
		baseOverride: baseURL,
	}
}

func (t *TPClient) baseURL() string {
	if t.baseOverride != "" {
		return t.baseOverride
	}
	return "https://api.turbopuffer.com"
}

// namespaceName returns the tp namespace for a root.
func namespaceName(rootID string) string {
	return "root-" + rootID
}

// UpsertRows writes documents to a namespace.
func (t *TPClient) UpsertRows(ns string, rows []map[string]any, distanceMetric string) error {
	body := map[string]any{
		"upsert_rows":     rows,
		"distance_metric": distanceMetric,
		"schema": map[string]any{
			"content": map[string]any{
				"type":             "string",
				"full_text_search": true,
			},
			"file_path":                 map[string]any{"type": "string"},
			"chunk_index":               map[string]any{"type": "uint"},
			"content_hash":              map[string]any{"type": "string"},
			"file_hash":                 map[string]any{"type": "string"},
			"file_type":                 map[string]any{"type": "string"},
			"page_number":               map[string]any{"type": "uint"},
			"image_path":                map[string]any{"type": "string"},
			"root_id":                   map[string]any{"type": "string"},
			"generation_id":             map[string]any{"type": "string"},
			"valid_from_generation":     map[string]any{"type": "string"},
			"valid_from_generation_seq": map[string]any{"type": "uint"},
			"valid_to_generation":       map[string]any{"type": "string"},
			"valid_to_generation_seq":   map[string]any{"type": "uint"},
		},
	}
	_, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s", ns), body)
	return err
}

// PatchRows updates attributes on existing documents without replacing vectors.
func (t *TPClient) PatchRows(ns string, rows []map[string]any) error {
	body := map[string]any{
		"patch_rows": rows,
	}
	_, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s", ns), body)
	return err
}

// DeleteByFilter deletes documents that match a Turbopuffer filter.
func (t *TPClient) DeleteByFilter(ns string, filters any, allowPartial bool) (bool, error) {
	body := map[string]any{
		"delete_by_filter": filters,
	}
	if allowPartial {
		body["delete_by_filter_allow_partial"] = true
	}
	resp, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s", ns), body)
	if err != nil {
		return false, err
	}
	var result struct {
		RowsRemaining bool `json:"rows_remaining"`
	}
	if len(resp) > 0 {
		_ = json.Unmarshal(resp, &result)
	}
	return result.RowsRemaining, nil
}

func (t *TPClient) DeleteNamespace(ns string) error {
	url := t.baseURL() + fmt.Sprintf("/v2/namespaces/%s", ns)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodDelete, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+t.apiKey)

		resp, err := t.httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("turbopuffer HTTP %d: %s", resp.StatusCode, string(respBody))
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return lastErr
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return lastErr
}

// PatchByFilter updates attributes on documents that match a Turbopuffer filter.
func (t *TPClient) PatchByFilter(ns string, filters any, patch map[string]any, allowPartial bool) (bool, int, error) {
	body := map[string]any{
		"patch_by_filter": map[string]any{
			"filters": filters,
			"patch":   patch,
		},
	}
	if allowPartial {
		body["patch_by_filter_allow_partial"] = true
	}
	resp, err := t.request("POST", fmt.Sprintf("/v2/namespaces/%s", ns), body)
	if err != nil {
		return false, 0, err
	}
	var result struct {
		RowsRemaining bool `json:"rows_remaining"`
		RowsAffected  int  `json:"rows_affected"`
		RowsPatched   int  `json:"rows_patched"`
		Count         int  `json:"count"`
	}
	if len(resp) > 0 {
		_ = json.Unmarshal(resp, &result)
	}
	affected := result.RowsAffected
	if affected == 0 {
		affected = result.RowsPatched
	}
	if affected == 0 {
		affected = result.Count
	}
	return result.RowsRemaining, affected, nil
}

// Query performs a search query.
func (t *TPClient) Query(ns string, rankBy any, limit int, filters any, includeAttrs []string) ([]map[string]any, error) {
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
func (t *TPClient) MultiQuery(ns string, queries []map[string]any) ([][]map[string]any, error) {
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
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(method, url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := t.httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}
		lastErr = fmt.Errorf("turbopuffer HTTP %d: %s", resp.StatusCode, string(respBody))
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return nil, lastErr
}

// HybridSearch performs a hybrid BM25+vector search with reciprocal rank fusion.
func (t *TPClient) HybridSearch(ns string, queryText string, queryVector []float64, topK int, filters any) ([]map[string]any, error) {
	includeAttrs := []string{"content", "file_path", "absolute_path", "chunk_index", "content_hash", "file_hash", "file_type", "page_number", "image_path", "generation_id", "valid_from_generation", "valid_from_generation_seq", "valid_to_generation", "valid_to_generation_seq"}

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

	if filters != nil {
		for i := range queries {
			queries[i]["filters"] = filters
		}
	}

	resultSets, err := t.MultiQuery(ns, queries)
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
	ranked := make([]scored, 0, len(scores))
	for id, score := range scores {
		ranked = append(ranked, scored{id, score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	var results []map[string]any
	for _, s := range ranked {
		doc := docs[s.id]
		doc["$dist"] = s.score
		results = append(results, doc)
	}
	return results
}
