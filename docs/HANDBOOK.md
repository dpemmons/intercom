# Intercom Handbook

## NAME

`intercom-handbook` — installation and operating procedures for Intercom peers.

## CONTENTS

1. [Install Intercom](#1-install-intercom)
2. [Configure a Claude Code peer](#2-configure-a-claude-code-peer)
3. [Add a managed Codex peer](#3-add-a-managed-codex-peer)
4. [List peers and exchange messages](#4-list-peers-and-exchange-messages)
5. [Resume or replace a managed Codex thread](#5-resume-or-replace-a-managed-codex-thread)
6. [Stop a managed Codex peer](#6-stop-a-managed-codex-peer)
7. [Run isolated broker groups](#7-run-isolated-broker-groups)
8. [Diagnose failures](#8-diagnose-failures)

## 1. Install Intercom

### Purpose

This procedure installs the `intercom` command and the `intercom-codex-project` launcher.

### Prerequisites

One of the following build environments is required:

- Nix with flakes enabled.
- Go 1.25.5 and Bash.

Linux and macOS are supported. Windows is not supported.

### Concepts

The Nix package installs both programs. A Go build produces only `intercom`; the launcher is a Bash program and must be installed separately.

### Procedure

The Nix procedure runs from the repository root:

```sh
nix profile add path:.
hash -r
```

The checkout-local Go procedure runs from the repository root:

```sh
mkdir -p bin
go build -o bin/intercom ./cmd/intercom
install -m 0755 scripts/intercom-codex-project bin/intercom-codex-project
export PATH="$PWD/bin:$PATH"
```

The exported `PATH` must remain in the shell that starts either program. A checkout-local launcher invocation without that export requires an explicit adapter selection:

```sh
INTERCOM_BIN=./bin/intercom ./bin/intercom-codex-project --name reviewer --cwd .
```

Invoking only `./bin/intercom-codex-project` can select another `intercom` from `PATH`.

### Verification

The following transcript verifies command discovery and argument handling. It does not start a broker or an app-server.

```console
$ intercom help codex >/dev/null
$ intercom codex attach --help >/dev/null
$ intercom-codex-project --help >/dev/null
$ INTERCOM_NAME=handbook-check intercom name
handbook-check
```

For a checkout-local build without the exported `PATH`, the first command is `./bin/intercom help codex`, and the launcher path is `./bin/intercom-codex-project`. The Codex help check distinguishes this build from an Intercom executable that contains only Claude commands.

### Notes

`nix profile add path:.` includes the working tree visible through the path flake. A bare `.` flake reference reads the Git-tracked view and can omit untracked source files in a development checkout.

### See also

[Command reference](REFERENCE.md), [development procedures](DEVELOPMENT.md)

## 2. Configure a Claude Code peer

### Purpose

This procedure registers the Intercom MCP server and starts one Claude Code Channels peer.

### Prerequisites

- Claude Code 2.1.80 or later.
- `intercom` available on `PATH`.
- Claude Code authentication through claude.ai or an Anthropic Console API key.

Organization-managed Team and Enterprise accounts require the Channels setting to be enabled. Anthropic Console organizations that deploy managed settings require `channelsEnabled: true`; Console organizations without managed settings have Channels enabled by default. Channels is unavailable through Amazon Bedrock, Google Vertex AI, and Microsoft Foundry.

### Concepts

Claude Code starts `intercom shim` as a stdio MCP server. The shim registers with the broker after the MCP initialization handshake. The shim derives its peer name from `INTERCOM_NAME` when that variable contains a nonblank value; otherwise it uses the working-directory basename.

The bare MCP server name is `intercom`. Claude Code requires explicit development-channel authorization for a custom channel server.

### Procedure

The user-scoped MCP registration is created once:

```sh
claude mcp add --transport stdio --scope user intercom -- intercom shim
```

The registration is inspected before the first launch:

```sh
claude mcp get intercom
```

The peer starts from the project directory:

```sh
INTERCOM_NAME=implementer claude --dangerously-load-development-channels server:intercom
```

The `INTERCOM_NAME` assignment may be omitted when the project-directory basename is the required peer name.

### Verification

Claude Code lists `intercom` as a connected MCP server under `/mcp`. Another terminal resolves the same selected name when run from the same directory and with the same environment:

```console
$ INTERCOM_NAME=implementer intercom name
implementer
```

### Notes

Two live peers cannot share a name. A Claude shim remains available as an MCP process after an eager broker registration failure and retries registration on a later tool call. The colliding peer does not appear in `list_peers` until the name becomes available.

The Channels option is required on every Claude Code launch that uses the Intercom channel. MCP registration alone exposes the tools but does not authorize inbound channel notifications.

### See also

[Reference: `intercom shim`](REFERENCE.md#intercom-shim), [Claude Code Channels](https://code.claude.com/docs/en/channels), [Claude Code MCP](https://code.claude.com/docs/en/mcp)

## 3. Add a managed Codex peer

### Purpose

This procedure starts an interactive managed Codex service and attaches one Codex TUI to its thread.

### Prerequisites

- `codex-cli` 0.144.1 available as `codex`.
- `intercom-codex-project` and `intercom` available on `PATH`.
- Codex authentication available to the child `codex app-server` process.
- A project directory writable under the selected Codex sandbox policy.

### Concepts

The launcher starts one child `codex app-server`, waits for its Unix socket, and starts one child `intercom codex` adapter/proxy. A mode-0700 runtime directory contains two unique endpoints:

- `app-server.sock` is the private upstream connection from the adapter to app-server;
- `client.sock` is the private downstream connection from one stock Codex TUI to the adapter/proxy.

The adapter creates or resumes one non-ephemeral thread with the following unattended policy:

- working directory set to the selected project directory;
- runtime workspace roots set to the selected project directory only;
- approval policy `never`;
- workspace-write sandbox;
- `send_message` and `list_peers` registered as dynamic tools;
- one inbound Intercom message serialized into one Codex turn.

The proxy forwards the documented closed request allowlist. TUI turns and Intercom delivery turns share one scheduler, so only one turn starts at a time. An Intercom delivery waits while a TUI turn is active. A TUI turn request is rejected while another managed turn is active. Interactive approval and input requests use the attached TUI when it remains connected; the unattended fallback policy applies without a TUI. Client notifications other than `initialized` and request methods outside the allowlist are rejected.

The attached TUI may select approval, permission, model, reasoning, personality, and collaboration settings for its own turns. The proxy pins both the managed directory and the runtime workspace-root list to that directory. Each later Intercom delivery reasserts approval policy `never`, the validated workspace-write sandbox, and the managed runtime root. Thread-level Intercom developer instructions remain separate from additive collaboration-mode instructions.

Codex may create child threads while a managed root turn runs. Child lifecycle events do not complete or replace the root turn. A child whose parent or fork ancestry is verified through `thread/read` may use inherited Intercom dynamic tools. This behavior depends on the launcher's dedicated app-server; the lower-level adapter is not valid with a shared app-server.

The adapter publishes a live descriptor only after the managed thread, broker registration, and client proxy are ready. The descriptor identifies the peer, broker, project directory, client endpoint, thread, service process, instance nonce, and Codex version. It is separate from the durable thread binding.

All peers using the same `INTERCOM_SOCKET` belong to the same broker. The default socket therefore joins a new Codex peer to existing default-configured Claude peers without another connection step.

### Procedure

The managed service starts in its own terminal:

```console
$ intercom-codex-project --name reviewer --cwd .
Intercom Codex peer reviewer is ready.

Attach from another terminal:
  INTERCOM_DIR=STATE_DIRECTORY INTERCOM_SOCKET=BROKER_SOCKET CODEX_BIN=codex INTERCOM_EXECUTABLE codex attach --name reviewer

Direct Codex command:
  codex resume --remote unix:///RUNTIME/intercom-codex.INSTANCE/client.sock THREAD_ID
```

The actual readiness block substitutes shell-quoted concrete values for `STATE_DIRECTORY`, `BROKER_SOCKET`, `INTERCOM_EXECUTABLE`, the client endpoint, and the thread ID. It also includes an explicit `CODEX_HOME` assignment when configured. The complete printed attachment line preserves descriptor discovery and Intercom and Codex executable selection in a second terminal. Provider authentication variables are not copied into the output and must already be available there.

The shorter name-based command is valid when the second terminal already has the same Intercom and Codex environment:

```sh
intercom codex attach --name reviewer
```

The attach command changes to the managed project directory and replaces itself with the displayed `codex resume --remote` operation. The launcher remains in the foreground. `--name` may be omitted from the launcher when `INTERCOM_NAME` or the selected directory basename supplies the required name; `intercom codex attach` always requires `--name`.

### Verification

The attached terminal displays the resumed managed thread. Another terminal lists the managed peer while the service is running:

```console
$ intercom peers
reviewer
```

The output contains all other peers in bytewise sorted order. Existing Claude peer names therefore appear in the same output when they remain connected.

### Notes

The readiness block is not printed before startup completes. An adapter, broker, thread, proxy, descriptor, or output failure prevents the block and terminates the service group.

One TUI may attach to an instance at a time. A simultaneous second attachment receives a busy error. A connection that does not complete initialization and managed-thread resume within 30 seconds is closed and frees the slot. Exiting or disconnecting the attached TUI frees the slot and leaves the launcher, app-server, broker peer, queued deliveries, durable binding, live descriptor, and private sockets running.

The attached TUI is a current-thread interface rather than a general Codex session manager. Ordinary prompts, current-thread history, turn interruption, approvals, requested input, documented metadata updates, and project-scoped skill, hook, and file search remain available. The proxy rejects `/new`, `/fork`, thread archive, unarchive, and deletion, `/review`, manual `/compact`, rollback, shell escape, goal mutation, raw history injection, guardian-denied action approval, background-terminal mutation, realtime mutation, and every unlisted protocol operation.

The launcher owns the app-server. A separately started app-server is supported only through the lower-level `intercom codex --app-server ENDPOINT` command. Intercom cannot adopt an arbitrary existing Codex CLI, TUI, desktop, or shared app-server session. The TUI attaches only to the dedicated thread created or resumed by the managed service.

The launcher does not daemonize or restart a failed child. A shell or service manager owns launcher lifetime and restart policy.

Intercom does not discover the socket used by another peer. Existing peers and the new launcher must inherit the same explicit `INTERCOM_SOCKET`, or all must use the default. One broker socket should contain peers and a broker from the same Intercom build because the wire protocol performs no version negotiation.

### See also

[Reference: `intercom-codex-project`](REFERENCE.md#intercom-codex-project), [Reference: `intercom codex`](REFERENCE.md#intercom-codex), [Reference: `intercom codex attach`](REFERENCE.md#intercom-codex-attach), [Codex documentation](https://developers.openai.com/codex/)

## 4. List peers and exchange messages

### Purpose

This procedure discovers live peer names and sends a message from one connected model to another.

### Prerequisites

At least two peers must be connected to the same broker.

### Concepts

The agent tools, not the shell command, send messages. `list_peers` excludes the calling peer. `send_message` accepts one destination and one message body.

A successful tool result means that the broker wrote a delivery frame to the live destination adapter. No offline mailbox, delivery retry, model-observation receipt, or reply receipt exists.

### Procedure

The connected `implementer` model calls `list_peers`:

```text
list_peers()
```

When `reviewer` is the other connected peer, the tool result is:

```text
Connected peers: reviewer
```

The connected model sends one message:

```text
send_message(to="reviewer", message="Inspect the current diff and report correctness defects.")
```

The accepted result is:

```text
Message sent to "reviewer".
```

### Verification

Claude receives the message as an Intercom channel event containing sender and UTC timestamp metadata. Codex receives an `Intercom message` user turn containing `From`, `Sent`, and `Message-ID` fields followed by the body.

A replying model calls `send_message` with the original sender name. A normal Codex final answer is retained in Codex history and is not forwarded.

### Notes

The destination must be connected when the broker handles the send. Sending to the caller is rejected. A model can create a message loop by repeatedly replying; Intercom performs no loop detection.

Message bodies are model instructions. System and developer instructions retain precedence at each receiving agent.

### See also

[Tool reference](REFERENCE.md#agent-tools), [broker protocol](BROKER_PROTOCOL.md)

## 5. Resume or replace a managed Codex thread

### Purpose

This procedure reattaches a TUI, restarts a managed service with its saved thread, or deliberately selects a new thread.

### Prerequisites

A running service is required for TUI reattachment. A prior `intercom-codex-project` invocation must have created the durable binding for service restart.

### Concepts

The binding is stored in `$INTERCOM_DIR/codex/NAME.json`; the default base is `$HOME/.claude-intercom`. Codex conversation data remains under the app-server-reported `CODEX_HOME`.

The binding is durable service state. It contains the managed thread identity and compatibility metadata and remains after the launcher stops. It does not contain the client socket, service PID, or attachment state.

The live descriptor is stored under `$INTERCOM_DIR/codex/live` after readiness. It selects one running service by broker identity and peer name and contains the current client endpoint, thread ID, process ID, instance nonce, project directory, and Codex version. A normal TUI disconnect leaves the descriptor in place. Clean service shutdown removes it. A hard process or host failure can leave a stale descriptor, which attachment detects and a later service publication replaces.

Resume requires the same peer name, canonical working directory, `CODEX_HOME`, app-server identity, Codex version, state schema, and tool-contract version. The app-server must return the saved thread as idle and non-ephemeral, approval policy `never`, runtime workspace roots containing only the canonical managed directory, and workspace-write sandbox without additional writable roots.

The binding becomes materialized after a terminal result for the first managed turn, whether TUI-originated or Intercom-delivered, is confirmed through `thread/read`. A restart before materialization attempts resume; when Codex reports that no rollout exists for the pending thread, Intercom replaces that unmaterialized binding with a new thread.

### Procedure

After a normal TUI exit or connection loss, the running service accepts the same attachment command again:

```sh
intercom codex attach --name reviewer
```

No launcher restart is required for this case.

The existing service group must stop as described in Chapter 6 before another invocation can acquire the peer lock. The same launcher invocation resumes the durable binding and publishes a new live descriptor with new private endpoints:

```sh
intercom-codex-project --name reviewer --cwd .
```

The new readiness block supplies the valid attachment commands for that service lifetime:

```sh
intercom codex attach --name reviewer
```

A deliberate replacement uses `--new`:

```sh
intercom-codex-project --name reviewer --cwd . --new
```

### Verification

Reattachment displays the same current thread without changing the launcher process. The launcher log records a TUI detach and later attachment. The state record contains the active durable binding:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
sed -n '/"peer"/p; /"threadId"/p; /"materialized"/p' "$state_dir/codex/reviewer.json"
```

The same `threadId` denotes TUI reattachment or service resume. A different `threadId` denotes `--new` or replacement of an unmaterialized missing rollout. The live descriptor directory contains a descriptor only while the service remains attachable:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
ls "$state_dir/codex/live"
```

### Notes

`--new` replaces only the Intercom binding. It does not delete the previous Codex rollout or conversation history.

The direct `codex resume --remote` command printed by one launcher is valid only for that service lifetime. A service restart creates a different private runtime directory and client endpoint. The name-based attachment command resolves the current descriptor and remains the normal interface.

A live service holds the peer state lock even when no TUI is attached. Starting another service with the same peer fails until the first service stops. A second TUI attachment also fails while the first TUI remains connected, but it does not stop or replace the first attachment.

The binding changes only after the replacement thread starts and passes managed-thread validation. A failed replacement leaves the prior binding unchanged.

A failure after Codex creates the replacement can leave an unbound thread in Codex storage. Intercom does not delete Codex threads.

A changed project symlink that resolves to the same canonical directory remains compatible. A different canonical directory requires `--new`.

### See also

[State files](REFERENCE.md#files), [Codex lifecycle](ARCHITECTURE.md#codex-adapter-lifecycle)

## 6. Stop a managed Codex peer

### Purpose

This procedure distinguishes TUI detachment from termination of the adapter/proxy and app-server service group.

### Prerequisites

The launcher must be running in the foreground.

### Concepts

Exiting the Codex TUI closes only its client connection. The service remains registered with the broker, retains queued deliveries and its live descriptor, and accepts a later `intercom codex attach --name NAME` command.

Service termination is a separate launcher operation. The launcher sends `SIGTERM` to the adapter/proxy first. The adapter removes its live descriptor, marks the controller unavailable, stops broker reconnection, closes its broker connection, interrupts an active turn when necessary, and drains the turn and reverse-request handlers within one shutdown budget. The proxy then closes its listener and attached TUI. The launcher next sends `SIGTERM` to app-server. A child that exceeds the configured per-child shutdown timeout receives `SIGKILL`.

Clean service shutdown closes an attached TUI connection and removes both private socket entries and their runtime directory. The durable binding and Codex conversation remain available for a later service restart.

### Procedure

TUI detachment exits or closes only the TUI terminal. The same running service accepts reattachment:

```sh
intercom codex attach --name reviewer
```

Full service shutdown sends `Ctrl-C` to the launcher terminal, or a service manager sends `SIGTERM` to the launcher process. Closing only the TUI does not perform this step.

### Verification

After TUI detachment, another terminal still lists the managed peer and the attachment command still succeeds. After service shutdown, another terminal no longer lists the stopped peer. For a broker whose remaining agent peer is `implementer`, the transcript is:

```console
$ intercom peers
implementer
```

The broker can remain alive with zero peers until its idle timeout expires.

After clean shutdown, the durable binding remains and the live directory no longer contains the stopped instance descriptor:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
sed -n '/"peer"/p; /"threadId"/p' "$state_dir/codex/reviewer.json"
ls "$state_dir/codex/live"
```

### Notes

The launcher maps `SIGHUP`, `SIGINT`, and `SIGTERM` to status 129, 130, and 143. Normal adapter status is propagated. A nonzero app-server status is propagated; an unexpected zero app-server status maps to 1.

Stopping during a starting or active turn interrupts that delivery. Intercom does not retry it. Deliveries waiting in the in-memory queue are lost when the adapter exits. A turn whose result matters must reach terminal completion before the service group stops.

A hard kill, shell failure, or host failure can prevent descriptor and runtime-directory cleanup. The attach command reports a stale descriptor when its recorded process no longer exists. A later launcher publication for the same broker and peer replaces that stale descriptor without changing the durable binding.

### See also

[Launcher exit status](REFERENCE.md#intercom-codex-project), [failure semantics](ARCHITECTURE.md#failure-semantics)

## 7. Run isolated broker groups

### Purpose

This procedure separates peers into independent routing groups.

### Prerequisites

Each group requires a distinct Unix socket path. Parent directories must exist and be writable by the current user.

### Concepts

`INTERCOM_SOCKET` selects both the broker socket and its sibling `.lock` file. Every adapter and diagnostic command in one group must receive the same value. `INTERCOM_DIR` does not override an explicit `INTERCOM_SOCKET`.

One broker group supports multiple concurrently running managed Codex instances when every peer name is distinct. Each launcher creates a separate random runtime directory, upstream app-server socket, downstream client socket, managed-thread lock, and live descriptor. Each instance accepts one TUI; the one-TUI limit is per instance rather than per machine.

The attach command selects a descriptor by the canonical broker socket and peer name. Its terminal must therefore inherit the same `INTERCOM_SOCKET` and `INTERCOM_DIR` selection as the matching launcher.

### Procedure

Every terminal in the following transcript starts in the same directory containing `project-a` and `project-b`. The first terminal creates a private broker runtime directory:

```sh
mkdir -p .intercom-runtime
chmod 700 .intercom-runtime
```

Claude and Codex commands in the group derive the same canonical socket in each terminal. Two existing project directories start two named Codex services in separate terminals:

```sh
group_dir=$(cd .intercom-runtime && pwd -P)
INTERCOM_SOCKET="$group_dir/broker.sock" \
  intercom-codex-project --name reviewer --cwd project-a
```

```sh
group_dir=$(cd .intercom-runtime && pwd -P)
INTERCOM_SOCKET="$group_dir/broker.sock" \
  intercom-codex-project --name planner --cwd project-b
```

Two more terminals attach one TUI to each service:

```sh
group_dir=$(cd .intercom-runtime && pwd -P)
INTERCOM_SOCKET="$group_dir/broker.sock" intercom codex attach --name reviewer
```

```sh
group_dir=$(cd .intercom-runtime && pwd -P)
INTERCOM_SOCKET="$group_dir/broker.sock" intercom codex attach --name planner
```

A second broker group uses another directory and socket value.

### Verification

The selected group is queried with the same environment:

```console
$ group_dir=$(cd .intercom-runtime && pwd -P)
$ INTERCOM_SOCKET="$group_dir/broker.sock" intercom peers
planner
reviewer
```

The output contains the other live peers in bytewise order. An invocation with `INTERCOM_SOCKET` unset queries the default group instead and cannot resolve the selected group’s live attachment descriptors.

### Notes

`INTERCOM_SOCKET` is inherited by the launcher adapter. It does not select either private Codex socket; each launcher creates and owns its app-server and client socket separately.

Two services in one broker cannot share a peer name. The broker registration, durable binding lock, and live descriptor claim all preserve single ownership. Distinct names are sufficient for ordinary multi-instance operation under one `INTERCOM_DIR`.

Broker isolation does not isolate managed Codex binding files or the default broker log. Codex peers with the same name and different sockets still contend for `$INTERCOM_DIR/codex/NAME.lock`. Distinct `INTERCOM_DIR` values provide separate Codex bindings and default logs when those resources must also be isolated.

### See also

[Environment](REFERENCE.md#environment), [files](REFERENCE.md#files)

## 8. Diagnose failures

### Purpose

This procedure identifies peer-name, broker, Claude MCP, and Codex lifecycle failures.

### Prerequisites

The failing command's standard error and environment must be available.

### Concepts

Broker messages are written to the configured broker log unless `intercom broker --foreground` owns the broker. Adapter and proxy diagnostics are written to standard error. Claude Code captures shim standard error in its MCP diagnostics. The Codex launcher leaves both child diagnostics attached to its terminal and reserves standard output for the post-readiness attachment block.

Attachment discovery depends on three values: `INTERCOM_DIR`, the canonical `INTERCOM_SOCKET`, and the requested peer name. A durable binding proves that a managed thread exists; it does not prove that a service or client socket is live. A live descriptor proves that a service published an attachment endpoint, subject to its recorded process still existing.

### Procedure

The resolved name is checked without connecting:

```sh
intercom name
```

The broker path and peer list are checked under the same environment as the agent:

```sh
intercom peers
```

The default broker log is inspected with:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
tail -n 50 "$state_dir/broker.log"
```

Durable bindings and currently published live descriptors are inspected separately:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
ls "$state_dir/codex"
ls "$state_dir/codex/live"
```

The name-based attachment is tested under the same broker and directory environment as the launcher:

```sh
intercom codex attach --name reviewer
```

Claude Code MCP state is inspected with `/mcp` and:

```sh
claude mcp get intercom
```

An explicitly owned isolated diagnostic broker writes logs to the terminal:

```sh
mkdir -p .intercom-diagnostic
chmod 700 .intercom-diagnostic
INTERCOM_SOCKET="$PWD/.intercom-diagnostic/broker.sock" \
  intercom broker --foreground --idle-after 0
```

The foreground command starts a broker; it does not attach to a broker that already owns the selected lock. Foreground logging for an existing group therefore requires that group's current broker to stop before this command starts with the same environment.

### Verification

The following table maps diagnostics to binding conditions.

| Diagnostic | Condition | Remedy |
|---|---|---|
| `invalid peer name` | The selected name is empty, longer than 64 bytes, or contains a character outside ASCII letters, digits, `_`, and `-`. | A valid `INTERCOM_NAME` or `--name` is required. |
| `name_taken` | A live peer has registered the same name. | The duplicate must stop, or another name must be selected. |
| `no_such_peer` | The destination is absent when the broker handles the send. | A retry is appropriate only after `list_peers` reports the reconnected peer. |
| `no_self_send` | The destination equals the sender. | Another destination peer is required. |
| `broker did not start within retry budget` | Broker spawning succeeded but the socket never accepted the client within the fixed dial sequence. | The broker log, socket directory, and `INTERCOM_BROKER_BIN` identify the startup failure. |
| `unsupported app-server version` | The app-server user agent does not report `0.144.1`. | Codex CLI 0.144.1 is required. |
| `Codex client version ... is incompatible` | The attaching TUI version differs from the running app-server version. | The same Codex CLI version used by the service is required in the attachment terminal. |
| `peer is already managed` | Another `intercom codex` process holds the peer state lock. | The other service group must stop, or another peer name and state binding must be selected. |
| `Codex instance is already live` | Another service owns the same broker-and-peer live descriptor. | The existing service must stop, or another peer name must be selected. |
| `no live Codex instance named ...` | The service is stopped, has not reached readiness, uses another broker identity, uses another `INTERCOM_DIR`, or has another peer name. | The launcher readiness block and matching environment identify the attachable instance. |
| `descriptor is stale` | A prior service ended without removing a descriptor and its recorded process no longer exists. | A new launcher for the same broker and peer replaces the stale descriptor after successful startup. |
| `a TUI is already connected` or HTTP conflict | One TUI already owns the instance attachment slot. | The existing TUI must disconnect before another attachment starts. The service does not require restart. |
| `... is unavailable while attached to an Intercom-managed thread` | The TUI requested `/new`, `/fork`, archive, unarchive, deletion, `/review`, manual compact, rollback, shell escape, or realtime control. | Current-thread prompts and supported current-thread operations remain available; unsupported session-management operations require a separate unmanaged Codex session. |
| `use --new to replace the binding` | Saved identity or tool state differs from the selected runtime. | The matching runtime must be restored, or `--new` must select a deliberate replacement. |
| `managed thread is ... want idle` | The dedicated thread is active or in an error state during startup. | The service group must stop until Codex settles before restart. |
| `active turn had no app-server activity` | An active managed turn emits no app-server activity for 15 minutes. | App-server diagnostics determine whether the service group requires restart. |

### Notes

`intercom peers` starts a broker when none is listening. Its successful empty-list marker proves broker availability, not agent availability.

The fixed diagnostic peer name is `intercom-peers`. The name remains legal for an agent; a live peer using it causes `intercom peers` to fail with `name_taken`.

The direct resume command from an old readiness block can fail after a service restart because its private client endpoint is ephemeral. `intercom codex attach --name NAME` resolves the current live descriptor and avoids that stale endpoint.

Descriptor lookup checks descriptor structure and the recorded process ID; it does not connect to the client socket. After successful process replacement, remote-socket, Codex authentication, version, and proxy-initialization failures are Codex diagnostics and use the Codex process exit status.

A TUI disconnect is not a service failure. The launcher continues running, the peer remains discoverable, and the same name-based command reattaches. A launcher exit, adapter exit, or app-server exit is a service failure boundary and requires service restart before attachment.

### See also

[Errors](REFERENCE.md#errors), [broker error codes](BROKER_PROTOCOL.md#error-codes)
