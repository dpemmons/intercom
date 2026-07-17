# intercom

## NAME

`intercom` — routes messages between local Claude Code sessions and managed Codex app-server sessions.

## SYNOPSIS

```text
intercom [--help] [--version]
intercom shim
intercom codex --app-server ENDPOINT [--client-endpoint ENDPOINT] [--mcp-bridge PATH] [--name NAME] [--cwd DIRECTORY] [--new] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom codex --app-server ENDPOINT [--client-endpoint ENDPOINT] --mcp-bridge PATH [--name NAME] [--cwd DIRECTORY] --adopt-session ID [--replace-binding] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom codex --app-server ENDPOINT [--client-endpoint ENDPOINT] --mcp-bridge PATH [--name NAME] [--cwd DIRECTORY] --fork-session ID [--replace-binding] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom codex attach --name NAME
intercom codex sessions --app-server ENDPOINT [--cwd DIRECTORY] [--all] [--list]
intercom broker [--idle-after DURATION] [--foreground]
intercom name
intercom peers
intercom completion {bash|fish|powershell|zsh} [--no-descriptions]
intercom help [COMMAND ...]

intercom-codex-project [--name NAME] [--cwd DIRECTORY] [--new] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom-codex-project [--name NAME] [--cwd DIRECTORY] --adopt [SESSION_ID] [--all-sessions] [--replace-binding] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom-codex-project [--name NAME] [--cwd DIRECTORY] --fork-from [SESSION_ID] [--all-sessions] [--replace-binding] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom-codex-project [--cwd DIRECTORY] --list-sessions [--all-sessions]

send_message(to=NAME, message=TEXT)
list_peers()
```

## DESCRIPTION

Intercom provides named, same-machine peers behind one Unix-domain-socket broker. `intercom shim` adapts the broker to Claude Code MCP and Channels. `intercom codex` adapts the broker to one dedicated Codex app-server thread and exposes that managed thread to one attachable Codex TUI. Both adapters expose the same `send_message` and `list_peers` tools to their models.

The broker stores no messages. A successful send means that the broker completed a delivery-frame write to the connected destination adapter. It does not establish that the destination model observed, processed, or answered the message.

## QUICK START

### Requirements

- Linux or macOS.
- Nix with flakes, or Go 1.25.5 and Bash.
- Claude Code 2.1.80 or later for Claude peers.
- `codex-cli` 0.144.1 or later for Codex peers.
- A Claude authentication method supported by Claude Code Channels.
- A Codex authentication method supported by `codex app-server`.

