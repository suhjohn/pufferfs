package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

func readCmd() *cobra.Command {
	var (
		rootID     string
		pages      string
		lines      string
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "read [file path]",
		Short: "Read exact pages or lines from an indexed file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if cfg.Server.URL == "" {
				return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
			}
			return runRead(cfg, args[0], rootID, pages, lines, jsonOutput)
		},
	}

	cmd.Flags().StringVar(&rootID, "root", "", "Root ID or name to read from")
	cmd.Flags().StringVar(&pages, "pages", "", "1-based inclusive page range, e.g. 10:20")
	cmd.Flags().StringVar(&lines, "lines", "", "1-based inclusive line range, e.g. 200:400")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print raw read response as JSON")
	return cmd
}

func runRead(cfg *appconfig.Config, filePath, rootID, pages, lines string, jsonOutput bool) error {
	if (strings.TrimSpace(pages) == "") == (strings.TrimSpace(lines) == "") {
		return fmt.Errorf("exactly one of --pages or --lines is required")
	}
	client := newAPIClient(cfg)
	if strings.TrimSpace(rootID) == "" {
		detected, err := detectRootFromCwd()
		if err != nil {
			return fmt.Errorf("could not detect root from cwd; use --root to specify: %w", err)
		}
		rootID = detected
	}
	if !isUUID(rootID) {
		resolvedID, err := resolveRootName(client, rootID)
		if err != nil {
			return fmt.Errorf("resolving root %q: %w", rootID, err)
		}
		rootID = resolvedID
	}

	req := models.ReadFileRequest{
		Path: filePath,
	}
	var err error
	if strings.TrimSpace(pages) != "" {
		req.Pages, err = parseReadRange(pages)
	} else {
		req.Lines, err = parseReadRange(lines)
	}
	if err != nil {
		return err
	}

	respBody, err := client.post(fmt.Sprintf("/roots/%s/read", rootID), req)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if jsonOutput {
		return writeRawJSONLine(os.Stdout, respBody)
	}

	var resp models.ReadFileResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	writeReadResults(os.Stdout, resp)
	return nil
}

func parseReadRange(raw string) (*models.ReadRange, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return nil, fmt.Errorf("range must use start:end")
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("invalid range start %q", parts[0])
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return nil, fmt.Errorf("invalid range end %q", parts[1])
	}
	if start <= 0 || end <= 0 || end < start {
		return nil, fmt.Errorf("range must be positive and end must be >= start")
	}
	return &models.ReadRange{Start: start, End: end}, nil
}

func writeReadResults(w io.Writer, resp models.ReadFileResponse) {
	switch resp.Mode {
	case "pages":
		for i, page := range resp.Pages {
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "page: %d\n", page.Page)
			if page.FileType != "" {
				writeKV(w, "file_type", page.FileType)
			}
			writeKV(w, "content", page.Content)
		}
	case "lines":
		for _, line := range resp.Lines {
			fmt.Fprintf(w, "%d\t%s\n", line.LineNumber, line.Content)
		}
	default:
		writeKV(w, "file", resp.FilePath)
	}
}
