package main

import (
	"fmt"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/spf13/cobra"
)

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize PufferFs configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := &appconfig.Config{
				Server: appconfig.ServerConfig{
					URL: "http://localhost:8080",
				},
				Turbopuffer: appconfig.TurbopufferConfig{
					Region: "gcp-us-central1",
				},
				Storage: appconfig.StorageConfig{
					Bucket: "pufferfs",
				},
			}

			if err := appconfig.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Printf("Config written to %s\n", appconfig.ConfigPath())
			fmt.Println("Edit the file to add your API keys and server URL.")
			return nil
		},
	}
}
