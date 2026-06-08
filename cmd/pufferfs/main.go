// PufferFs CLI - sync, query, and search your filesystem.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"
var commit = ""
var date = ""

func main() {
	root := &cobra.Command{
		Use:     "pufferfs",
		Short:   "Hybrid search for your filesystem",
		Version: version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return checkCLICompatibility(cmd)
		},
	}

	root.AddCommand(syncCmd())
	root.AddCommand(queryCmd())
	root.AddCommand(watchCmd())
	root.AddCommand(rootCmd())
	root.AddCommand(serviceCmd())
	root.AddCommand(initCmd())
	root.AddCommand(upgradeCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
