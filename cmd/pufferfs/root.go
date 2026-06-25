package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "root",
		Short: "Manage synced roots",
	}
	cmd.AddCommand(rootListCmd(), rootCurrentCmd(), rootDeleteCmd())
	return cmd
}

func rootListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List accessible synced roots",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return runRootList(cfg, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print raw roots JSON")
	return cmd
}

func rootCurrentCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "current",
		Short: "Show the root for the current directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRootCurrent(jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print current root metadata as JSON")
	return cmd
}

func runRootList(cfg *appconfig.Config, jsonOut bool) error {
	if cfg.Server.URL == "" {
		return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}
	client := newAPIClient(cfg)
	raw, err := client.get("/roots")
	if err != nil {
		return fmt.Errorf("listing roots: %w", err)
	}
	if jsonOut {
		return writeRawJSONLine(os.Stdout, raw)
	}

	var roots []models.RootMetadata
	if err := json.Unmarshal(raw, &roots); err != nil {
		return fmt.Errorf("parsing roots: %w", err)
	}
	writeRootList(os.Stdout, roots)
	return nil
}

func runRootCurrent(jsonOut bool) error {
	rootID, err := detectRootFromCwd()
	if err != nil {
		return fmt.Errorf("detecting current root: %w", err)
	}
	meta, err := loadRootMeta(rootID)
	if err != nil {
		return fmt.Errorf("loading current root metadata: %w", err)
	}
	if jsonOut {
		return writePrettyJSON(os.Stdout, meta)
	}
	writeRootMeta(os.Stdout, meta)
	return nil
}

func writeRootList(w io.Writer, roots []models.RootMetadata) {
	if len(roots) == 0 {
		fmt.Fprintln(w, "No roots found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tSCOPE\tACCESS\tGENERATION\tSOURCE_PATH")
	for _, root := range roots {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			root.Name,
			root.ID,
			root.Scope,
			formatRootAccess(root.Access),
			formatGeneration(root.VisibleGenerationID, root.VisibleGenerationSeq),
			root.SourcePath,
		)
	}
	_ = tw.Flush()
}

func formatRootAccess(access []string) string {
	if len(access) == 0 {
		return "-"
	}
	return strings.Join(access, ",")
}

func writeRootMeta(w io.Writer, meta *rootMeta) {
	writeKV(w, "id", meta.ID)
	writeKV(w, "name", meta.Name)
	writeKV(w, "source_path", meta.SourcePath)
	writeKV(w, "generation", formatGeneration(meta.GenerationID, meta.GenerationSeq))
}

func formatGeneration(id string, seq int64) string {
	if id == "" {
		return "-"
	}
	if seq == 0 {
		return id
	}
	return fmt.Sprintf("%s/%d", id, seq)
}

func rootDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete [root-id-or-name]",
		Short: "Delete a synced root and its PufferFS artifacts",
		Long: strings.TrimSpace(`Delete a synced root and its PufferFS artifacts.

When ROOT is omitted, PufferFS deletes the root that contains the current
working directory. The confirmation prompt still requires the root ID unless
--yes is passed.

Root deletion removes PufferFS metadata, stored source copies, sync artifacts,
chunk/page artifacts, Turbopuffer namespaces, and the local PufferFS cache. It
does not delete the original source files.`),
		Example: strings.TrimSpace(`  pufferfs root delete
  pufferfs root delete --yes
  pufferfs root delete workspace
  pufferfs root delete 11111111-1111-1111-1111-111111111111`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			rootRef := ""
			if len(args) > 0 {
				rootRef = args[0]
			}
			return runRootDelete(cfg, rootRef, yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

type deleteRootResponse struct {
	Status           string `json:"status"`
	RootID           string `json:"root_id"`
	Name             string `json:"name"`
	TurbopufferNS    string `json:"turbopuffer_ns"`
	S3ObjectsDeleted int    `json:"s3_objects_deleted"`
}

func runRootDelete(cfg *appconfig.Config, rootRef string, yes bool) error {
	if cfg.Server.URL == "" {
		return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}
	rootRef = strings.TrimSpace(rootRef)
	detectedCurrentRoot := false
	if rootRef == "" {
		detectedRootID, err := detectRootFromCwd()
		if err != nil {
			return fmt.Errorf("could not detect root from cwd; specify a root id or name: %w", err)
		}
		rootRef = detectedRootID
		detectedCurrentRoot = true
	}
	client := newAPIClient(cfg)

	rootID := rootRef
	if !detectedCurrentRoot && !isUUID(rootRef) {
		resolvedID, err := resolveRootName(client, rootRef)
		if err != nil {
			return fmt.Errorf("resolving root %q: %w", rootRef, err)
		}
		rootID = resolvedID
	}
	root, err := loadRemoteRoot(client, rootID)
	if err != nil {
		return fmt.Errorf("loading remote root metadata: %w", err)
	}

	if !yes {
		if err := confirmRootDelete(root.ID, root.Name); err != nil {
			return err
		}
	}

	respBody, err := client.delete(fmt.Sprintf("/roots/%s", url.PathEscape(root.ID)))
	if err != nil {
		return fmt.Errorf("deleting root: %w", err)
	}

	var resp deleteRootResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("parsing delete response: %w", err)
	}

	localPath := appconfig.RootDir(root.ID)
	if err := os.RemoveAll(localPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to remove local cache %s: %v\n", localPath, err)
	}

	fmt.Printf("Deleted root %s (%s)\n", resp.Name, resp.RootID)
	fmt.Printf("Deleted Turbopuffer namespace: %s\n", resp.TurbopufferNS)
	fmt.Printf("Deleted %d storage objects\n", resp.S3ObjectsDeleted)
	return nil
}

func confirmRootDelete(rootID, name string) error {
	fmt.Fprintf(os.Stderr, "This deletes PufferFS copies, index rows, metadata, and local cache for root %q (%s).\n", name, rootID)
	fmt.Fprintln(os.Stderr, "It does not delete the original source files.")
	fmt.Fprintf(os.Stderr, "Type the root ID to confirm: ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil && input == "" {
		return err
	}
	if strings.TrimSpace(input) != rootID {
		return fmt.Errorf("confirmation did not match root ID; delete cancelled")
	}
	return nil
}
