package main

import (
	"encoding/json"
	"fmt"
	"strings"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

func queryCmd() *cobra.Command {
	var (
		mode   string
		glob   string
		rootID string
		topK   int
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

			return runQuery(cfg, queryText, mode, glob, rootID, topK)
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "hybrid", "Search mode: fts, vector, or hybrid")
	cmd.Flags().StringVar(&glob, "glob", "", "Filter results by file path glob")
	cmd.Flags().StringVar(&rootID, "root", "", "Root ID or name to search")
	cmd.Flags().IntVar(&topK, "top-k", 10, "Number of results to return")

	return cmd
}

func runQuery(cfg *appconfig.Config, queryText, mode, glob, rootID string, topK int) error {
	client := newAPIClient(cfg)

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

	var resp models.QueryResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	if len(resp.Results) == 0 {
		fmt.Println("No results found.")
		return nil
	}

	for i, r := range resp.Results {
		fmt.Printf("\n--- Result %d (score: %.4f) ---\n", i+1, r.Score)
		fmt.Printf("File: %s", r.FilePath)
		if r.PageNumber != nil {
			fmt.Printf(" (page %d)", *r.PageNumber)
		}
		fmt.Printf(" [chunk %d]\n", r.ChunkIndex)
		fmt.Printf("Type: %s\n", r.FileType)

		// Show content preview (first 500 chars)
		content := r.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		fmt.Println(content)
	}

	return nil
}

func detectRootFromCwd() (string, error) {
	// TODO: walk ~/.tpfs/roots/ and match source_path against cwd
	return "", fmt.Errorf("auto-detect not yet implemented")
}
