package main

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/brokerclient"
	"github.com/dpemmons/intercom/internal/paths"
)

func newPeersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "peers",
		Short: "Prints the names of connected peers",
		Long: `intercom peers connects as the transient peer "intercom-peers", requests the peer list, prints it, and disconnects.

The command starts the broker when no broker is listening.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sock, err := paths.Socket()
			if err != nil {
				return err
			}
			brokerBin, err := resolveBrokerBinary()
			if err != nil {
				return fmt.Errorf("peers: %w", err)
			}

			c := brokerclient.NewClient(brokerclient.ClientOptions{
				Name:       "intercom-peers",
				Version:    version,
				SocketPath: sock,
				BrokerBin:  brokerBin,
				Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			defer c.Close()

			peers, err := c.ListPeers(cmd.Context())
			if err != nil {
				return err
			}
			if err := writePeerList(cmd.OutOrStdout(), peers); err != nil {
				return fmt.Errorf("peers: write output: %w", err)
			}
			return nil
		},
	}
}

func writePeerList(w io.Writer, peers []string) error {
	if len(peers) == 0 {
		_, err := fmt.Fprintln(w, "(no other peers connected)")
		return err
	}
	for _, peer := range peers {
		if _, err := fmt.Fprintln(w, peer); err != nil {
			return err
		}
	}
	return nil
}
