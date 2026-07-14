# INTERCOM ARCHITECTURE

## NAME

Intercom architecture — local peer registration, message routing, Claude Code channel delivery, and managed Codex thread delivery.

## CONTENTS

- [Scope](#scope)
- [Topology](#topology)
- [Components](#components)
- [Invariants](#invariants)
- [Flows](#flows)
- [State](#state)
- [Lifecycles](#lifecycles)
- [Failure semantics](#failure-semantics)
- [Security](#security)
- [Compatibility](#compatibility)
- [Source map](#source-map)
- [See also](#see-also)

## SCOPE

Intercom connects coding-agent sessions that run under one operating-system user on one Unix host. A singleton broker routes messages by peer name. Each agent integration translates between the broker protocol and the agent host's native protocol.

This document specifies process ownership, component responsibilities, invariants, data flows, state, lifecycle, and failure boundaries. [REFERENCE.md](REFERENCE.md) specifies commands, options, environment variables, files, limits, and tool contracts. [BROKER_PROTOCOL.md](BROKER_PROTOCOL.md) specifies the broker wire format.

Intercom provides live routing. It does not provide an offline mailbox, message history, delivery retries, durable message queues, agent authentication, model inference, or coding-agent conversation storage. The Codex adapter resumes only the thread named by its saved Intercom binding; it does not adopt an arbitrary Codex CLI or TUI thread.

## TOPOLOGY

The system uses one broker and one adapter process per connected agent session.

```text
Claude Code process
    │
    │ newline-delimited MCP over stdin and stdout
    ▼
intercom shim ────────────────┐
                              │
                              │ length-prefixed JSON over a Unix socket
                              ▼
                         intercom broker
                              ▲
                              │ length-prefixed JSON over a Unix socket
                              │
intercom codex ───────────────┘
    │
    │ JSON-RPC-shaped text messages over WebSocket over a Unix socket
    ▼
Codex app-server process
```

The Claude Code process starts `intercom shim` as an MCP channel server. The shim remains a child of that Claude Code session.

The `intercom codex` command connects to an app-server endpoint supplied by its caller. It does not start, stop, or otherwise own the app-server process. The `intercom-codex-project` launcher starts one Codex app-server and one `intercom codex` adapter as sibling child processes, supervises both, and stops both as one service group.

The first broker client that cannot reach the configured broker socket starts `intercom broker` as a detached process. The broker survives the spawning client and exits after the configured zero-peer idle interval when idle exit is enabled. A non-blocking file lock selects one broker for each configured socket path.

All adapters attached to the same broker socket share one peer namespace. Different broker socket paths form independent Intercom networks.

## COMPONENTS

### Command dispatcher

The `intercom` executable dispatches the broker, Claude shim, Codex adapter, and diagnostic commands. It supplies the running binary path to broker auto-spawn unless `INTERCOM_BROKER_BIN` overrides that path. This rule keeps a client and its auto-spawned broker on the same executable by default.

### Broker

The broker owns the Unix listener and the in-memory map from peer name to live connection. It performs the following operations:

- accepts one `hello` registration per connection;
- rejects an invalid or already-connected peer name;
- returns the sorted set of other connected peer names;
- writes each `deliver` frame directly to the selected destination connection;
- acknowledges or rejects each `send` request on the sender connection;
- removes an unresponsive destination after a delivery write failure;
- sends a best-effort `goodbye` during broker shutdown;
- attempts to remove the socket entry when shutdown completes and logs an unlink failure.

The broker does not inspect message meaning. It does not retain a message after the delivery write completes or fails.

### Broker client

The broker client is shared by the Claude and Codex integrations. It owns one registered broker connection, a single reader, serialized writes, and request-ID correlation for `send` and `list_peers` operations.

The client first dials the socket. A missing or refused socket causes a detached broker spawn followed by bounded dial retries. Permission errors and other non-startable dial failures do not cause a spawn. Concurrent connect attempts for one client serialize through a connection gate.

A connection loss fails every request pending on that connection. A later operation may reconnect. The Codex adapter also observes connection-state events and initiates reconnection without waiting for a tool call.

### Claude Code shim

The Claude Code shim is an MCP server over standard input and standard output. Standard error carries logs. The shim advertises the `claude/channel` experimental capability and two tools: `send_message` and `list_peers`.

After the MCP client sends `notifications/initialized`, the shim attempts broker registration. Failure of this eager registration is logged and does not terminate the MCP server. A later tool call attempts registration again.

An inbound broker delivery becomes a `notifications/claude/channel` notification. The notification contains the message body in `content`, the registered sender name in `meta.from`, and the broker timestamp in `meta.timestamp`.

The shim processes `initialize`, `notifications/initialized`, `tools/list`, `tools/call`, and `ping`. Unrelated client notifications are ignored. Unsupported requests receive a method-not-found response.

### Codex adapter

The Codex adapter owns one app-server WebSocket connection and one non-ephemeral Codex thread. It acquires a lifetime lock for the selected peer before contacting the app-server.

The adapter initializes the app-server with experimental API support and requires the app-server user agent to identify Codex CLI version `0.144.1`. It starts or resumes a thread with the following policy:

- the project directory is canonicalized to an existing directory with symbolic links resolved;
- the approval policy is `never`;
- the sandbox mode is `workspace-write`;
- no additional writable roots are accepted;
- the returned network-access field is required to be Boolean and is retained for each turn;
- the thread is non-ephemeral and idle when ownership begins;
- a new thread receives `send_message` and `list_peers` as dynamic function tools, and a resumed binding requires the matching tool-contract version;
- the thread receives developer instructions that require explicit `send_message` use for an Intercom reply.

The adapter registers with the broker only after app-server initialization, thread ownership checks, dynamic-tool startup checks, and persisted-state checks succeed. A peer therefore does not appear in broker discovery before its managed thread can accept a delivery.

Inbound deliveries enter a bounded FIFO queue. The controller starts the next Codex turn only while the managed thread is idle. A terminal `completed`, `failed`, or `interrupted` notification releases the next queued delivery. Every app-server notification resets the active-turn inactivity watchdog. Progress and unknown notifications otherwise have no controller-state effect and are discarded.

An ordinary Codex final answer remains in the Codex thread. Only a successful `send_message` dynamic-tool call creates an outbound Intercom message.

### App-server client

The app-server client performs an HTTP WebSocket upgrade over the supplied Unix socket. Its synthetic WebSocket URL supplies the HTTP request target and host header only; the dial transport has no TCP fallback.

App-server messages use JSON-RPC-shaped request, response, notification, and reverse-request objects without a `jsonrpc` member. The client assigns numeric request IDs, correlates out-of-order responses, preserves correlation across the write and await phases of `turn/start`, and terminates on malformed envelopes, unknown response IDs, duplicate response IDs, binary messages, or transport failure.

Lifecycle notifications execute in reader order. Reverse requests execute on independent handler goroutines so a tool or approval request cannot block lifecycle notifications. Each reverse request permits one result-or-error response attempt; a response write failure enters the adapter's fatal path.

### Project launcher

The project launcher creates a private runtime directory and app-server socket, starts `codex app-server --listen` for that socket, waits for the socket entry, and starts `intercom codex` with the matching endpoint. Non-help arguments other than the prohibited `--app-server` option pass to `intercom codex`.

The launcher treats either child exit as termination of the service group. Adapter exit stops the app-server. App-server exit stops the adapter. Signal handling stops the adapter before the app-server so the adapter can deregister from the broker and interrupt or drain an active Codex turn.

This service group gives the managed thread a dedicated app-server process. The lower-level `intercom codex` command relies on its caller to provide the equivalent ownership boundary.

### State store

The state store binds a peer name to the Codex thread exclusively managed by that peer. It stores identity and compatibility metadata only. Codex conversation content and rollout data remain under `CODEX_HOME`.

The state store holds a non-blocking lifetime lock. State replacement uses a mode-`0600` temporary file, file synchronization, atomic rename, and parent-directory synchronization. Directory synchronization errors that report the documented unsupported operation on Darwin are treated as a portability exception.

## INVARIANTS

The following invariants define a valid running system:

1. One process holds the broker lock for a configured broker socket.
2. One live broker connection owns a peer name. A second connection cannot register that name until the first connection deregisters.
3. A peer name contains 1 through 64 ASCII letters, digits, hyphens, or underscores.
4. A broker assigns the sender identity in each delivery from the registered source connection. A sender cannot select the `from` field of a routed delivery.
5. A successful send acknowledgement means that the broker completed the destination-frame write. It does not mean that an agent observed, processed, or answered the message.
6. A message has no durable Intercom copy. A process failure after acknowledgement can still prevent model processing.
7. One Codex adapter process owns a managed peer lock, one app-server connection, and one managed thread.
8. A Codex binding retains the same peer, canonical project directory, `CODEX_HOME`, app-server identity, Codex version, state schema, and dynamic-tool contract.
9. A managed Codex thread is idle, non-ephemeral, approval-free, and workspace-write sandboxed when the adapter becomes discoverable.
10. One Intercom delivery drives at most one active Codex turn in an adapter process. Deliveries wait in FIFO order while a turn is starting or active.
11. A Codex dynamic-tool call is accepted only for the owned thread and the starting or active turn.
12. A Codex final answer does not imply an Intercom reply. The `send_message` tool is the only outbound agent-message operation.
13. Broker writes and adapter-protocol writes serialize per connection. Concurrent frames cannot interleave at the byte level.
14. Peer discovery excludes the requesting connection and sorts names lexicographically.

## FLOWS

### Broker registration

1. The adapter dials the configured broker socket.
2. A missing or refused socket causes one detached broker start attempt.
3. The client retries the socket according to its bounded startup backoff.
4. The client writes `hello` with its peer name and binary version.
5. The broker validates the first-frame kind and peer name.
6. The broker inserts the connection into the peer map if the name is free.
7. The broker returns `welcome`.
8. The client starts its steady-state reader.

The broker records the client version in its log. The broker does not negotiate protocol behavior from that value.

### Claude Code startup and delivery

1. Claude Code starts `intercom shim` and opens the MCP stdio transport.
2. The client sends `initialize`; the shim returns server identity, tool capability, channel capability, and agent instructions.
3. The client sends `notifications/initialized`.
4. The shim attempts broker registration.
5. A sender calls `send_message`.
6. The source adapter validates tool arguments and writes `send` to the broker.
7. The broker writes `deliver` to the Claude shim.
8. The shim writes `notifications/claude/channel` to Claude Code.
9. The broker returns `send_ack` to the source adapter after the delivery write.

The channel notification and send acknowledgement use independent connections. Their observation order at the two endpoints is not a cross-process ordering guarantee.

### Codex startup

1. The adapter canonicalizes configuration and acquires the peer state lock.
2. The adapter loads the saved binding unless `--new` selects a replacement thread.
3. The adapter retries the app-server Unix socket within the startup deadline.
4. The adapter sends `initialize` with `experimentalApi` enabled and validates the returned app-server identity.
5. The adapter sends `initialized`.
6. The adapter starts a thread when no binding is loaded, or resumes the bound thread when a binding is loaded.
7. The adapter verifies thread identity, directory, idle status, non-ephemeral status, approval policy, and sandbox policy.
8. A new binding is written after the app-server accepts the new thread.
9. The adapter rejects any dynamic-tool request observed before ownership is established.
10. The adapter registers its peer name with the broker.
11. The adapter changes from booting to idle and accepts deliveries.

`--new` replaces the binding only after a replacement thread starts and passes validation. It does not delete the prior Codex thread or its history.

### Codex resume and materialization

1. A saved binding supplies the thread ID and identity constraints.
2. The adapter requires the same peer, canonical directory, `CODEX_HOME`, app-server user agent, Codex version, and tool-contract version.
3. The adapter resumes the thread with the required developer instructions, approval policy, sandbox mode, and directory.
4. An unmaterialized binding is checked with `thread/read`.
5. A terminal first turn causes a successful `thread/read` and sets `materialized` in the binding.

Codex may not create a rollout record before a first user turn. If resume reports a missing rollout for an unmaterialized binding, the adapter starts a replacement thread. A missing rollout for a materialized binding is fatal and does not trigger replacement.

### Codex inbound delivery

1. The broker writes a delivery containing an ID, sender, timestamp, and body.
2. The broker client enqueues the delivery in the adapter's FIFO.
3. The idle controller formats the delivery as one text user input with `From`, `Sent`, and `Message-ID` fields.
4. The controller writes `turn/start` with the delivery ID as `clientUserMessageId` and reasserts the owned directory, approval policy, and sandbox policy.
5. The controller reconciles the `turn/start` response with `turn/started` and `turn/completed` notifications.
6. Dynamic-tool calls route through the broker while the turn is starting or active.
7. A terminal turn notification marks the controller idle and permits the next queued delivery.
8. The first terminal turn also confirms thread materialization.

### Outbound message

1. The agent invokes `send_message` with a destination peer and nonempty message.
2. The adapter rejects unknown fields, an invalid destination, an empty body, a raw body above the tool limit, or a body whose JSON expansion exceeds the broker-frame limit.
3. The broker rejects self-delivery or an absent destination.
4. The broker writes the delivery to the destination within the delivery deadline.
5. The broker returns a successful acknowledgement after the write completes.
6. The adapter returns the acknowledgement as tool output.

No component retries a rejected send or a send whose connection drops while awaiting acknowledgement.

## STATE

### Volatile state

| Owner | State | Lifetime |
|---|---|---|
| Broker | Peer-name map, connections, idle interval, shutdown flag | Broker process |
| Broker client | Connection generation, pending request map, latest connection event | Adapter process |
| Claude shim | MCP tool registry and initialization state | Shim process |
| Codex adapter | Controller phase, active delivery, active turn ID, FIFO deliveries, lifecycle notifications | Adapter process |
| App-server client | Request correlations, response-ID history, reverse-handler count | Adapter process |
| Project launcher | Child process IDs and private socket directory | Launcher process |

Volatile state is not recovered after process termination.

### Persistent Intercom state

| Entry | Content | Persistence rule |
|---|---|---|
| `$INTERCOM_DIR/codex/<peer>.json` | Schema version, peer, thread ID, canonical directory, `CODEX_HOME`, app-server identity, Codex version, tool-contract version, materialization flag | Atomically replaced after a valid new binding or materialization transition |
| `$INTERCOM_DIR/codex/<peer>.lock` | Lifetime ownership lock | File persists; advisory lock exists only while held |
| `<broker-socket>.lock` | Broker singleton lock | File persists; advisory lock exists only while held |
| Broker log | Structured broker lifecycle records | Appended when the broker does not run in foreground mode |

The broker socket is a live transport endpoint, not persistent state. The launcher socket and its containing directory exist only for one service-group lifetime.

### External state

Codex stores thread conversation and rollout data under `CODEX_HOME`. Claude Code owns its session state and channel consumption. Intercom binding files do not duplicate either store.

## LIFECYCLES

### Broker lifecycle

The broker acquires its lock before removing a stale socket entry and opening the listener. A second broker that finds the lock held exits successfully. When idle exit is enabled, the idle interval begins whenever the peer count becomes zero and a new registration cancels the active interval. The idle deadline or a termination signal begins shutdown.

Shutdown closes unregistered accepted connections, writes a best-effort `goodbye` to registered peers, closes every connection, drains handlers, attempts to remove the socket entry, and releases the lock. A socket-removal failure is logged and does not make clean shutdown fail. The lock file remains on disk.

### Claude shim lifecycle

The shim lifetime follows MCP standard input, process cancellation, or fatal I/O. End-of-file is a clean shutdown. Shutdown closes the broker connection and waits for active MCP tool handlers. A broker disconnection does not terminate the shim; a later tool call may reconnect. The shim does not run a continuous broker reconnect loop.

### Codex adapter lifecycle

The adapter progresses through booting, idle, starting, active, and failed phases. It appears in broker discovery only in the idle, starting, or active portion after startup ownership checks.

A broker disconnect starts a reconnect loop with exponential backoff. Queued and active Codex work remains in the adapter, but the peer is absent from broker routing until registration succeeds. An app-server disconnect is fatal.

Cancellation marks the adapter unavailable, stops broker reconnect work, and closes the broker connection before app-server turn cleanup when the shutdown budget permits. A starting or active turn receives `turn/interrupt`. The adapter then drains an ambiguous `turn/start` result, a terminal turn notification, and outstanding reverse-request handlers within the shared control deadline. Closing the app-server client does not stop an externally supervised app-server process.

### Project launcher lifecycle

The launcher creates the private socket directory before either child starts. It waits for the app-server socket before starting the adapter. A startup timeout, early app-server exit, child failure, or termination signal initiates cleanup.

Cleanup terminates the adapter first and the app-server second. A child that exceeds the shutdown timeout receives a kill signal. Cleanup removes the private runtime directory. The 100-millisecond supervision poll checks the adapter before app-server. The observed adapter status is propagated when the adapter is found stopped; a nonzero app-server status is propagated when app-server alone is found stopped, and an unexpected zero app-server status becomes failure. Adapter status therefore takes precedence when both children stop between observations.

## FAILURE SEMANTICS

| Condition | Local result | Message or peer result |
|---|---|---|
| Broker socket is missing or refuses a connection | The broker client starts a detached broker and retries | No message is issued until registration succeeds |
| Broker socket fails for a non-startable reason | The connect operation fails | The peer remains unregistered |
| Broker lock is already held | The extra broker exits successfully | The lock owner remains authoritative |
| Peer name is invalid | Registration fails with `bad_name` | The peer remains unregistered |
| Peer name is already connected | Registration fails with `name_taken` | Claude logs eager failure and remains on MCP; Codex startup fails |
| Destination is absent | `send_ack` carries `no_such_peer` | No delivery occurs |
| Destination equals sender | `send_ack` carries `no_self_send` | No delivery occurs |
| Delivery exceeds the frame limit | `send_ack` carries `oversize` | The destination connection remains usable when rejection occurs before writing |
| Destination write fails or exceeds its deadline | The broker removes the destination and returns `deliver_failed` | Delivery outcome may be partial at the failed stream; no retry occurs |
| Source connection drops while awaiting a reply | The pending operation returns a disconnect error | The operation is not replayed because its outcome may be ambiguous |
| Broker process exits | Claude reconnects on a later tool call; Codex reconnects in the background | Disconnected peers cannot receive messages |
| Claude channel notification write fails | The shim logs the write failure | A broker acknowledgement may already have been returned |
| A delivery arrives while the Codex inbound FIFO already contains 64 queued messages | The adapter enters a fatal shutdown path | The attempted 65th queued delivery is not admitted; queued messages have no durable recovery path |
| A selected lifecycle notification arrives while its FIFO already contains 256 notifications | The adapter enters a fatal shutdown path | The attempted 257th notification is not admitted; controller state is no longer trusted |
| App-server exceeds the reverse-handler concurrency limit | The app-server client terminates | The adapter enters a fatal shutdown path |
| App-server message is binary, oversized, malformed, duplicated, or uncorrelated | The app-server client terminates | The adapter enters a fatal shutdown path |
| App-server disconnects | The adapter fails and deregisters | The managed peer becomes unavailable |
| Active Codex turn produces no app-server activity before the watchdog deadline | The adapter fails, deregisters, and interrupts the turn during cleanup | The inbound message is not retried |
| Codex turn reports `failed` or `interrupted` | The delivery is terminal and the controller returns to idle | No automatic reply or retry occurs |
| Dynamic tool call names a different thread or turn | The tool call returns failure and the adapter enters a fatal path | No broker operation occurs |
| Unsupported app-server reverse request arrives | The adapter returns a denial, unavailable error, or method-not-found error according to request type | Headless ownership does not wait for user input |
| Persisted Codex identity is incompatible | Startup fails with an instruction to select `--new` where replacement is valid | The saved binding remains unchanged |
| Materialized Codex rollout is missing | Resume fails | The adapter does not replace the thread implicitly |

Send acknowledgement is a transport event. It is not an end-to-end processing receipt. Neither adapter sends acknowledgements back to the originating agent after model observation.

## SECURITY

Intercom's transport boundary is the local Unix host. The broker socket, broker lock, broker log, Codex binding, and Codex peer lock use owner-only modes when Intercom creates them. Intercom creates its state directories with owner-only mode when absent. It does not repair permissions on pre-existing directories.

The broker has no cryptographic authentication. Filesystem access to the Unix socket is the admission control. A process with socket access can register any free valid peer name and send broker frames. The `version` field in `hello` is diagnostic and does not authorize a client.

The broker supplies `from` from the registered connection, but registration does not prove that the process represents a particular project or human. Peer names are routing labels, not security principals.

Inbound message bodies are untrusted agent input. Claude Code receives them as channel content. Codex receives them as user-turn content under existing system and developer instructions. Neither representation grants message content a higher instruction priority.

Managed Codex threads use approval policy `never`. The adapter declines command and file-change approvals, returns an empty turn-scoped permission grant, declines MCP elicitation, and cannot answer interactive user-input requests. The workspace-write sandbox still permits actions allowed by the app-server's returned sandbox policy. The adapter refuses additional writable roots but accepts either Boolean value for the returned network-access setting.

The app-server connection accepts Unix endpoints only and cannot fall back to TCP. The project launcher places the endpoint in an owner-only private runtime directory.

“Local” describes Intercom routing and transport. Claude Code, Codex app-server, model providers, configured MCP servers, and agent tools may transmit message content or derived data according to their own configuration. Intercom does not impose an offline inference boundary.

## COMPATIBILITY

Intercom requires Unix-domain sockets and advisory file locking. The packaged targets are `x86_64-linux`, `aarch64-linux`, `x86_64-darwin`, and `aarch64-darwin`. The project launcher requires Bash and standard Unix process utilities.

The broker protocol has no negotiated protocol version. JSON decoders ignore unknown object fields, and the receiver accepts a delivery without an ID. These decoding properties do not establish general mixed-build compatibility; one broker socket should use one Intercom build. Frame kinds and required behavioral constraints remain fixed by [BROKER_PROTOCOL.md](BROKER_PROTOCOL.md).

The Claude shim implements a bounded MCP subset. It echoes a client-supplied MCP protocol version during initialization and uses `2025-11-25` when the client omits one. Claude channel delivery depends on support for the advertised `claude/channel` experimental capability.

The Codex adapter requires the experimental app-server schema from Codex CLI `0.144.1`. Startup rejects a different semantic version in the app-server user agent. Unknown fields in recognized app-server objects are ignored, but message envelopes, request correlation, lifecycle status, thread policy, and sandbox policy are validated.

Codex binding schema version `1` and dynamic-tool contract version `1` are exact compatibility gates. An incompatible saved binding is not migrated in place. `--new` establishes a replacement binding after a replacement thread starts successfully.

## SOURCE MAP

| Responsibility | Source |
|---|---|
| Command registration and broker binary selection | [`cmd/intercom/main.go`](../cmd/intercom/main.go) |
| Broker command and logging policy | [`cmd/intercom/broker_cmd.go`](../cmd/intercom/broker_cmd.go) |
| Peer-name diagnostic command | [`cmd/intercom/name_cmd.go`](../cmd/intercom/name_cmd.go) |
| Peer-list diagnostic command | [`cmd/intercom/peers_cmd.go`](../cmd/intercom/peers_cmd.go) |
| Claude shim command | [`cmd/intercom/shim_cmd.go`](../cmd/intercom/shim_cmd.go) |
| Codex adapter command | [`cmd/intercom/codex_cmd.go`](../cmd/intercom/codex_cmd.go) |
| Broker registration, routing, idle exit, and shutdown | [`internal/broker/broker.go`](../internal/broker/broker.go) |
| Shared broker connection and auto-spawn behavior | [`internal/brokerclient/client.go`](../internal/brokerclient/client.go) |
| Broker frame types and codec | [`internal/wire/wire.go`](../internal/wire/wire.go) |
| Claude MCP-to-broker adapter | [`internal/shim/shim.go`](../internal/shim/shim.go) |
| MCP server subset | [`internal/mcp/mcp.go`](../internal/mcp/mcp.go) |
| Shared agent tool contract | [`internal/intercomtools/tools.go`](../internal/intercomtools/tools.go) |
| Codex controller and lifecycle | [`internal/codex/controller.go`](../internal/codex/controller.go) |
| Codex dynamic-tool and reverse-request handling | [`internal/codex/tools.go`](../internal/codex/tools.go) |
| Codex binding and lifetime lock | [`internal/codex/state.go`](../internal/codex/state.go) |
| Codex app-server protocol types | [`internal/appserver/protocol.go`](../internal/appserver/protocol.go) |
| Codex app-server connection and dispatch | [`internal/appserver/client.go`](../internal/appserver/client.go) |
| Unix WebSocket transport | [`internal/appserver/unixws.go`](../internal/appserver/unixws.go) |
| Runtime and state paths | [`internal/paths/paths.go`](../internal/paths/paths.go) |
| Peer-name resolution and validation | [`internal/peername/name.go`](../internal/peername/name.go) |
| Project service-group supervision | [`scripts/intercom-codex-project`](../scripts/intercom-codex-project) |

## SEE ALSO

- [README.md](../README.md) — public synopsis and quick start.
- [HANDBOOK.md](HANDBOOK.md) — installation and operating procedures.
- [REFERENCE.md](REFERENCE.md) — commands, tools, environment, files, limits, and errors.
- [BROKER_PROTOCOL.md](BROKER_PROTOCOL.md) — broker transport and frame contract.
- [DEVELOPMENT.md](DEVELOPMENT.md) — build and verification procedures.
