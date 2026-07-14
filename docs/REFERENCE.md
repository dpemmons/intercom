# Intercom Command and Tool Reference

## NAME

`intercom-reference` — public command, agent-tool, environment, file, limit, and error contract.

## SYNOPSIS

```text
intercom [--help] [--version]
intercom shim
intercom codex --app-server ENDPOINT [--name NAME] [--cwd DIRECTORY] [--new]
intercom broker [--idle-after DURATION] [--foreground]
intercom name
intercom peers
intercom completion {bash|fish|powershell|zsh} [--no-descriptions]
intercom help [COMMAND ...]
intercom-codex-project [--name NAME] [--cwd DIRECTORY] [--new]

send_message(to=NAME, message=TEXT)
list_peers()
```

## CONTENTS

- [Description](#description)
- [Commands](#commands)
- [Agent tools](#agent-tools)
- [Peer names](#peer-names)
- [Environment](#environment)
- [Files](#files)
- [Limits and timers](#limits-and-timers)
- [Errors](#errors)
- [Notes](#notes)
- [See also](#see-also)

## DESCRIPTION

The `intercom` binary contains the broker, the Claude Code adapter, the managed Codex adapter, and diagnostic commands. `intercom-codex-project` is the supported supervisor for one dedicated Codex app-server and one adapter.

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

| Argument | Type | Mode | Default | Meaning |
|---|---|---|---|---|
| command | command name | optional | none | Selects one command described below. |

#### Options

| Option | Type | Default | Meaning |
|---|---|---|---|
| `-h`, `--help` | Boolean | false | Prints command help and exits. |
| `-v`, `--version` | Boolean | false | Prints `intercom version VERSION (REVISION)` and exits. |

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

| Option | Type | Default | Meaning |
|---|---|---|---|
| `-h`, `--help` | Boolean | false | Prints command help and exits. |

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
intercom codex --app-server ENDPOINT [--name NAME] [--cwd DIRECTORY] [--new]
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Mode | Default | Meaning |
|---|---|---|---|---|
| `--app-server ENDPOINT` | Unix endpoint or filesystem path | required | none | Selects the dedicated Codex app-server Unix socket. A bare value must be an absolute path. A URL value must use `unix`, contain an absolute path, and contain no host, query, fragment, or NUL byte. |
| `--name NAME` | peer name | optional | `INTERCOM_NAME`, then selected-directory basename | Selects the Intercom peer and state filename. The flag takes precedence over the environment. Surrounding whitespace is removed. |
| `--cwd DIRECTORY` | directory path | optional | current working directory | Selects the managed project directory. The adapter resolves the path to an absolute, symlink-canonical directory. |
| `--new` | Boolean | optional | false | Starts a new thread and replaces the saved Intercom binding. It does not delete previous Codex history. |
| `-h`, `--help` | Boolean | optional | false | Prints command help and exits. |

#### Semantics

The command connects to an app-server that is already listening and is externally supervised. It does not start or stop app-server.

The adapter initializes the experimental app-server API and requires server version 0.144.1. It creates or resumes one non-ephemeral thread with approval policy `never`, workspace-write sandbox, no additional writable roots, Intercom developer instructions, and the two Intercom dynamic tools. It acquires a nonblocking lifetime lock for the selected peer before connecting.

One broker delivery occupies one FIFO slot. The adapter starts a Codex turn only while the managed thread is idle. Deliveries that arrive during a turn remain queued. A normal final answer remains in Codex history. Only a successful `send_message` dynamic-tool call creates an outbound Intercom message.

The `completed`, `failed`, and `interrupted` turn statuses are terminal. Each returns the controller to idle without retrying the delivered message. Other completion statuses are fatal protocol violations.

On broker disconnect, the adapter retries registration indefinitely while app-server remains usable. On adapter shutdown, broker disconnect begins before active-turn interruption and app-server reverse-request drain.

The headless app-server reverse-request policy is fixed:

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

#### Errors

The following table enumerates externally visible fatal error classes. Every fatal condition writes a diagnostic and produces status 1.

| Condition | Diagnostic class |
|---|---|
| `--app-server` is absent. | required-flag error |
| The endpoint is relative, malformed, non-`unix`, host-bearing, query-bearing, fragment-bearing, NUL-bearing, or not a usable Unix WebSocket endpoint. | `invalid --app-server`, app-server parse, dial, or upgrade error |
| `--cwd` is absent and the current working directory cannot be obtained. | `codex: get working directory` |
| `--cwd` cannot be made absolute, resolved through symlinks, statted, or does not name a directory. | `codex: resolve cwd`, `resolve cwd symlinks`, `stat cwd`, or `cwd is not a directory` |
| The selected peer name violates the peer-name grammar. | `invalid peer name` |
| The state directory, state file, or lifetime lock cannot be opened, decoded, validated, written, synchronized, replaced, or removed. | `codex state` or `persist new thread binding` |
| Another adapter holds the same peer lifetime lock. | `codex state: peer is already managed` |
| The app-server cannot be reached within 30 seconds. | `codex: app-server unavailable after 30s` |
| App-server initialization or the initialized notification fails. | `initialize app-server` or `send initialized` |
| The app-server user agent has no semantic version or reports a version other than 0.144.1. | `cannot determine app-server version` or `unsupported app-server version` |
| Saved peer, canonical directory, `CODEX_HOME`, server identity, Codex version, state schema, or tool-contract version differs. | identity diagnostic; identity changes require `--new` |
| `thread/start`, `thread/resume`, `thread/read`, or state persistence fails. | operation-specific Codex RPC diagnostic |
| A resumed unmaterialized thread has no rollout. | The adapter replaces the pending binding. This exact case is not fatal. |
| App-server returns the wrong thread ID or directory, an ephemeral thread, a non-idle thread, another approval policy, another sandbox type, extra writable roots, or a non-Boolean workspace network setting. | managed-thread invariant diagnostic |
| A dynamic tool request arrives before adapter ownership is established. | `dynamic tool request arrived before adapter ownership was established` |
| Broker registration fails during startup, including a live peer-name collision. | `codex: register with broker` |
| A delivery arrives while 64 deliveries are already queued. | `inbound delivery queue is full (64)`; the attempted 65th queued delivery is not admitted. |
| A selected lifecycle notification arrives while 256 notifications are already queued. | `app-server notification queue is full`; the attempted 257th notification is not admitted. |
| App-server disconnects. | `codex: app-server disconnected` |
| A 65th app-server reverse request arrives while 64 handlers remain active, or a reverse request arrives after handler draining begins. | `appserver: concurrent reverse request limit exceeded` or `appserver: reverse request received after handler drain began` |
| App-server sends a binary WebSocket message or a text message larger than 16777216 bytes. | `appserver: binary websocket message` or `appserver: websocket message too large` |
| App-server sends malformed JSON or a request, notification, response, error, method, or ID whose envelope cannot be decoded. | `appserver: decode message`, malformed-envelope, decode, or request-ID diagnostic |
| App-server sends a response ID with no pending request. | `appserver: unknown response id` |
| App-server repeats one of the 1024 most recently completed response IDs. | `appserver: duplicate response id` |
| A turn start cannot be written or completed within its control budget. | `codex: start delivery` |
| A `thread/started`, `turn/started`, or `turn/completed` notification cannot decode or violates its managed thread ID, turn ID, controller phase, in-progress status, or terminal-status invariant. | thread, turn, event, completion, or notification consistency diagnostic |
| An `error` lifecycle notification cannot decode. | notification decode diagnostic; a decoded error notification is logged without validating its thread ID or turn ID. |
| An active turn produces no app-server message or handled reverse request for 15 minutes. | `active turn had no app-server activity for 15m0s` |
| A dynamic tool call has invalid Intercom tool arguments or names an unknown tool. | The call returns `success: false`; the adapter continues. |
| Parameters cannot be decoded for a dynamic-tool, command-approval, file-approval, permission-approval, user-input, MCP-elicitation, legacy apply-patch approval, or legacy command-execution approval request. | The call receives app-server error -32602; the adapter continues when the error response succeeds. |
| A dynamic tool call carries a namespace, omits routing identity, arrives before ownership, or names another owned thread or turn. | The call receives a failure result when possible; the ownership violation then terminates the adapter. |
| A reverse-request result or error response cannot be written. | The response-write failure terminates the adapter. |

Approval, elicitation, authentication-refresh, attestation, time, and user-input reverse requests are declined or rejected according to the headless policy. Those expected denials do not terminate the adapter. Shutdown interrupt and drain failures are warnings and do not replace the initiating shutdown status.

Authentication-refresh, attestation, current-time, and unknown reverse-request handlers do not decode their parameter value. They return their fixed unavailable or method-not-found error for any parameter shape. Every app-server notification resets the active-turn inactivity watchdog; unrecognized notifications are otherwise ignored.

#### Exit status

Status is 0 after help or a handled `SIGHUP`, `SIGINT`, or `SIGTERM`. Status is 1 after argument, configuration, startup, protocol, queue, or lifecycle failure.

#### Example

The following Bash transcript supplies the app-server ownership that the lower-level command requires:

```sh
runtime_dir=$(mktemp -d)
chmod 700 "$runtime_dir"
socket="$runtime_dir/app-server.sock"
codex app-server --listen "unix://$socket" &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; rm -rf "$runtime_dir"' EXIT
while [ ! -S "$socket" ]; do
  kill -0 "$server_pid" 2>/dev/null || exit 1
  sleep 0.1
done
intercom codex --app-server "unix://$socket" --name reviewer --cwd .
```

#### See also

[`intercom-codex-project`](#intercom-codex-project), [managed state](#managed-codex-binding), [Codex lifecycle](ARCHITECTURE.md#codex-adapter-lifecycle)

### intercom broker

#### Synopsis

```text
intercom broker [--idle-after DURATION] [--foreground]
```

#### Arguments

No positional arguments are accepted.

#### Options

| Option | Type | Units and format | Default | Meaning |
|---|---|---|---|---|
| `--idle-after DURATION` | duration | Go duration | `10m` | Exits after the broker has no registered peers for the selected duration. `0` disables idle exit. An explicit flag takes precedence over `INTERCOM_IDLE_EXIT`. |
| `--foreground` | Boolean | none | false | Writes structured logs to standard error instead of the broker log file. It does not daemonize the process. |
| `-h`, `--help` | Boolean | none | false | Prints command help and exits. |

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

| Option | Type | Default | Meaning |
|---|---|---|---|
| `-h`, `--help` | Boolean | false | Prints command help and exits. |

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

| Option | Type | Default | Meaning |
|---|---|---|---|
| `-h`, `--help` | Boolean | false | Prints command help and exits. |

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

| Argument | Type | Mode | Default | Meaning |
|---|---|---|---|---|
| shell | enumeration | required for generation | none | Selects exactly one of `bash`, `fish`, `powershell`, or `zsh`. An absent or unsupported value selects parent help instead of generation. |

No positional arguments follow the shell name.

#### Options

| Option | Type | Default | Meaning |
|---|---|---|---|
| `--no-descriptions` | Boolean | false | Omits command and flag descriptions from generated candidates. |
| `-h`, `--help` | Boolean | false | Prints help for the selected completion command. |

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

| Argument | Type | Mode | Default | Meaning |
|---|---|---|---|---|
| `COMMAND ...` | command-path components | optional | root command | Selects the command whose help is printed. |

#### Options

| Option | Type | Default | Meaning |
|---|---|---|---|
| `-h`, `--help` | Boolean | false | Prints help for the help command. |

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
intercom-codex-project [--name NAME] [--cwd DIRECTORY] [--new]
```

#### Arguments

The launcher scans every token for `--app-server` before any other action and rejects that token because the launcher owns the endpoint. When no prohibited token is present, any `-h` or `--help` token prints launcher help and suppresses timeout validation, child creation, and validation of other tokens. Without help, all tokens reach `intercom codex`; that command rejects unknown tokens.

#### Options

| Option | Type | Mode | Default | Meaning |
|---|---|---|---|---|
| `--name NAME` | peer name | optional | adapter default | Forwards the peer name. |
| `--cwd DIRECTORY` | directory path | optional | adapter default | Forwards the managed directory. |
| `--new` | Boolean | optional | false | Forwards the new-binding selection. |
| `-h`, `--help` | Boolean | optional | false | Prints launcher help without starting children. |
| `--app-server` | prohibited | none | launcher-owned | Produces status 2 before starting children, in both split and `=` forms. |

#### Semantics

The launcher selects `${XDG_RUNTIME_DIR}`, then `${TMPDIR}`, then the system temporary directory as its runtime base. It creates a mode-0700 temporary directory and an `app-server.sock` endpoint within it. It starts `CODEX_BIN app-server --listen ENDPOINT`, polls every 100 milliseconds for the socket, then starts `INTERCOM_BIN codex --app-server ENDPOINT` with the forwarded options.

The service group remains in the foreground. A 100-millisecond poll checks the adapter first and app-server second. An observed child exit stops the other child. When both children become nonrunning between observations, adapter status takes precedence. Signal cleanup stops the adapter before app-server and removes the temporary directory. Each child receives `SIGTERM`, then `SIGKILL` when it remains running past the per-child timeout.

#### Errors

| Condition | Result |
|---|---|
| Either nonblank timeout variable is nondecimal, zero, or greater than 922337203685477580 seconds, and help is not requested. | Status 2 before runtime-directory or child creation. Leading zeroes are accepted for a positive value. An unset or empty variable selects its default. |
| `--app-server` or `--app-server=...` is supplied. | Status 2 before child creation. |
| Temporary-directory creation or chmod fails. | Status 1. |
| Codex executable startup fails. | Shell child-start failure status. |
| App-server exits before its socket appears. | Its nonzero status is propagated; an observed zero status maps to 1 when it is the operational failure. |
| The socket does not appear before the startup timeout. | Status 1; app-server is stopped. |
| Adapter startup or runtime fails. | Adapter status is propagated; app-server is stopped. |
| App-server exits while the adapter runs. | Nonzero app-server status is propagated; status 0 maps to 1; adapter is stopped. |
| A child ignores `SIGTERM` for the shutdown timeout. | A warning is written, the child receives `SIGKILL`, and the initiating status remains in effect. |
| Launcher help output cannot be written completely. | The shell can report the `printf` failure on standard error, but the launcher does not propagate it and help retains status 0. No children start. |

#### Exit status

| Status | Meaning |
|---|---|
| 0 | Help was requested, including when its output write failed, or the adapter exited successfully. |
| 1 | Launcher startup or readiness failed, app-server exited unexpectedly with status 0, or the observed child returned status 1. |
| 2 | Launcher option or timeout validation failed, or the observed child returned status 2. |
| 129 | `SIGHUP` terminated the launcher, or the observed child returned status 129. |
| 130 | `SIGINT` terminated the launcher, or the observed child returned status 130. |
| 143 | `SIGTERM` terminated the launcher, or the observed child returned status 143. |
| other | The nonzero status of the child observed to have exited. Adapter status takes precedence when both children stop between polls. |

#### Example

```sh
intercom-codex-project --name reviewer --cwd .
```

#### See also

[`intercom codex`](#intercom-codex), [Handbook: managed Codex](HANDBOOK.md#3-add-a-managed-codex-peer)

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

Claude MCP returns `{"content":[{"type":"text","text":"TEXT"}]}` on success and adds `"isError":true` on a tool failure. Codex returns `{"contentItems":[{"type":"inputText","text":"TEXT"}],"success":true}` on success and changes `success` to false on a tool failure.

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

Claude MCP results mark these failures with `isError: true`. Codex dynamic-tool results set `success: false`.

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

Claude MCP returns `{"content":[{"type":"text","text":"TEXT"}]}` on success and adds `"isError":true` on a tool failure. Codex returns `{"contentItems":[{"type":"inputText","text":"TEXT"}],"success":true}` on success and changes `success` to false on a tool failure.

#### Errors

| Exact condition | Tool error text or class |
|---|---|
| Argument bytes are invalid JSON or the value is an array, string, number, or Boolean. | `decode args: ...` |
| The value is null. | `list_peers arguments must be an object` |
| The object contains any member. | `list_peers does not accept arguments` |
| Broker connection, write, correlation, or reply fails. | `list_peers failed: ...` |

Claude MCP results mark these failures with `isError: true`. Codex dynamic-tool results set `success: false`.

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
| `INTERCOM_NAME` | peer name | `shim`, `codex`, `name` | selected-directory basename | Supplies the peer name after whitespace trimming. `--name` takes precedence for `codex`. Blank means unset. Invalid content is fatal. |
| `INTERCOM_DIR` | directory path | broker, shim, Codex adapter, `peers` | `$HOME/.claude-intercom` | Supplies the runtime and binding directory. A missing directory is created with mode 0700. Existing permissions are not repaired. |
| `INTERCOM_SOCKET` | Unix socket path | broker, shim, Codex adapter, `peers` | `$INTERCOM_DIR/broker.sock` | Overrides only the broker socket. Its lock path is the string plus `.lock`. Its parent is not created as a consequence of this override. |
| `INTERCOM_BROKER_LOG` | file path | broker | `$INTERCOM_DIR/broker.log` | Selects the append-only structured log. `--foreground` writes to standard error instead. |
| `INTERCOM_BROKER_BIN` | executable path or name | shim, Codex adapter, `peers` | running `intercom` executable | Selects the command executed with argument `broker` during auto-start. Empty means default. |
| `INTERCOM_IDLE_EXIT` | Go duration | broker | `10m` | Supplies idle exit only when `--idle-after` is absent. `0` disables. Blank means default. Invalid or negative content is fatal. |
| `CODEX_BIN` | executable path or name | launcher | `codex` | Selects the child command executed as `app-server --listen ENDPOINT`. Unset or empty selects the default. |
| `INTERCOM_BIN` | executable path or name | launcher | `intercom` | Selects the child adapter command. Unset or empty selects the default. The Nix-packaged launcher defaults to its bundled Intercom binary. |
| `INTERCOM_CODEX_STARTUP_TIMEOUT_SECONDS` | positive decimal seconds | launcher | `30` | Bounds socket readiness. Unset or empty selects the default. Otherwise zero, nondigits, and values above 922337203685477580 are fatal with status 2 unless help suppresses timeout validation. |
| `INTERCOM_CODEX_SHUTDOWN_TIMEOUT_SECONDS` | positive decimal seconds per child | launcher | `40` | Bounds each child `SIGTERM` wait before `SIGKILL`. Unset or empty selects the default; other validation matches the startup timeout. |
| `XDG_RUNTIME_DIR` | directory path | launcher | none | Supplies the first-choice base for the launcher temporary directory. |
| `TMPDIR` | directory path | launcher | system temporary directory | Supplies the second-choice base when `XDG_RUNTIME_DIR` is empty. |
| `CODEX_HOME` | directory path | app-server and binding identity | Codex-defined | Selects Codex configuration and rollout storage through Codex. A changed app-server-reported value makes an existing binding incompatible without `--new`. |
| `HOME` | directory path | path defaults, Codex, shell | operating-system account home | Supplies the Intercom default directory when `INTERCOM_DIR` is empty. Home-resolution failure is fatal for commands that need the default directory. |
| `PATH` | command search list | shell and launcher | shell-defined | Resolves `intercom`, `codex`, and launcher utility programs when their variables contain bare names. |

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

The binding object contains every member in the following table:

| Member | JSON type | Required value or meaning |
|---|---|---|
| `schemaVersion` | number | State schema `1`. |
| `peer` | string | Selected Intercom peer name. |
| `threadId` | string | Dedicated Codex thread identifier. |
| `cwd` | string | Canonical managed working directory. |
| `codexHome` | string | App-server-reported Codex home identity. |
| `serverUserAgent` | string | App-server user-agent identity. |
| `codexVersion` | string | Extracted Codex semantic version. |
| `toolContractVersion` | number | Dynamic-tool contract `1`. |
| `materialized` | Boolean | True after the first terminal turn is confirmed readable. |

The state decoder ignores unknown object members. A missing member receives its JSON zero value. Missing identity members, schema version 0, and tool-contract version 0 fail validation; an omitted `materialized` member is false.

Codex rollout and conversation files remain under `CODEX_HOME` and are owned by Codex. `--new` does not delete them.

### Launcher files

The launcher creates `intercom-codex.XXXXXX` with mode 0700 beneath its selected runtime base, and creates the `app-server.sock` pathname within it through Codex. It removes the temporary directory on handled exit. `SIGKILL`, host failure, or shell failure can leave the directory behind.

## LIMITS AND TIMERS

| Boundary | Value | Unit and scope |
|---|---|---|
| Peer name | 64 | ASCII bytes |
| Raw agent-tool message | 204800 | UTF-8 bytes before JSON escaping |
| Broker JSON payload | 262144 | bytes, excluding the four-byte frame prefix |
| Claude MCP input line | 8388608 | bytes; a line reaching scanner capacity is rejected |
| Codex app-server JSON message | 16777216 | bytes per WebSocket text message |
| Broker accepted connections and registered peers | no Intercom limit | operating-system descriptors, memory, and process resources bound the count |
| Claude concurrent MCP tool handlers | no Intercom limit | one goroutine per active `tools/call`; process resources bound the count |
| Codex inbound delivery queue | 64 | messages not yet started; an attempted 65th queued delivery is fatal |
| Codex selected notification queue | 256 | lifecycle notifications; an attempted 257th queued notification is fatal |
| Concurrent app-server reverse handlers | 64 | handlers; an attempted 65th concurrent request is fatal |
| Completed app-server response-ID history | 1024 | response IDs; a repeated older ID is classified as unknown rather than duplicate |
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
| Codex app-server control operation | 30 | seconds per initialize, initialized notification, thread control call, turn-start write, or turn-start response |
| Codex startup broker registration | 30 | seconds total around broker-client connection and registration |
| Codex post-terminal materialization confirmation | 30 | seconds total; `thread/read` retries after 50-millisecond delays while Codex reports pre-materialization |
| Codex shutdown | 30 | seconds shared across broker close, turn interrupt and drain, and reverse-handler drain |
| Codex broker-close wait during shutdown | 15 | seconds by default; first half of the shared shutdown budget is reserved for broker close before app-server cleanup proceeds |
| Codex app-server reverse request | 10 | seconds total for every supported, denied, or rejected reverse request: 9 seconds for handling and 1 second for the response write |
| Codex active-turn inactivity | 15 | minutes since app-server activity |
| Codex broker reconnect sleeps | 100 ms doubling while below 5 s | sequence reaches an effective 6.4 s repeated step |
| Launcher readiness poll | 100 | milliseconds |
| Launcher readiness timeout | 30 | seconds, configurable |
| Launcher shutdown timeout | 40 | seconds per child, configurable |

Tool-call reply waiting uses the calling MCP or Codex request context. It has no additional independent Intercom timeout beyond connection and write budgets.

## ERRORS

`intercom` command failures have the form `intercom: MESSAGE` on standard error and status 1. Subcommand sections enumerate command-specific failure conditions. Broker routing codes are exhaustive in [Broker Protocol: error codes](BROKER_PROTOCOL.md#error-codes).

Tool argument and routing failures are returned to the model as tool failures. They do not terminate a Claude shim. A Codex dynamic-tool protocol or ownership violation is fatal to the managed adapter because it invalidates sole thread control.

Operating-system errors preserve their underlying diagnostic. They arise when required directories, files, sockets, executables, signals, locks, or standard streams cannot be accessed as described in the command entry.

## NOTES

The broker has no persistent queue, offline mailbox, retransmission log, or end-to-end acknowledgement. A peer disconnect after broker delivery write can lose a message that produced a positive `send_ack`.

Transport is restricted to same-machine Unix sockets. Model traffic is not restricted to the machine: each agent uses its configured model provider.

Peer-delivered text is untrusted model input. Intercom relies on process-account file permissions, unique live peer names, agent instruction precedence, and the managed Codex sandbox. It supplies no peer cryptographic identity or message authorization layer.

## SEE ALSO

[Handbook](HANDBOOK.md), [architecture](ARCHITECTURE.md), [broker protocol](BROKER_PROTOCOL.md), [development](DEVELOPMENT.md)
