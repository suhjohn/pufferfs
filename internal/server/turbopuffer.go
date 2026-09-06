package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// TPClient talks to the Turbopuffer API.
type TPClient struct {
	apiKey       string
	region       string
	httpClient   *http.Client
	baseOverride string
}

type tpHTTPError struct {
	StatusCode int
	Body       string
}

func (e *tpHTTPError) Error() string {
	return fmt.Sprintf("turbopuffer HTTP %d: %s", e.StatusCode, e.Body)
}

func isTPNotFound(err error) bool {
	var httpErr *tpHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusNotFound
	}
	return err != nil && strings.Contains(err.Error(), "turbopuffer HTTP 404:")
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

func (t *TPClient) baseURL() string {
	if t.baseOverride != "" {
		return t.baseOverride
	}
	return "https://api.turbopuffer.com"
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

// TPQuery is the index's wire contract. Attribute inclusion and exclusion are
// mutually exclusive; exclusion avoids fetching vectors and internal metadata.
type TPQuery struct {
	RankBy            any      `json:"rank_by"`
	Limit             int      `json:"limit"`
	Filters           any      `json:"filters,omitempty"`
	IncludeAttributes []string `json:"include_attributes,omitempty"`
	ExcludeAttributes []string `json:"exclude_attributes,omitempty"`
}

// Query performs a search query.
func (t *TPClient) Query(ctx context.Context, ns string, query TPQuery) ([]map[string]any, error) {
	resp, err := t.requestContext(ctx, "POST", fmt.Sprintf("/v2/namespaces/%s/query", ns), query)
	if err != nil {
		if isTPNotFound(err) {
			return nil, nil
		}
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
func (t *TPClient) MultiQuery(ctx context.Context, ns string, queries []TPQuery) ([][]map[string]any, error) {
	body := map[string]any{
		"queries": queries,
	}
	resp, err := t.requestContext(ctx, "POST", fmt.Sprintf("/v2/namespaces/%s/query", ns), body)
	if err != nil {
		if isTPNotFound(err) {
			return make([][]map[string]any, len(queries)), nil
		}
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
	if len(result.Results) != len(queries) {
		return nil, errors.New("index returned an incomplete multi-query response")
	}

	var allRows [][]map[string]any
	for _, r := range result.Results {
		allRows = append(allRows, r.Rows)
	}
	return allRows, nil
}

func (t *TPClient) requestContext(ctx context.Context, method, path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := t.baseURL() + path
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := t.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}
		lastErr = &tpHTTPError{StatusCode: resp.StatusCode, Body: string(respBody)}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, lastErr
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
