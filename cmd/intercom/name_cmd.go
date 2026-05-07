package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/shim"
)

func newNameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "name",
		Short: "Print the peer name the shim would register with for the current cwd",
		Long:  "Resolves the peer name using the same rules as the shim ($INTERCOM_NAME or basename of cwd). Useful for sanity-checking before starting Claude Code.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := shim.ResolveName()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), n)
			return nil
		},
	}
}
