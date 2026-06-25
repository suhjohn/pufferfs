package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

type adminOptions struct {
	AdminKey string
	JSON     bool
}

func adminCmd() *cobra.Command {
	options := &adminOptions{}
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Platform admin provisioning commands",
		Long: strings.TrimSpace(`Provision organizations, groups, restricted roots, and root grants.

Admin commands call /admin/* routes and require the platform admin key. Pass it
with --admin-key, PUFFERFS_ADMIN_API_KEY, PUFFERFS_ADMIN_KEY, or initialize the
CLI with the admin key intentionally.`),
	}
	cmd.PersistentFlags().StringVar(&options.AdminKey, "admin-key", "", "Platform admin API key")
	cmd.PersistentFlags().BoolVar(&options.JSON, "json", false, "Print raw JSON responses")
	cmd.AddCommand(adminGroupCmd(options), adminRootCmd(options))
	return cmd
}

func adminGroupCmd(options *adminOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group",
		Short: "Manage provisioned groups",
	}
	cmd.AddCommand(
		adminGroupCreateCmd(options),
		adminGroupListCmd(options),
		adminGroupMembersCmd(options),
		adminGroupMemberCmd(options),
	)
	return cmd
}

func adminGroupCreateCmd(options *adminOptions) *cobra.Command {
	var orgID, groupID, name, externalID string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create or update a group",
		Example: strings.TrimSpace(`  pufferfs admin group create --org org_acme --name Finance
  pufferfs admin group create --org org_acme --id group_finance --name Finance --external-id okta-finance`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireFlags(map[string]string{"--org": orgID, "--name": name}); err != nil {
				return err
			}
			client, err := newAdminClient(options)
			if err != nil {
				return err
			}
			payload := map[string]string{
				"id":          strings.TrimSpace(groupID),
				"name":        strings.TrimSpace(name),
				"external_id": strings.TrimSpace(externalID),
			}
			raw, err := client.post("/admin/orgs/"+url.PathEscape(orgID)+"/groups", payload)
			if err != nil {
				return fmt.Errorf("creating group: %w", err)
			}
			return writeAdminResponse(raw, options.JSON, func() error {
				var group models.Group
				if err := json.Unmarshal(raw, &group); err != nil {
					return err
				}
				fmt.Printf("Group %s (%s)\n", group.Name, group.ID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID")
	cmd.Flags().StringVar(&groupID, "id", "", "Stable group ID")
	cmd.Flags().StringVar(&name, "name", "", "Group name")
	cmd.Flags().StringVar(&externalID, "external-id", "", "External identity-provider group ID")
	return cmd
}

func adminGroupListCmd(options *adminOptions) *cobra.Command {
	var orgID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List groups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireFlags(map[string]string{"--org": orgID}); err != nil {
				return err
			}
			client, err := newAdminClient(options)
			if err != nil {
				return err
			}
			raw, err := client.get("/admin/orgs/" + url.PathEscape(orgID) + "/groups")
			if err != nil {
				return fmt.Errorf("listing groups: %w", err)
			}
			return writeAdminResponse(raw, options.JSON, func() error {
				var groups []models.Group
				if err := json.Unmarshal(raw, &groups); err != nil {
					return err
				}
				for _, group := range groups {
					fmt.Printf("%s\t%s\t%s\n", group.ID, group.Name, group.ExternalID)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID")
	return cmd
}

func adminGroupMembersCmd(options *adminOptions) *cobra.Command {
	var orgID, groupID string
	cmd := &cobra.Command{
		Use:   "members",
		Short: "List group members",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireFlags(map[string]string{"--org": orgID, "--group": groupID}); err != nil {
				return err
			}
			client, err := newAdminClient(options)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/admin/orgs/%s/groups/%s/members", url.PathEscape(orgID), url.PathEscape(groupID))
			raw, err := client.get(path)
			if err != nil {
				return fmt.Errorf("listing group members: %w", err)
			}
			return writeAdminResponse(raw, options.JSON, func() error {
				var members []models.GroupMember
				if err := json.Unmarshal(raw, &members); err != nil {
					return err
				}
				for _, member := range members {
					fmt.Printf("%s\t%s\t%s\n", member.UserID, member.Email, member.Name)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID")
	cmd.Flags().StringVar(&groupID, "group", "", "Group ID")
	return cmd
}

func adminGroupMemberCmd(options *adminOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "member",
		Short: "Add or remove group members",
	}
	cmd.AddCommand(adminGroupMemberAddCmd(options), adminGroupMemberRemoveCmd(options))
	return cmd
}

func adminGroupMemberAddCmd(options *adminOptions) *cobra.Command {
	return adminGroupMemberActionCmd(options, "add", "Add a user to a group", func(client *apiClient, orgID, groupID, userID string) ([]byte, error) {
		path := fmt.Sprintf("/admin/orgs/%s/groups/%s/members/%s", url.PathEscape(orgID), url.PathEscape(groupID), url.PathEscape(userID))
		return client.put(path, nil)
	})
}

func adminGroupMemberRemoveCmd(options *adminOptions) *cobra.Command {
	return adminGroupMemberActionCmd(options, "remove", "Remove a user from a group", func(client *apiClient, orgID, groupID, userID string) ([]byte, error) {
		path := fmt.Sprintf("/admin/orgs/%s/groups/%s/members/%s", url.PathEscape(orgID), url.PathEscape(groupID), url.PathEscape(userID))
		return client.delete(path)
	})
}

func adminGroupMemberActionCmd(options *adminOptions, use, short string, action func(*apiClient, string, string, string) ([]byte, error)) *cobra.Command {
	var orgID, groupID, userID string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireFlags(map[string]string{"--org": orgID, "--group": groupID, "--user": userID}); err != nil {
				return err
			}
			client, err := newAdminClient(options)
			if err != nil {
				return err
			}
			raw, err := action(client, orgID, groupID, userID)
			if err != nil {
				return fmt.Errorf("%s group member: %w", use, err)
			}
			return writeAdminResponse(raw, options.JSON, func() error {
				if use == "remove" {
					fmt.Printf("Removed %s from %s\n", userID, groupID)
				} else {
					fmt.Printf("Added %s to %s\n", userID, groupID)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID")
	cmd.Flags().StringVar(&groupID, "group", "", "Group ID")
	cmd.Flags().StringVar(&userID, "user", "", "User ID")
	return cmd
}

func adminRootCmd(options *adminOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "root",
		Short: "Provision roots and root grants",
	}
	cmd.AddCommand(adminRootCreateCmd(options), adminRootGrantCmd(options))
	return cmd
}

func adminRootCreateCmd(options *adminOptions) *cobra.Command {
	var orgID, name, sourcePath, scope, ownerUserID string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a root in an organization",
		Example: strings.TrimSpace(`  pufferfs admin root create --org org_acme --name finance --source-path /tenant/finance --scope restricted
  pufferfs admin root create --org org_acme --name alice --source-path /tenant/alice --scope user --owner-user user_alice`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireFlags(map[string]string{"--org": orgID, "--name": name, "--source-path": sourcePath}); err != nil {
				return err
			}
			client, err := newAdminClient(options)
			if err != nil {
				return err
			}
			payload := map[string]string{
				"name":          strings.TrimSpace(name),
				"source_path":   strings.TrimSpace(sourcePath),
				"scope":         strings.TrimSpace(scope),
				"owner_user_id": strings.TrimSpace(ownerUserID),
			}
			raw, err := client.post("/admin/orgs/"+url.PathEscape(orgID)+"/roots", payload)
			if err != nil {
				return fmt.Errorf("creating root: %w", err)
			}
			return writeAdminResponse(raw, options.JSON, func() error {
				var root models.RootMetadata
				if err := json.Unmarshal(raw, &root); err != nil {
					return err
				}
				fmt.Printf("Root %s (%s) scope=%s\n", root.Name, root.ID, root.Scope)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID")
	cmd.Flags().StringVar(&name, "name", "", "Root name")
	cmd.Flags().StringVar(&sourcePath, "source-path", "", "Original source path")
	cmd.Flags().StringVar(&scope, "scope", models.RootScopeRestricted, "Root scope: org, user, or restricted")
	cmd.Flags().StringVar(&ownerUserID, "owner-user", "", "Owner user ID for user-scoped roots")
	return cmd
}

func adminRootGrantCmd(options *adminOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Manage root grants",
	}
	cmd.AddCommand(adminRootGrantCreateCmd(options), adminRootGrantListCmd(options), adminRootGrantDeleteCmd(options))
	return cmd
}

func adminRootGrantCreateCmd(options *adminOptions) *cobra.Command {
	var orgID, rootID, principalType, principalID string
	var permissions []string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create or update a root grant",
		Example: strings.TrimSpace(`  pufferfs admin root grant create --org org_acme --root root_123 --principal-type group --principal-id group_finance --permission read
  pufferfs admin root grant create --org org_acme --root root_123 --principal-type user --principal-id user_alice --permission read --permission sync`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireFlags(map[string]string{"--org": orgID, "--root": rootID, "--principal-type": principalType, "--principal-id": principalID}); err != nil {
				return err
			}
			if len(permissions) == 0 {
				return fmt.Errorf("--permission is required")
			}
			client, err := newAdminClient(options)
			if err != nil {
				return err
			}
			payload := map[string]any{
				"principal_type": strings.TrimSpace(principalType),
				"principal_id":   strings.TrimSpace(principalID),
				"permissions":    permissions,
			}
			path := fmt.Sprintf("/admin/orgs/%s/roots/%s/grants", url.PathEscape(orgID), url.PathEscape(rootID))
			raw, err := client.post(path, payload)
			if err != nil {
				return fmt.Errorf("creating root grant: %w", err)
			}
			return writeAdminResponse(raw, options.JSON, func() error {
				var grant models.RootGrant
				if err := json.Unmarshal(raw, &grant); err != nil {
					return err
				}
				fmt.Printf("Grant %s %s:%s -> %s\n", grant.ID, grant.PrincipalType, grant.PrincipalID, strings.Join(grant.Permissions, ","))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID")
	cmd.Flags().StringVar(&rootID, "root", "", "Root ID")
	cmd.Flags().StringVar(&principalType, "principal-type", "", "Principal type: org, user, or group")
	cmd.Flags().StringVar(&principalID, "principal-id", "", "Principal ID")
	cmd.Flags().StringArrayVar(&permissions, "permission", nil, "Root permission: read, sync, delete, or admin; can be repeated")
	return cmd
}

func adminRootGrantListCmd(options *adminOptions) *cobra.Command {
	var orgID, rootID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List root grants",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireFlags(map[string]string{"--org": orgID, "--root": rootID}); err != nil {
				return err
			}
			client, err := newAdminClient(options)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/admin/orgs/%s/roots/%s/grants", url.PathEscape(orgID), url.PathEscape(rootID))
			raw, err := client.get(path)
			if err != nil {
				return fmt.Errorf("listing root grants: %w", err)
			}
			return writeAdminResponse(raw, options.JSON, func() error {
				var grants []models.RootGrant
				if err := json.Unmarshal(raw, &grants); err != nil {
					return err
				}
				for _, grant := range grants {
					fmt.Printf("%s\t%s:%s\t%s\n", grant.ID, grant.PrincipalType, grant.PrincipalID, strings.Join(grant.Permissions, ","))
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID")
	cmd.Flags().StringVar(&rootID, "root", "", "Root ID")
	return cmd
}

func adminRootGrantDeleteCmd(options *adminOptions) *cobra.Command {
	var orgID, rootID, grantID string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a root grant",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireFlags(map[string]string{"--org": orgID, "--root": rootID, "--grant": grantID}); err != nil {
				return err
			}
			client, err := newAdminClient(options)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/admin/orgs/%s/roots/%s/grants/%s", url.PathEscape(orgID), url.PathEscape(rootID), url.PathEscape(grantID))
			raw, err := client.delete(path)
			if err != nil {
				return fmt.Errorf("deleting root grant: %w", err)
			}
			return writeAdminResponse(raw, options.JSON, func() error {
				fmt.Printf("Deleted grant %s\n", grantID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&orgID, "org", "", "Organization ID")
	cmd.Flags().StringVar(&rootID, "root", "", "Root ID")
	cmd.Flags().StringVar(&grantID, "grant", "", "Grant ID")
	return cmd
}

func newAdminClient(options *adminOptions) (*apiClient, error) {
	cfg, err := appconfig.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("server URL not configured; run 'pufferfs init' first or set PUFFERFS_SERVER_URL")
	}
	adminKey := strings.TrimSpace(options.AdminKey)
	if adminKey == "" {
		adminKey = strings.TrimSpace(os.Getenv("PUFFERFS_ADMIN_API_KEY"))
	}
	if adminKey == "" {
		adminKey = strings.TrimSpace(os.Getenv("PUFFERFS_ADMIN_KEY"))
	}
	if adminKey == "" {
		adminKey = cfg.Server.APIKey
	}
	if strings.TrimSpace(adminKey) == "" {
		return nil, fmt.Errorf("admin key not configured; pass --admin-key or set PUFFERFS_ADMIN_API_KEY")
	}
	adminCfg := *cfg
	adminCfg.Server.APIKey = adminKey
	return newAPIClient(&adminCfg), nil
}

func writeAdminResponse(raw []byte, jsonOut bool, human func() error) error {
	if jsonOut {
		return writeRawJSONLine(os.Stdout, raw)
	}
	return human()
}

func requireFlags(values map[string]string) error {
	for name, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}
