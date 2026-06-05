package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

func queryCmd() *cobra.Command {
	var (
		mode       string
		glob       string
		rootID     string
		topK       int
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "query [query text]",
		Short: "Search indexed content",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			queryText := strings.Join(args, " ")

			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			if cfg.Server.URL == "" {
				return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
			}

			// Auto-detect root if not specified
			if rootID == "" {
				rootID, err = detectRootFromCwd()
				if err != nil {
					return fmt.Errorf("could not detect root from cwd; use --root to specify: %w", err)
				}
			}

			return runQuery(cfg, queryText, mode, glob, rootID, topK, jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print raw query response as JSON")
	cmd.Flags().StringVar(&mode, "mode", "hybrid", "Search mode: fts, vector, or hybrid")
	cmd.Flags().StringVar(&glob, "glob", "", "Filter results by file path glob")
	cmd.Flags().StringVar(&rootID, "root", "", "Root ID or name to search")
	cmd.Flags().IntVar(&topK, "top-k", 10, "Number of results to return")

	return cmd
}

func runQuery(cfg *appconfig.Config, queryText, mode, glob, rootID string, topK int, jsonOutput bool) error {
	client := newAPIClient(cfg)

	// Resolve root name to ID if it's not a UUID
	if rootID != "" && !isUUID(rootID) {
		resolvedID, err := resolveRootName(client, rootID)
		if err != nil {
			return fmt.Errorf("resolving root %q: %w", rootID, err)
		}
		rootID = resolvedID
	}

	req := models.QueryRequest{
		Query:  queryText,
		Mode:   mode,
		RootID: rootID,
		Glob:   glob,
		TopK:   topK,
	}

	respBody, err := client.post("/query", req)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}

	if jsonOutput {
		return writeRawJSONLine(os.Stdout, respBody)
	}

	var resp models.QueryResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	writeQueryResults(os.Stdout, resp)
	return nil
}

func writeQueryResults(w io.Writer, resp models.QueryResponse) {
	if len(resp.Results) == 0 {
		fmt.Fprintln(w, "No results found.")
		return
	}

	for i, r := range resp.Results {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%d.\n", i+1)
		displayPath := r.FilePath
		if r.AbsolutePath != "" {
			displayPath = r.AbsolutePath
		}
		writeKV(w, "score", fmt.Sprintf("%.4f", r.Score))
		writeKV(w, "file", displayPath)
		if r.PageNumber != nil {
			writeKV(w, "page_number", *r.PageNumber)
		}
		writeKV(w, "chunk_index", r.ChunkIndex)
		writeKV(w, "file_type", r.FileType)
		if !writeJSONValueAsDotNotation(w, "content", r.Content) {
			writeKV(w, "content", r.Content)
		}
	}
}

func writeKV(w io.Writer, key string, value any) {
	fmt.Fprintf(w, "%s: %v\n", key, value)
}

func writeJSONValueAsDotNotation(w io.Writer, prefix, raw string) bool {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return false
	}
	pairs := flattenJSONValue(prefix, value)
	if len(pairs) == 0 {
		writeKV(w, prefix, raw)
		return true
	}
	for _, pair := range pairs {
		writeKV(w, pair.key, pair.value)
	}
	return true
}

type kvPair struct {
	key   string
	value any
}

func flattenJSONValue(prefix string, value any) []kvPair {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var pairs []kvPair
		for _, key := range keys {
			pairs = append(pairs, flattenJSONValue(prefix+"."+key, v[key])...)
		}
		return pairs
	case []any:
		var pairs []kvPair
		for i, item := range v {
			pairs = append(pairs, flattenJSONValue(fmt.Sprintf("%s[%d]", prefix, i), item)...)
		}
		return pairs
	default:
		return []kvPair{{key: prefix, value: v}}
	}
}

func detectRootFromCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	rootsDir := filepath.Join(appconfig.DefaultConfigDir(), "roots")
	entries, err := os.ReadDir(rootsDir)
	if err != nil {
		return "", fmt.Errorf("no roots found in %s", rootsDir)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metaPath := filepath.Join(rootsDir, entry.Name(), "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta struct {
			ID         string `json:"id"`
			SourcePath string `json:"source_path"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if cwd == meta.SourcePath || strings.HasPrefix(cwd, meta.SourcePath+string(filepath.Separator)) {
			return meta.ID, nil
		}
	}
	return "", fmt.Errorf("no root matches cwd %s", cwd)
}

func resolveRootName(client *apiClient, name string) (string, error) {
	respBody, err := client.get("/roots")
	if err != nil {
		return "", err
	}
	var roots []models.RootMetadata
	if err := json.Unmarshal(respBody, &roots); err != nil {
		return "", err
	}
	for _, r := range roots {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("root %q not found", name)
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
