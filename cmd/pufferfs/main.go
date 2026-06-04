// PufferFs CLI — sync, query, and watch your filesystem.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:     "pufferfs",
		Short:   "Hybrid search for your filesystem",
		Version: version,
	}

	root.AddCommand(syncCmd())
	root.AddCommand(queryCmd())
	root.AddCommand(watchCmd())
	root.AddCommand(serviceCmd())
	root.AddCommand(initCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
