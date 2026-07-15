# intercom

## NAME

`intercom` — routes messages between local Claude Code sessions and managed Codex app-server sessions.

## SYNOPSIS

```text
intercom [--help] [--version]
intercom shim
intercom codex --app-server ENDPOINT [--client-endpoint ENDPOINT] [--name NAME] [--cwd DIRECTORY] [--new]
intercom codex attach --name NAME
intercom broker [--idle-after DURATION] [--foreground]
intercom name
intercom peers
intercom completion {bash|fish|powershell|zsh} [--no-descriptions]
intercom help [COMMAND ...]

intercom-codex-project [--name NAME] [--cwd DIRECTORY] [--new]

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

Claude Code Channels is a research-preview interface. Organization-managed Claude Team and Enterprise accounts require the Channels setting to be enabled. Anthropic Console organizations that deploy managed settings require `channelsEnabled: true`; Console organizations without managed settings have Channels enabled by default. Channels is unavailable through Amazon Bedrock, Google Vertex AI, and Microsoft Foundry.

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

The exported `PATH` must remain in the shell that starts either program. Without that export, checkout-local launcher use must set `INTERCOM_BIN=./bin/intercom`; invoking only `./bin/intercom-codex-project` can select another `intercom` from `PATH`.

### Connect Claude Code

The MCP registration is user-scoped and is performed once:

```sh
claude mcp add --transport stdio --scope user intercom -- intercom shim
claude mcp get intercom
```

Each Claude Code peer starts with Channels enabled:

```sh
INTERCOM_NAME=implementer claude --dangerously-load-development-channels server:intercom
```

`INTERCOM_NAME` is optional when the working-directory basename is a suitable peer name.

### Connect Codex

The launcher owns one child app-server and one child adapter/proxy. Each invocation creates a private runtime directory containing two unique Unix sockets: `app-server.sock` connects the adapter to app-server, and `client.sock` accepts one Codex TUI connection.

The service starts in its own terminal:

```console
$ intercom-codex-project --name reviewer --cwd .
Intercom Codex peer reviewer is ready.

Attach from another terminal:
  INTERCOM_DIR=STATE_DIRECTORY INTERCOM_SOCKET=BROKER_SOCKET CODEX_BIN=codex INTERCOM_EXECUTABLE codex attach --name reviewer

Direct Codex command:
  codex resume --remote unix:///RUNTIME/intercom-codex.INSTANCE/client.sock THREAD_ID
```

The readiness block appears only after the managed thread, broker registration, client proxy, and live descriptor are ready. Its actual commands contain shell-quoted concrete values rather than the metavariables above. The name-based command includes the canonical `INTERCOM_DIR` and `INTERCOM_SOCKET`, the selected Codex and Intercom executables, and an explicit `CODEX_HOME` when one is configured. Copying that line preserves instance discovery and client selection in another terminal. Provider authentication variables that are not named in the block remain the responsibility of the attachment terminal.

The shorter attachment form is equivalent when the second terminal already has the launcher's environment:

```sh
intercom codex attach --name reviewer
```

The command replaces itself with `codex resume --remote ENDPOINT THREAD_ID` from the managed project directory. Exiting or disconnecting the TUI releases the single attachment slot without stopping the service. The same attachment command reconnects later. A second simultaneous TUI attachment is rejected; it does not disturb the attached TUI or service.

The launcher remains in the foreground and owns service shutdown. `Ctrl-C` in the launcher terminal stops the adapter/proxy and app-server. Exiting the TUI alone does not stop either child.

Distinct names run concurrently on one machine. Each command occupies a separate launcher terminal and receives a separate private socket directory:

```sh
intercom-codex-project --name reviewer --cwd project-a
intercom-codex-project --name planner --cwd project-b
```

The launcher adapter joins the broker selected by its inherited `INTERCOM_SOCKET`. The value must match the value inherited by existing Claude peers; leaving it unset joins the default broker group. Attachment uses the same broker identity and `INTERCOM_DIR` as the launcher.

The process remains in the foreground. A subsequent invocation with the same peer name, canonical working directory, Codex home, state schema, and dynamic-tool contract attempts to resume the saved managed thread. The saved app-server user agent and Codex version are diagnostics, not binding identity; a successful resume refreshes them. A Codex upgrade therefore does not require `--new`. Before the first turn is materialized, a missing Codex rollout causes the adapter to start a replacement thread. A materialized binding is never replaced implicitly. `--new` creates another thread and replaces the Intercom binding.

The app-server protocol provides no feature or schema-version negotiation. Intercom accepts an app-server user-agent version of 0.144.1 or later, then executes and validates the request, response, lifecycle, managed-thread, sandbox, and dynamic-tool contract it consumes. Unknown additive object fields are ignored. A newer version that changes a consumed contract fails at the affected startup or runtime validation. An attached TUI must use the same Codex version as the currently running app-server. A launcher that was already running when Codex was upgraded must restart before the upgraded TUI attaches; the binding does not require `--new`.

The durable binding at `$INTERCOM_DIR/codex/NAME.json` survives service shutdown and identifies the resumable thread. The live descriptor under `$INTERCOM_DIR/codex/live` is published while an attachable service owns that name and broker identity. The two private sockets and their runtime directory belong to the launcher lifetime. TUI disconnect does not remove the live descriptor; clean service shutdown removes the descriptor, sockets, and runtime directory. Process or host failure can leave stale entries, which do not represent a usable service.

Arbitrary Codex CLI, TUI, desktop, and shared app-server threads cannot be adopted. The attached TUI controls only the Intercom-managed thread. Ordinary prompts and the documented current-thread reads, settings, interruption, metadata, and project-search operations are supported. `/new`, `/fork`, thread archive, unarchive, or deletion, `/review`, manual `/compact`, rollback, shell escape, goal mutation, raw history injection, guardian-denied action approval, background-terminal mutation, realtime mutation, and unlisted protocol operations are unavailable through the managed attachment.

Thread creation, resume, TUI turns, and Intercom-delivered turns pin the runtime workspace-root list to the managed directory. Thread creation and resume establish approval policy `never` and workspace-write sandboxing without additional writable roots. An attached stock TUI may select its normal interactive approval, permission, model, and collaboration settings for TUI-originated turns. Every Intercom-delivered turn separately reasserts approval policy `never`, the validated workspace-write sandbox, and the managed runtime root. The thread-level Intercom developer instructions and dynamic tools remain available to both turn sources.

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
