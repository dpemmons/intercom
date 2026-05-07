// Command intercom is the all-in-one binary for the intercom system. It
// dispatches to one of: shim (per-Claude MCP server), broker (the local
// router), name (print the resolved peer name), or peers (list connected
// peers).
//
// See DESIGN.md at the repo root for the full architecture.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/shim"
)

// version is the binary version, overridable via -ldflags '-X main.version=...'.
var version = "0.1.0-dev"

// commit is the git SHA the binary was built from, overridable likewise.
var commit = "unknown"

func main() {
	// Make the shim package's reported version match the binary's.
	shim.Version = version

	root := &cobra.Command{
		Use:           "intercom",
		Short:         "Local-only chat bridge between Claude Code sessions",
		Long:          "intercom routes messages between two or more local Claude Code sessions over a Unix-socket broker. See `intercom --help` for subcommands.",
		Version:       fmt.Sprintf("%s (%s)", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newShimCmd(),
		newBrokerCmd(),
		newNameCmd(),
		newPeersCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "intercom:", err)
		os.Exit(1)
	}
}
