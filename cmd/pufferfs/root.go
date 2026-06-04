package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/spf13/cobra"
)

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "root",
		Short: "Manage synced roots",
	}
	cmd.AddCommand(rootDeleteCmd())
	return cmd
}

func rootDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <root-id-or-name>",
		Short: "Delete a synced root and its PufferFS artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return runRootDelete(cfg, args[0], yes)
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
	client := newAPIClient(cfg)

	rootID := rootRef
	if !isUUID(rootRef) {
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
