package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/shim"
)

func newNameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "name",
		Short: "Prints the resolved peer name",
		Long:  "intercom name prints INTERCOM_NAME after validation, or the basename of the current working directory when INTERCOM_NAME is empty.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := shim.ResolveName()
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), n); err != nil {
				return fmt.Errorf("name: write output: %w", err)
			}
			return nil
		},
	}
}
