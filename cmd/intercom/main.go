// Command intercom is the all-in-one binary for the intercom system. It
// dispatches to the local broker and its supported coding-agent adapters.
//
// See docs/ARCHITECTURE.md for the architecture contract.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is the binary version, overridable via -ldflags '-X main.version=...'.
var version = "0.3.0-dev"

// commit is the git SHA the binary was built from, overridable likewise.
var commit = "unknown"

const brokerBinEnv = "INTERCOM_BROKER_BIN"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "intercom:", err)
		os.Exit(1)
	}
}

// resolveBrokerBinary keeps every command's auto-spawn behavior consistent.
// An explicit environment override is useful when the adapter and broker are
// installed or upgraded independently; otherwise the running binary is the
// safest version match.
func resolveBrokerBinary() (string, error) {
	if value := os.Getenv(brokerBinEnv); value != "" {
		return value, nil
	}
	value, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate intercom executable: %w", err)
	}
	return value, nil
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "intercom",
		Short:         "Routes local messages between coding-agent sessions",
		Long:          "intercom routes messages between local coding-agent sessions through a Unix-socket broker.",
		Version:       fmt.Sprintf("%s (%s)", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newShimCmd(),
		newCodexCmd(),
		newCodexAppServerExecCmd(),
		newCodexProcessSessionCleanupCmd(),
		newCodexMCPBridgeCmd(),
		newBrokerCmd(),
		newNameCmd(),
		newPeersCmd(),
	)
	configureGeneratedCommands(root)
	return root
}

func configureGeneratedCommands(root *cobra.Command) {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	help, _, err := root.Find([]string{"help"})
	if err == nil && help.Name() == "help" {
		help.Short = "Prints help for a command"
		help.Long = "intercom help prints the complete command help for the selected command path."
	}

	completion, _, err := root.Find([]string{"completion"})
	if err != nil || completion.Name() != "completion" {
		return
	}
	completion.Short = "Generates a shell-completion program"
	completion.Long = "intercom completion writes a completion program for the selected shell to standard output."
	descriptions := map[string]string{
		"bash": `intercom completion bash writes a Bash completion program to standard output.
The program requires bash-completion. The following command loads it into the active shell:

	source <(intercom completion bash)`,
		"fish": `intercom completion fish writes a fish completion program to standard output.
The following command loads it into the active shell:

	intercom completion fish | source`,
		"powershell": `intercom completion powershell writes a PowerShell completion program to standard output.
The following command loads it into the active shell:

	intercom completion powershell | Out-String | Invoke-Expression`,
		"zsh": `intercom completion zsh writes a Z shell completion program to standard output.
The active shell must have compinit enabled. The following command loads the program:

	source <(intercom completion zsh)`,
	}
	for _, command := range completion.Commands() {
		command.Short = "Generates the " + command.Name() + " completion program"
		command.Long = descriptions[command.Name()]
		if flag := command.Flags().Lookup("no-descriptions"); flag != nil {
			flag.Usage = "Omits completion descriptions"
		}
	}
}