Claude Code Channels account restrictions and managed-setting requirements are specified in [Handbook: Configure a Claude Code peer](docs/HANDBOOK.md#2-configure-a-claude-code-peer).

### Install with Nix

The following command runs from the repository root and installs `intercom` and `intercom-codex-project` into the active Nix profile:

```sh
nix profile add path:.
hash -r
```

### Build from a checkout

The following commands run from the repository root:

```sh
mkdir -p bin
go build -o bin/intercom ./cmd/intercom
install -m 0755 scripts/intercom-codex-project bin/intercom-codex-project
export PATH="$PWD/bin:$PATH"
```

Each independent terminal used by the quick start repeats the `PATH` export from the repository root before it invokes a bare `intercom` or `intercom-codex-project` command. Without that export, checkout-local launcher use must set `INTERCOM_BIN=./bin/intercom`; invoking only `./bin/intercom-codex-project` can select another `intercom` from `PATH`.

### Connect Claude Code

The MCP registration is user-scoped and is performed once:

```sh
claude mcp add --transport stdio --scope user intercom -- intercom shim
claude mcp get intercom
```

A Claude peer participates in the broker only after an explicit opt-in: `INTERCOM_ENABLE=1`, or a nonblank `INTERCOM_NAME`. Each Claude Code peer starts with Channels enabled; the following command sets `INTERCOM_NAME`, which both selects the peer name and supplies the opt-in:

```sh
INTERCOM_NAME=implementer claude --dangerously-load-development-channels server:intercom
```

When the working-directory basename is already the required peer name, `INTERCOM_ENABLE=1` supplies the opt-in in place of `INTERCOM_NAME`. A launch with neither variable set serves the Intercom tools without registering a peer name, and `send_message` and `list_peers` return an error result. Peer-name syntax is specified in [Command and tool reference: Peer names](docs/REFERENCE.md#peer-names).

### Connect Codex

The managed service starts in the foreground in terminal 1:

```sh
intercom-codex-project --name reviewer --cwd .
```

After the readiness block appears, terminal 2 runs the exact attachment command printed by the launcher. The following shorter form is valid when both terminals use the same Intercom and Codex environment:

```sh
intercom codex attach --name reviewer
```

A third terminal verifies both quick-start peers:

```console
$ intercom peers
implementer
reviewer
```

Exiting the attached TUI releases its attachment slot but leaves the service running. `Ctrl-C` in terminal 1 stops the adapter/proxy and app-server.

Session selection, adoption, forking, binding replacement, execution policy, restart behavior, and attachment restrictions are specified in [Handbook: Managed Codex](docs/HANDBOOK.md#3-add-a-managed-codex-peer), [Handbook: Resume, adopt, fork, or replace](docs/HANDBOOK.md#5-resume-adopt-fork-or-replace-a-codex-thread), and [Command and tool reference: `intercom-codex-project`](docs/REFERENCE.md#intercom-codex-project). [Handbook: Multiple managed peers](docs/HANDBOOK.md#7-run-multiple-managed-peers) specifies concurrent and isolated service groups.

### Exchange messages

The connected model invokes the tools. Intercom has no shell `send` command.

```text
Call list_peers. Send reviewer a message asking for a review of the current change.
```

Claude receives a delivery as a channel event. Codex receives a delivery as a serialized user turn after any active turn finishes. A normal Codex final response remains in its managed thread; only `send_message` sends content to another peer.

## PUBLIC INTERFACE

| Interface | Contract |
|---|---|
| [`intercom`](docs/REFERENCE.md#intercom-root) | Prints command help when invoked without a command. |
| [`intercom shim`](docs/REFERENCE.md#intercom-shim) | Runs the Claude Code stdio MCP and Channels adapter. |
| [`intercom codex`](docs/REFERENCE.md#intercom-codex) | Connects an externally supervised dedicated app-server to the broker. |
| [`intercom codex attach`](docs/REFERENCE.md#intercom-codex-attach) | Attaches one Codex TUI to a live managed peer by name. |
| [`intercom codex sessions`](docs/REFERENCE.md#intercom-codex-sessions) | Lists or selects resumable ordinary Codex CLI and VS Code sessions. |
| [`intercom broker`](docs/REFERENCE.md#intercom-broker) | Runs the broker. Adapters and `intercom peers` start it on demand. |
| [`intercom name`](docs/REFERENCE.md#intercom-name) | Prints the validated peer name resolved for the working directory. |
| [`intercom peers`](docs/REFERENCE.md#intercom-peers) | Prints other connected peer names through a transient broker connection. |
| [`intercom completion`](docs/REFERENCE.md#intercom-completion) | Generates a shell-completion program for a supported shell. |
| [`intercom help`](docs/REFERENCE.md#intercom-help) | Prints help for the selected command path. |
| [`intercom --help`](docs/REFERENCE.md#intercom-root) | Prints root command help. |
| [`intercom --version`](docs/REFERENCE.md#intercom-root) | Prints the Intercom version and build revision. |
| [`intercom-codex-project`](docs/REFERENCE.md#intercom-codex-project) | Supervises one dedicated Codex app-server and attachable adapter/proxy service group. |
| [`send_message`](docs/REFERENCE.md#send_message) | Sends one message to one connected peer. |
| [`list_peers`](docs/REFERENCE.md#list_peers) | Lists other peers connected to the same broker. |
| [`channel_status`](docs/REFERENCE.md#channel_status) | Reports a Claude peer's opt-in state, effective name, and broker connectivity. |

## DOCUMENTATION

- [Handbook](docs/HANDBOOK.md) — installation, Claude setup, Codex setup, operation, restart, and troubleshooting tasks.
- [Command and tool reference](docs/REFERENCE.md) — complete arguments, options, environment, files, limits, errors, and examples.
- [Architecture](docs/ARCHITECTURE.md) — components, invariants, lifecycles, state, and failure semantics.
- [Broker protocol](docs/BROKER_PROTOCOL.md) — transport, frame, tool, and error contracts.
- [Development](docs/DEVELOPMENT.md) — build and verification procedures.

## NOTES

Intercom is a local transport, not an offline inference system. Claude Code and Codex may send message content to their configured model providers.

Peer messages become model input. A trusted same-user environment is required. Intercom does not authenticate one local peer to another beyond Unix file permissions and unique live peer names.

All peers on one broker should use the same Intercom build. The broker protocol has no version negotiation; tolerant JSON decoding does not establish mixed-build compatibility.

## SEE ALSO

[Codex documentation](https://developers.openai.com/codex/), [Claude Code Channels](https://code.claude.com/docs/en/channels), [Claude Code MCP](https://code.claude.com/docs/en/mcp), [MIT license](LICENSE)
