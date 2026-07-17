# Intercom Handbook

## NAME

`intercom-handbook` — installation and operating procedures for Intercom peers.

## CONTENTS

1. [Install Intercom](#1-install-intercom)
2. [Configure a Claude Code peer](#2-configure-a-claude-code-peer)
3. [Add a managed Codex peer](#3-add-a-managed-codex-peer)
   - [TUI active-turn behavior](#tui-active-turn-behavior)
4. [List peers and exchange messages](#4-list-peers-and-exchange-messages)
5. [Resume, adopt, fork, or replace a Codex thread](#5-resume-adopt-fork-or-replace-a-codex-thread)
   - [5.1 Attach to a running service](#51-attach-to-a-running-service)
   - [5.2 Restart the saved thread](#52-restart-the-saved-thread)
   - [5.3 List resumable sessions](#53-list-resumable-sessions)
   - [5.4 Adopt an existing session](#54-adopt-an-existing-session)
   - [5.5 Fork an existing session](#55-fork-an-existing-session)
   - [5.6 Replace a saved binding](#56-replace-a-saved-binding)
   - [5.7 Start a new managed thread](#57-start-a-new-managed-thread)
   - [5.8 Select the execution policy](#58-select-the-execution-policy)
6. [Stop a managed Codex peer](#6-stop-a-managed-codex-peer)
7. [Run multiple managed peers](#7-run-multiple-managed-peers)
   - [7.1 Default broker group](#71-default-broker-group)
   - [7.2 Isolated broker group](#72-isolated-broker-group)
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

### Errors

| Condition | Result |
|---|---|
| Nix is absent, flakes are disabled, or package evaluation fails. | `nix profile add` exits nonzero and no profile package is installed. |
| Go is earlier than 1.25.5, a dependency is unavailable, or compilation fails. | `go build` exits nonzero and does not produce a usable `bin/intercom`. |
| `scripts/intercom-codex-project` is absent or cannot be installed. | `install` exits nonzero and the launcher is unavailable from `bin`. |
| The checkout-local `bin` directory is absent from `PATH`. | A bare command resolves another installation or fails with command-not-found. An explicit `./bin/` pathname selects the checkout build. |

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

Claude Code starts `intercom shim` as a stdio MCP server. The shim participates in the broker only for an opted-in session: an explicitly configured name, a nonblank `INTERCOM_NAME`, or `INTERCOM_ENABLE=1`. A session that has not opted in serves MCP tools without registering a peer name, and `send_message` and `list_peers` return an error result instead of contacting the broker. An opted-in shim connects after the MCP initialization handshake and derives its peer name from `INTERCOM_NAME` when that variable contains a nonblank value; otherwise it uses the working-directory basename.

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

The peer participates in the broker only after an explicit opt-in. Setting `INTERCOM_NAME` opts the session in and selects the peer name in one step; the peer starts from the project directory:

```sh
INTERCOM_NAME=implementer claude --dangerously-load-development-channels server:intercom
```

When the project-directory basename is already the required peer name, `INTERCOM_ENABLE=1` supplies the opt-in on its own:

```sh
INTERCOM_ENABLE=1 claude --dangerously-load-development-channels server:intercom
```

A launch with neither variable set serves the Intercom tools without registering a peer name; `send_message` and `list_peers` then return an error result.

### Verification

Claude Code lists `intercom` as a connected MCP server under `/mcp`. Another terminal resolves the same selected name when run from the same directory and with the same environment:

```console
$ INTERCOM_NAME=implementer intercom name
implementer
```

Calling `channel_status` from the running session confirms the opt-in state, the effective registered name after any auto-suffix, and broker connectivity:

```text
channel_status()
intercom status:
  enabled: yes
  peer name: implementer
  broker: connected
  other peers: none
```

### Errors

| Condition | Result |
|---|---|
| `claude mcp add` rejects the registration. | No user-scoped `intercom` MCP server is created. The command diagnostic identifies the invalid Claude configuration or unavailable executable. |
| The selected peer name is empty, exceeds 64 bytes, or contains a character outside ASCII letters, digits, `_`, and `-`. | `intercom shim` reports `invalid peer name` and exits. |
| Another live peer owns the selected name. | The shim retries registration under a numbered suffix (`NAME-2`, `NAME-3`, ... up to 20 candidates) instead of failing; `channel_status` reports the effective registered name. |
| The session did not opt in (no `INTERCOM_ENABLE=1`, `INTERCOM_NAME`, or explicit name). | `send_message` and `list_peers` return an error result reporting that intercom is not enabled; the shim never registers with the broker. |
| The Channels launch option is absent or organization policy disables Channels. | Intercom tools may remain available, but inbound channel notifications are not delivered to Claude Code. |
| Claude authentication is unavailable. | Claude Code reports its authentication error before the peer becomes usable. |

### Notes

Two live peers cannot share a base name; an opted-in shim resolves the collision itself by registering under a numbered suffix rather than failing, and a reconnect after a broker restart prefers the previously registered effective name. A non-opted-in shim never appears in `list_peers` and holds no peer name to receive against; a session that appears unreachable is often one that skipped the opt-in, and `channel_status` distinguishes the two conditions.

The Channels option is required on every Claude Code launch that uses the Intercom channel. MCP registration alone exposes the tools but does not authorize inbound channel notifications and does not by itself opt the session in to broker participation.

`Ctrl-C` in the Claude Code terminal stops Claude Code and its Intercom shim. The peer then disappears from broker discovery.

### See also

[Reference: `intercom shim`](REFERENCE.md#intercom-shim), [Reference: `channel_status`](REFERENCE.md#channel_status), [Peer names](REFERENCE.md#peer-names), [Claude Code Channels](https://code.claude.com/docs/en/channels), [Claude Code MCP](https://code.claude.com/docs/en/mcp)

## 3. Add a managed Codex peer

### Purpose

This procedure starts an interactive managed Codex service and executes the concrete attachment command printed by that service.

### Prerequisites

- `codex-cli` 0.144.1 or later available as `codex`.
- `intercom-codex-project` and `intercom` available on `PATH`.
- Linux or Darwin with `setsid(2)`, `getsid(2)`, and `ps -A -o pid= -o stat=` available.
- Codex authentication available to the child `codex app-server` process.
- A project directory writable under the selected Codex sandbox policy.

### Concepts

The launcher starts its hidden Intercom session-exec helper as a child. A terminal signal received during the asynchronous launch window is deferred until the launcher records the child PID, then dispatched immediately. The helper creates a process session, writes its own PID, forces the marker inode to mode 0600 independently of process umask, publishes it atomically, and replaces itself with `codex app-server`. Publication occurs after `setsid(2)` and before app-server execution or descendant creation, so the Codex child PID is also the dedicated session ID. The launcher validates that PID and waits for the Unix socket before it starts one `intercom codex` adapter/proxy child outside the Codex session. A mode-0700 runtime directory contains three unique endpoints and one readiness marker:

- `app-server.sock` is the private upstream connection from the adapter to app-server;
- `client.sock` is the private downstream connection from one stock Codex TUI to the adapter/proxy;
- `mcp-bridge.sock` is the authenticated private tool connection used by adopted and forked sessions;
- `app-server.session` is the mode-0600 decimal PID and session ID published without replacing an existing marker.

The adapter creates or resumes one non-ephemeral thread with the following unattended policy:

- working directory set to the selected project directory;
- runtime workspace roots set to the selected project directory only;
- approval policy `never`;
- approvals reviewer `user`;
- workspace-write sandbox by default, or `danger-full-access` when yolo mode is selected;
- `send_message` and `list_peers` registered as dynamic tools for a new Intercom thread or as a required MCP server for an adopted or forked interactive thread;
- one inbound Intercom message serialized into one Codex turn.

The proxy forwards the documented closed request allowlist. TUI turns, Intercom delivery turns, and Codex persistent-goal continuations share one scheduler. An Intercom delivery waits while another turn source owns it. A `turn/start` request is rejected while another managed turn or accepted settings update blocks local admission. Interactive approval and input requests use the attached TUI when it remains connected; the unattended fallback policy applies without a TUI. Client notifications other than `initialized` and request methods outside the allowlist are rejected.

The attached TUI may select model, service tier, reasoning effort and summary, personality, collaboration mode, and multi-agent mode for its own turns. The proxy pins the managed directory, runtime workspace-root list, approval policy, approvals reviewer, and sandbox policy to the service configuration. Settings update is allowed only while the controller is idle and drops permissions and unknown fields. Once accepted, it atomically blocks broker-delivery start, TUI `turn/start`, and another settings update through its upstream response or error. Independently arriving app-server goal and turn lifecycle remains authoritative. Both service configurations use approval policy `never` and approvals reviewer `user`. The default uses workspace-write sandboxing; yolo uses `danger-full-access`. Intercom reasserts the selected policy on thread resume, settings updates, TUI turns, and Intercom-delivered turns. Thread-level Intercom developer instructions remain separate from additive collaboration-mode instructions.

Codex may create child threads while a managed root turn runs. Child lifecycle events do not complete or replace the root turn. A child whose parent or fork ancestry is verified through `thread/read` may use inherited Intercom tools. This behavior depends on the launcher's dedicated app-server; the lower-level adapter is not valid with a shared app-server.

The adapter publishes a live descriptor only after the managed thread, broker registration, and client proxy are ready. The descriptor identifies the peer, broker, project directory, client endpoint, thread, service process, instance nonce, and Codex version. It is separate from the durable thread binding.

All peers using the same `INTERCOM_SOCKET` belong to the same broker. The default socket therefore joins a new Codex peer to existing default-configured Claude peers without another connection step.

### Procedure

The managed service starts in its own terminal and remains in the foreground:

```sh
intercom-codex-project --name reviewer --cwd .
```

The launcher prints a readiness block after the broker peer, managed thread, downstream proxy, and live descriptor are ready. In a second terminal, the operator copies and executes the complete shell command printed immediately below `Attach from another terminal:`. The command contains concrete, shell-quoted environment values and invokes the selected Intercom executable with arguments equivalent to `intercom codex attach --name reviewer`.

The generated command preserves the service's state directory, broker socket, Intercom executable, Codex executable, and optional `CODEX_HOME`. Provider authentication variables are not copied into the output and must already be available in the attachment terminal.

The attach command changes to the managed project directory and replaces itself with the generated `codex resume --remote` operation. The launcher remains in the foreground. `--name` may be omitted from the launcher when `INTERCOM_NAME` or the selected directory basename supplies the required name; `intercom codex attach` always requires `--name`.

### Verification

The attached terminal displays the resumed managed thread. Another terminal lists the managed peer while the service is running:

```console
$ intercom peers
reviewer
```

The output contains all other peers in bytewise sorted order. Existing Claude peer names therefore appear in the same output when they remain connected.

### Errors

| Condition | Result |
|---|---|
| App-server, managed-thread, tool, broker, proxy, descriptor, or readiness-output initialization fails. | The launcher does not print an attachment command, stops its service group, and exits nonzero. Standard error identifies the failed boundary. |
| The helper cannot resolve Codex, create its process session, publish the marker, or execute app-server. | `intercom` reports `resolve Codex executable`, `create app-server process session`, `publish app-server process session`, or `exec Codex app-server`; the launcher reports that app-server exited before readiness and exits nonzero. |
| The marker does not appear before the startup timeout. | The launcher reports `app-server did not establish its process session after Ns`, performs pre-marker direct-child cleanup, and exits with status 1. |
| The marker value differs from the direct-child PID. | The launcher reports `app-server published process session VALUE, want PID`, marks cleanup failed, invokes session cleanup against the expected PID, and exits nonzero. |
| The marker is valid but the socket does not appear before the startup timeout. | The launcher reports `app-server was not ready after Ns`, stops the process session, and exits with status 1. |
| Another service owns the peer binding or live broker-and-peer descriptor. | Startup reports `peer is already managed` or `Codex instance is already live`; the existing service remains active. |
| Another live broker peer owns `reviewer`. | Broker registration reports `name_taken`; the launcher exits without readiness output. |
| The generated attachment command cannot find the descriptor under its printed environment. | `intercom codex attach` reports that no live instance exists or that the descriptor is stale. The launcher must still be running and the complete printed command must be used. |
| The attaching Codex CLI version differs from the running app-server version. | Attachment reports an incompatible client version. The printed `CODEX_BIN` selection must be used, or the launcher must restart with the upgraded Codex CLI. |
| One TUI is already connected. | The second attachment receives a busy or HTTP conflict error. The first TUI and service remain active. |

### Notes

The readiness block is not printed before startup completes. An adapter, broker, thread, proxy, descriptor, or output failure prevents the block and terminates the service group.

One TUI may attach to an instance at a time. A simultaneous second attachment receives a busy error. A connection that does not complete initialization and managed-thread resume within 30 seconds is closed and frees the slot. Exiting or disconnecting the attached TUI frees the slot and leaves the launcher, app-server, broker peer, queued deliveries, durable binding, live descriptor, and private sockets running.

#### TUI active-turn behavior

The attached TUI is a current-thread interface rather than a general Codex session manager. Ordinary prompts, current-thread history, approvals, requested input, documented metadata updates, and project-scoped skill, hook, and file search remain available. Turn interruption and steering apply only to a turn owned by that TUI. Pressing Tab while a turn is active queues a follow-up locally; the TUI submits it after the active turn completes. Pressing Enter to steer an Intercom-owned turn sends a rejected `turn/steer` request. Rollback receives the same class of rejection. Client handling of either request error is version-dependent; `codex-cli` 0.144.4 exits with status 1, while the service and Intercom-owned turn remain active and the TUI can be reattached. The proxy also rejects `/new`, `/fork`, thread archive, unarchive, and deletion, `/review`, manual `/compact`, shell escape, goal mutation, raw history injection, guardian-denied action approval, background-terminal mutation, realtime mutation, and every unlisted protocol operation.

The launcher owns the app-server. A separately started app-server is supported only through the lower-level `intercom codex --app-server ENDPOINT` command. The launcher can adopt or fork a non-archived, non-ephemeral root session whose source is Codex CLI or VS Code, whose status is `idle` or `notLoaded`, and whose canonical working directory equals the managed directory. Web, desktop-app, child, active, failed, ephemeral, archived, and other source kinds or statuses are not eligible. The attached TUI controls only the selected managed thread.

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

### Errors

| Condition | Result |
|---|---|
| `list_peers` cannot register or communicate with the selected broker. | The tool returns an error result and no peer list. |
| The destination is absent when the broker handles `send_message`. | The tool returns `no_such_peer`; no offline delivery is retained. |
| The destination equals the sending peer. | The tool returns `no_self_send`; no delivery occurs. |
| The destination name or message body violates the documented grammar or byte limits. | The tool returns a validation error before broker delivery. |
| The broker accepts the delivery but the destination disconnects before observing it. | The sender still receives the accepted result; Intercom provides no observation receipt or retry. |

### Notes

The destination must be connected when the broker handles the send. Sending to the caller is rejected. A model can create a message loop by repeatedly replying; Intercom performs no loop detection.

Message bodies are model instructions. System and developer instructions retain precedence at each receiving agent.

### See also

[Tool reference](REFERENCE.md#agent-tools), [broker protocol](BROKER_PROTOCOL.md)

## 5. Resume, adopt, fork, or replace a Codex thread

### Purpose

This chapter provides separate procedures for TUI attachment, service restart, session discovery, adoption, fork, binding replacement, new-thread selection, and execution-policy selection.

### Prerequisites

`intercom-codex-project`, `intercom`, and `codex` must be available. Each task lists its additional service, binding, or source-session prerequisites.

### Concepts

The binding is stored in `$INTERCOM_DIR/codex/NAME.json`; the default base is `$HOME/.claude-intercom`. Codex conversation data remains under the app-server-reported `CODEX_HOME`.

The durable binding contains the managed thread identity and compatibility metadata. It survives launcher shutdown and does not contain the client socket, service PID, or TUI attachment state. The live descriptor selects one running service by broker identity and peer name. It contains the current client endpoint and is removed during clean service shutdown.

Adoption preserves the selected thread ID and transfers operational ownership to Intercom. Fork creates and manages another thread while leaving the source usable. `--new` creates and binds an unrelated thread. Adoption or fork of a thread other than the saved binding requires `--replace-binding`. A replacement locks the prior bound thread before validation and retains that lock through live-descriptor publication, readiness output, and final startup release. Success releases it after commit; failure releases it after leaving or restoring the prior binding. Fork from the prior thread uses the retained lock for its source and separately locks the returned fork.

### Procedure

#### 5.1 Attach to a running service

##### Purpose

This task attaches or reattaches one stock Codex TUI to a running managed service.

##### Prerequisites

The launcher must remain running and must have printed its readiness block. No other TUI may be attached to the same service.

##### Concepts

The name-based attachment command resolves the live descriptor for the selected broker and peer. A TUI disconnect leaves that descriptor and the service running. The direct `codex resume --remote` command contains an ephemeral socket and is valid only for the service lifetime that printed it.

##### Procedure

The attachment terminal executes the complete command printed under `Attach from another terminal:` in the launcher's readiness block. When that terminal already selects the same `INTERCOM_DIR`, `INTERCOM_SOCKET`, `CODEX_BIN`, and optional `CODEX_HOME`, the equivalent name-based command is:

```sh
intercom codex attach --name reviewer
```

##### Verification

The TUI displays the managed thread, and the launcher writes a `Codex TUI attached` diagnostic. Exiting the TUI produces a detach diagnostic while `intercom peers` continues to list `reviewer`.

##### Errors

| Condition | Result |
|---|---|
| The service is stopped, has not reached readiness, or uses another state directory, broker socket, or peer name. | Attachment reports `no live Codex instance named ...`. |
| The descriptor records a process that no longer exists. | Attachment reports that the descriptor is stale. |
| Another TUI owns the attachment slot. | The second connection receives a busy or HTTP conflict error; the first attachment remains active. |
| The attaching Codex CLI version differs from the service app-server version. | Attachment reports an incompatible client version and exits. |
| `codex-cli` 0.144.4 submits Enter while an Intercom-delivered turn owns the controller. | The proxy returns JSON-RPC error -32600. The TUI exits with status 1; the service and active turn continue. |
| `codex-cli` 0.144.4 requests rollback. | The proxy returns JSON-RPC error -32600. The TUI exits with status 1; the service continues. |

##### Notes

With no active turn and an empty composer, `Ctrl-C` exits the stock TUI and detaches it from the service. Pressing Tab while a turn is active queues a follow-up in the stock TUI. The TUI submits that message after the active turn completes. Steering and interruption are supported only for the active turn owned by the attached TUI. A TUI that exits after a rejected request can be reattached with the same name-based command.

A supported human-interaction reverse request reaches the attached TUI only when its `threadId` names the managed root or a descendant already recorded by managed lifecycle or tool authorization. An owned request waits for service readiness and for earlier controller-gated lifecycle notifications before TUI delivery. A malformed or unrelated supported request receives the applicable fixed headless-policy response. It does not reach the managed TUI and does not record managed activity or suspend the managed watchdog. The active-turn or persistent-goal inactivity interval is suspended only for owned requests awaiting TUI input.

##### See also

[Reference: `intercom codex attach`](REFERENCE.md#intercom-codex-attach), [proxy errors](REFERENCE.md#intercom-codex)

#### 5.2 Restart the saved thread

##### Purpose

This task starts a new service group for the thread stored in an existing durable binding.

##### Prerequisites

The prior launcher must be stopped. `$INTERCOM_DIR/codex/reviewer.json` must contain a binding created by an earlier ready service.

##### Concepts

Resume requires the same peer name, canonical working directory, `CODEX_HOME`, state schema, and tool-contract version. App-server user-agent and Codex-version fields are diagnostic and are refreshed after successful validation. Restart creates new private sockets and a new live descriptor.

After resume, the adapter calls `thread/goal/get` before broker readiness. A null goal does not reserve the scheduler. Status `active` or an unknown nonempty status reserves it; `paused`, `blocked`, `usageLimited`, `budgetLimited`, and `complete` release it. Error -32601 leaves initial state notification-driven. Another error or an invalid goal identity or empty status fails startup. Ordered goal update and clear notifications supersede an earlier read. A Codex-owned continuation may begin during resume; readiness does not admit a TUI turn or queued Intercom delivery until that continuation reaches terminal processing and its persistent-goal reservation releases.

##### Procedure

The same launcher selection resumes the binding:

```sh
intercom-codex-project --name reviewer --cwd .
```

The attachment terminal executes the newly printed command under `Attach from another terminal:`.

##### Verification

The binding retains the same thread ID:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
sed -n '/"peer"/p; /"threadId"/p; /"materialized"/p' "$state_dir/codex/reviewer.json"
```

The attached TUI displays the prior conversation.

##### Errors

| Condition | Result |
|---|---|
| The prior launcher or another adapter still holds the peer lock. | Startup reports `peer is already managed`; the running service remains unchanged. |
| The selected peer, canonical directory, `CODEX_HOME`, state schema, or tool contract differs from the binding. | Startup reports the identity or contract mismatch and directs deliberate replacement through `--new`. |
| The saved thread is active, failed, ephemeral, or violates the managed approval, reviewer, runtime-root, or sandbox invariants. | Startup reports the failed managed-thread invariant and leaves the binding unchanged. |
| The saved thread is already locked by another Intercom adapter using the same `CODEX_HOME`. | Startup reports that the thread is already managed by Intercom. |
| `thread/goal/get` returns an error other than -32601, or returns an invalid managed identity or empty status. | Startup reports the persistent-goal error and leaves the binding unchanged. Error -32601 continues with notification-driven goal state. |

##### Notes

The binding becomes materialized after a terminal result for its first managed turn is confirmed through `thread/read`. When a pre-materialization restart finds no Codex rollout, Intercom replaces that pending binding with a new thread. A Codex upgrade does not require `--new`; the launcher must restart so the app-server and attached TUI use the same Codex version.

##### See also

[State files](REFERENCE.md#files), [Codex lifecycle](ARCHITECTURE.md#codex-adapter-lifecycle)

#### 5.3 List resumable sessions

##### Purpose

This task lists Codex CLI and VS Code sessions eligible for adoption or fork.

##### Prerequisites

Codex authentication and the intended `CODEX_HOME` must be available. The selected `--cwd` must name an existing project directory.

##### Concepts

Eligible records are non-archived, non-ephemeral root sessions from Codex CLI or VS Code with status `idle` or `notLoaded`. Records are sorted newest first. The default list requires an exact canonical working-directory match. `--all-sessions` removes only that directory filter; it does not admit other sources, archived records, child threads, ephemeral threads, or other statuses.

##### Procedure

The current project's eligible records are written without starting an adapter or broker peer:

```sh
intercom-codex-project --cwd . --list-sessions
```

Records from every working directory are requested with:

```sh
intercom-codex-project --cwd . --list-sessions --all-sessions
```

##### Verification

Status 0 denotes a complete list, including an empty list. Each nonempty output line contains four tab-separated fields: complete thread ID, UTC RFC 3339 recency timestamp, sanitized working directory, and sanitized title.

##### Errors

| Condition | Result |
|---|---|
| `--cwd` cannot be resolved to an existing directory. | The launcher exits nonzero before listing. |
| App-server startup, initialization, or paginated `thread/list` fails. | The launcher reports the app-server or session-list error, stops app-server, and exits nonzero. |
| `--list-sessions` is combined with `--name`, yolo selection, an unknown option, a positional argument, or another adapter-only option. | The launcher reports that list mode does not accept the adapter argument and exits with status 2 before child creation. |

##### Notes

List mode does not read or change an Intercom binding. An all-directory record is not eligible for management under another `--cwd`; its displayed directory must be selected explicitly for adoption or fork.

##### See also

[Reference: `intercom codex sessions`](REFERENCE.md#intercom-codex-sessions)

#### 5.4 Adopt an existing session

##### Purpose

This task resumes an ordinary Codex CLI or VS Code root session under its existing thread ID and transfers operational ownership to Intercom.

##### Prerequisites

The source TUI or IDE process must be stopped. The source session must satisfy the eligibility rules in task 5.3 and have the exact canonical `--cwd`. A binding for another thread requires task 5.6.

##### Concepts

Adoption injects a required per-service MCP configuration for `send_message` and `list_peers`, validates both tools, and writes the binding only after managed-thread, tool, broker, and proxy validation succeeds. The MCP token and socket last for one service lifetime and are reinjected on cold resume; no permanent Codex configuration is written.

The picker status describes the dedicated discovery app-server, not persistent-goal state or ownership by an ordinary Codex process. An eligible `idle` or `notLoaded` session may contain an active persistent goal. After resume, the adapter calls `thread/goal/get` before broker readiness. A null goal does not reserve the scheduler. Status `active` or an unknown nonempty status reserves it; `paused`, `blocked`, `usageLimited`, `budgetLimited`, and `complete` release it. Error -32601 leaves initial state notification-driven; another error or an invalid goal identity or empty status fails startup. Ordered goal update and clear notifications supersede an earlier read.

Adoption may begin a Codex-owned continuation before broker readiness is printed. The continuation retains the controller until it completes; inbound Intercom messages remain queued. Its Intercom tool calls wait for broker registration during startup.

##### Procedure

The ID-less form prints a numbered newest-first picker and reads one selection:

```sh
intercom-codex-project --name reviewer --cwd . --adopt
```

An already recorded thread ID can be supplied explicitly:

```sh
session_id=$(intercom-codex-project --cwd . --list-sessions | awk -F '\t' 'NR == 1 { print $1 }')
intercom-codex-project --name reviewer --cwd . --adopt="${session_id:?no resumable session found}"
```

##### Verification

The readiness block is printed, the binding's `threadId` equals the selected source ID, and the attached history is the source conversation:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
sed -n '/"threadId"/p; /"toolTransport"/p' "$state_dir/codex/reviewer.json"
```

##### Errors

| Condition | Result |
|---|---|
| Interactive selection has no eligible record, lacks a terminal, is canceled, or selects another working directory. | Selection exits nonzero and no adapter starts. |
| The discovery app-server reports the source thread as archived, ephemeral, a child thread, active, failed, from another source kind, or in another canonical working directory. | Adoption reports an eligibility error and leaves the binding unchanged. An active persistent goal stored in an otherwise `idle` or `notLoaded` thread is resumed rather than rejected. |
| Another Intercom adapter owns the source thread. | Adoption reports that the thread is already managed by Intercom. |
| Another thread is already bound to the peer and `--replace-binding` is absent. | Adoption reports `use --replace-binding`; the existing binding remains unchanged. |
| Managed MCP configuration or required-tool validation fails. | Adoption exits before the provisional binding write; the prior binding remains unchanged. |
| `thread/goal/get` returns an error other than -32601, or returns an invalid managed identity or empty status. | Adoption fails before broker readiness and leaves the prior binding unchanged. Error -32601 continues with notification-driven goal state. |

##### Notes

Ordinary Codex processes do not acquire the Intercom thread lock. The stopped source TUI or IDE must not resume the adopted session while Intercom manages it. Concurrent writes can violate lifecycle, tool-routing, and conversation-order invariants. Forking is required when ordinary access to the source must continue.

Readiness can be printed while a resumed persistent-goal turn remains active. An attached TUI can inspect that turn but does not own it; a new TUI turn and queued Intercom deliveries wait until Codex publishes terminal completion.

##### See also

[Task 5.3](#53-list-resumable-sessions), [task 5.5](#55-fork-an-existing-session), [task 5.6](#56-replace-a-saved-binding)

#### 5.5 Fork an existing session

##### Purpose

This task creates a new managed thread from an ordinary Codex CLI or VS Code root session while preserving the source.

##### Prerequisites

The source session must satisfy the eligibility rules in task 5.3 and have the exact canonical `--cwd`. A binding for another thread requires task 5.6. The ordinary source process may remain available after fork creation.

##### Concepts

App-server creates a new thread whose fork ancestry names the source. Intercom locks the source while it reads and forks it, validates the returned ancestry, locks the returned thread ID, and leaves the source ID and conversation unchanged. Replacement fork from the prior bound thread retains that existing lock through commit or rollback.

##### Procedure

The ID-less form opens the session picker:

```sh
intercom-codex-project --name reviewer --cwd . --fork-from
```

An explicit selection can use the first listed project session:

```sh
session_id=$(intercom-codex-project --cwd . --list-sessions | awk -F '\t' 'NR == 1 { print $1 }')
intercom-codex-project --name reviewer --cwd . --fork-from="${session_id:?no resumable session found}"
```

##### Verification

The readiness block is printed. The binding's `threadId` differs from the selected source ID, and the attached history contains the source conversation.

##### Errors

| Condition | Result |
|---|---|
| The source fails the archive, source-kind, ephemeral, root-thread, status, or canonical-directory eligibility rules. | Fork reports an eligibility error and leaves the binding unchanged. |
| App-server returns an empty or unchanged thread ID, or ancestry does not identify the source. | Fork reports an invariant error and leaves the binding unchanged. Codex storage may retain the created but unbound thread. |
| Another thread is already bound to the peer and `--replace-binding` is absent. | Fork reports `use --replace-binding`; the existing binding remains unchanged. |
| Managed MCP or required-tool validation fails. | Fork exits before the provisional binding write; the prior binding remains unchanged. Codex storage may retain the created but unbound thread. |

##### Notes

Fork is the selection for simultaneous or later ordinary use of the source session. Intercom temporarily locks but does not modify the source thread. After ordinary fork startup, it manages only the returned fork ID. Replacement fork from the prior bound thread holds both the retained prior lock and returned fork lock until commit or rollback completes.

##### See also

[Task 5.3](#53-list-resumable-sessions), [task 5.4](#54-adopt-an-existing-session), [task 5.6](#56-replace-a-saved-binding)

#### 5.6 Replace a saved binding

##### Purpose

This task authorizes adoption or fork to replace a peer binding that names another thread.

##### Prerequisites

The existing service must be stopped. The replacement source must satisfy the adoption or fork prerequisites. Exact adoption also requires the source TUI or IDE to be stopped.

##### Concepts

`--replace-binding` is explicit authorization, not an independent selection mode. The adapter acquires the prior binding's thread lock before app-server validation and retains it while it acquires and validates the selected or returned replacement thread. Adoption or fork validates the selected thread, required tools, broker registration, and proxy before provisionally writing the replacement. Live-descriptor publication, readiness output, and final startup release commit that replacement. The prior lock is released only after commit, or after rollback on failure. Fork from the prior bound thread reuses the retained lock as its source lock and also acquires the new fork lock.

##### Procedure

Interactive exact adoption with replacement uses:

```sh
intercom-codex-project --name reviewer --cwd . --adopt --replace-binding
```

Interactive fork with replacement uses:

```sh
intercom-codex-project --name reviewer --cwd . --fork-from --replace-binding
```

##### Verification

After readiness, the binding contains the selected adopted ID or returned fork ID:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
sed -n '/"threadId"/p; /"toolTransport"/p' "$state_dir/codex/reviewer.json"
```

##### Errors

| Condition | Result |
|---|---|
| `--replace-binding` is supplied without `--adopt` or `--fork-from`. | The launcher exits with status 2 before child creation. |
| Another Intercom adapter holds the prior binding's thread lock. | Startup reports `lock prior thread ID during replacement`; validation does not begin and the saved binding remains unchanged. |
| Selection, source validation, managed-thread validation, required-tool validation, broker registration, or proxy creation fails. | The prior binding remains unchanged. |
| Live-descriptor publication, readiness-output writing, or final startup release fails after the provisional write. | The launcher exits nonzero and restores the prior binding, or removes the replacement when no prior binding existed. No usable attachment command is printed. |
| Rollback of a provisional replacement also fails. | The replacement rollback diagnostic is joined to the startup error; the durable binding requires inspection. |
| Fork creation succeeds but a validation before the provisional binding write fails. | The prior binding remains unchanged; Codex storage may retain an unbound fork. |

##### Notes

Selecting the thread ID already stored in the binding is an idempotent resume and does not require replacement authorization or a second thread lock. Replacement changes only the Intercom binding. It does not delete the previous Codex rollout or conversation. Thread locks coordinate Intercom adapters only; an ordinary Codex process does not honor them.

##### See also

[Task 5.4](#54-adopt-an-existing-session), [task 5.5](#55-fork-an-existing-session), [binding state](REFERENCE.md#managed-codex-binding)

#### 5.7 Start a new managed thread

##### Purpose

This task creates an unrelated managed thread and replaces the peer's durable binding.

##### Prerequisites

The existing service for the peer must be stopped. Codex authentication and the selected project directory must be available.

##### Concepts

`--new` starts and validates a new thread, then commits its ID before broker registration, proxy creation, live-descriptor publication, and readiness output. The former Codex conversation remains in Codex storage.

##### Procedure

The deliberate new-thread selection is:

```sh
intercom-codex-project --name reviewer --cwd . --new
```

##### Verification

The readiness block is printed and the binding contains a thread ID different from the former binding. The attached TUI begins on the new thread.

##### Errors

| Condition | Result |
|---|---|
| `--new` is combined with adoption, fork, or list mode. | The launcher exits with status 2 before child creation. |
| New-thread creation or managed-thread validation fails. | The launcher exits before commit and retains the prior binding. |
| For `--new`, broker registration, proxy creation, descriptor publication, or readiness-output writing fails after the new binding write. | The launcher exits nonzero and the new binding remains stored. Adoption and fork use the rollback rules in task 5.6. |

##### Notes

`--new` replaces only the Intercom binding. It does not archive or delete the previous thread. A later launcher invocation without `--new` resumes the newly committed binding.

##### See also

[Reference: `intercom-codex-project`](REFERENCE.md#intercom-codex-project), [task 5.6](#56-replace-a-saved-binding)

#### 5.8 Select the execution policy

##### Purpose

This task selects workspace-write or danger-full-access sandboxing for one managed service lifetime.

##### Prerequisites

The service must be starting or restarting. Changing policy requires the running launcher to stop before another launcher starts for the same peer.

##### Concepts

Both policies use approval policy `never`, approvals reviewer `user`, and the canonical managed directory as the only runtime workspace root. The default uses workspace-write sandboxing. Yolo mode uses `danger-full-access`. Execution policy is service configuration rather than persistent binding identity.

##### Procedure

The default policy requires no policy option:

```sh
intercom-codex-project --name reviewer --cwd .
```

Danger-full-access uses either equivalent spelling:

```sh
intercom-codex-project --name reviewer --cwd . --yolo
```

```sh
intercom-codex-project --name reviewer --cwd . --dangerously-bypass-approvals-and-sandbox
```

The same option is valid with `--new`, `--adopt`, or `--fork-from`.

##### Verification

The readiness block prints `Execution policy: workspace-write` or `Execution policy: danger-full-access`. Its name-based and direct attachment commands include the required Codex CLI policy automatically.

##### Errors

| Condition | Result |
|---|---|
| Yolo selection is combined with `--list-sessions`. | The launcher reports that list mode does not accept the adapter argument and exits with status 2. |
| The attached TUI attempts to change the pinned approval, reviewer, sandbox, directory, or runtime-root policy. | The proxy drops or overrides the disallowed values and reasserts the service policy. |
| The app-server returns a thread whose policy does not match the selected service configuration after policy reassertion. | Startup or turn validation reports a managed-thread invariant error. |

##### Notes

The policy applies to TUI turns and Intercom-delivered turns. Omitting yolo on a later restart returns the thread to workspace-write. The generated attachment command must be used so the stock TUI receives the matching policy selection.

##### See also

[Reference: execution-policy options](REFERENCE.md#intercom-codex), [security](ARCHITECTURE.md#security)

### Verification

Every service-starting task completes only when the readiness block is printed. The state record identifies the durable binding:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
sed -n '/"peer"/p; /"threadId"/p; /"materialized"/p' "$state_dir/codex/reviewer.json"
```

The live descriptor directory contains a descriptor only while the service remains attachable:

```sh
state_dir=${INTERCOM_DIR:-"$HOME/.claude-intercom"}
ls "$state_dir/codex/live"
```

### Errors

| Condition | Result |
|---|---|
| Another service holds the peer state lock. | A second service for that peer cannot start until the first stops. |
| Another Intercom adapter sharing `CODEX_HOME` holds the selected thread lock. | Adoption, fork, or resume fails even when the adapters use different Intercom directories or peer names. |
| A service-starting task fails before readiness. | No attachment command is usable. The task-local error table states whether the prior or replacement binding remains stored. |

### Notes

A changed project symlink that resolves to the same canonical directory remains compatible. A different canonical directory requires a separate binding, `--new`, or explicit replacement with a session whose working directory matches the new directory.

The app-server protocol has no feature or schema-version negotiation. The adapter accepts a user-agent version of 0.144.1 or later and validates its consumed request, response, session-list, fork, lifecycle, managed-thread, sandbox, dynamic-tool, and MCP contracts during startup and operation. Unknown additive object fields are ignored. A Codex upgrade does not require `--new`; a changed consumed contract fails at the affected validation. A TUI attached to a running service must use the same Codex version as that service's app-server. The launcher must restart after a Codex upgrade before the upgraded TUI attaches.

### See also

[State files](REFERENCE.md#files), [Codex lifecycle](ARCHITECTURE.md#codex-adapter-lifecycle)

## 6. Stop a managed Codex peer

### Purpose

This procedure distinguishes TUI detachment from termination of the adapter/proxy and app-server service group.

### Prerequisites

The launcher must be running in the foreground.

### Concepts

Exiting the Codex TUI closes only its client connection. The service remains registered with the broker, retains queued deliveries and its live descriptor, and accepts a later `intercom codex attach --name NAME` command.

Service termination is a separate launcher operation. The launcher sends `SIGTERM` to the adapter/proxy first. The adapter removes its live descriptor, marks the controller unavailable, stops broker reconnection, closes its broker connection, interrupts an active turn when necessary, and drains the turn and reverse-request handlers within one shutdown budget. The proxy then closes its listener and attached TUI. An adapter that survives the launcher timeout receives `SIGKILL` and a second timeout wait.

The launcher starts Codex in a dedicated process session and keeps the adapter outside that session. After adapter shutdown, the hidden session-cleanup helper obtains candidate PIDs and states from `ps`, excludes zombies, and verifies session membership with `getsid(2)`. It repeats the membership check immediately before each signal, sends the signal to descendants before the leader, and repeats enumeration until the session is empty or the phase deadline expires. A PID receives at most one successful signal in each phase; a failed verification or signal is retried on a later pass. The `SIGTERM` and `SIGKILL` phases have separate full timeout intervals measured with a monotonic clock. Enumeration and signaling time count against the applicable interval, and each phase enumeration receives the remaining interval as its context deadline.

Transient process enumeration, membership, and signaling errors are retried until the applicable deadline. The post-`SIGTERM` leader classification and final `SIGKILL` inspection are each bounded by the smaller of one second and the configured timeout. Final inspection failure, a persistent signal failure, or surviving verified PIDs makes cleanup fail. A cleanup-helper failure also causes a final `SIGKILL` attempt against the still-owned direct child. A descendant that changes process group remains covered. A descendant that calls `setsid(2)` leaves the cleanup session. The final `getsid(2)` verification and `kill(2)` call are not atomic; PID reuse between those operations remains a small best-effort race.

A termination signal received before the session marker exists sends `SIGTERM` only to the direct child while the launcher watches for marker publication. Publication switches to session cleanup. A child that remains alive without a marker for one timeout receives direct-child `SIGKILL`. The helper publishes the marker before it executes app-server, so an app-server descendant cannot exist before this transition.

Clean service shutdown closes an attached TUI connection and removes every private socket entry and its runtime directory. The durable binding and Codex conversation remain available for a later service restart.

### Procedure

With no active turn and an empty composer, `Ctrl-C` exits the stock TUI and detaches it without stopping the service. Closing the TUI terminal has the same service-level effect. The same running service accepts reattachment:

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

### Errors

| Condition | Result |
|---|---|
| Only the TUI exits or disconnects. | The service does not stop; the peer remains registered and attachable. |
| Adapter shutdown cleanup fails. | The launcher propagates the nonzero adapter status after attempting to stop app-server. |
| App-server exits unexpectedly while the adapter runs. | The launcher stops the adapter and propagates the nonzero app-server status; unexpected app-server status 0 maps to 1. |
| The adapter remains alive after its shutdown timeout. | The launcher writes `adapter did not stop; killing it`, sends `SIGKILL`, and retains the initiating exit status. |
| The adapter remains alive after its post-`SIGKILL` timeout. | The launcher writes `adapter survived SIGKILL`, marks cleanup failed, and retains an existing nonzero or signal status; an otherwise successful exit becomes status 1. |
| The Codex session leader remains after the app-server `SIGTERM` deadline. | The cleanup helper writes `intercom-codex-project: app-server did not stop; killing it` and begins an independent full-timeout `SIGKILL` phase. |
| The Codex session leader exits, but another verified member remains after the `SIGTERM` deadline. | The cleanup helper writes `intercom-codex-project: app-server descendants did not stop; killing them` and begins an independent full-timeout `SIGKILL` phase. |
| Process enumeration, `getsid(2)`, or signaling continues to fail through cleanup. | The helper reports the precise `enumerate processes`, `inspect process PID session`, `reverify process PID before SIGNAL`, or `signal process PID with SIGNAL` failure and returns nonzero. The launcher marks cleanup failed and attempts direct-child `SIGKILL`. |
| Final session inspection fails after the `SIGKILL` deadline. | `intercom` reports `inspect app-server process session SID after SIGKILL`; the launcher marks cleanup failed and attempts direct-child `SIGKILL`. |
| A persistent `SIGKILL` signaling failure remains after final inspection. | `intercom` reports `stop app-server process session SID with SIGKILL`; the launcher marks cleanup failed and attempts direct-child `SIGKILL`. |
| Verified members survive the `SIGKILL` deadline. | `intercom` reports `app-server process session SID still has processes after SIGKILL: PID, ...`; the launcher marks cleanup failed and attempts direct-child `SIGKILL`. |
| The session-cleanup helper reports success while the direct child remains live. | The launcher writes `app-server process-session cleanup left its direct child running; killing it`, marks cleanup failed, and sends fallback `SIGKILL`. |
| The direct child survives fallback `SIGKILL` after session-helper failure. | The launcher writes `app-server direct child survived fallback SIGKILL`; cleanup remains failed. |
| A pre-marker child survives `SIGTERM` for one timeout. | The launcher writes `app-server did not stop before creating its process session; killing it` and sends direct-child `SIGKILL`. |
| A pre-marker child survives the post-`SIGKILL` timeout. | The launcher writes `app-server direct child survived SIGKILL`; cleanup remains failed. |
| Private runtime-directory removal fails. | The launcher writes `could not remove runtime directory PATH`; cleanup remains failed. |
| Descriptor removal fails during shutdown. | The adapter retries removal after controller return and exits nonzero if the second attempt fails. |

### Notes

The launcher maps the first `SIGHUP`, `SIGINT`, or `SIGTERM` to status 129, 130, or 143 and ignores repeated terminal signals during cleanup. Normal adapter status is propagated. A nonzero app-server status is propagated; an unexpected zero app-server status maps to 1. Cleanup failure changes status 0 to 1 but does not replace an existing nonzero child status or the status selected by the first terminal signal.

Stopping during a starting or active turn interrupts that delivery. Intercom does not retry it. Deliveries waiting in the in-memory queue are lost when the adapter exits. A turn whose result matters must reach terminal completion before the service group stops.

A hard kill, shell failure, or host failure can prevent descriptor and runtime-directory cleanup. A Codex descendant that calls `setsid(2)` leaves the launcher-owned process session and is not terminated by session cleanup. Session membership is reverified immediately before signaling, but `getsid(2)` and `kill(2)` are separate calls; the remaining PID-reuse window cannot be closed by the launcher. Reuse of the original leader PID as an unrelated new session ID after the original session becomes empty is also a very-low-probability best-effort boundary. The attach command reports a stale descriptor when its recorded process no longer exists. A later launcher publication for the same broker and peer replaces that stale descriptor without changing the durable binding.

### See also

[Launcher exit status](REFERENCE.md#intercom-codex-project), [failure semantics](ARCHITECTURE.md#failure-semantics)

## 7. Run multiple managed peers

### Purpose

This procedure runs multiple managed Codex peers in one broker group or separates them into independent routing groups.

### Prerequisites

Each peer requires a distinct name, managed thread, and existing project directory. An isolated group additionally requires a distinct Unix socket path whose parent directory is writable by the current user.

### Concepts

`INTERCOM_SOCKET` selects both the broker socket and its sibling `.lock` file. Every adapter and diagnostic command in one group must receive the same value. `INTERCOM_DIR` does not override an explicit `INTERCOM_SOCKET`.

One broker group supports multiple concurrently running managed Codex instances when every peer name and managed thread is distinct. Each launcher creates a separate random runtime directory, upstream app-server socket, downstream client socket, MCP-bridge socket, managed-thread lock, and live descriptor. Each instance accepts one TUI; the one-TUI limit is per instance rather than per machine.

The attach command selects a descriptor by the canonical broker socket and peer name. Its terminal must therefore inherit the same `INTERCOM_SOCKET` and `INTERCOM_DIR` selection as the matching launcher.

### Procedure

#### 7.1 Default broker group

Both service terminals start in the same parent directory containing the existing `project-a` and `project-b` directories. They start distinct peers without setting `INTERCOM_SOCKET`:

```sh
intercom-codex-project --name reviewer --cwd project-a
```

```sh
intercom-codex-project --name planner --cwd project-b
```

Each service prints its own attachment command. Two additional terminals execute those respective commands. The launchers create separate private socket directories; no port or endpoint allocation is required.

#### 7.2 Isolated broker group

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

The default group is queried without an `INTERCOM_SOCKET` assignment:

```console
$ intercom peers
planner
reviewer
```

The selected group is queried with the same environment:

```console
$ group_dir=$(cd .intercom-runtime && pwd -P)
$ INTERCOM_SOCKET="$group_dir/broker.sock" intercom peers
planner
reviewer
```

The output contains the other live peers in bytewise order. An invocation with `INTERCOM_SOCKET` unset queries the default group instead and cannot resolve the selected group’s live attachment descriptors.

### Errors

| Condition | Result |
|---|---|
| The selected socket's parent directory is absent, not writable, or not searchable. | Broker creation or connection fails with the path error. |
| Two peers in one broker group select the same peer name. | The later registration receives `name_taken`. |
| Two launchers under one `INTERCOM_DIR` select the same peer name on different broker sockets. | The later launcher reports `peer is already managed` because the binding lock is shared. |
| Two Intercom adapters sharing one `CODEX_HOME` select the same managed thread. | The later adapter reports that the thread is already managed by Intercom. |
| The attachment terminal selects another `INTERCOM_SOCKET` or `INTERCOM_DIR`. | Name-based attachment reports no matching live instance. |

### Notes

`INTERCOM_SOCKET` is inherited by the launcher adapter. It does not select either private Codex socket; each launcher creates and owns its app-server and client socket separately.

Two services in one broker cannot share a peer name. The broker registration, durable binding lock, and live descriptor claim all preserve single ownership. Separately created default threads with distinct names support ordinary multi-instance operation under one `INTERCOM_DIR`. Adoption or a saved binding that selects the same Codex thread as another live Intercom adapter is rejected by the thread lock even when peer names differ.

Broker isolation does not isolate managed Codex binding files or the default broker log. Codex peers with the same name and different sockets still contend for `$INTERCOM_DIR/codex/NAME.lock`. Distinct `INTERCOM_DIR` values provide separate Codex bindings and default logs. They do not isolate thread ownership: adapters that use the same `CODEX_HOME` share its thread-lock namespace.

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

Durable bindings and published live descriptors are inspected separately:

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

The name command prints the expected peer name. The peer command reaches the selected broker. A running Codex service has both a durable binding and a live descriptor; a stopped service retains only its durable binding. A configured Claude peer appears as connected under `/mcp`.

### Errors

The following table maps diagnostics to exact operating conditions.

| Diagnostic | Condition | Remedy |
|---|---|---|
| `invalid peer name` | The selected name is empty, longer than 64 bytes, or contains a character outside ASCII letters, digits, `_`, and `-`. | A valid `INTERCOM_NAME` or `--name` is required. |
| `name_taken` | A live peer has registered the same name. | The duplicate must stop, or another name must be selected. |
| `no_such_peer` | The destination is absent when the broker handles the send. | A retry is appropriate only after `list_peers` reports the reconnected peer. |
| `no_self_send` | The destination equals the sender. | Another destination peer is required. |
| `broker did not start within retry budget` | Broker spawning succeeded but the socket never accepted the client within the fixed dial sequence. | The broker log, socket directory, and `INTERCOM_BROKER_BIN` identify the startup failure. |
| `unsupported app-server version` | The app-server user agent reports a version earlier than `0.144.1`. | Codex CLI 0.144.1 or later is required. |
| `Codex client version ... is incompatible` | The attaching TUI version differs from the running app-server version. | The attachment terminal must select the Codex CLI used to start the service, or the launcher must restart with the upgraded CLI. |
| `peer is already managed` | Another `intercom codex` process holds the peer state lock. | The other service group must stop, or another peer name and state binding must be selected. |
| `Codex instance is already live` | Another service owns the same broker-and-peer live descriptor. | The existing service must stop, or another peer name must be selected. |
| `no live Codex instance named ...` | The service is stopped, has not reached readiness, uses another broker identity, uses another `INTERCOM_DIR`, or has another peer name. | The launcher readiness block and matching environment identify the attachable instance. |
| `descriptor is stale` | A prior service ended without removing a descriptor and its recorded process no longer exists. | A new launcher for the same broker and peer replaces the stale descriptor after successful startup. |
| `a TUI is already connected` or HTTP conflict | One TUI already owns the instance attachment slot. | The existing TUI must disconnect before another attachment starts. The service does not require restart. |
| `... is unavailable while attached to an Intercom-managed thread` | The TUI requested `/new`, `/fork`, archive, unarchive, deletion, `/review`, manual compact, rollback, shell escape, or realtime control. | The proxy returns JSON-RPC error -32600. `codex-cli` 0.144.4 exits with status 1 after rollback while the service remains active and attachable. Other unsupported session-management operations require a separate unmanaged Codex session. |
| `the attached TUI does not own the managed thread's active turn` | Enter attempted `turn/steer` while an Intercom-delivered turn owned the controller. | The proxy returns JSON-RPC error -32600. `codex-cli` 0.144.4 exits with status 1. The service and active turn remain alive; the same attachment command reconnects after the turn. Tab queues a follow-up without sending this request. |
| `use --new to replace the binding` | The saved peer, canonical directory, `CODEX_HOME`, state schema, or tool contract differs from the selected runtime. | The matching exact binding values must be restored, or `--new` must select a deliberate replacement. App-server user-agent and Codex-version changes do not produce this diagnostic. |
| `managed thread is ... want idle` | The dedicated thread is active or in an error state during startup. | The service group must stop until Codex settles before restart. |
| `codex: active turn or persistent goal had no app-server activity for 15m0s` | An active managed turn or an idle controller reserved by an active or unknown persistent goal emits no app-server activity for 15 minutes while no owned interactive reverse request awaits TUI input. | App-server diagnostics determine whether the service group requires restart. |

### Notes

`intercom peers` starts a broker when none is listening. Its successful empty-list marker proves broker availability, not agent availability.

The fixed diagnostic peer name is `intercom-peers`. The name remains legal for an agent; a live peer using it causes `intercom peers` to fail with `name_taken`.

The direct resume command from an old readiness block can fail after a service restart because its private client endpoint is ephemeral. `intercom codex attach --name NAME` resolves the current live descriptor and avoids that stale endpoint.

Descriptor lookup checks descriptor structure and the recorded process ID; it does not connect to the client socket. After successful process replacement, remote-socket, Codex authentication, version, and proxy-initialization failures are Codex diagnostics and use the Codex process exit status.

A TUI disconnect is not a service failure. The launcher continues running, the peer remains discoverable, and the same name-based command reattaches. A launcher exit, adapter exit, or app-server exit is a service failure boundary and requires service restart before attachment.

### See also

[Errors](REFERENCE.md#errors), [broker error codes](BROKER_PROTOCOL.md#error-codes)
