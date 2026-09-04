package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

func whoamiCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the currently authenticated PufferFS identity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return runWhoami(cfg, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print identity as JSON")
	return cmd
}

func runWhoami(cfg *appconfig.Config, jsonOut bool) error {
	if strings.TrimSpace(cfg.Server.URL) == "" {
		return fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}
	raw, err := newAPIClient(cfg).get("/auth/me")
	if err != nil {
		return fmt.Errorf("checking authenticated identity: %w", err)
	}
	if jsonOut {
		return writeRawJSONLine(os.Stdout, raw)
	}

	var identity models.AuthMeResponse
	if err := json.Unmarshal(raw, &identity); err != nil {
		return fmt.Errorf("parsing authenticated identity: %w", err)
	}
	writeKV(os.Stdout, "email", identity.User.Email)
	writeKV(os.Stdout, "user_id", identity.User.ID)
	writeKV(os.Stdout, "org_id", identity.OrgID)
	writeKV(os.Stdout, "role", identity.Role)
	writeKV(os.Stdout, "scopes", formatIdentityScopes(identity.Scopes))
	return nil
}

func formatIdentityScopes(scopes []string) string {
	if scopes == nil {
		return "unknown (server does not report credential scopes)"
	}
	if len(scopes) == 0 {
		return "unrestricted"
	}
	return strings.Join(scopes, ",")
}
