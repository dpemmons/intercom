# Intercom Command and Tool Reference

## NAME

`intercom-reference` — public command, agent-tool, environment, file, limit, and error contract.

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

## CONTENTS

- [Description](#description)
- [Commands](#commands)
  - [`intercom` root](#intercom-root)
  - [`intercom shim`](#intercom-shim)
  - [`intercom codex`](#intercom-codex)
  - [`intercom codex attach`](#intercom-codex-attach)
  - [`intercom codex sessions`](#intercom-codex-sessions)
  - [`intercom broker`](#intercom-broker)
  - [`intercom name`](#intercom-name)
  - [`intercom peers`](#intercom-peers)
  - [`intercom completion`](#intercom-completion)
  - [`intercom help`](#intercom-help)
  - [`intercom-codex-project`](#intercom-codex-project)
- [Agent tools](#agent-tools)
  - [`send_message`](#send_message)
  - [`list_peers`](#list_peers)
- [Peer names](#peer-names)
- [Environment](#environment)
- [Files](#files)
  - [Broker files](#broker-files)
  - [Managed Codex binding](#managed-codex-binding)
  - [Live Codex instance descriptors](#live-codex-instance-descriptors)
  - [Launcher files](#launcher-files)
- [Limits and timers](#limits-and-timers)
- [Errors](#errors)
- [Notes](#notes)
- [See also](#see-also)

## DESCRIPTION

The `intercom` binary contains the broker, the Claude Code adapter, the managed Codex adapter and TUI proxy, and diagnostic commands. `intercom-codex-project` is the supported supervisor for one dedicated Codex app-server and one adapter/proxy.

All commands use standard output for results and standard error for diagnostics unless a command entry states otherwise. All durations accepted by `intercom` use Go duration syntax: a sequence of decimal numbers with units such as `ns`, `us`, `µs`, `ms`, `s`, `m`, and `h`.

After successful flag parsing, a recognized `-h` or `--help` takes precedence over positional-argument checks, required-flag checks, and command runtime validation. Extra positional arguments and invalid runtime values therefore do not change help status 0. Unknown options and values that cannot be parsed as the option's declared type fail before help. Root `-v` or `--version` likewise takes precedence over command selection and remaining positional arguments.

Generated help output, including no-command root help, does not report a standard-output write failure and retains status 0. Version output reports a write failure with status 1; Cobra writes the underlying diagnostic and `intercom` writes the same error with its prefix. Command-result and completion-output failures follow their command entries.

## COMMANDS

### intercom root

#### Synopsis

```text
intercom
intercom --help
intercom --version
```

#### Arguments

| Argument | Type | Mode | Units or limits | Default | Meaning |
|---|---|---|---|---|---|
| command | command name | optional | one command token | none | Selects one command described below. |

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `-h`, `--help` | Boolean | optional | none | false | Prints command help and exits. |
| `-v`, `--version` | Boolean | optional | none | false | Prints `intercom version VERSION (REVISION)` and exits. |

#### Semantics

An invocation with no command prints root help and exits successfully. Options are parsed at the command position where they occur.

#### Errors

Without help or version selection, an unknown command, unknown option, or unexpected positional argument produces a diagnostic and status 1. An unknown option remains an error when help or version is also present.

A version standard-output failure writes the underlying error and then `intercom: ERROR` to standard error and produces status 1. Help standard-output failures are not reported.

#### Exit status

Status is 0 after help, successful version output, or no-command output. Status is 1 after command selection, option parsing, or version output fails. A selected subcommand supplies the remaining status.

#### Example

```sh
intercom --help >/dev/null
```

#### See also

[`intercom help`](#intercom-help), [`intercom completion`](#intercom-completion)

### intercom shim

#### Synopsis

```text
intercom shim
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `-h`, `--help` | Boolean | optional | none | false | Prints command help and exits. |

#### Semantics

`intercom shim` serves newline-delimited JSON-RPC 2.0 MCP on standard input and standard output. It exposes `send_message` and `list_peers`, advertises the `claude/channel` experimental capability, and emits inbound broker deliveries as `notifications/claude/channel` notifications.

The peer name is `INTERCOM_NAME` after surrounding Unicode whitespace recognized by Go's `strings.TrimSpace` is removed, or the current working-directory basename when the variable is empty. The shim attempts broker registration after the MCP initialized notification. That eager attempt is asynchronous and nonfatal; a tool call attempts connection again when registration failed.

End of standard input, `SIGHUP`, `SIGINT`, and `SIGTERM` produce clean shutdown. Logs are written to standard error. No log text is written to the MCP standard-output stream.

#### Errors

| Condition | Result |
|---|---|
| A positional argument is present and help is not requested. | The command exits with status 1. |
| An unknown option is present. | The command exits with status 1, including when help is also present. |
| `INTERCOM_NAME` is blank and the current working directory cannot be obtained for the default name. | The command reports `shim: getwd` and exits with status 1. |
| The selected peer name violates the peer-name grammar. | The command reports `invalid peer name` and exits with status 1 before serving MCP. |
| The runtime directory cannot be resolved or created. | The command reports the filesystem error and exits with status 1. |
| The current executable cannot be located and `INTERCOM_BROKER_BIN` is empty. | The command reports `shim: locate intercom executable` and exits with status 1. |
| Standard-input scanning fails, including an MCP line reaching the 8 MiB scanner limit. | The command reports `mcp: read` and exits with status 1. |
| A channel notification cannot be encoded or written. | The failure is logged and that notification is lost. The shim continues reading MCP input. |
| An MCP response cannot be written. | The response is lost. The response path does not make the write failure a direct process error. |
| Broker registration is rejected or temporarily unavailable. | Eager registration logs a warning. A tool call returns an error result. The MCP process remains running. |

Malformed JSON input and unrelated JSON-RPC notifications are discarded. Unsupported JSON-RPC requests receive method-not-found responses and do not terminate the shim.

#### Exit status

Status is 0 after help, standard-input EOF, or handled termination signal. Status is 1 after command, configuration, or fatal MCP input failure.

#### Example

The normal invocation is registered with Claude Code rather than typed into an interactive terminal:

```sh
claude mcp add --transport stdio --scope user intercom -- intercom shim
INTERCOM_NAME=implementer claude --dangerously-load-development-channels server:intercom
```

#### See also

[Agent tools](#agent-tools), [MCP and Channels protocol](BROKER_PROTOCOL.md#claude-mcp-mapping), [Handbook: Claude Code](HANDBOOK.md#2-configure-a-claude-code-peer)

### intercom codex

#### Synopsis

```text
intercom codex --app-server ENDPOINT [--client-endpoint ENDPOINT] [--mcp-bridge PATH] [--name NAME] [--cwd DIRECTORY] [--new] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom codex --app-server ENDPOINT [--client-endpoint ENDPOINT] --mcp-bridge PATH [--name NAME] [--cwd DIRECTORY] --adopt-session ID [--replace-binding] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom codex --app-server ENDPOINT [--client-endpoint ENDPOINT] --mcp-bridge PATH [--name NAME] [--cwd DIRECTORY] --fork-session ID [--replace-binding] [--yolo | --dangerously-bypass-approvals-and-sandbox]
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `--app-server ENDPOINT` | Unix endpoint or filesystem path | required | absolute path or `unix` URL | none | Selects the dedicated Codex app-server Unix socket. A bare value must be an absolute path. A URL value must use `unix`, contain an absolute path, and contain no host, query, fragment, or NUL byte. |
| `--client-endpoint ENDPOINT` | Unix endpoint or filesystem path | optional | absolute path or `unix` URL | none | Creates a stock-Codex TUI proxy at the selected Unix socket. The syntax is identical to `--app-server`; the two normalized endpoints must differ. An absent option selects headless operation and suppresses live-instance publication and readiness output. |
| `--mcp-bridge PATH` | Unix socket path | conditional | absolute filesystem path | none | Creates the authenticated controller bridge used by the required Intercom MCP server. Adoption, fork, and resume of a binding whose `toolTransport` is `mcpBridge` require it. The project launcher supplies this option. |
| `--name NAME` | peer name | optional | 1–64 ASCII bytes; `[A-Za-z0-9_-]` | `INTERCOM_NAME`, then selected-directory basename | Selects the Intercom peer and state filename. The flag takes precedence over the environment. Surrounding whitespace is removed. |
| `--cwd DIRECTORY` | directory path | optional | filesystem path | current working directory | Selects the managed project directory. The adapter resolves the path to an absolute, symlink-canonical directory. |
| `--new` | Boolean | optional | none | false | Starts a new thread and replaces the saved Intercom binding. It does not delete previous Codex history. |
| `--adopt-session ID` | Codex thread ID | optional | when nonempty, 1–256 printable UTF-8 bytes with no whitespace | none | Resumes and binds the existing eligible CLI or VS Code root thread without changing its ID. An explicit empty value is treated as an absent option. |
| `--fork-session ID` | Codex thread ID | optional | when nonempty, 1–256 printable UTF-8 bytes with no whitespace | none | Forks the existing eligible CLI or VS Code root thread and binds the new thread ID. The source remains unchanged. An explicit empty value is treated as an absent option. |
| `--replace-binding` | Boolean | optional | none | false | Authorizes adoption or fork to replace an existing binding for another thread. It requires `--adopt-session` or `--fork-session`. When a prior binding exists, its thread lock is retained from before replacement validation through commit or rollback. |
| `--yolo` | Boolean | optional | none | false | Selects approval policy `never` and sandbox policy `danger-full-access` for the service. |
| `--dangerously-bypass-approvals-and-sandbox` | Boolean | optional | none | false | Alias for `--yolo`. |
| `-h`, `--help` | Boolean | optional | none | false | Prints command help and exits. |

#### Semantics

The command connects to an app-server that is already listening and is externally supervised. It does not start or stop app-server. `--client-endpoint` adds a private Unix-WebSocket proxy for a stock Codex TUI. The adapter remains the app-server's sole Intercom-owned subscriber and the proxy multiplexes TUI traffic through that connection.

The adapter initializes the experimental app-server API and requires the app-server user agent to report semantic version 0.144.1 or later. The protocol provides no feature or schema-version negotiation. The adapter establishes compatibility by executing and validating the request, response, lifecycle, managed-thread, sandbox, tool-registration, and session-selection contracts it consumes. JSON decoding ignores unknown additive object fields; required fields, enumerated values, correlation, ownership, and behavioral invariants remain validated. It acquires a nonblocking lifetime lock for the selected peer before connecting and a separate nonblocking lock for the managed Codex thread before ownership begins. Authorized replacement also acquires the prior binding's thread lock before app-server initialization and retains it through commit or rollback.

Without a saved binding or nonempty selection option, the adapter creates one non-ephemeral thread with the two Intercom app-server dynamic tools. An explicit `--adopt-session=` or `--fork-session=` value is indistinguishable from an absent option. `--adopt-session` and `--fork-session` with nonempty IDs resolve the explicit ID through non-archived `thread/list` results and require a non-ephemeral CLI or VS Code root in `idle` or `notLoaded` status with the exact managed `cwd`. Adoption locks and resumes that thread under the same ID. Fork locks its source while reading and forking it, validates the returned new ID, and locks that returned thread for managed lifetime. When replacement forks the prior bound thread, the already-retained prior lock supplies source ownership while the returned fork receives its own lock.

Session status is local to the app-server process that performs discovery. A selected `idle` or `notLoaded` thread can contain an active persistent goal. After a saved or adopted thread resumes, the adapter calls `thread/goal/get` before broker readiness. A null goal does not reserve the scheduler. Status `active` reserves it. Status `paused`, `blocked`, `usageLimited`, `budgetLimited`, or `complete` releases it. An unknown nonempty status reserves it conservatively. App-server error -32601 leaves initial state notification-driven; another error fails startup. Ordered `thread/goal/updated` and `thread/goal/cleared` notifications supersede an earlier goal-read result and reconcile later state. App-server may publish a continuation immediately after the resume response. The adapter pins the selected thread before resume, retains the reservation observed when each continuation start is admitted, records that turn as Codex-owned, and queues Intercom deliveries until the goal releases its reservation and admitted continuations reach terminal processing. A matching Intercom tool call waits for published readiness; no broker operation occurs before registration. Ordinary Codex processes do not participate in this ownership protocol, so the source TUI or IDE must still be stopped before exact adoption.

Adopted and forked threads receive a request-scoped MCP server named `intercom_managed`. Its command is the selected Intercom executable with arguments `codex-mcp-bridge --socket MCP_BRIDGE_PATH --timeout REVERSE_TIMEOUT`; its environment contains the random service-lifetime token as `INTERCOM_CODEX_BRIDGE_TOKEN`. It is required, permits parallel tool calls, enables only `send_message` and `list_peers`, sets default tool approval to `approve`, uses the adapter control timeout in whole seconds for startup, and uses the reverse-request timeout in whole seconds for app-server tool calls. The helper and private controller bridge use the exact reverse-request duration. Each app-server timeout rounded to whole seconds has a minimum of one second; command defaults produce 30-second startup and 10-second tool timeouts. The adapter validates that app-server reports the server and both tools before provisionally writing the binding. A cold resume of an MCP-bridge binding reinjects and revalidates the same per-service configuration.

Selecting `--adopt-session` with the current binding's own thread ID is an idempotent normal resume and preserves its recorded tool transport. Any selection that would produce another bound thread requires `--replace-binding`. When a prior binding exists, its lock remains held throughout selected-thread validation, provisional state write, live-descriptor publication, readiness-output writing, and final startup release. Success releases the prior lock only after final startup commit. Failure leaves or restores the prior binding while its lock remains held, then releases it; a replacement with no predecessor is removed instead. This rollback does not apply to `--new`. A created fork can remain in Codex storage when later validation fails.

The default execution policy is approval `never`, approvals reviewer `user`, workspace-write sandbox, no additional writable roots, and a runtime workspace-root list containing only the canonical managed directory. Yolo mode replaces the sandbox with `danger-full-access`; approval, reviewer, and runtime root remain pinned. The selected policy is a service configuration rather than persistent binding identity.

One broker delivery occupies one FIFO slot. The adapter starts a delivered Codex root turn only while the managed thread is idle. A TUI `turn/start` reserves the same controller before it reaches app-server. An accepted `thread/settings/update` atomically blocks broker-delivery start, TUI turn admission, and another settings update until its upstream response or error. App-server may independently publish a persistent-goal continuation without a client `turn/start`; ordered remote lifecycle remains authoritative and is not blocked by the settings reservation. Deliveries that arrive while another local source owns the scheduler or a lifecycle boundary remains deferred stay queued. A TUI turn may call the Intercom tools through its binding transport. Codex child threads whose reported parent or fork ancestry leads to the managed root may use inherited Intercom tools without replacing root lifecycle state. A normal final answer remains in Codex history. Only a successful `send_message` tool call creates an outbound Intercom message.

The `completed`, `failed`, and `interrupted` turn statuses are terminal. Each enters controller completion processing without retrying a delivered message. An Intercom- or TUI-started turn returns to idle only after that processing and the corresponding `turn/start` response have both finished. A Codex-owned turn has no corresponding client response and returns to idle after terminal processing. A continuation published behind an unfinished terminal boundary and all following lifecycle events remain queued in source order until the controller catches up. Other completion statuses are fatal protocol violations for the managed thread. Codex app-server can publish lifecycle notifications for child threads created by the managed agent. The proxy still offers those notifications to the TUI, while the controller ignores their lifecycle state after decoding a nonempty foreign thread ID.

On broker disconnect, the adapter retries registration indefinitely while app-server remains usable. On adapter shutdown, the live TUI descriptor is removed before broker disconnect, active-turn interruption, and app-server reverse-request drain. A TUI disconnect does not stop the adapter or app-server. A later `intercom codex attach` invocation can reconnect to the same managed thread. A proxy-listener failure is fatal.

The proxy accepts WebSocket upgrades at `/` and `/rpc` and accepts one downstream TUI session at a time, including a session that has not initialized. A concurrent upgrade receives HTTP status 409; another request path receives HTTP status 404. A session that does not complete initialize and managed-thread resume within 30 seconds closes with a policy-violation status and releases the slot. The TUI must report the same client version as the currently running app-server, a nonempty client name, and the experimental API capability. Request-attestation and OpenAI-form-elicitation capabilities are rejected. Proxy readiness requires a successful local or upstream `thread/resume` result for the managed thread.

The proxy returns the adapter's cached initialize result, consumes the TUI's `initialized` notification, honors its `optOutNotificationMethods`, remaps TUI request IDs before upstream forwarding, remaps app-server reverse-request IDs before TUI forwarding, and restores each caller's original ID in the response. A rejected initialize attempt may be retried; a successful initialize makes every later initialize invalid for that session. A downstream request ID remains claimed until its terminal response is written and may then be reused. `thread/resume` is restricted to the managed thread and is rewritten with the managed directory, one-entry managed runtime-root list, Intercom developer instructions, service policy, and full turn inclusion. `thread/settings/update` requires an idle managed thread, rebuilds a closed set of interactive settings, pins service policy, and holds an atomic local-admission reservation through its upstream response or error. `turn/start` is rewritten with the managed directory, one-entry managed runtime-root list, and service policy and preserves other raw fields. Pipelined `turn/start` requests enter controller admission in downstream wire order; a later request cannot reserve the controller before an earlier request finishes admission. The request allowlist below is closed; a method absent from it is not forwarded.

Stock Codex uses preserved non-policy fields to apply interactive model, service-tier, reasoning effort and summary, personality, output-schema, collaboration-mode, and multi-agent-mode selections. The proxy replaces TUI approval, approvals-reviewer, sandbox, permissions, working-directory, and runtime-root fields with the configured service policy during `thread/resume`, `thread/settings/update`, and `turn/start`. Settings update drops every field outside `threadId`, `model`, `serviceTier`, `effort`, `summary`, `collaborationMode`, `multiAgentMode`, and `personality` before adding pinned fields. Each Intercom-delivered turn supplies the same managed directory, runtime root, approval policy, approvals reviewer, and sandbox policy. The thread-level Intercom developer instructions remain a separate developer section when Codex adds collaboration-mode developer instructions.

`turn/interrupt` and `turn/steer` are forwarded only when the attached TUI owns the named starting or active turn. They cannot control an Intercom-delivery turn. Every TUI `turn/start` holds app-server notifications behind its response. Controller lifecycle state is reconciled before the held notifications are exposed. Every managed terminal notification and each later notification remain held until controller completion processing and the corresponding start response have both finished. No later delivery or TUI turn can overtake either boundary.

During an active TUI-owned turn, Enter submits `turn/steer` and the proxy forwards it. During an Intercom-owned turn, Enter submits `turn/steer` and the proxy rejects it. Tab queues composer text inside the TUI and does not submit another proxy request until the active turn completes. The queued text can then start the next TUI-owned turn. Client handling of a rejected request is outside the proxy contract; `codex-cli` 0.144.4 treats this steer rejection as fatal and exits with status 1.

| Downstream operation | Proxy and controller handling |
|---|---|
| `initialize` | Terminates locally, validates client identity, version, capabilities, and notification opt-outs, and returns the cached upstream result. |
| `initialized` | Terminates locally after successful initialize. |
| `thread/resume` | Validates the managed thread and rewrites directory, runtime workspace roots, developer instructions, approval policy, approvals reviewer, sandbox policy, and `excludeTurns`; permission overrides are removed. A successful local or upstream result makes the session ready. The initial resume executes in downstream reader order. |
| `thread/settings/update` | Atomically blocks broker-delivery start, TUI turn admission, and another settings update through the upstream response or error; it does not block ordered app-server lifecycle. The request validates the managed thread, retains only `threadId`, `model`, `serviceTier`, `effort`, `summary`, `collaborationMode`, `multiAgentMode`, and `personality`, then sets the managed `cwd`, approval policy `never`, approvals reviewer `user`, and service sandbox policy. Permissions and unknown fields are dropped. |
| `turn/start` | Validates the managed thread, enters controller admission in downstream wire order, reserves the idle controller, pins `cwd`, `runtimeWorkspaceRoots`, approval policy, approvals reviewer, and sandbox policy, removes permission overrides, preserves every other supplied field, and forwards with a 30-second deadline. |
| `turn/interrupt`, `turn/steer` | Validates TUI ownership of the named current turn and forwards. |
| `thread/read`, `thread/turns/list`, `thread/items/list`, `thread/goal/get`, `thread/backgroundTerminals/list` | Requires the managed `threadId` and forwards the read. |
| `thread/name/set`, `thread/metadata/update`, `thread/memoryMode/set` | Requires the managed `threadId` and forwards the bounded current-thread metadata operation. |
| `skills/list`, `hooks/list`, `plugin/list`, `plugin/installed` | Pins `cwds` to the managed directory, preserves every other supplied field, and forwards. |
| `fuzzyFileSearch/sessionStart` | Pins `roots` to the managed directory and forwards. |
| `fuzzyFileSearch/sessionUpdate`, `fuzzyFileSearch/sessionStop` | Forwards the project search-session update or termination. Neither changes managed thread or turn ownership. |
| `config/read`, `permissionProfile/list` | Pins `cwd` to the managed directory, preserves every other supplied field, and forwards. |
| `configRequirements/read`, `model/list`, `modelProvider/capabilities/read`, `collaborationMode/list` | Forwards the named global read. |
| `account/read` | Forces `refreshToken` to false, preserves every other supplied field, and forwards the account read without refreshing authentication state. |
| `account/rateLimits/read`, `account/usage/read`, `account/workspaceMessages/read` | Forwards the named account read. |
| `mcpServer/resource/read`, `mcpServerStatus/list`, `app/list`, `experimentalFeature/list` | Allows absent or null `threadId`; otherwise requires the managed `threadId`, then forwards the resource, status, app, or feature read. |
| `plugin/read`, `plugin/skill/read`, `plugin/share/list`, `environment/info`, `thread/realtime/listVoices` | Forwards the named catalog, environment, or voice read. |
| `thread/unsubscribe` | Returns local result `{"status":"unsubscribed"}` and closes the downstream session without forwarding. |
| Any other request | Returns error -32600 without forwarding. |
| Any client notification other than `initialized` | Closes the TUI connection with a policy-violation status without forwarding. |
| App-server notification before a valid initial `thread/resume` begins | Drops the notification because no downstream thread context exists. |
| App-server notification during `thread/resume` or TUI `turn/start` | Buffers at most 256 notifications, writes the request response first, then flushes the notifications in source order before releasing the barrier. An opted-out method is discarded instead. |
| Managed terminal notification, and each later notification while its controller gate remains active | Buffers at most 256 notifications until controller completion processing and the corresponding start response have both finished, then offers them to the downstream response barrier in source order. |
| Other app-server notification after the session is ready and outside a response or controller barrier | Enqueues the notification for downstream delivery unless its method is opted out. Controller lifecycle handling remains independent. |

For `thread/resume`, `cwd` is always the managed directory, `runtimeWorkspaceRoots` is always an array containing that directory, `approvalPolicy` is always `never`, `approvalsReviewer` is always `user`, `sandbox` is the service policy, and `excludeTurns` is always false. Missing, null, empty, or whitespace-only client developer instructions become the Intercom binding instructions. Other client developer instructions are followed by two newline bytes and the Intercom binding instructions. An already-running thread can retain its configured developer instructions instead of applying a downstream resume override; Intercom's binding is already installed by the adapter's own thread start or cold resume. For `thread/settings/update`, `cwd` is always the managed directory and all policy fields are reconstructed by Intercom.

A newly started managed thread can be attached before its first turn creates a Codex rollout. Before the first managed `turn/started` or terminal lifecycle event, the adapter terminates TUI `thread/resume` locally with the validated `thread/start` snapshot and `initialTurnsPage: null`; it does not send that resume upstream. The session becomes ready from this synthetic result and can start the first turn. The first managed turn lifecycle event clears the zero-turn snapshot so an attachment made during that turn resumes upstream and receives its current state. A successful materializing `thread/read` also clears the snapshot.

The following TUI operations are unavailable and receive JSON-RPC error -32600 without changing the binding or managed thread:

| TUI operation | Rejected app-server method |
|---|---|
| `/new` | `thread/start` |
| `/fork` | `thread/fork` |
| Archive, unarchive, or delete | `thread/archive`, `thread/unarchive`, `thread/delete` |
| `/review` | `review/start` |
| `/compact` or compact action | `thread/compact/start` |
| Rollback action | `thread/rollback` |
| Shell-command action | `thread/shellCommand` |
| Realtime audio, text, speech, or stop action | `thread/realtime/start`, `thread/realtime/appendAudio`, `thread/realtime/appendText`, `thread/realtime/appendSpeech`, `thread/realtime/stop` |
| Goal set or clear | `thread/goal/set`, `thread/goal/clear` |
| Guardian-denied action approval | `thread/approveGuardianDeniedAction` |
| Raw history injection | `thread/inject_items` |
| Background-terminal cleanup or termination | `thread/backgroundTerminals/clean`, `thread/backgroundTerminals/terminate` |
| Every unlisted protocol operation | The unlisted method name |

Client handling of a rejected `thread/rollback` response is outside the proxy contract. `codex-cli` 0.144.4 treats the response as fatal and exits with status 1. The adapter, app-server connection, managed thread, active turn, and queued deliveries remain active after that TUI process exits. A later attachment resumes the same managed thread.

`thread/unsubscribe` is acknowledged locally and is not forwarded upstream. The proxy then clears readiness, settles pending TUI reverse requests through the headless fallback, closes that downstream session, and frees the attachment slot. The adapter retains app-server subscription and thread ownership.

Dynamic-tool calls always terminate at the Intercom adapter. A root-thread call requires the starting or active managed turn and matching turn ID. For another thread, the adapter issues `thread/read` without turns and follows `parentThreadId` and `forkedFromId` links until it reaches the managed root or a cached descendant. Explicit `thread/started` ancestry can populate the same cache before a call. A successful walk caches its path and authorizes the child without changing the root turn ID. Cycles terminate as unrelated ancestry; a walk examining more than 64 distinct threads is fatal. This descendant rule relies on the required dedicated app-server boundary.

A supported human-interaction reverse request must carry `params.threadId` equal to the managed root or a descendant already present in the ancestry cache. Interactive authorization does not perform `thread/read`. Malformed routing parameters, an empty thread ID, or an unrelated thread receives the applicable fixed headless-policy response. It is not forwarded to the managed TUI and does not record managed activity or suspend the managed watchdog. When a proxy exists, an owned request waits for published adapter readiness and for controller-gated notifications to be offered to the TUI first; this ordering admission uses the 30-second control timeout. The request then reaches a ready TUI. An absent proxy, TUI absence or disconnection, or TUI input timeout selects the fixed headless policy. An owned pending request suspends the active-turn or persistent-goal inactivity watchdog from receipt through TUI response or headless fallback. Completion of the last owned request starts a full watchdog interval. The following headless policy applies whenever a supported request is not delegated to a ready TUI:

| App-server request | Response |
|---|---|
| `item/tool/call` for `send_message` or `list_peers` | Executes the Intercom tool after thread, turn, call, namespace, and argument validation. |
| `item/tool/call` for another tool | Returns a dynamic-tool result with `success: false`. |
| `item/commandExecution/requestApproval` | Returns decision `decline`. |
| `item/fileChange/requestApproval` | Returns decision `decline`. |
| `item/permissions/requestApproval` | Returns an empty granted-permission profile with scope `turn`. |
| `item/tool/requestUserInput` | Returns app-server error -32603 because no interactive user is attached. |
| `mcpServer/elicitation/request` | Returns action `decline` with null content and metadata. |
| `applyPatchApproval`, `execCommandApproval` | Returns legacy decision `denied`. |
| `account/chatgptAuthTokens/refresh`, `attestation/generate`, `currentTime/read` | Returns app-server error -32603 because the service is unavailable to a headless peer. |
| Any other reverse-request method | Returns app-server error -32601. |

Command-execution approval, file-change approval, permission approval, user input, and MCP elicitation are the interactive reverse requests eligible for TUI forwarding. Authentication refresh, attestation, current-time, legacy approval, dynamic-tool, and unknown methods always use the adapter policy.

With `--client-endpoint`, readiness occurs after managed-thread validation, broker registration, proxy-listener creation, and live-descriptor publication. The command then writes the following shell-safe block to standard output, with concrete values replacing the metavariables:

```text
Intercom Codex peer NAME is ready.
Execution policy: POLICY

Attach from another terminal:
  INTERCOM_DIR=STATE_DIRECTORY INTERCOM_SOCKET=BROKER_SOCKET CODEX_BIN=CODEX_EXECUTABLE CODEX_HOME=CODEX_DIRECTORY INTERCOM_EXECUTABLE codex attach --name NAME

Direct Codex command:
  CODEX_HOME=CODEX_DIRECTORY CODEX_EXECUTABLE resume --remote CLIENT_ENDPOINT THREAD_ID
```

`POLICY` is `workspace-write` or `danger-full-access`. The displayed direct command for danger-full-access inserts `--dangerously-bypass-approvals-and-sandbox` between `resume` and `--remote`; the block above shows workspace-write. `STATE_DIRECTORY`, `BROKER_SOCKET`, and slash-containing relative `INTERCOM_BIN`, `CODEX_BIN`, or `CODEX_HOME` values are made absolute before display. `CODEX_HOME` assignments are omitted when that variable is unset. Every displayed value is shell-quoted when required. The name-based command therefore carries descriptor lookup, execution policy, and Intercom and Codex executable selection into another terminal; the direct command carries Codex home and execution-policy selection. The displayed executables must remain available, and neither command reproduces unnamed provider-authentication variables. The live descriptor remains published until shutdown begins. A readiness-output failure removes the descriptor and fails startup.

#### Errors

The following table enumerates externally visible adapter and proxy error classes. A fatal diagnostic writes to standard error and produces status 1. A row that specifies a TUI response or connection result is nonfatal unless that row states that the adapter terminates.

| Condition | Result |
|---|---|
| An unknown option is present, or a positional argument is present without help selection. | Command parsing diagnostic; status 1. |
| `--app-server` is absent. | required-flag error |
| The endpoint is relative, malformed, non-`unix`, host-bearing, query-bearing, fragment-bearing, NUL-bearing, or not a usable Unix WebSocket endpoint. | `invalid --app-server`, app-server parse, dial, or upgrade error |
| `--client-endpoint` is present and is relative, malformed, non-`unix`, host-bearing, query-bearing, fragment-bearing, or NUL-bearing. | `invalid --client-endpoint` |
| The normalized client and app-server endpoints are equal. | `--client-endpoint must differ from --app-server` |
| `--mcp-bridge` is relative. | `codex: MCP bridge socket path must be absolute` |
| More than one of `--new`, `--adopt-session`, and `--fork-session` is selected. | `codex: --new, --adopt-session, and --fork-session are mutually exclusive` |
| `--replace-binding` is selected without adoption or fork. | `codex: --replace-binding requires --adopt-session or --fork-session` |
| Adoption or fork omits either `--mcp-bridge` or the selected Intercom executable. | `codex: adopted and forked threads require the managed MCP bridge` |
| A nonempty session ID is longer than 256 bytes, invalid UTF-8, contains whitespace or a control character, or has leading or trailing whitespace. | `codex: adopt session` or `codex: fork session` validation diagnostic. An explicit empty value selects no adoption or fork and receives no ID-validation diagnostic. |
| `--cwd` is absent and the current working directory cannot be obtained. | `codex: get working directory` |
| `--cwd` cannot be made absolute, resolved through symlinks, statted, or does not name a directory. | `codex: resolve cwd`, `resolve cwd symlinks`, `stat cwd`, or `cwd is not a directory` |
| The selected peer name violates the peer-name grammar. | `invalid peer name` |
| The Intercom runtime directory cannot be created, made absolute, resolved through symlinks, statted, or confirmed as a directory for readiness output. | `resolve runtime directory` or `canonicalize runtime directory` |
| A slash-containing relative `INTERCOM_BIN` or `CODEX_BIN`, or a nonempty relative `CODEX_HOME`, cannot be made absolute for readiness output. | `resolve INTERCOM_BIN`, `resolve CODEX_BIN`, or `resolve CODEX_HOME` |
| The state directory, state file, or lifetime lock cannot be opened, decoded, validated, written, synchronized, replaced, or removed. | `codex state` or `persist new thread binding` |
| Another adapter holds the same peer lifetime lock. | `codex state: peer is already managed` |
| Another Intercom adapter holds the selected thread lock. | `codex thread lock: thread is already managed by Intercom` |
| Another Intercom adapter holds the prior binding's thread lock when replacement is authorized. | `codex: lock prior thread ID during replacement: codex thread lock: thread is already managed by Intercom`; failure occurs before app-server validation and the saved binding remains unchanged. |
| A non-replacement fork cannot release its temporary source lock after app-server returns the fork. | `codex: release source session lock`; the returned fork can remain in Codex storage and no replacement binding is written. |
| Adoption or fork selects another thread while a binding exists and `--replace-binding` is absent. | Binding replacement diagnostic; the saved binding remains unchanged. |
| The app-server cannot be reached within 30 seconds. | `codex: app-server unavailable after 30s` |
| App-server initialization or the initialized notification fails. | `initialize app-server` or `send initialized` |
| The app-server user agent does not have the required product/version form. | `cannot determine app-server version` |
| The app-server user agent reports a semantic version earlier than 0.144.1. | `unsupported app-server version` |
| App-server version 0.144.1 or later changes a consumed request, response, lifecycle, managed-thread, sandbox, dynamic-tool, MCP-status, listing, or fork contract incompatibly. | The affected RPC, protocol, or managed-thread invariant diagnostic. A replacement binding remains unchanged when the condition occurs before its provisional write. |
| Saved peer, canonical directory, `CODEX_HOME`, state schema, or tool-contract version differs. | identity or contract diagnostic; exact binding changes require `--new` |
| A resumed thread validates successfully but the refreshed app-server user-agent or Codex-version diagnostics cannot be persisted. | `persist validated app-server diagnostics` |
| `thread/start`, `thread/resume`, `thread/read`, `thread/fork`, MCP status, or state persistence fails. | operation-specific Codex RPC diagnostic |
| `thread/goal/get` returns app-server error -32601. | Startup continues without an initial goal snapshot. Ordered goal notifications remain authoritative. |
| `thread/goal/get` returns another error. | `read persistent goal`; startup fails. A saved binding remains unchanged, and an adoption replacement is not retained. Fork does not perform this read. |
| `thread/goal/get` carries an empty goal thread ID or empty status; `thread/goal/updated` cannot decode, carries an empty outer or nested thread ID for the managed thread, carries an empty status, or disagrees between its outer and nested thread IDs; or `thread/goal/cleared` cannot decode or carries an empty thread ID. | Persistent-goal invariant diagnostic; startup or the running adapter fails. A decoded update or clear for another nonempty thread is ignored. |
| An adoption source is archived, is not a CLI or VS Code session, is ephemeral, has another working directory, has a parent, or reports a status other than `idle` or `notLoaded`. | `codex: adopt session` eligibility diagnostic; the binding remains unchanged. |
| A fork source is archived, is not a CLI or VS Code session, is ephemeral, has another working directory, has a parent, or reports a status other than `idle` or `notLoaded`. | `codex: fork session` eligibility diagnostic; the binding remains unchanged. |
| App-server fork returns an empty or unchanged thread ID, or its `forkedFromId` does not equal the source. | `codex: fork session` invariant diagnostic; the binding remains unchanged. |
| The managed MCP bridge token cannot be generated, its private parent or socket cannot be validated or created, or its listener stops. | `codex bridge` startup or listener diagnostic. A replacement binding remains unchanged when the condition occurs before its provisional write. |
| App-server MCP status omits `intercom_managed`, omits `send_message` or `list_peers`, fails, or returns an empty or repeated cursor. | `verify managed MCP server` or managed-MCP invariant diagnostic; a replacement binding remains unchanged because validation precedes the provisional write. |
| A resumed unmaterialized thread has no rollout. | The adapter replaces the pending binding. This exact case is not fatal. |
| App-server returns the wrong thread ID or directory, runtime workspace roots other than the managed directory alone, an ephemeral thread, a non-idle thread, another approval policy, another approvals reviewer, another sandbox type, extra sandbox writable roots, or a non-Boolean workspace network setting. | managed-thread invariant diagnostic |
| A dynamic tool request arrives before adapter ownership is established and does not belong to the pinned Codex-owned startup turn. | `dynamic tool request arrived before adapter ownership was established` |
| Broker registration fails during startup, including a live peer-name collision. | `codex: register with broker` |
| The client socket path exists, cannot be inspected, listened on, or changed to mode 0600. | `codex: start TUI proxy` |
| The app-server client does not provide raw request forwarding. | `app-server client does not expose raw proxy calls` |
| The live registry cannot be created or secured, a descriptor is malformed or insecure, another live process owns its broker-and-peer key, descriptor publication or synchronization fails, or the 128-bit instance nonce cannot be generated. | Live-instance registry or `publish live instance` diagnostic. Startup fails. Adoption or fork restores the prior binding, or removes the replacement when no binding existed. A `--new` binding remains stored. |
| Readiness instructions cannot be written to standard output. | `write readiness instructions`; the descriptor is removed before startup fails. Adoption or fork restores the prior binding, or removes the replacement when no binding existed. A `--new` binding remains stored. |
| Adoption or fork cannot restore or remove its provisionally written binding after publication, output, or final startup-release failure. | `roll back replacement thread binding` is joined to the initiating startup error; durable binding state can require inspection. |
| Live-descriptor removal fails during shutdown and again after the controller returns. | `remove live instance descriptor`; status 1. The controller logs the first failure and the command retries once. |
| The TUI proxy listener stops while the adapter context remains active. | `codex: TUI proxy listener stopped` |
| A delivery arrives while 64 deliveries are already queued. | `inbound delivery queue is full (64)`; the attempted 65th queued delivery is not admitted. |
| A selected lifecycle notification arrives while 256 notifications are already queued. | `app-server notification queue is full`; the attempted 257th notification is not admitted. |
| A notification arrives while 256 controller-gated TUI notifications await terminal processing or the corresponding start response. | `deferred TUI notification queue is full`; controller ordering is no longer trusted and the adapter terminates. |
| App-server disconnects. | `codex: app-server disconnected` |
| A 65th app-server reverse request arrives while 64 handlers remain active, or a reverse request arrives after handler draining begins. | `appserver: concurrent reverse request limit exceeded` or `appserver: reverse request received after handler drain began` |
| App-server sends a binary WebSocket message or a text message larger than 134217728 bytes. | `appserver: binary websocket message` or `appserver: websocket message too large` |
| App-server sends malformed JSON or a request, notification, response, error, method, or ID whose envelope cannot be decoded. | `appserver: decode message`, malformed-envelope, decode, or request-ID diagnostic |
| App-server sends a response ID with no pending request. | `appserver: unknown response id` |
| App-server repeats one of the 1024 most recently completed response IDs. | `appserver: duplicate response id` |
| App-server sends one late response for a request canceled within the 1024-entry expired-response history. | The response is discarded and moved to completed response history; the adapter continues. A second response for that ID is duplicate. |
| A turn start cannot be written or completed within its control budget. | `codex: start delivery` |
| A `thread/started`, `turn/started`, or `turn/completed` notification cannot decode, carries an empty thread ID, or violates the managed thread's turn ID, controller phase, in-progress status, or terminal-status invariant. | thread, turn, event, completion, or notification consistency diagnostic. A decoded notification for another nonempty thread is ignored by the controller. |
| An `error` lifecycle notification cannot decode. | notification decode diagnostic; a decoded error notification is logged without validating its thread ID or turn ID. |
| An active managed turn, or an idle controller with an active or unknown persistent-goal reservation, produces no app-server activity for 15 minutes outside an interval with a pending owned interactive reverse request. | `codex: active turn or persistent goal had no app-server activity for 15m0s` |
| A dynamic tool call has invalid Intercom tool arguments or names an unknown tool. | The call returns `success: false`; the adapter continues. |
| Dynamic-tool parameters cannot be decoded, or a supported interactive request has valid owned routing but its method-specific parameters cannot be decoded by the headless handler. | The call receives app-server error -32602; the adapter continues when the error response succeeds. |
| A supported interactive reverse request has malformed routing parameters, omits `threadId`, carries an empty `threadId`, or names a thread outside the managed root and cached descendants. | The adapter applies the applicable fixed headless policy and sends its protocol response. It does not forward the request to the managed TUI and does not change managed activity or watchdog accounting. |
| A dynamic tool call carries a namespace, omits routing identity, arrives before ownership, names another root turn, or names a foreign thread whose `thread/read` ancestry fails, cycles without reaching the root, exceeds 64 distinct threads, returns another ID, or has no parent or fork path to the root. | The call receives a failure result when possible; the ownership violation then terminates the adapter. A verified or cached descendant is accepted. |
| An MCP tool call has an invalid bridge token or frame, exceeds 1048576 bytes, names an unsupported method, or carries invalid arguments. | The bridge returns its protocol error when possible and then closes that one-request connection. |
| A 65th MCP bridge connection is accepted while 64 handlers hold the bridge semaphore. | The accepted connection waits without being read or authenticated until a handler slot becomes available or the bridge shuts down. No overload response is defined. The client can reach its own deadline while waiting. |
| An admitted MCP bridge handler returns an error after its controller or caller-supplied context deadline has expired. | The bridge returns `deadline_exceeded` when the response can still be written; the client can instead observe its own timeout or connection result. |
| An MCP tool call omits top-level `threadId`, `x-codex-turn-metadata.session_id`, `thread_id`, or `turn_id`; carries unequal top-level and nested thread identities; carries a session identity other than the managed root; or names a thread or turn outside current managed ownership. | The tool call fails and the ownership violation terminates the adapter. No broker operation occurs. |
| A reverse-request result or error response cannot be written. | The response-write failure terminates the adapter. |
| A TUI request has malformed parameters or names another thread. | The TUI receives error -32602; the adapter continues. |
| A TUI repeats `initialize`, sends a request before initialization, reports another client version, or requests attestation or OpenAI-form elicitation. | The TUI receives error -32600; it does not become ready. |
| TUI initialize parameters omit client identity or the experimental capability, or cannot be decoded. | The TUI receives error -32602; it does not become ready. |
| A TUI sends `initialized` or another notification before successful initialization. | The TUI connection closes with a policy-violation status. The adapter remains active. |
| A TUI sends a client notification other than `initialized` after initialization. | The TUI connection closes with a policy-violation status. The adapter remains active. |
| A TUI does not complete initialize and managed-thread resume within 30 seconds of connection. | The TUI connection closes with a policy-violation status, the sole-session slot is released, and the adapter remains active. |
| A TUI repeats an in-flight request ID. | The TUI connection closes because exactly one terminal response can no longer be correlated to the duplicated ID. The adapter remains active. |
| A TUI requests an unavailable thread operation other than rollback. | The TUI receives error -32600; the adapter continues. The requesting client determines whether the request error is fatal to that client. |
| `codex-cli` 0.144.4 requests `thread/rollback`. | The TUI receives error -32600 and exits with status 1. The adapter, app-server connection, managed thread, active turn, and queued deliveries remain active; a later attachment can resume the thread. Other client versions determine their own response to the request error. |
| A TUI requests `thread/settings/update` while the controller is not idle. | The TUI receives error -32600 with `thread/settings/update is allowed only while the managed thread is idle`; the current operation continues. |
| A broker delivery, TUI `turn/start`, or another settings update races an accepted settings update. | The accepted update retains local-admission ownership through its upstream response or error. Deliveries wait; competing TUI requests receive their active-turn or idle-only error. Independently arriving app-server lifecycle remains authoritative. |
| A TUI `turn/interrupt` or `turn/steer` does not name the managed thread and TUI-owned turn or omits its required turn ID. | The TUI receives error -32600 or -32602; the existing turn continues and the adapter remains active. |
| `codex-cli` 0.144.4 submits Enter while an Intercom turn owns the controller. | The proxy rejects `turn/steer` with error -32600 and the TUI exits with status 1. The Intercom turn, adapter, app-server connection, and queued deliveries remain active; a later attachment can resume the thread. Other client versions determine their own response to the request error. |
| A TUI starts a turn while another TUI, Intercom, or Codex-owned turn is starting, active, completing, or ordered ahead of it. | The TUI receives error -32600 with `managed thread already has an active turn`; the existing turn continues. |
| A forwarded TUI request other than `turn/start` exceeds its 30-second deadline. | The TUI receives error -32603, one late upstream response is discarded through bounded expired-ID tracking, and the adapter continues. |
| A TUI `turn/start` has an ambiguous failure, malformed result, wrong turn ID, empty turn ID, or non-in-progress result. | Turn-start consistency diagnostic; the adapter terminates because scheduler ownership is ambiguous. |
| A second TUI connects while a connection is being accepted or remains attached. | The WebSocket upgrade receives HTTP status 409. The attached TUI remains connected. |
| A TUI sends binary data, a malformed envelope, an unrelated or no-longer-tracked reverse-response ID, or a message above 134217728 bytes. | The TUI connection closes. The adapter remains active unless the condition also creates an ambiguous managed request. |
| A TUI request other than `initialize` or `thread/unsubscribe` arrives while 64 forwarded-request handlers remain active across current or detached sessions. | The request receives error -32001 with `Server overloaded; retry later.` The connection remains active. |
| An app-server notification cannot enter the TUI's 256-entry outbound write queue or response-barrier queue, its encoded downstream notification exceeds 134217728 bytes, or its WebSocket write exceeds 30 seconds. | The slow TUI connection closes. The adapter remains active. |
| A result or error intended for the TUI exceeds 134217728 bytes, exceeds the 30-second WebSocket write deadline, or otherwise cannot be written to that connection. | The TUI connection closes. The adapter remains active unless the condition also creates an ambiguous managed request. |
| An interactive reverse request cannot be enqueued to the TUI, including when its encoded downstream message exceeds 134217728 bytes. | The adapter applies the headless policy. A TUI connection that remains usable stays attached. |
| An owned interactive reverse request cannot pass published-readiness and notification-ordering admission within 30 seconds. | The adapter enters the fatal path and attempts the fixed headless response. |
| A TUI `turn/start` does not complete within 30 seconds. | The request receives an error; an ambiguous outcome terminates the adapter. |
| A forwarded interactive reverse request remains unanswered for 15 minutes. | The managed active-turn or persistent-goal inactivity watchdog remains suspended, the TUI relay times out, and the headless policy runs. The watchdog restarts after fallback. One later response for any of the 1024 most recently expired relay IDs is ignored without disconnecting the TUI. |
| A TUI response is accepted, but its result or error cannot be relayed to app-server within the fresh 30-second control deadline. | The response-relay failure terminates the adapter. |

Without a ready TUI, owned approval, elicitation, authentication-refresh, attestation, time, and user-input reverse requests are declined or rejected according to the headless policy. Those expected denials do not terminate the adapter. Shutdown interrupt and drain failures are warnings and do not replace the initiating shutdown status. Live-descriptor removal is retried after controller return and a repeated failure is returned by the command.

Authentication-refresh, attestation, current-time, and unknown reverse-request handlers do not decode their parameter value. They return their fixed unavailable or method-not-found error for any parameter shape. Every app-server notification resets the active-turn or persistent-goal inactivity watchdog; unrecognized notifications are otherwise ignored. Unrelated supported interactive requests do not reset or suspend that watchdog.

#### Exit status

Status is 0 after help or a handled `SIGHUP`, `SIGINT`, or `SIGTERM` when shutdown cleanup succeeds. Status is 1 after argument, configuration, startup, protocol, queue, lifecycle, or repeated live-descriptor cleanup failure.

#### Example

The following Bash transcript supplies the app-server ownership that the lower-level command requires:

```sh
(
  runtime_dir=$(mktemp -d)
  chmod 700 "$runtime_dir"
  app_socket="$runtime_dir/app-server.sock"
  client_socket="$runtime_dir/client.sock"
  codex app-server --listen "unix://$app_socket" &
  server_pid=$!
  cleanup() {
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
    rm -rf "$runtime_dir"
  }
  trap cleanup EXIT
  for ((attempt = 0; attempt < 300; attempt++)); do
    [ -S "$app_socket" ] && break
    kill -0 "$server_pid" 2>/dev/null || exit 1
    sleep 0.1
  done
  if [ ! -S "$app_socket" ]; then
    printf 'app-server socket did not appear within 30 seconds\n' >&2
    exit 1
  fi
  intercom codex --app-server "unix://$app_socket" --client-endpoint "unix://$client_socket" --name reviewer --cwd .
)
```

#### See also

[`intercom codex attach`](#intercom-codex-attach), [`intercom-codex-project`](#intercom-codex-project), [managed state](#managed-codex-binding), [Codex lifecycle](ARCHITECTURE.md#codex-adapter-lifecycle)

### intercom codex attach

#### Synopsis

```text
intercom codex attach --name NAME
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `--name NAME` | peer name | required | 1–64 ASCII bytes; `[A-Za-z0-9_-]` | none | Selects the live managed Codex instance. `INTERCOM_NAME` is not a fallback for this command. |
| `-h`, `--help` | Boolean | optional | none | false | Prints command help and exits. |

#### Semantics

The command opens the live registry selected by the current `INTERCOM_DIR` and derives the descriptor key from the canonical current `INTERCOM_SOCKET` and explicit peer name. These environment selections must match the service. It loads and strictly validates the descriptor, verifies that its recorded adapter PID exists, resolves `CODEX_BIN` or `codex` through `PATH`, makes the located executable path absolute, changes to the descriptor's managed project directory, and replaces itself with one of these argument vectors:

```text
codex resume --remote CLIENT_ENDPOINT THREAD_ID
codex resume --dangerously-bypass-approvals-and-sandbox --remote CLIENT_ENDPOINT THREAD_ID
```

The second form is selected when the live descriptor records execution policy `danger-full-access`; the first form is selected for workspace-write.

The replacement process inherits the attach command's environment and standard input, standard output, and standard error. Descriptor lookup validates the descriptor and recorded PID but does not probe the downstream socket. Codex performs the Unix-WebSocket connection, authentication-dependent client startup, and proxy initialization after process replacement. Successful process replacement does not return to `intercom`; later socket, authentication, version, and initialization diagnostics and exit status belong to Codex.

The selected `CODEX_BIN` must identify the same Codex version as the currently running app-server. A different client version reaches the proxy but receives JSON-RPC error -32600 during initialization. The descriptor lookup is scoped to the selected broker socket; equal peer names on different broker sockets resolve independently.

#### Errors

| Condition | Result |
|---|---|
| An unknown option is present, or a positional argument is present without help selection. | Command parsing diagnostic; status 1. |
| `--name` is absent or blank. | Required-flag or `--name must not be empty` diagnostic; status 1. |
| `--name` violates the peer-name grammar. | `invalid peer name`; status 1. |
| The broker socket or Intercom directory cannot be resolved or created. | Path diagnostic; status 1. The command does not start a broker. |
| No descriptor exists for the broker-and-peer key. | `no live Codex instance named`; status 1. |
| The descriptor PID no longer exists. | `descriptor is stale`; status 1. The stale file remains for a later publisher to replace. |
| The live directory or descriptor has an insecure type or mode, or descriptor JSON is empty, larger than 65536 bytes, malformed, duplicated, unknown-field-bearing, trailing-data-bearing, incompatible, or inconsistent with its key. | Live-instance registry diagnostic; status 1. Attach reads descriptors without acquiring the registry lock. |
| `CODEX_BIN` or `codex` cannot be found as an executable through `PATH`. | `codex attach: locate`; status 1. |
| The located executable path cannot be made absolute before the managed-directory change. | `codex attach: resolve executable`; status 1. |
| The managed directory no longer exists or cannot become the process working directory. | `codex attach: change directory`; status 1. |
| The attach process cannot obtain its current working directory before replacement. | `codex attach: get working directory`; status 1. |
| Process replacement fails. | `codex attach: execute`; status 1 after the original working directory is restored when restoration succeeds. |
| Process replacement fails and the original working directory cannot be restored. | Joined execute and restore diagnostics; status 1. |
| Process replacement succeeds but Codex cannot connect, authenticate, match the running app-server version, or initialize the proxy. | Codex diagnostic and Codex exit status. `intercom` has already been replaced. |

#### Exit status

Status is 0 after help. Configuration, discovery, validation, directory, or process-replacement failure produces status 1. Successful replacement has the Codex process's eventual exit status.

#### Example

With `intercom-codex-project --name reviewer --cwd .` running in one terminal, a second terminal attaches the TUI:

```sh
intercom codex attach --name reviewer
```

#### See also

[`intercom codex`](#intercom-codex), [`intercom codex sessions`](#intercom-codex-sessions), [`intercom-codex-project`](#intercom-codex-project), [live Codex instance descriptors](#live-codex-instance-descriptors)

### intercom codex sessions

#### Synopsis

```text
intercom codex sessions --app-server ENDPOINT [--cwd DIRECTORY] [--all] [--list]
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `--app-server ENDPOINT` | Unix endpoint or filesystem path | required | absolute path or `unix` URL | none | Selects the dedicated Codex app-server Unix socket. Syntax matches `intercom codex --app-server`. |
| `--cwd DIRECTORY` | existing directory path | optional | filesystem path | current working directory | Supplies the working-directory filter and the directory required for the selected result. The command converts it to an absolute path, resolves symbolic links, and requires a directory. |
| `--all` | Boolean | optional | none | false | Omits the working-directory filter from app-server listing. Source, archive, ephemeral, root-thread, and idle-or-not-loaded status eligibility rules remain active. |
| `--list` | Boolean | optional | none | false | Writes every matching record without reading a terminal selection. |
| `-h`, `--help` | Boolean | optional | none | false | Prints command help and exits. |

#### Semantics

The command connects to an already listening app-server, initializes its experimental API as `intercom_session_picker`, and requests all pages of non-archived CLI and VS Code threads in descending recency order. It defensively removes empty or invalid IDs, ephemeral threads, child threads, statuses other than `idle` or `notLoaded`, other sources, duplicate IDs, and, unless `--all` is set, records whose `cwd` does not exactly equal the selected absolute directory. It does not start or stop app-server and does not read or change an Intercom binding.

Without `--list`, standard input must be a character device. The numbered picker is written to standard error. Each entry contains its UTC recency time, source, sanitized title, complete thread ID, and working directory. It accepts a number from 1 through the number of entries or `q`, case-insensitively. Invalid input prints the permitted range and prompts again. A valid selection writes only the complete thread ID and a newline to standard output.

With `--list`, each output line contains four tab-separated fields in this order: complete thread ID, UTC RFC 3339 recency timestamp, sanitized working directory, and sanitized title. The title is the nonblank thread name, then preview, then `(untitled)`. Newlines and other whitespace in displayed metadata collapse to one ASCII space; nonprinting characters become `�`; picker source and title fields are bounded for display, while IDs and working directories are not truncated.

`--all` does not authorize an implicit managed-directory change. An interactive selection whose returned `cwd` differs from the selected `--cwd` fails and identifies the required `--cwd`. List mode reports records from all directories without this final equality check.

#### Errors

| Condition | Result |
|---|---|
| An unknown option or positional argument is present. | Command parsing diagnostic; status 1. |
| `--app-server` is absent, malformed, or cannot be reached. | Required-flag, endpoint, dial, or WebSocket-upgrade diagnostic; status 1. |
| `--cwd` is absent and the current directory cannot be read, or its value cannot be made absolute, resolved through symbolic links, statted, or confirmed as a directory. | `codex: get working directory`, `codex: resolve cwd`, or `codex sessions: canonicalize project directory`; status 1. |
| App-server initialization or the initialized notification fails. | `codex sessions: initialize app-server` or `complete app-server initialization`; status 1. |
| Any `thread/list` page fails, returns an empty cursor, or repeats a cursor. | `codexsession: list sessions` or pagination diagnostic; status 1. |
| Interactive mode receives no eligible records. | `codexsession: no resumable sessions`; status 1. |
| Interactive mode's standard input is not a terminal. | `codex sessions: interactive selection requires a terminal; supply an explicit session id`; status 1. |
| Interactive input ends or reads `q`, case-insensitively. | `codexsession: selection canceled`; status 1. |
| An interactive input token reaches the picker's 4096-byte scanner capacity, or the input stream fails. | `codexsession: read picker`; status 1. |
| An interactive all-directory selection has another working directory. | Diagnostic names the selected directory and required `--cwd`; status 1. |
| Picker, list, or selected-ID output fails. | Write diagnostic; status 1. |

#### Exit status

Status is 0 after help, complete list output including an empty list, or one valid same-directory selection. Status is 1 after parsing, connection, protocol, discovery, terminal, selection, or output failure.

#### Example

The following transcript starts a temporary app-server and writes eligible current-project sessions:

```sh
(
  runtime_dir=$(mktemp -d ./.intercom-sessions.XXXXXX)
  chmod 700 "$runtime_dir"
  socket=$(cd "$runtime_dir" && pwd -P)/app-server.sock
  codex app-server --listen "unix://$socket" &
  server_pid=$!
  cleanup() {
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
    rm -rf "$runtime_dir"
  }
  trap cleanup EXIT
  for ((attempt = 0; attempt < 300; attempt++)); do
    [ -S "$socket" ] && break
    kill -0 "$server_pid" 2>/dev/null || exit 1
    sleep 0.1
  done
  if [ ! -S "$socket" ]; then
    printf 'app-server socket did not appear within 30 seconds\n' >&2
    exit 1
  fi
  intercom codex sessions --app-server "$socket" --cwd . --list
)
```

#### See also

[`intercom codex`](#intercom-codex), [`intercom-codex-project`](#intercom-codex-project), [Handbook: resume, adopt, fork, or replace](HANDBOOK.md#5-resume-adopt-fork-or-replace-a-codex-thread)

### intercom broker

#### Synopsis

```text
intercom broker [--idle-after DURATION] [--foreground]
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `--idle-after DURATION` | duration | optional | Go duration | `10m` | Exits after the broker has no registered peers for the selected duration. `0` disables idle exit. An explicit flag takes precedence over `INTERCOM_IDLE_EXIT`. |
| `--foreground` | Boolean | optional | none | false | Writes structured logs to standard error instead of the broker log file. It does not daemonize the process. |
| `-h`, `--help` | Boolean | optional | none | false | Prints command help and exits. |

#### Semantics

The broker obtains an exclusive nonblocking advisory lock, removes a stale socket entry, binds the Unix socket, sets socket mode 0600, and routes in-memory frames until a signal or idle exit. A second broker invocation that finds the lock held exits successfully without changing the running broker.

The broker accepts multiple connections concurrently. Each connection must send `hello` first and must claim a unique valid peer name. Shutdown sends `goodbye` with reason `shutdown` when the connection remains writable.

#### Errors

| Condition | Result |
|---|---|
| A positional argument is present and help is not requested. | Status 1. |
| An unknown option or an invalid Go-duration option value is present. | Status 1, including when help is also present. |
| `--idle-after` or `INTERCOM_IDLE_EXIT` is not a Go duration. | The parse error is reported; status 1. |
| The selected idle duration is negative. | `broker: idle exit must be non-negative`; status 1. |
| Runtime-directory creation or home lookup fails. | The path error is reported; status 1. |
| The broker log cannot be opened in non-foreground mode. | `open broker log`; status 1. |
| Lock open or `flock` fails for a reason other than an already-held lock. | `broker: open lock` or `broker: flock`; status 1. |
| Stale socket removal, socket bind, or socket chmod fails. | Operation-specific broker diagnostic; status 1. |
| Another process holds the broker lock. | Status 0; the existing broker is left unchanged. |

Per-connection malformed frames and routing failures produce protocol error or negative acknowledgement frames. They do not normally terminate the broker.

#### Exit status

Status is 0 after help, handled signal, idle exit, or already-held broker lock. Status is 1 after option, path, log, lock, or listener failure.

#### Example

```sh
intercom broker --foreground --idle-after 0
```

#### See also

[Broker protocol](BROKER_PROTOCOL.md), [broker files](#broker-files)

### intercom name

#### Synopsis

```text
intercom name
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `-h`, `--help` | Boolean | optional | none | false | Prints command help and exits. |

#### Semantics

The command prints one peer name and a newline. It trims and validates a nonblank `INTERCOM_NAME`; otherwise it validates and prints the current working-directory basename. It does not connect to or start a broker.

#### Errors

Without help selection, the command exits with status 1 when arguments are present, the working directory cannot be obtained, the selected name violates the peer-name grammar, or standard output rejects the result. An output failure is reported as `name: write output: ...`.

#### Exit status

Status is 0 after a name or help is printed. Status is 1 after argument, working-directory, validation, or output failure.

#### Example

```console
$ INTERCOM_NAME=reviewer intercom name
reviewer
```

#### See also

[Peer names](#peer-names), [`INTERCOM_NAME`](#environment)

### intercom peers

#### Synopsis

```text
intercom peers
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `-h`, `--help` | Boolean | optional | none | false | Prints command help and exits. |

#### Semantics

The command connects as transient peer `intercom-peers`, starts a broker when needed, lists other connected peer names in bytewise sorted order, prints one name per line, and disconnects. It prints `(no other peers connected)` when the list is empty.

Starting a broker through this command leaves that broker running until its idle timeout.

#### Errors

Without help selection, the command exits with status 1 when arguments are present, runtime paths or broker executable cannot be resolved, broker startup or connection fails, `intercom-peers` is already registered, the broker rejects the request, the connection drops, or standard output rejects a result line. An output failure is reported as `peers: write output: ...`.

#### Exit status

Status is 0 after the list, empty marker, or help is printed. Status is 1 after command, connection, broker, protocol, or output failure.

#### Example

```console
$ intercom peers
implementer
reviewer
```

#### See also

[`list_peers`](#list_peers), [broker auto-start](ARCHITECTURE.md#broker-lifecycle)

### intercom completion

#### Synopsis

```text
intercom completion bash [--no-descriptions]
intercom completion fish [--no-descriptions]
intercom completion powershell [--no-descriptions]
intercom completion zsh [--no-descriptions]
```

#### Arguments

| Argument | Type | Mode | Units or limits | Default | Meaning |
|---|---|---|---|---|---|
| shell | enumeration | required for generation | one token; `bash`, `fish`, `powershell`, or `zsh` | none | Selects the completion target. An absent or unsupported value selects parent help instead of generation. |

No positional arguments follow the shell name.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `--no-descriptions` | Boolean | optional | none | false | Omits command and flag descriptions from generated candidates. |
| `-h`, `--help` | Boolean | optional | none | false | Prints help for the selected completion command. |

#### Semantics

The command writes a shell program to standard output and does not install it. Bash output requires the `bash-completion` package. Z shell output requires `compinit` to be enabled.

#### Errors

An absent or unsupported shell name prints completion help and produces status 0. An extra argument after a supported shell produces status 1 unless help is requested. An unknown option produces status 1 even with help. A generator-reported standard-output failure produces status 1.

#### Exit status

Status is 0 after generation, explicit help, or parent help caused by an absent or unsupported shell. Status is 1 after an extra argument, unknown option, or generator-reported output failure.

#### Example

```sh
intercom completion bash > intercom.bash
```

#### See also

[`intercom help`](#intercom-help)

### intercom help

#### Synopsis

```text
intercom help [COMMAND ...]
```

#### Arguments

| Argument | Type | Mode | Units or limits | Default | Meaning |
|---|---|---|---|---|---|
| `COMMAND ...` | command-path components | optional | zero or more command tokens | root command | Selects the command whose help is printed. |

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `-h`, `--help` | Boolean | optional | none | false | Prints help for the help command. |

#### Semantics

The command prints the same help selected by `COMMAND --help`. Command-path components after a recognized leaf command are ignored.

#### Errors

An unknown command path writes `Unknown help topic`, prints root usage, and produces status 0. An unknown option produces status 1.

#### Exit status

Status is 0 after help or the unknown-topic diagnostic is printed. Status is 1 after option parsing fails.

#### Example

```sh
intercom help codex
```

#### See also

[`intercom root`](#intercom-root)

### intercom-codex-project

#### Synopsis

```text
intercom-codex-project [--name NAME] [--cwd DIRECTORY] [--new] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom-codex-project [--name NAME] [--cwd DIRECTORY] --adopt [SESSION_ID] [--all-sessions] [--replace-binding] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom-codex-project [--name NAME] [--cwd DIRECTORY] --fork-from [SESSION_ID] [--all-sessions] [--replace-binding] [--yolo | --dangerously-bypass-approvals-and-sandbox]
intercom-codex-project [--cwd DIRECTORY] --list-sessions [--all-sessions]
```

#### Arguments

The launcher scans every token for launcher-owned transport options and internal selection options before any other action and rejects them. When no prohibited token is present, any `-h` or `--help` token prints launcher help and suppresses timeout validation, child creation, and validation of other tokens. It then consumes launcher session-selection options and forwards the remaining tokens to `intercom codex`; that command rejects unknown forwarded tokens. List-only mode accepts only `--cwd`, `--all-sessions`, `--list-sessions`, and help and rejects an adapter-only, unknown, or positional token before child creation. `--adopt` and `--fork-from` consume the immediately following token as an optional session ID only when that token does not begin with `-`. A consumed empty token disables the picker and forwards an empty internal ID, which the adapter treats as no adoption or fork selection. The `--adopt=SESSION_ID` and `--fork-from=SESSION_ID` forms always supply an explicit, nonempty ID. After the preliminary scan has found neither a prohibited token nor help, split-form `--cwd` consumes the immediately following token as its directory even when that token begins with `-`; `--cwd=DIRECTORY` does not consume another token.

#### Options

| Option | Type | Mode | Units or format | Default | Meaning |
|---|---|---|---|---|---|
| `--name NAME` | peer name | optional | 1–64 ASCII bytes; `[A-Za-z0-9_-]` | adapter default | Forwards the peer name. |
| `--cwd DIRECTORY` | directory path | optional | one following token in split form, or the `=` value | adapter default | Forwards the managed directory. After the preliminary help and prohibited-option scan, split form consumes the next token even when it resembles another option. |
| `--new` | Boolean | optional | none | false | Forwards the new-binding selection. |
| `--adopt [SESSION_ID]` | optional Codex thread ID | optional | zero or one token; a nonempty selected ID is 1–256 printable UTF-8 bytes with no whitespace | interactive selection | Selects exact adoption. A separate ID token cannot begin with `-`. A separate empty token disables the picker but selects no adoption. The `--adopt=SESSION_ID` form accepts any nonempty token for later ID validation. |
| `--fork-from [SESSION_ID]` | optional Codex thread ID | optional | zero or one token; a nonempty selected ID is 1–256 printable UTF-8 bytes with no whitespace | interactive selection | Forks the selected session into a distinct managed thread and leaves the source unchanged. A separate empty token disables the picker but selects no fork. Other optional-value parsing matches `--adopt`. |
| `--all-sessions` | Boolean | optional | none | false | Removes the working-directory filter from list or ID-less interactive selection. It is invalid with an explicit session ID or without `--list-sessions`, ID-less `--adopt`, or ID-less `--fork-from`. |
| `--list-sessions` | Boolean | optional | none | false | Lists matching resumable session records and exits after stopping app-server. It does not start the adapter, broker peer, TUI proxy, or MCP bridge. |
| `--replace-binding` | Boolean | optional | none | false | Forwards explicit authorization to replace another saved binding. It requires `--adopt` or `--fork-from`. |
| `--yolo` | Boolean | optional | none | false | Forwards service-wide approval `never` and `danger-full-access` selection. |
| `--dangerously-bypass-approvals-and-sandbox` | Boolean | optional | none | false | Alias for `--yolo`. |
| `-h`, `--help` | Boolean | optional | none | false | Prints launcher help without starting children. |
| `--app-server` | option token | prohibited | split or `=` form | not applicable | Produces status 2 before starting children. The launcher owns the endpoint. |
| `--client-endpoint` | option token | prohibited | split or `=` form | not applicable | Produces status 2 before starting children. The launcher owns the endpoint. |
| `--mcp-bridge` | option token | prohibited | split or `=` form | not applicable | Produces status 2 before starting children. The launcher owns the endpoint. |
| `--adopt-session` | option token | prohibited | split or `=` form | not applicable | Produces status 2 and directs the caller to `--adopt`. |
| `--fork-session` | option token | prohibited | split or `=` form | not applicable | Produces status 2 and directs the caller to `--fork-from`. |

#### Semantics

After option and timeout validation, the launcher selects `${XDG_RUNTIME_DIR}`, then `${TMPDIR}`, then `/tmp` as its runtime base. It creates a unique mode-0700 temporary directory, resolves that directory to a physical absolute path, and rejects a resolved pathname containing `%`, `?`, or `#` because the raw Unix endpoint must be accepted by both Codex and the Go URL parser. The directory assigns the `app-server.sock`, `client.sock`, `mcp-bridge.sock`, and `app-server.session` pathnames. Separate launcher processes therefore use separate endpoints without port allocation.

The launcher disables inherited shell job control and starts `INTERCOM_BIN codex-app-server-exec --ready-file SESSION_FILE -- CODEX_BIN app-server --listen APP_SERVER_ENDPOINT`. App-server standard output is redirected to launcher standard error. A first terminal signal received between asynchronous child launch and capture of `$!` is deferred until the launcher records that PID; another terminal signal in the same interval does not replace it. The deferred signal is dispatched immediately after capture, so cleanup never begins with an unrecorded app-server child.

The hidden helper requires an absolute readiness-file path, resolves `CODEX_BIN`, calls `setsid(2)`, writes its PID and a newline to a temporary file, forces that file to mode 0600 with `chmod(2)` independently of process umask, closes it, publishes the inode at `SESSION_FILE` with an atomic hard link that does not replace an existing marker, removes the temporary link, and calls `exec(2)`. The marker is therefore visible only after the helper becomes a session leader and before Codex app-server can execute or create descendants. Codex retains the launcher's direct-child PID as both PID and session ID; the helper does not remain as a wrapper process.

The launcher polls every 100 milliseconds until the marker contains the direct-child PID and the upstream socket exists. It fails immediately when the marker contains another value or the direct child exits. An early direct-child exit runs a process-session sweep against the recorded PID before the shell collects the child status. Marker and socket waits share the startup timeout. The adapter starts later as an ordinary launcher child outside the Codex process session.

For `--list-sessions`, the launcher next runs `INTERCOM_BIN codex sessions --app-server APP_SERVER_ENDPOINT [--cwd DIRECTORY] [--all] --list`, propagates its status, and stops app-server. For an ID-less adopt or fork, it runs the same selector without `--list`, leaves its standard error attached to the terminal, captures the one-line session ID from standard output, and rejects an empty or multiline result. An explicit ID bypasses selector startup.

The service path starts `INTERCOM_BIN codex --app-server APP_SERVER_ENDPOINT --client-endpoint CLIENT_ENDPOINT --mcp-bridge MCP_BRIDGE_PATH`, adds the internal adopt or fork ID when selected, and forwards the remaining adapter options. Adapter standard output remains launcher standard output and carries the readiness block. A dynamic-tool thread does not create the MCP socket; an adopted, forked, or resumed MCP-bridge thread creates it with mode 0600 inside the private directory.

The first printed attach command appears only after app-server initialization, managed-thread and tool validation, broker registration, downstream proxy startup, and live-descriptor publication. It includes the execution-policy name and attachment commands that inherit that policy. The launcher continues running after a TUI disconnect. Multiple launchers may run on one machine subject to operating-system resource limits. Peer names must be distinct within one broker and within one `INTERCOM_DIR` managed-binding namespace. Managed thread IDs must also be distinct across live Intercom adapters sharing one `CODEX_HOME`. Equal names on different broker sockets still contend for `$INTERCOM_DIR/codex/NAME.lock`. Separate `INTERCOM_DIR` values isolate binding names but do not isolate thread ownership because the thread lock is stored under `CODEX_HOME`. Each launcher accepts at most one TUI at a time.

The service group remains in the foreground. A 100-millisecond poll checks the adapter first and app-server session leader second. An observed child exit stops the other child. When both children become nonrunning between observations, adapter status takes precedence. Signal cleanup stops the adapter before app-server and removes the temporary directory. The launcher ignores later `SIGHUP`, `SIGINT`, and `SIGTERM` after cleanup begins.

An established marker causes the launcher to invoke `INTERCOM_BIN codex-process-session-cleanup --sid PID --leader PID --timeout DURATION`. A marker with another value marks cleanup failed but still selects the expected direct-child PID for both hidden-helper arguments. The Linux and Darwin helper ignores `SIGHUP`, `SIGINT`, and `SIGTERM` after argument validation. It invokes `ps -A -o pid= -o stat=` only to obtain candidate PID and state values. It considers only positive-PID, non-zombie candidates. `getsid(2)` establishes current membership in the recorded session; `ESRCH` means that the candidate has gone. Another enumeration or membership error is retained while other candidates remain eligible.

Each cleanup pass sorts verified member PIDs, calls `getsid(2)` again immediately before signaling each one, signals descendants first, and signals the leader last. A successful signal or `ESRCH` marks that PID complete for the current phase; it is not signaled again in that phase. A failed membership check or non-`ESRCH` signal remains eligible for a later pass. Other enumeration, verification, or signal errors are retained and retried until the applicable phase deadline. The process-table row does not authorize a signal. The final membership verification and `kill(2)` are separate operations, so PID reuse between them remains a small best-effort race. A PID reused after it is marked complete is also suppressed for the remainder of that phase. Reuse of the original leader PID as the ID of an unrelated new session after the original session becomes empty is a second, very-low-probability best-effort boundary.

The `SIGTERM` phase and `SIGKILL` phase receive independent full timeout intervals measured with a monotonic clock. Enumeration, membership checks, signals, and sleeps count against the applicable interval. Each phase-bounded process enumeration receives the remaining phase interval as a context deadline, and each phase polls at intervals no greater than 100 milliseconds. If the session is not empty at the `SIGTERM` deadline, cleanup separately checks whether the non-zombie leader still belongs to the session under a context deadline equal to the smaller of one second and the configured phase timeout. It then prints the leader or descendants diagnostic and starts the `SIGKILL` phase. Failure of that diagnostic-only leader check selects the descendants diagnostic; the `SIGKILL` phase performs authoritative membership checks. The final inspection has the same bounded context rule. It reports inspection failure, a retained persistent signal failure, or the sorted surviving member list. An empty final session succeeds even when a transient phase error occurred.

Before marker publication, cleanup sends `SIGTERM` to the launcher-owned direct child and polls for up to one shutdown timeout. Marker appearance, including a marker with the wrong value, switches to established-session cleanup. A child that exits without a marker is reaped. A child that remains alive without a marker receives direct-child `SIGKILL` and another shutdown-timeout wait. The marker is published before app-server execution, so no app-server descendant can exist in the unmarked state.

If the session-cleanup helper fails, the launcher marks cleanup failed and sends `SIGKILL` to the direct child when that child remains live. It waits one further shutdown timeout for that fallback. If the helper reports success while the direct child remains live, the launcher treats that result as inconsistent, reports it, marks cleanup failed, and applies the same fallback. A descendant that creates another process group remains a member of the dedicated session and is covered. A descendant that calls `setsid(2)` creates another session and is outside the cleanup guarantee. The original leader can exit while descendants retain the original session ID.

The launcher attempts recursive removal of the private runtime directory after both child cleanups. Removal failure prints a diagnostic and marks cleanup failed. Cleanup failure changes an initiating status 0 to status 1. It does not replace an existing nonzero child status or the 129, 130, or 143 status selected by the first terminal signal.

#### Errors

| Condition | Result |
|---|---|
| Either nonblank timeout variable is nondecimal, zero, or greater than 9223372036 seconds, and help is not requested. | Status 2 before runtime-directory or child creation. Leading zeroes are accepted for a positive value. An unset or empty variable selects its default. |
| `--app-server`, `--client-endpoint`, or `--mcp-bridge` is supplied in split or `=` form. | Status 2 before child creation. |
| `--adopt-session` or `--fork-session` is supplied in split or `=` form. | Status 2 before child creation; the diagnostic names the public launcher option. |
| More than one occurrence or kind among `--new`, `--adopt`, `--fork-from`, and `--list-sessions` is selected. | Status 2 before child creation. |
| `--adopt=` or `--fork-from=` has an empty value. | Status 2 before child creation. |
| `--cwd` is the final token and has no directory operand. | Status 2 before child creation. |
| Split-form `--cwd` is followed by an option-looking token other than a globally scanned help or prohibited token. | The launcher consumes that token as the directory operand, so it is not processed as an option. Remaining tokens are processed independently and can fail launcher validation or adapter parsing before directory validation. Otherwise, an invalid consumed path fails directory validation after app-server startup. The `--cwd=DIRECTORY` form prevents option-token consumption. |
| `--all-sessions` is not paired with `--list-sessions` or ID-less adoption or fork. | Status 2 before child creation. |
| `--replace-binding` is not paired with adoption or fork. | Status 2 before child creation. |
| `--list-sessions` is combined with `--name`, yolo selection, an unknown option, or a positional or other adapter-only token. | `--list-sessions does not accept adapter argument`; status 2 before child creation. |
| Temporary-directory creation or chmod fails. | Status 1. |
| The created runtime directory cannot be resolved to a physical absolute path. | Status 1 before child creation; the directory is removed. |
| The resolved runtime directory contains `%`, `?`, or `#`. | Status 2 before child creation; the directory is removed. |
| The internal session-exec helper receives a non-absolute readiness path. | `intercom: app-server session ready file must be absolute: "PATH"`; status 1. The launcher always supplies an absolute path. |
| Process sessions are unavailable on the built platform. | `intercom: app-server process sessions are unavailable on this platform`; app-server startup fails. |
| The hidden helper cannot resolve the Codex executable. | `intercom: resolve Codex executable "CODEX": ...`; app-server startup fails. |
| `setsid(2)` fails in the hidden helper. | `intercom: create app-server process session: ...`; app-server startup fails. |
| Atomic marker creation, writing, mode forcing, closing, linking, or temporary-file removal fails. | `intercom: publish app-server process session: ...`; app-server startup fails. The marker can already exist when removal of its temporary hard link fails. |
| `exec(2)` fails after marker publication. | `intercom: exec Codex app-server: ...`; app-server exits before readiness. |
| App-server exits before both the marker and socket are ready. | `intercom-codex-project: app-server exited before readiness`; its nonzero status is propagated, and status 0 maps to 1. |
| The marker contains a value other than the launcher direct-child PID. | `intercom-codex-project: app-server published process session VALUE, want PID`; startup fails, cleanup is marked failed, and session cleanup targets the expected PID. |
| The marker does not appear before the startup timeout. | `intercom-codex-project: app-server did not establish its process session after Ns`; status 1. Cleanup sends direct-child signals while the marker remains absent. |
| The marker is valid but the socket does not appear before the startup timeout. | `intercom-codex-project: app-server was not ready after Ns`; status 1 and established-session cleanup. |
| Session listing or interactive selection fails. | Selector status is propagated; app-server is stopped and no adapter starts. |
| Interactive selection returns an empty or multiline ID despite selector success. | Status 1; app-server is stopped and no adapter starts. |
| Adapter startup or runtime fails. | Adapter status is propagated; app-server is stopped. |
| Downstream proxy creation, live-descriptor publication, or readiness-output writing fails. | Adapter status 1 is propagated; app-server is stopped. No usable attach command is printed. |
| App-server exits while the adapter runs. | Nonzero app-server status is propagated; status 0 maps to 1; adapter is stopped. |
| The adapter ignores `SIGTERM` for the shutdown timeout. | `intercom-codex-project: adapter did not stop; killing it`; the adapter receives `SIGKILL`, and the initiating status remains in effect. |
| The adapter survives the post-`SIGKILL` shutdown timeout. | `intercom-codex-project: adapter survived SIGKILL`; cleanup is marked failed. |
| A pre-marker app-server direct child survives `SIGTERM` for one shutdown timeout. | `intercom-codex-project: app-server did not stop before creating its process session; killing it`; the child receives `SIGKILL` and a second timeout wait. |
| A pre-marker app-server direct child survives the post-`SIGKILL` timeout. | `intercom-codex-project: app-server direct child survived SIGKILL`; cleanup is marked failed. |
| The app-server session leader remains alive at the process-session `SIGTERM` deadline. | `intercom-codex-project: app-server did not stop; killing it`; verified members enter an independent full-timeout `SIGKILL` phase. |
| The app-server session leader is absent, but another member remains at the `SIGTERM` deadline. | `intercom-codex-project: app-server descendants did not stop; killing them`; verified members enter an independent full-timeout `SIGKILL` phase. A descendant that calls `setsid(2)` is outside this guarantee; changing process group alone does not leave the session. |
| The internal cleanup helper receives a nonpositive session ID, a nonpositive leader PID, unequal session and leader values, or a nonpositive timeout. | `intercom: --sid must be a positive process ID`, `--leader must be a positive process ID`, `--leader must equal --sid`, or `--timeout must be a positive duration`; cleanup is marked failed. The launcher supplies valid values. |
| `ps` cannot run or its output cannot be scanned or parsed through the final `SIGKILL` inspection. | `intercom: inspect app-server process session SID after SIGKILL: enumerate processes: run ps: ...`, `scan ps output: ...`, or `parse ps output line N: ...`; cleanup is marked failed. A parse error reports `expected pid and state` or identifies an invalid PID. |
| `getsid(2)` fails for a candidate through the final `SIGKILL` inspection. | `intercom: inspect app-server process session SID after SIGKILL: inspect process PID session: ...`; `ESRCH` is not an error. Cleanup is marked failed. |
| Immediate membership reverification or signaling fails persistently during `SIGKILL`, and final inspection still finds members. | `intercom: stop app-server process session SID with SIGKILL: reverify process PID before SIGKILL: ...` or `signal process PID with SIGKILL: ...`; `ESRCH` is not an error. Cleanup is marked failed. |
| Verified process-session members survive the `SIGKILL` deadline without a retained signal error. | `intercom: app-server process session SID still has processes after SIGKILL: PID, PID`; cleanup is marked failed. |
| The session-cleanup helper is unavailable on the built platform. | `intercom: app-server process-session cleanup is unavailable on this platform`; cleanup is marked failed. |
| The session-cleanup helper reports success while the direct app-server child remains live. | `intercom-codex-project: app-server process-session cleanup left its direct child running; killing it`; cleanup is marked failed and the child receives fallback `SIGKILL`. |
| The session-cleanup helper fails and its direct child survives fallback `SIGKILL`. | `intercom-codex-project: app-server direct child survived fallback SIGKILL`; cleanup remains failed. |
| Recursive private-runtime-directory removal fails. | `intercom-codex-project: could not remove runtime directory PATH`; cleanup is marked failed. |
| Another `SIGHUP`, `SIGINT`, or `SIGTERM` arrives after cleanup begins. | The launcher and session-cleanup helper ignore it; the initiating status remains in effect. |
| Launcher help output cannot be written completely. | The shell can report the `printf` failure on standard error, but the launcher does not propagate it and help retains status 0. No children start. |

#### Exit status

| Status | Meaning |
|---|---|
| 0 | Help was requested, including when its output write failed, or the requested list or service operation completed successfully and cleanup did not fail. |
| 1 | Launcher startup or readiness failed, app-server exited unexpectedly with status 0, the observed child returned status 1, or cleanup failed after an otherwise successful operation. |
| 2 | Launcher option, timeout, or runtime-path URL-delimiter validation failed, or the observed child returned status 2. |
| 129 | `SIGHUP` terminated the launcher, or the observed child returned status 129. |
| 130 | `SIGINT` terminated the launcher, or the observed child returned status 130. |
| 143 | `SIGTERM` terminated the launcher, or the observed child returned status 143. |
| other | The nonzero status of the child observed to have exited. Adapter status takes precedence when both children stop between polls. Cleanup failure does not replace an existing nonzero child status or signal status. |

#### Example

```sh
intercom-codex-project --name reviewer --cwd .
```

The command prints a shell-safe `intercom codex attach --name reviewer` command prefixed with its descriptor-discovery and Codex-selection environment. That complete line runs in another terminal while the launcher remains in the foreground.

The following invocation presents eligible sessions in the current project, adopts the selected ID exactly, and permits replacement of another saved binding only after the selected session passes validation:

```sh
intercom-codex-project --name reviewer --cwd . --adopt --replace-binding
```

#### See also

[`intercom codex`](#intercom-codex), [`intercom codex attach`](#intercom-codex-attach), [`intercom codex sessions`](#intercom-codex-sessions), [Handbook: managed Codex](HANDBOOK.md#3-add-a-managed-codex-peer), [Handbook: resume, adopt, fork, or replace](HANDBOOK.md#5-resume-adopt-fork-or-replace-a-codex-thread)

## AGENT TOOLS

### send_message

#### Signature

```text
send_message(to=NAME, message=TEXT) -> tool result
```

#### Arguments

| Argument | JSON type | Mode | Units and limit | Default | Meaning |
|---|---|---|---|---|---|
| `to` | string | required | 1–64 ASCII bytes; peer-name grammar | none | Names the live destination peer. The value is not trimmed. |
| `message` | string | required | 1–204800 UTF-8 bytes before JSON escaping; encoded delivery at most 262144 bytes | none | Supplies the exact delivered body. The value is not trimmed. |

The argument value must be one JSON object. Additional members are rejected.

#### Semantics

The tool validates arguments, connects or reconnects to the broker, issues one correlated `send`, and waits for `send_ack`. An accepted result is `Message sent to "NAME".`.

Acceptance means that the broker wrote a `deliver` frame to the live destination connection. It does not mean that the destination adapter retained the frame after the write, that the model observed it, or that a reply exists.

Claude MCP and the Codex MCP bridge return `{"content":[{"type":"text","text":"TEXT"}]}` on success and add `"isError":true` on a tool failure. A Codex app-server dynamic-tool result is `{"contentItems":[{"type":"inputText","text":"TEXT"}],"success":true}` on success and changes `success` to false on a tool failure.

#### Errors

| Exact condition | Tool error text or class |
|---|---|
| After whitespace removal, the argument is empty or its first byte is not `{`. | `send_message arguments must be an object` |
| An argument beginning with `{` is malformed, has a member of the wrong JSON type, contains an unknown member, or is followed by another JSON value. | `decode args: ...` |
| `to` is absent or the empty string. | `"to" is required` |
| `to` violates the peer-name grammar. | `invalid destination peer "..."` |
| `message` is absent or the empty string. | `"message" is required` |
| Raw message length exceeds 204800 bytes. | `message exceeds 204800-byte limit` |
| JSON escaping plus the maximum delivery envelope exceeds 262144 bytes. | `message expands beyond 262144-byte wire frame limit` |
| Broker connection, write, correlation, or reply fails. | `send failed: ...` |
| Destination is not live. | `send rejected (no_such_peer): ...` |
| Destination equals sender. | `send rejected (no_self_send): ...` |
| Delivery write fails or times out. | `send rejected (deliver_failed): ...` |
| The broker-generated delivery envelope exceeds 262144 bytes. | `send rejected (oversize): ...`; shared tool preflight prevents this under the standard broker. |

Claude MCP and Codex MCP-bridge results mark these failures with `isError: true`. Codex dynamic-tool results set `success: false`.

#### Minimal example

```text
send_message(to="reviewer", message="Report correctness defects in the current diff.")
Message sent to "reviewer".
```

#### See also

[`list_peers`](#list_peers), [send protocol](BROKER_PROTOCOL.md#send)

### list_peers

#### Signature

```text
list_peers() -> tool result
```

#### Arguments

The JSON argument must be exactly an object with no members. `{}` is valid. `null`, arrays, scalars, and objects with members are invalid.

#### Semantics

The tool connects or reconnects to the broker and returns other registered peer names in bytewise sorted order. It excludes the caller.

The empty result text is `No other peers are connected.`. A nonempty result has the form `Connected peers: NAME, NAME`.

Claude MCP and the Codex MCP bridge return `{"content":[{"type":"text","text":"TEXT"}]}` on success and add `"isError":true` on a tool failure. A Codex app-server dynamic-tool result is `{"contentItems":[{"type":"inputText","text":"TEXT"}],"success":true}` on success and changes `success` to false on a tool failure.

#### Errors

| Exact condition | Tool error text or class |
|---|---|
| Argument bytes are invalid JSON or the value is an array, string, number, or Boolean. | `decode args: ...` |
| The value is null. | `list_peers arguments must be an object` |
| The object contains any member. | `list_peers does not accept arguments` |
| Broker connection, write, correlation, or reply fails. | `list_peers failed: ...` |

Claude MCP and Codex MCP-bridge results mark these failures with `isError: true`. Codex dynamic-tool results set `success: false`.

#### Minimal example

```text
list_peers()
Connected peers: implementer, reviewer
```

#### See also

[`send_message`](#send_message), [list protocol](BROKER_PROTOCOL.md#list_peers)

## PEER NAMES

A peer name is a nonempty ASCII string matching `[A-Za-z0-9_-]+` and containing at most 64 bytes. Case is significant. Names are unique among live connections to one broker.

Nonblank explicit `--name` and `INTERCOM_NAME` values have surrounding Unicode whitespace removed before validation. A value that becomes blank is treated as unset. Destination names passed to `send_message` are not trimmed.

The operational command `intercom peers` claims `intercom-peers` for the lifetime of its query. That name remains legal for an agent but collides with the operational command while live.

## ENVIRONMENT

| Variable | Type and units | Used by | Default | Semantics and errors |
|---|---|---|---|---|
| `INTERCOM_NAME` | peer name | `shim`, `codex` service, `name` | selected-directory basename | Supplies the peer name after whitespace trimming. `--name` takes precedence for the service. Blank means unset. Invalid content is fatal. `codex attach` ignores this variable and requires `--name`. |
| `INTERCOM_DIR` | directory path | broker, shim, Codex adapter and readiness output, Codex attach, `peers` | `$HOME/.claude-intercom` | Supplies the runtime, binding, and live-instance directory. A missing base directory is created with mode 0700. Existing base-directory permissions are not repaired; the live-instance subdirectory is forced to mode 0700. Readiness prints the symlink-resolved absolute directory. |
| `INTERCOM_SOCKET` | Unix socket path | broker, shim, Codex adapter and readiness output, Codex attach, `peers` | `$INTERCOM_DIR/broker.sock` | Overrides the broker socket and selects the broker identity used to publish or find a live Codex descriptor. Descriptor operations resolve a relative value against each command's current directory, so service and attach invocations must produce the same canonical path. Readiness prints that canonical identity. Its broker lock path is the string plus `.lock`. Its parent is not created as a consequence of this override. |
| `INTERCOM_BROKER_LOG` | file path | broker | `$INTERCOM_DIR/broker.log` | Selects the append-only structured log. `--foreground` writes to standard error instead. |
| `INTERCOM_BROKER_BIN` | executable path or name | shim, Codex adapter, `peers` | running `intercom` executable | Selects the command executed with argument `broker` during auto-start. Empty means default. |
| `INTERCOM_IDLE_EXIT` | Go duration | broker | `10m` | Supplies idle exit only when `--idle-after` is absent. `0` disables. Blank means default. Invalid or negative content is fatal. |
| `CODEX_BIN` | executable path or name | launcher, Codex adapter readiness output, session selection, and `codex attach` | `codex` | Selects the executable passed to the hidden process-session helper with `app-server --listen ENDPOINT`, and the attach executable resolved through `PATH`. Unset or empty selects the default. The readiness attachment command includes this assignment; a slash-containing relative value is displayed as an absolute path. |
| `INTERCOM_BIN` | executable path or name | launcher, process-session helpers, session selection, MCP bridge, and Codex adapter readiness output | `intercom` | Selects the hidden app-server session-exec and session-cleanup helpers, child adapter and selector command, the command injected as the managed MCP server, and the Intercom executable token displayed in the attachment command. Unset or empty selects the default. A slash-containing relative value is displayed as an absolute path. The Nix-packaged launcher defaults to its bundled Intercom binary. |
| `INTERCOM_CODEX_STARTUP_TIMEOUT_SECONDS` | positive decimal seconds | launcher | `30` | Bounds the shared session-marker and socket readiness loop. Unset or empty selects the default. Otherwise zero, nondigits, and values above 9223372036 are fatal with status 2 unless help suppresses timeout validation. |
| `INTERCOM_CODEX_SHUTDOWN_TIMEOUT_SECONDS` | positive decimal seconds per cleanup phase | launcher | `40` | Independently bounds the adapter direct-child `SIGTERM` and `SIGKILL` waits, the app-server process-session `SIGTERM` and `SIGKILL` phases, the pre-marker direct-child `SIGTERM` and `SIGKILL` waits, and the direct-child fallback wait after session-helper failure. Unset or empty selects the default; other validation matches the startup timeout. Established-session phase deadlines use a monotonic clock; shell direct-child waits use 100-millisecond polling. |
| `INTERCOM_CODEX_BRIDGE_TOKEN` | 64-character lowercase hexadecimal token | internal `codex-mcp-bridge` helper | none | Authenticates one service's helper to its private controller socket. The adapter generates and injects it through request-scoped MCP configuration. Users do not set or persist it. An absent value makes the helper fail before serving MCP. |
| `XDG_RUNTIME_DIR` | directory path | launcher | none | Supplies the first-choice base for the launcher temporary directory. |
| `TMPDIR` | directory path | launcher | none | Supplies the second-choice base when `XDG_RUNTIME_DIR` is empty. An unset or empty value selects `/tmp`. |
| `CODEX_HOME` | directory path | app-server, session discovery, thread locks, binding identity, and readiness output | Codex-defined | Selects Codex configuration and rollout storage through Codex. A changed app-server-reported value makes an existing binding incompatible without a new or explicitly replaced selection. A nonempty value is displayed as an absolute assignment in both readiness commands. |
| `HOME` | directory path | path defaults, Codex, shell | operating-system account home | Supplies the Intercom default directory when `INTERCOM_DIR` is empty. Home-resolution failure is fatal for commands that need the default directory. |
| `PATH` | command search list | shell, launcher, and `codex attach` | shell-defined | Resolves `intercom`, `codex`, `ps`, and other launcher utility programs when their variables contain bare names. Process-session cleanup requires `ps -A -o pid= -o stat=`; `getsid(2)` rather than a `ps` field verifies session membership. |

All child processes inherit other environment variables unchanged. Provider authentication and model selection remain Codex and Claude Code concerns.

## FILES

### Broker files

| Path | Mode on creation | Lifetime | Contents and semantics |
|---|---|---|---|
| `$INTERCOM_DIR` | 0700 | persistent | Runtime and managed-state base. Existing mode is not changed. |
| `$INTERCOM_SOCKET` or `$INTERCOM_DIR/broker.sock` | 0600 | broker lifetime | Unix stream socket. A stale entry is removed at broker start. Active-entry removal is attempted on clean broker exit; unlink failure is logged and ignored. |
| `BROKER_SOCKET.lock` | 0600 | persistent | Advisory `flock` file. File existence does not indicate a running broker; lock ownership does. |
| `$INTERCOM_BROKER_LOG` or `$INTERCOM_DIR/broker.log` | 0600 on first creation | persistent | Append-only structured broker log. Existing mode is not changed. |

### Managed Codex binding

`$INTERCOM_DIR/codex` is created with mode 0700 when absent.

| Path | Mode on creation | Lifetime | Contents and semantics |
|---|---|---|---|
| `$INTERCOM_DIR/codex/NAME.json` | 0600 | persistent | Atomic Intercom-to-Codex binding. It contains identity metadata, not conversation messages. |
| `$INTERCOM_DIR/codex/NAME.lock` | 0600 | persistent | Lifetime advisory lock. A running adapter holds `flock`; file existence alone has no ownership meaning. |
| `$CODEX_HOME/.intercom/thread-locks/DIGEST.lock` | 0600 | persistent | Thread-ownership advisory lock shared by Intercom adapters that use this Codex home. `DIGEST` is the lowercase SHA-256 of the thread ID. A running adapter holds it for the managed thread, including across different `INTERCOM_DIR` values. During replacement it also holds the prior binding's lock through commit or rollback. Fork from that prior thread reuses the retained lock and separately locks the returned fork. Ordinary Codex processes do not acquire these locks. |

The binding object contains every member in the following table:

| Member | JSON type | Required value or meaning |
|---|---|---|
| `schemaVersion` | number | State schema `1`. |
| `peer` | string | Selected Intercom peer name. |
| `threadId` | string | Dedicated Codex thread identifier. |
| `cwd` | string | Canonical managed working directory. |
| `codexHome` | string | App-server-reported Codex home identity. |
| `serverUserAgent` | string | App-server user agent from the runtime that most recently passed thread start or resume validation. The value is diagnostic and does not gate resume. |
| `codexVersion` | string | Extracted Codex semantic version from the runtime that most recently passed thread start or resume validation. The value is diagnostic and does not gate resume. |
| `toolContractVersion` | number | Intercom tool contract `1`. |
| `toolTransport` | string | `dynamic` for a thread created by Intercom, or `mcpBridge` for an adopted or forked interactive thread. An omitted schema-1 member is interpreted as `dynamic` and receives the explicit member after a successful persisted update. |
| `materialized` | Boolean | True after the first terminal turn is confirmed readable. |

The state decoder ignores unknown object members. A missing member receives its JSON zero value except for the `toolTransport` compatibility rule above. Missing required members, schema version 0, tool-contract version 0, and an unknown tool transport fail validation; an omitted `materialized` member is false. A successful saved-thread resume refreshes `serverUserAgent` and `codexVersion` atomically after the returned thread, tool status, and materialization state pass validation. A failed resume leaves both diagnostic values unchanged. Adoption and fork replace the entire record only after startup, thread, and tool validation succeeds.

Codex rollout and conversation files remain under `CODEX_HOME` and are owned by Codex. `--new` does not delete them.

### Live Codex instance descriptors

`$INTERCOM_DIR/codex/live` is created or opened when `--client-endpoint` or `intercom codex attach` is used. The directory must be a real directory rather than a symbolic link and is forced to mode 0700.

| Path | Mode on creation | Lifetime | Contents and semantics |
|---|---|---|---|
| `$INTERCOM_DIR/codex/live/.registry.lock` | 0600 | persistent | Cross-process advisory lock for descriptor publication and ownership-checked removal. The mode is repaired on each lock use. |
| `$INTERCOM_DIR/codex/live/NAME-DIGEST.json` | 0600 | adapter readiness interval | Atomic live descriptor. `DIGEST` is the lowercase hexadecimal SHA-256 of the canonical broker socket, one NUL separator, and the peer name. A clean stopping callback removes only the descriptor carrying that adapter's nonce. |

The publisher writes a schema-2 descriptor containing every member below. The reader also accepts schema 1 when `executionPolicy` is omitted, null, or empty and normalizes that descriptor in memory to schema 2 with `workspace-write`. Unknown members, duplicate members, other missing required values, trailing JSON, control characters in text identities, and a total file size outside 1 through 65536 bytes are rejected.

| Member | JSON type | Required value or meaning |
|---|---|---|
| `schemaVersion` | number | The publisher writes `2`. The reader accepts `1` only under the `executionPolicy` compatibility rule above. |
| `peer` | string | Selected Intercom peer name, 1 through 64 bytes under the peer-name grammar. |
| `cwd` | string | Clean absolute managed directory spelling, at most 4096 bytes and without NUL. Symbolic links are not resolved by descriptor validation. |
| `brokerSocketIdentity` | string | Clean absolute broker socket identity used in the descriptor key, at most 4096 bytes and without NUL. |
| `downstreamUnixEndpoint` | string | Canonical `unix` URL for an absolute TUI proxy socket path, at most 4096 bytes, without host, user, query, fragment, opaque form, or NUL. |
| `threadId` | string | Nonblank managed Codex thread identifier, at most 4096 bytes and without control characters. |
| `pid` | number | Positive adapter process ID. |
| `instanceNonce` | string | Random 128-bit lowercase hexadecimal owner nonce. Accepted descriptors permit 16 through 256 ASCII letters, digits, hyphens, or underscores. |
| `codexVersion` | string | Nonblank Codex version, at most 4096 bytes and without control characters. |
| `executionPolicy` | string or null on schema-1 input | `workspace-write` or `danger-full-access`. It is required as a string in schema 2. An omitted, null, or empty value is accepted only in schema 1 and normalizes to `workspace-write`. Attach derives the Codex CLI policy option from the normalized value. |

Publication holds the registry lock and replaces an absent descriptor, a descriptor with the same nonce, or a descriptor whose recorded PID does not exist. A descriptor with another nonce and an existing PID blocks publication. Attach validates the file and checks the PID but does not probe the downstream socket. A process crash can leave a stale descriptor; attach reports it and the next publisher may replace it.

### Launcher files

The launcher creates `intercom-codex.XXXXXX` with mode 0700 beneath its selected runtime base and resolves the created directory to its physical absolute pathname before constructing endpoints. Codex creates `app-server.sock`; the containing directory supplies its Intercom-side access boundary. The adapter creates `client.sock` with mode 0600. An adapter managing an adopted, forked, or resumed MCP-bridge thread also creates `mcp-bridge.sock` with mode 0600 and validates that the parent is a real mode-0700 directory owned by the effective user.

The session-exec helper writes its decimal PID and a newline to `app-server.session.PID.tmp`, calls `chmod(0600)` before close so process umask cannot reduce the final permissions, and hard-links the closed inode to `app-server.session`. An existing destination makes publication fail rather than replace prior content. Successful publication removes the temporary link. The published PID equals the app-server process-session ID and launcher's direct-child PID. The launcher attempts to remove the temporary directory on handled exit even when process cleanup fails; removal failure marks cleanup failed. `SIGKILL`, host failure, shell failure, or removal failure can leave the directory, marker, temporary link, or sockets behind.

## LIMITS AND TIMERS

| Boundary | Value | Unit and scope |
|---|---|---|
| Peer name | 64 | ASCII bytes |
| Raw agent-tool message | 204800 | UTF-8 bytes before JSON escaping |
| Broker JSON payload | 262144 | bytes, excluding the four-byte frame prefix |
| Intercom stdio MCP input line | 8388608 | bytes for Claude shim or Codex MCP helper; a line reaching scanner capacity is rejected |
| Codex app-server JSON message | 134217728 | bytes per WebSocket text message |
| Codex session-list page request | 50 | records requested per app-server page; all cursored pages are collected |
| Codex session ID | 256 | UTF-8 bytes; printable and free of whitespace |
| Codex session-picker input | 4096 | scanner-buffer bytes per entered token; a token that reaches the scanner capacity is rejected |
| Codex MCP bridge frame | 1048576 | JSON bytes excluding the terminating newline; the largest accepted on-wire record is 1048577 bytes |
| Concurrent Codex MCP bridge handlers | 64 | accepted connections admitted to request handling; authentication occurs after admission. The next accepted connection waits for a slot without an overload response, and the listener accepts no additional connection while it waits. |
| Broker accepted connections and registered peers | no Intercom limit | operating-system descriptors, memory, and process resources bound the count |
| Claude concurrent MCP tool handlers | no Intercom limit | one goroutine per active `tools/call`; process resources bound the count |
| Codex inbound delivery queue | 64 | messages not yet started; an attempted 65th queued delivery is fatal |
| Codex selected notification queue | 256 | lifecycle notifications; an attempted 257th queued notification is fatal |
| Codex deferred TUI notification queue | 256 | notifications held behind terminal processing or the corresponding start response; an attempted 257th queued notification is fatal |
| Concurrent app-server reverse handlers | 64 | handlers; an attempted 65th concurrent request is fatal |
| Completed app-server response-ID history | 1024 | response IDs; a repeated older ID is classified as unknown rather than duplicate |
| Expired app-server response-ID history | 1024 | canceled request IDs; one late response is ignored and moved to completed history, while a response for an older expired ID is unknown |
| Attached Codex TUIs per adapter | 1 | downstream WebSocket session, including a session that has not initialized; a concurrent upgrade receives HTTP status 409 |
| Codex TUI proxy JSON message | 134217728 | bytes per WebSocket text message in either direction; matches the stock Codex remote-client limit |
| Concurrent forwarded Codex TUI request handlers | 64 | global across the current TUI, wire-ordered `turn/start` admissions, and detached sessions with unfinished upstream requests; `initialize` and `thread/unsubscribe` do not consume a slot; excess requests receive error -32001 |
| Codex TUI outbound write queue | 256 | responses, notifications, and reverse requests awaiting WebSocket writes; a nonblocking notification enqueue that finds the queue full disconnects the TUI |
| Codex TUI response-barrier queue | 256 | app-server notifications held behind one `thread/resume` or `turn/start` response; an attempted 257th notification disconnects the TUI |
| Expired Codex TUI reverse-response ID history | 1024 | IDs; one late response for a tracked expired relay is ignored, while a response for an older or unrelated ID closes the TUI connection |
| Broker idle exit | 10 | minutes with zero registered peers; configurable |
| Broker initial `hello` deadline | 5 | seconds per accepted connection |
| Broker delivery write deadline | 5 | seconds per destination write |
| Broker shutdown `goodbye` write budget | 1 | second per peer |
| Broker `welcome`, `send_ack`, `list_peers_reply`, and ordinary error writes | no deadline | a connection failure or broker shutdown can still unblock the write |
| Broker-client hello write and welcome read | 5 | seconds each |
| Broker-client request write | 5 | seconds |
| Broker initial dial | 1 | second |
| Broker spawn retry waits | 100 ms, 300 ms, 1 s, 3 s | maximum scheduled waits before up to four one-second-bounded dials; spawned-broker exit ends the current wait early, and scheduling plus dial duration add to elapsed time |
| Codex app-server initial connection | 30 | seconds total; retries begin at 50 ms and double up to an effective 1.6 s step |
| Codex session discovery | 30 | seconds total for app-server connection, initialization, and all list pages; terminal prompting occurs after discovery |
| Codex app-server control operation | 30 | seconds per initialize, initialized notification, thread control call, turn-start write, or turn-start response |
| Codex managed MCP status validation | 30 | seconds total across all status pages during one startup or cold resume |
| Codex startup broker registration | 30 | seconds total around broker-client connection and registration |
| Codex post-terminal materialization confirmation | 30 | seconds total; `thread/read` retries after 50-millisecond delays while Codex reports pre-materialization |
| Codex shutdown | 30 | seconds shared across broker close, turn interrupt and drain, and reverse-handler drain |
| Codex broker-close wait during shutdown | 15 | seconds by default; first half of the shared shutdown budget is reserved for broker close before app-server cleanup proceeds |
| Codex headless or dynamic-tool reverse request | 10 | seconds total: 9 seconds for handling and 1 second for the response write; a request delegated to the TUI uses the separate interactive limit |
| Codex MCP bridge request | 10 | seconds per authenticated tool operation, including broker handling and reply |
| Codex Intercom-tool descendant ancestry | 64 | distinct thread IDs examined by one dynamic-tool or MCP authorization `thread/read` parent-and-fork walk |
| Codex active-turn or persistent-goal inactivity | 15 | minutes since app-server activity while a managed turn is active or an idle persistent goal reserves the scheduler; excludes intervals with one or more pending owned interactive TUI reverse requests and restarts when the last response or fallback finishes |
| Codex TUI pre-ready handshake | 30 | seconds from WebSocket acceptance through successful managed-thread resume; uses the adapter control timeout and releases the sole-session slot on expiry |
| Codex TUI WebSocket write | 30 | seconds per downstream response, notification, or reverse-request frame; uses the adapter control timeout and disconnects the TUI on expiry |
| Codex TUI `turn/start` | 30 | seconds from proxy forwarding through upstream response; uses the adapter control timeout |
| Codex TUI interactive reverse request | 30 seconds, then 15 minutes | control-timeout admission through published-readiness and notification-ordering gates when a proxy exists, followed by the activity timeout awaiting a TUI response; records the expired relay ID when the input deadline wins |
| Codex TUI interactive reverse-response relay | 30 | seconds to write an accepted TUI result or error to app-server; uses a fresh adapter control-timeout context |
| Other forwarded Codex TUI requests | 30 | seconds from proxy forwarding through upstream response; uses the adapter control timeout, returns error -32603 on expiry, and leaves the adapter active |
| Codex broker reconnect sleeps | 100 ms doubling while below 5 s | sequence reaches an effective 6.4 s repeated step |
| Launcher readiness poll | 100 | milliseconds |
| Launcher readiness timeout | 30 | seconds shared by app-server session-marker and socket readiness; configurable |
| Launcher shutdown timeout | 40 | seconds independently for each adapter direct-child phase, app-server process-session phase, pre-marker direct-child phase, and session-helper direct-child fallback wait; configurable |

Tool-call reply waiting uses the calling MCP or Codex request context. It has no additional independent Intercom timeout beyond connection and write budgets.

## ERRORS

`intercom` command failures have the form `intercom: MESSAGE` on standard error and status 1. Subcommand sections enumerate command-specific failure conditions. Broker routing codes are exhaustive in [Broker Protocol: error codes](BROKER_PROTOCOL.md#error-codes).

Tool argument and routing failures are returned to the model as tool failures. They do not terminate a Claude shim. A Codex dynamic-tool or MCP-bridge ownership violation is fatal to the managed adapter because it invalidates sole thread control.

Operating-system errors preserve their underlying diagnostic. They arise when required directories, files, sockets, executables, signals, locks, or standard streams cannot be accessed as described in the command entry.

## NOTES

The broker has no persistent queue, offline mailbox, retransmission log, or end-to-end acknowledgement. A peer disconnect after broker delivery write can lose a message that produced a positive `send_ack`.

Transport is restricted to same-machine Unix sockets. Model traffic is not restricted to the machine: each agent uses its configured model provider.

Peer-delivered text is untrusted model input. Intercom relies on process-account file permissions, unique live peer names, agent instruction precedence, and the managed Codex sandbox. It supplies no peer cryptographic identity or message authorization layer.

## SEE ALSO

[Handbook](HANDBOOK.md), [architecture](ARCHITECTURE.md), [broker protocol](BROKER_PROTOCOL.md), [development](DEVELOPMENT.md)
