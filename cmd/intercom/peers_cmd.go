package main

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/paths"
	"github.com/dpemmons/intercom/internal/shim"
)

func newPeersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "peers",
		Short: "Print the names of currently-connected peers",
		Long: `Connects to the broker as a transient peer named "intercom-peers", asks list_peers, prints the result, and disconnects.

Skips Claude Code entirely — useful for debugging connectivity.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sock, err := paths.Socket()
			if err != nil {
				return err
			}

			c := shim.NewClient(shim.ClientOptions{
				Name:       "intercom-peers",
				SocketPath: sock,
				// BrokerBin defaults to os.Executable() — same behavior as
				// the shim auto-spawn path.
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			defer c.Close()

			peers, err := c.ListPeers(cmd.Context())
			if err != nil {
				return err
			}
			if len(peers) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no other peers connected)")
				return nil
			}
			for _, p := range peers {
				fmt.Fprintln(cmd.OutOrStdout(), p)
			}
			return nil
		},
	}
}
