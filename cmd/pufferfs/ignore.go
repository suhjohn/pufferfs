package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

func ignorePolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ignore",
		Short: "Manage server-side ignore policies",
	}
	cmd.AddCommand(ignoreGetCmd("get"), ignoreGetCmd("show"), ignoreSetCmd(), ignoreEditCmd())
	return cmd
}

func ignoreGetCmd(use string) *cobra.Command {
	var level string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   use,
		Short: "Show ignore policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return runIgnoreGet(cfg, level, jsonOut, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&level, "level", "effective", "Policy level: effective, org, or user")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print raw policy JSON")
	return cmd
}

func ignoreSetCmd() *cobra.Command {
	var level string
	var filePath string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set an org or user ignore policy from a file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return runIgnoreSet(cfg, level, filePath, os.Stdin, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&level, "level", "user", "Policy level: org or user")
	cmd.Flags().StringVar(&filePath, "file", "", "Read policy patterns from this file, or '-' for stdin")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func ignoreEditCmd() *cobra.Command {
	var level string
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "Edit an org or user ignore policy in $EDITOR",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return runIgnoreEdit(cfg, level, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&level, "level", "user", "Policy level: org or user")
	return cmd
}

func runIgnoreGet(cfg *appconfig.Config, level string, jsonOut bool, w io.Writer) error {
	client, err := configuredAPIClient(cfg)
	if err != nil {
		return err
	}
	level, err = normalizeIgnoreLevel(level, true)
	if err != nil {
		return err
	}
	switch level {
	case "effective":
		policy, err := fetchEffectiveIgnorePolicy(client)
		if err != nil {
			return fmt.Errorf("loading effective ignore policy: %w", err)
		}
		if jsonOut {
			return writePrettyJSON(w, policy)
		}
		writeEffectiveIgnorePolicy(w, policy)
		return nil
	case "org", "user":
		policy, err := fetchIgnorePolicy(client, level)
		if err != nil {
			return fmt.Errorf("loading %s ignore policy: %w", level, err)
		}
		if jsonOut {
			return writePrettyJSON(w, policy)
		}
		writeIgnorePolicyText(w, policy.Patterns)
		return nil
	default:
		return fmt.Errorf("unsupported ignore policy level %q", level)
	}
}

func runIgnoreSet(cfg *appconfig.Config, level, filePath string, stdin io.Reader, w io.Writer) error {
	client, err := configuredAPIClient(cfg)
	if err != nil {
		return err
	}
	level, err = normalizeIgnoreLevel(level, false)
	if err != nil {
		return err
	}
	patterns, err := readIgnorePolicyInput(filePath, stdin)
	if err != nil {
		return err
	}
	policy, err := updateIgnorePolicy(client, level, patterns)
	if err != nil {
		return fmt.Errorf("updating %s ignore policy: %w", level, err)
	}
	fmt.Fprintf(w, "Updated %s ignore policy at %s\n", level, policy.UpdatedAt.Format(time.RFC3339))
	return nil
}

func runIgnoreEdit(cfg *appconfig.Config, level string, w io.Writer) error {
	client, err := configuredAPIClient(cfg)
	if err != nil {
		return err
	}
	level, err = normalizeIgnoreLevel(level, false)
	if err != nil {
		return err
	}
	policy, err := fetchIgnorePolicy(client, level)
	if err != nil {
		return fmt.Errorf("loading %s ignore policy: %w", level, err)
	}
	tmp, err := os.CreateTemp("", fmt.Sprintf("pufferfs-%s-*.tpfsignore", level))
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(policy.Patterns); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return fmt.Errorf("EDITOR is empty")
	}
	args := append(parts[1:], tmpPath)
	cmd := exec.Command(parts[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running editor: %w", err)
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	updated, err := updateIgnorePolicy(client, level, string(data))
	if err != nil {
		return fmt.Errorf("updating %s ignore policy: %w", level, err)
	}
	fmt.Fprintf(w, "Updated %s ignore policy at %s\n", level, updated.UpdatedAt.Format(time.RFC3339))
	return nil
}

func configuredAPIClient(cfg *appconfig.Config) (*apiClient, error) {
	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("server URL not configured; run 'pufferfs init' first")
	}
	return newAPIClient(cfg), nil
}

func fetchEffectiveIgnorePolicy(client *apiClient) (*models.EffectiveIgnorePolicy, error) {
	raw, err := client.get("/ignore-policy")
	if err != nil {
		return nil, err
	}
	var policy models.EffectiveIgnorePolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

func fetchIgnorePolicy(client *apiClient, level string) (*models.IgnorePolicy, error) {
	raw, err := client.get("/ignore-policy/" + url.PathEscape(level))
	if err != nil {
		return nil, err
	}
	var policy models.IgnorePolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

func updateIgnorePolicy(client *apiClient, level, patterns string) (*models.IgnorePolicy, error) {
	raw, err := client.put("/ignore-policy/"+url.PathEscape(level), models.IgnorePolicyUpdateRequest{Patterns: patterns})
	if err != nil {
		return nil, err
	}
	var policy models.IgnorePolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

func normalizeIgnoreLevel(level string, allowEffective bool) (string, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		if allowEffective {
			return "effective", nil
		}
		return "user", nil
	}
	switch level {
	case "user", "org":
		return level, nil
	case "effective":
		if allowEffective {
			return level, nil
		}
	}
	if allowEffective {
		return "", fmt.Errorf("level must be effective, org, or user")
	}
	return "", fmt.Errorf("level must be org or user")
}

func readIgnorePolicyInput(filePath string, stdin io.Reader) (string, error) {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return "", fmt.Errorf("--file is required")
	}
	if filePath == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeEffectiveIgnorePolicy(w io.Writer, policy *models.EffectiveIgnorePolicy) {
	fmt.Fprintln(w, "# Organization ignore policy")
	writeIgnorePolicyText(w, policy.OrgPatterns)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# User ignore policy")
	writeIgnorePolicyText(w, policy.UserPatterns)
}

func writeIgnorePolicyText(w io.Writer, patterns string) {
	if strings.TrimSpace(patterns) == "" {
		fmt.Fprintln(w, "(empty)")
		return
	}
	fmt.Fprint(w, patterns)
	if !strings.HasSuffix(patterns, "\n") {
		fmt.Fprintln(w)
	}
}
