# INTERCOM ARCHITECTURE

## NAME

Intercom architecture — local peer registration, message routing, Claude Code channel delivery, and managed Codex thread delivery.

## CONTENTS

- [Scope](#scope)
- [Topology](#topology)
- [Components](#components)
  - [Command dispatcher](#command-dispatcher)
  - [Broker](#broker)
  - [Broker client](#broker-client)
  - [Claude Code shim](#claude-code-shim)
  - [Codex adapter](#codex-adapter)
  - [Codex MCP bridge](#codex-mcp-bridge)
  - [App-server client](#app-server-client)
  - [Codex TUI proxy](#codex-tui-proxy)
  - [Project launcher](#project-launcher)
  - [Live-instance registry](#live-instance-registry)
  - [State store](#state-store)
- [Invariants](#invariants)
- [Flows](#flows)
  - [Broker registration](#broker-registration)
  - [Claude Code startup and delivery](#claude-code-startup-and-delivery)
  - [Codex startup](#codex-startup)
  - [Codex session discovery and selection](#codex-session-discovery-and-selection)
  - [Codex resume and materialization](#codex-resume-and-materialization)
  - [Codex TUI attachment](#codex-tui-attachment)
  - [Codex TUI turn](#codex-tui-turn)
  - [Codex reverse request](#codex-reverse-request)
  - [Codex MCP tool call](#codex-mcp-tool-call)
  - [Codex inbound delivery](#codex-inbound-delivery)
  - [Outbound message](#outbound-message)
- [State](#state)
  - [Volatile state](#volatile-state)
  - [Persistent Intercom state](#persistent-intercom-state)
  - [Live discovery state](#live-discovery-state)
  - [External state](#external-state)
- [Lifecycles](#lifecycles)
  - [Broker lifecycle](#broker-lifecycle)
  - [Claude shim lifecycle](#claude-shim-lifecycle)
  - [Codex adapter lifecycle](#codex-adapter-lifecycle)
  - [Codex TUI lifecycle](#codex-tui-lifecycle)
  - [Project launcher lifecycle](#project-launcher-lifecycle)
- [Failure semantics](#failure-semantics)
- [Security](#security)
- [Compatibility](#compatibility)
- [Source map](#source-map)
- [See also](#see-also)

## SCOPE

Intercom connects coding-agent sessions that run under one operating-system user on one Unix host. A singleton broker routes messages by peer name. Each agent integration translates between the broker protocol and the agent host's native protocol.

This document specifies process ownership, component responsibilities, invariants, data flows, state, lifecycle, and failure boundaries. [REFERENCE.md](REFERENCE.md) specifies commands, options, environment variables, files, limits, and tool contracts. [BROKER_PROTOCOL.md](BROKER_PROTOCOL.md) specifies the broker wire format.

Intercom provides live routing. It does not provide an offline mailbox, message history, delivery retries, durable message queues, agent authentication, model inference, or coding-agent conversation storage. The Codex adapter creates a thread, resumes its saved binding, adopts an eligible ordinary CLI or VS Code root thread, or forks one into a new managed thread. It does not manage web, desktop-app, ephemeral, or child threads as roots.

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
    │  ▲
    │  │ downstream JSON-RPC-shaped WebSocket over private client.sock
    │  │
    │  └──────────────── stock Codex TUI
    │
    │ upstream JSON-RPC-shaped WebSocket over private app-server.sock
    ▼
private Codex app-server process
    │
    │ MCP stdio for an adopted or forked thread
    ▼
intercom codex-mcp-bridge ── authenticated JSON over private mcp-bridge.sock ──► intercom codex
```

The Claude Code process starts `intercom shim` as an MCP channel server. The shim remains a child of that Claude Code session.

The `intercom codex` command connects to an app-server endpoint supplied by its caller. It does not start, stop, or otherwise own the app-server process. An optional downstream endpoint exposes the adapter's managed thread to one stock Codex TUI. An optional private MCP bridge carries tool calls from adopted and forked sessions back to the adapter that owns the broker connection. The `intercom-codex-project` launcher allocates all private socket paths, starts Codex through its hidden process-session helper, starts one `intercom codex` adapter/proxy as a sibling outside that session, supervises both direct children, and stops both as one service group. The TUI and app-server-spawned MCP helper are not separately supervised launcher children; session cleanup covers the MCP helper while it remains in the Codex session.

The first broker client that cannot reach the configured broker socket starts `intercom broker` as a detached process. The broker survives the spawning client and exits after the configured zero-peer idle interval when idle exit is enabled. A non-blocking file lock selects one broker for each configured socket path.

All adapters attached to the same broker socket share one peer namespace. Different broker socket paths form independent Intercom networks.

## COMPONENTS

### Command dispatcher

The `intercom` executable dispatches the broker, Claude shim, Codex adapter, and diagnostic commands. It supplies the running binary path to broker auto-spawn unless `INTERCOM_BROKER_BIN` overrides that path. This rule keeps a client and its auto-spawned broker on the same executable by default.

### Broker

The broker owns the Unix listener and the in-memory map from peer name to live connection. It performs the following operations:

- accepts one `hello` registration per connection;
- rejects an invalid or already-connected peer name and logs a `register rejected` line naming the peer and the rejection code;
- returns the sorted set of other connected peer names;
- writes each `deliver` frame directly to the selected destination connection;
- acknowledges or rejects each `send` request on the sender connection;
- removes an unresponsive destination after a delivery write failure and logs a `peer deregistered` line for any peer removal, whether by clean disconnect or a failed delivery write;
- sends a best-effort `goodbye` during broker shutdown;
- attempts to remove the socket entry when shutdown completes and logs an unlink failure.

The broker does not inspect message meaning. It does not retain a message after the delivery write completes or fails.

### Broker client

The broker client is shared by the Claude and Codex integrations. It owns one registered broker connection, a single reader, serialized writes, and request-ID correlation for `send` and `list_peers` operations.

The client first dials the socket. A missing or refused socket causes a detached broker spawn followed by bounded dial retries. Permission errors and other non-startable dial failures do not cause a spawn. Concurrent connect attempts for one client serialize through a connection gate.

A connection loss fails every request pending on that connection. A later operation may reconnect. The Codex adapter also observes connection-state events and initiates reconnection without waiting for a tool call.

### Claude Code shim

The Claude Code shim is an MCP server over standard input and standard output. Standard error carries logs. The shim advertises the `claude/channel` experimental capability and three tools: `send_message`, `list_peers`, and `channel_status`.

The shim participates in the broker only when the session opts in: an explicitly configured name, a nonblank `INTERCOM_NAME`, or `INTERCOM_ENABLE=1`. MCP gives the shim no way to observe whether the launching Claude Code process actually loaded the channel, so unconditional registration previously produced dark receivers — sessions that held a peer name while silently dropping every delivery. A session that has not opted in never dials the broker and never registers a name; `send_message` and `list_peers` return an inert error result stating that intercom is not enabled, and `channel_status` reports the disabled condition without contacting the broker.

An opted-in session connects once the MCP client sends `notifications/initialized` and hands the connection to a background reconnect supervisor goroutine that holds it for the life of the process. The supervisor reconnects with increasing backoff after any disconnect — a broker restart, an idle-exit shutdown, or a transient dial failure — so a receive-only session keeps receiving deliveries without an outbound tool call to trigger reconnection. A registration rejected for a reason other than a name collision, such as an invalid name, stops the supervisor instead of retrying indefinitely. On a name collision the client retries under a numbered suffix (see [Peer names](REFERENCE.md#peer-names)). The broker logs a `register rejected` line for the collision behind each retry and a `peer deregistered` line whenever any peer, suffixed or not, is removed; both lines were previously silent, which made a stuck name collision hard to diagnose from the broker log alone.

An inbound broker delivery becomes a `notifications/claude/channel` notification. The notification contains the message body in `content`, the registered sender name in `meta.from`, and the broker timestamp in `meta.timestamp`. Delivery notifications queue to a dedicated goroutine so a slow or stalled standard-output write cannot block the broker read loop.

The shim processes `initialize`, `notifications/initialized`, `tools/list`, `tools/call`, and `ping`. Unrelated client notifications are ignored. Unsupported requests receive a method-not-found response.

### Codex adapter

The Codex adapter owns one app-server WebSocket connection, one non-ephemeral Codex thread, an optional downstream TUI proxy, and an optional MCP bridge listener. It acquires a lifetime lock for the selected peer before contacting app-server and a separate thread-identity lock before taking thread ownership. Adoption or fork that replaces an existing binding also retains the prior binding's thread lock until replacement commit or rollback completes.

The adapter initializes the app-server with experimental API support and requires the app-server user agent to identify Codex CLI version `0.144.1` or later. It starts or resumes a thread with the following unattended baseline:

- the project directory is canonicalized to an existing directory with symbolic links resolved;
- the runtime workspace-root list contains only that canonical project directory;
- the approval policy is `never`;
- the approvals reviewer is `user`;
- the sandbox mode is `workspace-write` by default or `danger-full-access` in yolo mode;
- no additional writable roots are accepted;
- the returned workspace-write network-access field is required to be Boolean and is retained for each turn; danger-full-access must not return that field;
- the thread is non-ephemeral and the start, resume, or fork response reports it idle; a resumed active persistent goal may publish a Codex-owned turn immediately after that response;
- after a saved or adopted thread resumes, `thread/goal/get` establishes its persistent-goal scheduler reservation before broker readiness; ordered goal update and clear notifications reconcile later state;
- a new thread receives `send_message` and `list_peers` as dynamic function tools;
- an adopted or forked thread receives the same tools from a required request-scoped MCP server whose status is validated before the provisional binding write;
- a resumed binding requires the matching tool-contract and tool-transport values and reinjects MCP configuration when its transport is `mcpBridge`;
- the thread receives developer instructions that require explicit `send_message` use for an Intercom reply.

An attached stock TUI can select model, service tier, reasoning effort and summary, personality, collaboration mode, and multi-agent mode for TUI-originated turns. The proxy pins the managed directory, runtime workspace-root list, approval policy, approvals reviewer, sandbox, and permission boundary to the service configuration. Settings update is accepted only while the controller is idle, retains only that closed interactive-field set, and drops permissions and unknown fields. Acceptance atomically prevents broker-delivery start, TUI `turn/start`, and another settings update until the upstream response or error. Independently arriving app-server goal and turn lifecycle remains authoritative. Every Intercom-delivered turn supplies the same service policy again. Thread developer instructions and collaboration-mode developer instructions occupy separate additive sections.

The adapter registers with the broker only after app-server initialization, thread ownership checks, selected-tool startup checks, and persisted-state checks succeed. A peer therefore does not appear in broker discovery before its managed thread can accept or queue a delivery. A persistent-goal continuation may begin during resume before broker registration. Its Intercom tool authorization waits for broker registration and validated ownership or fails with startup; it does not execute against an unregistered broker.

Inbound deliveries enter a bounded FIFO queue. The controller starts the next delivery turn only while the managed thread is idle and no persistent goal or settings update reserves local admission. A downstream TUI reserves the same controller before a forwarded `turn/start`. An accepted `thread/settings/update` holds a separate local admission reservation through its upstream result. App-server may independently publish a persistent-goal continuation without a client `turn/start`; ordered remote lifecycle remains authoritative and is not blocked by that settings reservation. The three turn sources share one root-turn scheduler. A terminal `completed`, `failed`, or `interrupted` notification enters completion processing. A client-started turn returns to idle only after that processing and its `turn/start` response have both finished. A Codex-owned turn has no client start response and returns to idle after terminal processing. Lifecycle notifications behind an unfinished terminal boundary remain ordered until completion processing admits them. Codex can create child threads while the root turn runs. Their decoded lifecycle notifications remain available to the TUI but do not alter root controller state. The inactivity watchdog runs during an active managed turn and during an idle interval reserved by an active or unknown persistent-goal status. Every app-server notification restarts its interval. A supported interactive reverse request for the managed root or a cached descendant records activity when admitted, suspends the interval until the last pending TUI response or headless fallback finishes, and then starts a full interval. A malformed or unrelated interactive request does not alter watchdog state. Progress and unknown notifications otherwise have no controller-state effect and are discarded by the controller after optional TUI forwarding.

An ordinary Codex final answer remains in the Codex thread. Only a successful `send_message` call through the binding's dynamic-tool or MCP transport creates an outbound Intercom message.

### Codex MCP bridge

The controller-side bridge listens on a mode-0600 Unix socket inside the launcher's real, owner-owned mode-0700 directory. A 256-bit random token authenticates each connection. The token reaches the helper only through the request-scoped MCP server environment and is not stored in the binding or descriptor.

App-server starts `intercom codex-mcp-bridge` as a stdio MCP server for an adopted or forked thread. The helper exposes only `send_message` and `list_peers`. Each `tools/call` preserves Codex's raw `_meta`, opens one bridge connection, sends one bounded newline-terminated JSON request, receives one bounded response, and closes. The helper never opens the broker socket and never owns a peer identity.

The adapter requires the top-level and nested Codex thread identities to agree, requires the nested session identity to name the managed root, and requires the nested turn ID to satisfy the same root-or-descendant ownership rules as a dynamic-tool call. A metadata mismatch fails the tool call and terminates the adapter because sole thread ownership is no longer established.

### App-server client

The app-server client performs an HTTP WebSocket upgrade over the supplied Unix socket. Its synthetic WebSocket URL supplies the HTTP request target and host header only; the dial transport has no TCP fallback.

App-server messages use JSON-RPC-shaped request, response, notification, and reverse-request objects without a `jsonrpc` member. The client assigns numeric request IDs, correlates out-of-order responses, preserves correlation across the write and await phases of `turn/start`, retains a bounded tombstone for a canceled request so one late response is ignored, and terminates on malformed envelopes, unrelated or older unknown response IDs, duplicate response IDs, binary messages, or transport failure.

Lifecycle notifications execute in reader order. Reverse requests execute on independent handler goroutines so a tool or approval request cannot block lifecycle notifications. Each reverse request permits one result-or-error response attempt; a response write failure enters the adapter's fatal path.

### Codex TUI proxy

The TUI proxy is the app-server endpoint visible to stock Codex. It owns an owner-only Unix listener and at most one downstream WebSocket session. It does not open a second app-server subscription. The adapter's app-server client remains the sole upstream subscriber.

This ownership is required because app-server sends one reverse request with the same request ID to every subscriber and accepts the first response. A stock TUI does not implement Intercom's dynamic-tool calls and can reject them before the adapter responds. The proxy prevents that response race by intercepting every upstream reverse request on the sole connection. Intercom dynamic tools terminate in the adapter. Command approval, file approval, permission approval, user input, and MCP elicitation route to a ready TUI only when `params.threadId` names the managed root or a cached descendant. An unrelated or malformed supported interactive request receives the applicable fixed headless-policy response. It is not forwarded to the managed TUI and does not change managed activity or watchdog accounting. Absence or disconnect selects the same fixed headless policy for an owned request.

The proxy terminates downstream initialization with the cached upstream initialize response and consumes the downstream `initialized` notification. It validates the downstream Codex version and unavailable capabilities. It assigns independent upstream IDs to TUI requests and independent `intercom-N` IDs to reverse requests sent to the TUI. Responses are correlated back to the original side without exposing either remapped ID. Before the first rollout materializes, the controller can terminate `thread/resume` with a synthetic response derived from the validated `thread/start` snapshot; this makes a new thread attachable before any prior turn.

Upstream notifications received before a valid initial `thread/resume` begins are dropped because the downstream session has no thread context. The initial resume is admitted on the ordered downstream reader, so a later pipelined `turn/start` cannot overtake it. Ready-session `turn/start` requests enter controller admission in downstream wire order, so a later pipelined request cannot reserve the controller first. Once resume processing begins, a bounded barrier retains notifications until the resume response is written, then flushes them in source order before marking the session ready. Every downstream `turn/start` uses the same response barrier. A rejected request writes its error before releasing buffered notifications from an already-ready session. Controller lifecycle reconciliation occurs before proxy delivery. A managed terminal notification and every later notification remain behind a separate bounded controller gate until completion processing and the corresponding start response have both finished.

The proxy accepts only the `initialized` client notification. The controller applies a closed request allowlist. It validates the managed thread for every thread-scoped method; rewrites `thread/resume` ownership and policy fields; pins `turn/start` directory, roots, and policy; rebuilds idle `thread/settings/update` from the closed interactive-field set plus service policy and atomically reserves local scheduler admission until its upstream result; pins config, permission-profile, plugin, skill, hook, and fuzzy-search project scopes; suppresses account-token refresh; and restricts interrupt and steer to a TUI-owned turn. It acknowledges `thread/unsubscribe`, closes that downstream session, and retains the upstream subscription. Unknown methods and operations outside the current-thread scheduler receive an invalid-request error. The complete allowlist and common rejected operations are enumerated in [REFERENCE.md](REFERENCE.md#intercom-codex).

A rollback request is always outside the proxy allowlist. A steer request is accepted only for a TUI-owned starting or active turn; Enter during an Intercom-owned turn is therefore rejected. Tab queues composer text locally and sends no proxy request until the active turn completes. Client handling of a rejected request is outside the proxy boundary. `codex-cli` 0.144.4 treats both rejections as fatal and exits with status 1. TUI process exit removes only the downstream session; the adapter, app-server connection, managed thread, active turn, and queued deliveries remain active.

A downstream disconnect clears only the TUI session. The listener and managed service remain active and accept a later session. A second simultaneous connection is rejected. A connection that does not complete initialize and managed-thread resume within the control timeout is closed. A full outbound notification queue or a downstream frame write that exceeds the control timeout disconnects the slow TUI so its reader cannot block the ordered upstream app-server reader. Forwarded downstream requests share one global concurrency bound across the current session and detached sessions whose upstream calls remain active. Each forwarded request has a control-timeout deadline. An interactive reverse request waits for TUI input under the activity timeout. The active-turn or persistent-goal inactivity watchdog is suspended while at least one owned interactive reverse request awaits the TUI and restarts when the response or headless fallback finishes. Acceptance of a TUI response starts a fresh control-timeout context for the upstream response write. One response arriving after the input deadline is ignored while its relay ID remains in the bounded expired-ID history; an unrelated response ID closes the downstream session.

### Project launcher

The project launcher creates a private runtime directory with distinct upstream `app-server.sock`, downstream `client.sock`, tool `mcp-bridge.sock`, and session-readiness `app-server.session` paths. It disables inherited shell job control and starts `intercom codex-app-server-exec --ready-file SESSION_FILE -- CODEX app-server --listen ENDPOINT`. A terminal signal received during asynchronous child launch is deferred until the launcher records the child PID, then dispatched immediately. The hidden helper resolves the Codex executable, calls `setsid(2)`, writes its PID to a temporary file, forces that file to mode 0600 independently of process umask, closes it, atomically publishes its inode without replacing an existing marker, and replaces itself with Codex. Publication occurs after session creation and before `exec` or any app-server descendant creation. Codex therefore retains the launcher's direct-child PID as both PID and session ID. The launcher requires that exact PID in the readiness file and requires the socket entry before it starts the adapter outside the Codex session.

Session-list and ID-less selection modes query the dedicated app-server before adapter startup. The service mode starts `intercom codex` with all launcher-owned paths and an internal explicit session ID when selection is requested. Remaining adapter options pass through after the launcher consumes its selection options. The launcher treats either child exit as termination of the service group. Adapter exit stops the app-server. App-server exit stops the adapter. Signal handling stops the adapter before the app-server so the adapter can deregister from the broker and interrupt or drain an active Codex turn.

Established-session cleanup invokes the hidden `intercom codex-process-session-cleanup` helper with the recorded PID as both session ID and leader PID. On Linux and Darwin, that helper uses `ps` only to enumerate PID and process-state candidates. It excludes zombies, verifies each remaining PID with `getsid(2)`, and repeats that verification immediately before each signal. Each phase signals a verified PID at most once, with descendants in ascending PID order and the verified leader last; a failed verification or signal remains eligible for a later pass. A `SIGTERM` phase and a subsequent `SIGKILL` phase each receive an independent shutdown-timeout interval measured with a monotonic clock. Inspection and signaling time counts against the applicable phase, and each phase-bounded `ps` call receives only the remaining interval. Transient enumeration, membership, and signaling failures are retried until that phase deadline. A final inspection bounded by the smaller of one second and the configured timeout converts an inspection failure, persistent signal failure, or surviving member list into cleanup failure.

A terminating signal that arrives before readiness publication sends `SIGTERM` to the still-owned direct child and watches for the atomic marker. Marker publication switches cleanup to the full session helper. If the child remains alive without a marker for one timeout interval, only that child receives `SIGKILL`; the helper's publish-before-exec ordering means an app-server descendant cannot exist in this state. A marker whose value differs from the direct-child PID records cleanup failure and still invokes session cleanup against the expected PID.

Launcher signal handlers and the session-cleanup helper ignore repeated `SIGHUP`, `SIGINT`, and `SIGTERM` during cleanup. A descendant that changes process group remains in the session and is covered. A descendant that calls `setsid(2)` creates another process session and escapes this boundary. The membership check and signal operation are separate system calls; PID reuse in the interval between the final `getsid(2)` and `kill(2)` remains a small best-effort race. Reuse of the numeric leader PID as an unrelated new session ID after the original session becomes empty is a second, very-low-probability best-effort boundary.

This service group gives the managed thread a dedicated app-server process, unique TUI endpoint, and unique MCP bridge endpoint. Multiple launchers use independent private directories, so they require no shared port allocation. The lower-level `intercom codex` command relies on its caller to provide the equivalent ownership boundary.

### Live-instance registry

The live-instance registry maps a canonical broker-socket identity and peer name to the currently attachable downstream endpoint, managed thread, managed directory, adapter PID, Codex version, execution policy, and random owner nonce. Publication occurs after the broker and proxy are ready. Removal occurs before broker shutdown and removes a record only when its nonce still names the removing process.

The registry serializes publication and removal with an advisory lock. An atomic rename publishes complete descriptor JSON. A directory-sync failure after rename removes the descriptor only when a re-read still carries the failed publisher's nonce; cleanup failure is joined to the publication error. A live PID with another nonce blocks replacement; a missing PID permits replacement. Attach reads without the lock because rename exposes either a complete prior descriptor or a complete replacement.

### State store

The state store binds a peer name to the Codex thread exclusively managed by that peer. It stores identity, compatibility, and tool-transport metadata only. Codex conversation content and rollout data remain under `CODEX_HOME`.

The state store holds a non-blocking peer lifetime lock. Separate digest-named non-blocking locks under `CODEX_HOME` prevent two Intercom peers that use that Codex home from managing one thread, including peers that use different `INTERCOM_DIR` values. A replacement acquires the prior binding's lock before app-server validation and retains it while acquiring and validating the replacement thread. It releases that prior lock only after readiness commit, or after durable rollback on failure. Fork from the prior bound thread uses the retained prior lock as its source lock and also acquires the returned fork's lock. Ordinary Codex processes do not acquire these locks; exact adoption therefore requires the source TUI or IDE process to stop. State replacement uses a mode-`0600` temporary file, file synchronization, atomic rename, and parent-directory synchronization. Directory synchronization errors that report the documented unsupported operation on Darwin are treated as a portability exception.

## INVARIANTS

The following invariants define a valid running system:

1. One process holds the broker lock for a configured broker socket.
2. One live broker connection owns a peer name. A second connection cannot register that name until the first connection deregisters.
3. A peer name contains 1 through 64 ASCII letters, digits, hyphens, or underscores.
4. A broker assigns the sender identity in each delivery from the registered source connection. A sender cannot select the `from` field of a routed delivery.
5. A successful send acknowledgement means that the broker completed the destination-frame write. It does not mean that an agent observed, processed, or answered the message.
6. A message has no durable Intercom copy. A process failure after acknowledgement can still prevent model processing.
7. One Codex adapter process owns a managed peer lock, one app-server connection, the managed thread lock, and one managed thread. During authorized adoption or fork replacement it also owns the prior binding's thread lock through commit or rollback. Another Intercom peer using the same `CODEX_HOME` cannot manage either locked thread concurrently, regardless of `INTERCOM_DIR`.
8. A Codex binding retains the same peer, canonical project directory, `CODEX_HOME`, state schema, tool contract, and tool transport. Its app-server user agent and Codex version describe the last runtime that passed start or resume validation.
9. A managed Codex thread is non-ephemeral, configured with approval policy `never` and approvals reviewer `user`, and configured with exactly the service's workspace-write or `danger-full-access` sandbox when the adapter becomes discoverable. It is idle or owns one validated Codex persistent-goal turn.
10. One controller admits at most one Codex turn, unresolved start response, or unfinished terminal-processing operation, whether initiated by an Intercom delivery, the attached TUI, or app-server persistent-goal continuation. Deliveries wait in FIFO order while that reservation remains owned or ordered lifecycle events remain deferred. An accepted settings update additionally excludes local delivery start, TUI turn start, and another settings update until its upstream result; it does not supersede ordered app-server lifecycle.
11. A Codex dynamic-tool or MCP call is accepted for the managed root thread only during its starting or active turn, or for a child whose bounded `thread/read` parent or fork ancestry leads to that root. MCP metadata must contain equal top-level and nested thread identities, and its session identity must equal the managed root. Explicit lifecycle ancestry and successful reads are cached. Child authorization never replaces the controller's root turn ID.
12. A Codex final answer does not imply an Intercom reply. The `send_message` tool is the only outbound agent-message operation.
13. Broker writes and adapter-protocol writes serialize per connection. Concurrent frames cannot interleave at the byte level.
14. Peer discovery excludes the requesting connection and sorts names lexicographically.
15. The adapter is the sole subscriber to its dedicated app-server. A TUI reaches app-server only through the adapter proxy.
16. One proxy accepts at most one downstream TUI connection. TUI disconnection does not release the managed thread, broker peer, app-server subscription, or proxy listener.
17. App-server dynamic-tool reverse requests never reach the TUI. MCP tool calls reach only the authenticated private bridge. A supported human-interaction reverse request reaches a ready TUI under a newly allocated downstream ID only when it names the managed root or a cached descendant.
18. Adoption and fork provisionally replace a prior binding only after the selected thread, policy, ancestry where applicable, and required MCP tools pass startup validation. The adapter retains the prior binding's thread lock from before validation through readiness commit, or through binding rollback on failure. Publication or final startup failure restores the prior binding or removes a replacement that had no predecessor.
19. A live-instance descriptor becomes discoverable only after thread ownership, tool validation, broker registration, and proxy listening succeed, and becomes undiscoverable before broker shutdown begins.
20. The project launcher starts Codex as the leader of a dedicated process session whose ID equals its direct-child PID, publishes that equality before app-server execution or descendant creation, and starts the adapter outside that session. Established-session cleanup signals a non-zombie process as an app-server member only after a current `getsid(2)` check immediately before the signal.

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

This flow assumes an opted-in session (an explicitly configured name, a nonblank `INTERCOM_NAME`, or `INTERCOM_ENABLE=1`). A non-opted-in session completes steps 1 through 3 but never performs step 4 and returns an inert error result for a `send_message` or `list_peers` call.

1. Claude Code starts `intercom shim` and opens the MCP stdio transport.
2. The client sends `initialize`; the shim returns server identity, tool capability, channel capability, and agent instructions.
3. The client sends `notifications/initialized`.
4. The background reconnect supervisor connects to the broker, retrying under a numbered suffix on a name collision.
5. A sender calls `send_message`.
6. The source adapter validates tool arguments and writes `send` to the broker.
7. The broker writes `deliver` to the Claude shim.
8. The shim writes `notifications/claude/channel` to Claude Code.
9. The broker returns `send_ack` to the source adapter after the delivery write.

The channel notification and send acknowledgement use independent connections. Their observation order at the two endpoints is not a cross-process ordering guarantee.

### Codex startup

1. The adapter canonicalizes configuration and acquires the peer state lock.
2. The adapter loads the saved binding unless `--new` selects a replacement thread. Authorized adoption or fork replacement retains the prior binding as rollback state and acquires its thread lock before app-server validation.
3. The adapter retries the app-server Unix socket within the startup deadline.
4. The adapter sends `initialize` with `experimentalApi` enabled and validates the returned app-server identity.
5. The adapter sends `initialized`.
6. The adapter starts a thread, resumes the bound thread, adopts the selected existing ID, or requests a fork from the selected source ID. Adoption acquires the selected thread lock before reading it. Fork temporarily locks its source; when the source is the prior binding, the retained prior lock supplies that ownership. A successful fork acquires the returned thread lock.
7. The adapter verifies thread identity, directory, one-entry managed runtime-root list, idle status, non-ephemeral status, approval policy, approvals reviewer, and sandbox policy.
8. After resume or adoption, the adapter reads `thread/goal/get` and establishes the persistent-goal scheduler reservation before broker registration. A null goal does not reserve the scheduler. Status `active` reserves it; `paused`, `blocked`, `usageLimited`, `budgetLimited`, and `complete` release it; an unknown nonempty status reserves it conservatively. App-server error -32601 leaves startup notification-driven. Another goal-read error or an invalid goal identity or status fails startup. Ordered `thread/goal/updated` and `thread/goal/cleared` notifications supersede an earlier read and reconcile later state. A subsequent matching `turn/started` establishes Codex-owned active-turn state. A matching Intercom tool call waits for published readiness. Any other pre-ready tool request is rejected.
9. A new dynamic-tool binding is written after app-server accepts the new thread. Adoption and fork start the private MCP bridge and inject its required configuration before their thread operation. They validate both tools after managed-thread checks and, for adoption, the goal check, then retain the replacement as pending state. A resumed MCP-bridge binding reinjects that configuration during resume and revalidates it after its goal check. A resumed binding refreshes its app-server user-agent and Codex-version diagnostics only after resume, goal, applicable MCP, and pending-materialization validation succeeds.
10. The adapter registers its peer name with the broker.
11. The adapter changes from booting to idle when no Codex-owned turn is active. Otherwise it retains the active or terminal-processing state. Deliveries can enter the FIFO queue in either case and start only after the managed thread becomes idle.
12. When a client endpoint is configured, the adapter creates its downstream listener and begins proxy service.
13. A pending adoption or fork provisionally replaces the saved binding after proxy startup succeeds while the replacement thread lock and any prior binding lock remain held.
14. The adapter atomically publishes the live-instance descriptor, writes the attach commands to standard output, and releases startup handlers. Failure in publication, output, or final release restores the binding present before adoption or fork, or removes the replacement when no binding existed; any prior lock remains held through that operation.
15. Successful final startup release commits the replacement and releases the prior binding's thread lock. Failure releases that lock only after rollback has completed or failed.

`--new`, adoption, and fork do not delete the prior Codex thread or its history. Adoption preserves the selected ID. Fork preserves the source and manages the returned ID. An exact adoption source must stop before startup because ordinary Codex processes do not honor Intercom's thread lock.

### Codex session discovery and selection

1. The launcher starts its dedicated app-server and invokes `intercom codex sessions` before the adapter exists.
2. The selector requests non-archived CLI and VS Code sessions in newest-first pages.
3. Client-side filtering removes ephemeral records, child threads, statuses other than `idle` or `notLoaded`, invalid IDs, duplicates, and records outside the exact working directory unless all-directory mode is selected.
4. List mode writes tab-separated records and exits. Interactive mode writes a numbered terminal list and returns one complete ID on standard output.
5. An all-directory interactive selection must still equal the explicit managed directory; otherwise the selector reports the required `--cwd` and stops.
6. The launcher converts the returned ID to the adapter's internal adopt or fork option.

Explicit selection bypasses the terminal picker but resolves the ID through the same non-archived `thread/list` result and undergoes the same ID, source, working-directory, root-thread, idle-or-not-loaded status, and startup validation inside the adapter. A following `thread/read` refresh must remain eligible. Fork requires app-server to return a distinct ID whose `forkedFromId` equals the source.

### Codex resume and materialization

1. A saved binding supplies the thread ID and identity constraints.
2. The adapter requires the same peer, canonical directory, `CODEX_HOME`, state schema, tool-contract version, and known tool transport. The saved app-server user agent and Codex version are diagnostics rather than identity constraints.
3. The adapter resumes the thread with the required developer instructions, approval policy, approvals reviewer, sandbox mode, directory, and one-entry managed runtime-root list. An MCP-bridge binding also receives fresh request-scoped MCP configuration and tool-status validation.
4. The adapter reads persistent-goal state before broker readiness. An active or unknown goal status reserves the controller even when the corresponding `turn/started` notification has not arrived. A later matching continuation has no adapter-issued `turn/start` response and retains controller ownership through terminal processing.
5. An unmaterialized binding is checked with `thread/read`.
6. A terminal first turn causes a successful `thread/read` and sets `materialized` in the binding.

Codex may not create a rollout record before a first user turn. If resume reports a missing rollout for an unmaterialized binding, the adapter starts a replacement thread. A missing rollout for a materialized binding is fatal and does not trigger replacement.

### Codex TUI attachment

1. `intercom codex attach` canonicalizes the configured broker socket and combines it with the explicit peer name.
2. The registry returns a strictly validated descriptor for that key.
3. Attach verifies that the recorded adapter PID exists.
4. Attach resolves the selected Codex executable and changes to the descriptor's managed directory.
5. Attach replaces itself with `codex resume --remote` for workspace-write, or adds `--dangerously-bypass-approvals-and-sandbox` before `--remote` when the descriptor records danger-full-access.
6. Codex upgrades `/rpc` over the downstream Unix socket and sends `initialize`.
7. The proxy requires the TUI's Codex version to equal the currently running app-server version and returns the cached upstream initialize response.
8. The proxy consumes `initialized` and forwards setup reads through the existing app-server connection with remapped request IDs.
9. The proxy validates and rewrites `thread/resume` for the managed thread, directory, runtime root, approval policy, approvals reviewer, sandbox policy, and permissions boundary.
10. For an unmaterialized newly started thread, the controller returns the saved start snapshot with a null initial-turns page. Otherwise the proxy forwards resume upstream.
11. A successful local or upstream resume marks the TUI session ready for notifications and interactive reverse requests. Failure to reach this state within 30 seconds closes the session and releases the attachment slot.

TUI disconnection removes the downstream session. Repeating the flow attaches a later TUI to the same binding and service. A second TUI cannot pass the WebSocket upgrade while the first session exists.

### Codex TUI turn

1. The TUI submits `turn/start` for the managed thread.
2. The controller accepts the request only from its idle phase, marks the TUI as turn owner, and changes to starting.
3. The proxy rewrites the managed directory, runtime root, approval policy, approvals reviewer, sandbox policy, and permissions boundary, preserves the TUI's non-policy interactive settings, and forwards the call under a new upstream request ID.
4. Ordered `turn/started` and `turn/completed` notifications reach both the TUI and controller.
5. A valid start response and lifecycle event identify the same in-progress turn. A terminal event received first retains the reservation until that response is validated.
6. Intercom tool calls execute through the binding's dynamic-tool or MCP transport for the TUI-owned turn.
7. TUI interrupt or steer requests may target this TUI-owned turn and cannot target an Intercom-delivery turn.
8. Broker deliveries remain in the bounded FIFO while the TUI owns the turn.
9. A terminal lifecycle notification enters completing after the validated start response, or awaiting-start-response before it.
10. Completion processing and the validated start response return the controller to idle, expose the held terminal notification, and permit the next delivery.

If an Intercom delivery reserves the idle controller first, TUI `turn/start` receives a deterministic active-turn error. If the TUI reserves it first, a concurrently selected delivery remains pending and starts after TUI completion. An ambiguous TUI start result is fatal because the controller cannot admit another turn under uncertain ownership.

During a TUI-owned turn, Enter submits a steer request and the proxy forwards it. Tab queues composer text inside the TUI and submits it only after the turn completes. During an Intercom-owned turn, Enter submits a steer request that the proxy rejects. `codex-cli` 0.144.4 exits with status 1 after the rejection while the Intercom-owned turn and service continue.

### Codex reverse request

1. App-server sends one reverse request to the adapter's sole upstream connection.
2. A dynamic-tool method routes directly to Intercom tool handling. Root calls require the active managed turn. Calls from another thread perform a bounded parent-and-fork ancestry walk through `thread/read`, unless explicit lifecycle events or a prior walk already cached that descendant. Verified child calls retain the root's Intercom identity without changing root lifecycle state.
3. For a supported human-interaction method, the adapter decodes `params.threadId`. The ID must equal the managed root or name a descendant already present in the managed ancestry cache. Interactive authorization does not perform a `thread/read` ancestry walk.
4. A supported human-interaction request with malformed parameters, an empty `threadId`, or a thread outside managed ancestry receives the applicable fixed headless-policy response. It does not reach the managed TUI and does not record managed activity or suspend the managed watchdog.
5. When a TUI proxy exists, an owned request waits for published adapter readiness and for the controller notification gate to clear. Notifications retained behind a terminal boundary are offered to the TUI before the reverse request. This ordering wait uses the control timeout.
6. A ready TUI receives the owned request under a new downstream request ID. A TUI result or JSON-RPC error received within the activity deadline is relayed to app-server under the original upstream ID and a fresh control deadline.
7. TUI absence, disconnection, or input timeout before an answer applies the fixed headless handler. An ordering wait that exceeds the control timeout enters the fatal path and also attempts that handler.
8. One response received after an input timeout is discarded while the remapped ID remains in the bounded expired-ID history. An unrelated response ID terminates the TUI session.
9. Other methods use the fixed headless handler without TUI forwarding.

The managed-ancestry check prevents reverse requests for unrelated roots from reaching the managed TUI or affecting managed scheduler accounting. The fixed headless handler still answers supported requests for those roots. For an owned request, response correlation permits at most one adapter response under the original upstream ID. The proxy ID map isolates downstream TUI request IDs from app-server request IDs in both directions.

### Codex MCP tool call

1. App-server starts the required stdio MCP helper with the private socket path and service-lifetime token.
2. Codex sends `tools/call` with arguments and `_meta` containing top-level thread identity and nested session, thread, and turn identity.
3. The helper preserves `_meta`, authenticates one bounded request to the controller-side bridge, and waits under the bridge deadline.
4. The adapter requires the top-level and nested thread identities to agree, requires the session identity to equal the managed root, and authorizes the named turn under root-or-descendant controller ownership.
5. The adapter executes `send_message` or `list_peers` through its existing broker client.
6. The bridge returns one result to the helper; the helper returns an MCP tool result to Codex.

The bridge token authenticates a process with access to the private endpoint. Metadata authorization binds each authenticated call to current managed thread and turn ownership.

### Codex inbound delivery

1. The broker writes a delivery containing an ID, sender, timestamp, and body.
2. The broker client enqueues the delivery in the adapter's FIFO.
3. The idle controller formats the delivery as one text user input with `From`, `Sent`, and `Message-ID` fields.
4. The controller writes `turn/start` with the delivery ID as `clientUserMessageId` and reasserts the owned directory, runtime root, approval policy, approvals reviewer, and sandbox policy.
5. The controller reconciles the `turn/start` response with `turn/started` and `turn/completed` notifications.
6. Intercom tool calls route through the broker while the turn is starting or active.
7. A terminal turn notification enters completing after the validated start response, or awaiting-start-response before it.
8. The first terminal turn confirms thread materialization with `thread/read`.
9. Completed notification processing and the validated start response mark the controller idle, expose held TUI notifications, and permit the next queued delivery.

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
| Codex adapter | Controller phase, Intercom, TUI, or Codex turn owner, active delivery, active turn ID, pending start result, terminal-seen state, in-flight settings reservation, deferred ordered lifecycle count, pending interactive reverse-request count, FIFO deliveries, lifecycle notifications, deferred proxy notifications, pending-thread synthetic resume snapshot, pending replacement binding, replacement rollback snapshot, managed and prior thread locks, execution policy, MCP token and listener | Adapter process; settings reservation ends with its upstream response or error; synthetic snapshot ends at first managed turn activity or materialization; pending replacement ends at its provisional write; its rollback snapshot and prior lock remain until published readiness succeeds or rollback finishes |
| App-server client | Request correlations, completed and expired response-ID histories, reverse-handler count | Adapter process |
| Codex TUI proxy | Listener, downstream session, global forwarded-request slots, claimed downstream IDs, original-to-remapped IDs, pending reverse responses, bounded expired reverse-response IDs, write queue, response-barrier queue, notification opt-outs | Adapter process or downstream session as applicable |
| Codex MCP helper | MCP initialization state and one active bridge request per tool call | App-server child-process lifetime |
| Project launcher | Child process IDs, app-server session-marker state, cleanup-failure flag, and private runtime directory | Launcher process |

Volatile state is not recovered after process termination.

### Persistent Intercom state

| Entry | Content | Persistence rule |
|---|---|---|
| `$INTERCOM_DIR/codex/<peer>.json` | Schema version, peer, thread ID, canonical directory, `CODEX_HOME`, last validated app-server user agent and Codex version, tool-contract version, tool transport, materialization flag | Atomically replaced after a valid new binding, provisional adoption or fork write, successful resume with changed runtime diagnostics, or materialization transition; a provisional replacement is restored or removed if final startup fails |
| `$INTERCOM_DIR/codex/<peer>.lock` | Lifetime ownership lock | File persists; advisory lock exists only while held |
| `$CODEX_HOME/.intercom/thread-locks/<digest>.lock` | Thread-identity ownership lock shared across Intercom runtime directories | File persists; advisory lock exists while an adapter manages the thread or retains a prior binding through replacement commit or rollback |
| `<broker-socket>.lock` | Broker singleton lock | File persists; advisory lock exists only while held |
| Broker log | Structured broker lifecycle records | Appended when the broker does not run in foreground mode |

### Live discovery state

| Entry | Content | Lifetime rule |
|---|---|---|
| `$INTERCOM_DIR/codex/live/.registry.lock` | Advisory serialization lock | File persists; ownership exists only during publication or removal |
| `$INTERCOM_DIR/codex/live/<peer>-<digest>.json` | Broker identity, peer, downstream endpoint, thread, directory, PID, nonce, Codex version, execution policy, schema version | Published after readiness; removed by matching nonce before shutdown; stale after unclean process loss |
| Launcher `app-server.sock` | Upstream app-server transport | Launcher service-group lifetime |
| Launcher `client.sock` | Downstream TUI proxy transport | Adapter lifetime within the launcher service group |
| Launcher `mcp-bridge.sock` | Adopted or forked thread tool transport | Adapter lifetime when the binding transport is MCP bridge |
| Launcher `app-server.session` | Decimal direct-child PID and process-session ID | Atomically published after `setsid(2)` and removed with the private runtime directory |

The broker socket is a live transport endpoint, not persistent state. The launcher sockets and their containing directory exist only for one service-group lifetime.

### External state

Codex stores thread conversation and rollout data under `CODEX_HOME`. Claude Code owns its session state and channel consumption. Intercom binding files do not duplicate either store.

## LIFECYCLES

### Broker lifecycle

The broker acquires its lock before removing a stale socket entry and opening the listener. A second broker that finds the lock held exits successfully. When idle exit is enabled, the idle interval begins whenever the peer count becomes zero and a new registration cancels the active interval. The idle deadline or a termination signal begins shutdown.

Shutdown closes unregistered accepted connections, writes a best-effort `goodbye` to registered peers, closes every connection, drains handlers, attempts to remove the socket entry, and releases the lock. A socket-removal failure is logged and does not make clean shutdown fail. The lock file remains on disk.

### Claude shim lifecycle

The shim lifetime follows MCP standard input, process cancellation, or fatal I/O. End-of-file is a clean shutdown. Shutdown cancels the background reconnect supervisor before closing the broker connection, so a shutdown in progress is observed as cancellation rather than a disconnect the supervisor would otherwise reconnect through, and waits for active MCP tool handlers. A non-opted-in session never starts the supervisor and never holds a broker connection. An opted-in session's supervisor runs a continuous reconnect loop with increasing backoff for the process lifetime; a broker disconnection therefore does not terminate the shim and reconnects without waiting for a tool call.

### Codex adapter lifecycle

The adapter progresses through booting, idle, starting, active, awaiting-start-response, completing, and failed phases. The awaiting phase covers a terminal TUI or Intercom turn whose `turn/start` response has not yet arrived. The completing phase covers a terminal turn whose start response has arrived but whose controller-side completion processing has not finished. Neither phase admits another turn. The peer appears in broker discovery after startup ownership checks during every non-booting, non-failed phase. A configured TUI endpoint becomes discoverable after proxy creation and disappears before broker shutdown begins.

A broker disconnect starts a reconnect loop with exponential backoff. Queued and active Codex work remains in the adapter, but the peer is absent from broker routing until registration succeeds. An app-server disconnect is fatal.

Cancellation marks the adapter unavailable, removes its owned live descriptor, stops broker reconnect work, and closes the broker connection before app-server turn cleanup when the shutdown budget permits. A nonterminal starting or active turn from either source receives `turn/interrupt`. The adapter then drains any outstanding TUI or Intercom `turn/start` result, a terminal turn notification when one has not already been observed, and outstanding reverse-request handlers within the shared control deadline. Proxy closure disconnects the TUI and removes the downstream socket. Closing the app-server client does not stop an externally supervised app-server process.

### Codex TUI lifecycle

A downstream session progresses through accepted, initialized, resumed-ready, and disconnected states. Initialize and resume are required again after every disconnect and must complete within 30 seconds of acceptance. The adapter forwards upstream notifications after successful managed-thread resume. An owned interactive reverse request additionally waits for published adapter readiness and for earlier controller-gated notifications to be offered downstream.

TUI EOF, a client close, a slow outbound queue or write, invalid downstream data, or a proxy close ends that session. Session loss does not stop the proxy listener, broker peer, app-server client, managed thread, active turn, or queued deliveries. A later attachment observes subsequent thread state through `thread/resume`. Proxy listener loss, rather than session loss, terminates the adapter. Proxy shutdown waits for the active downstream handler and all forwarded-request handlers to finish after cancellation before its done state is published.

### Project launcher lifecycle

The launcher creates the private directory and assigns the socket and session-marker paths before either child starts. It starts the session-exec helper and waits for both a valid PID-equals-session marker and the app-server socket. It optionally performs session listing or selection and then starts the adapter with upstream, downstream, and MCP bridge paths. The adapter prints attachment instructions only after full readiness. A startup timeout, invalid marker, selection failure, early app-server exit, child failure, proxy failure, bridge failure, or termination signal initiates cleanup.

Cleanup terminates the adapter first and the app-server session second. The adapter is a normal launcher child outside that session. It receives `SIGTERM`, one shutdown-timeout wait, and, if required, `SIGKILL` followed by another shutdown-timeout wait. Established app-server cleanup delegates to the session-cleanup helper. Its independent monotonic `SIGTERM` and `SIGKILL` phases repeatedly enumerate non-zombie candidates and verify session membership with `getsid(2)` before every signal. Each PID receives a successful signal at most once per phase. A process-group change does not escape the session boundary; a descendant `setsid(2)` call does. A pre-marker app-server child receives direct-child cleanup until the marker appears, at which point established-session cleanup takes over.

The launcher and cleanup helper ignore repeated terminal signals after cleanup begins. Cleanup always attempts to remove the private runtime directory. Cleanup failure changes an otherwise successful launcher exit to status 1. It does not replace an existing nonzero child status or the 129, 130, or 143 status selected by the first terminal signal. The 100-millisecond supervision poll checks the adapter before the app-server session leader. The observed adapter status is propagated when the adapter is found stopped; a nonzero app-server status is propagated when app-server alone is found stopped, and an unexpected zero app-server status becomes failure. Adapter status therefore takes precedence when both children stop between observations.

## FAILURE SEMANTICS

| Condition | Local result | Message or peer result |
|---|---|---|
| Broker socket is missing or refuses a connection | The broker client starts a detached broker and retries | No message is issued until registration succeeds |
| Broker socket fails for a non-startable reason | The connect operation fails | The peer remains unregistered |
| Broker lock is already held | The extra broker exits successfully | The lock owner remains authoritative |
| Peer name is invalid | Registration fails with `bad_name` | The peer remains unregistered |
| Peer name is already connected | Registration fails with `name_taken`; the broker logs `register rejected` | An opted-in Claude shim retries under a numbered suffix (up to 20 candidates) and remains on MCP; Codex startup fails |
| Destination is absent | `send_ack` carries `no_such_peer` | No delivery occurs |
| Destination equals sender | `send_ack` carries `no_self_send` | No delivery occurs |
| Delivery exceeds the frame limit | `send_ack` carries `oversize` | The destination connection remains usable when rejection occurs before writing |
| Destination write fails or exceeds its deadline | The broker removes the destination and returns `deliver_failed` | Delivery outcome may be partial at the failed stream; no retry occurs |
| Source connection drops while awaiting a reply | The pending operation returns a disconnect error | The operation is not replayed because its outcome may be ambiguous |
| Broker process exits | An opted-in Claude shim and the Codex adapter both reconnect in the background with increasing backoff | Disconnected peers cannot receive messages until reconnection completes; a non-opted-in Claude shim never held a connection |
| Claude channel notification write fails | The shim logs the write failure | A broker acknowledgement may already have been returned |
| A delivery arrives while the Codex inbound FIFO already contains 64 queued messages | The adapter enters a fatal shutdown path | The attempted 65th queued delivery is not admitted; queued messages have no durable recovery path |
| A selected lifecycle notification arrives while its FIFO already contains 256 notifications | The adapter enters a fatal shutdown path | The attempted 257th notification is not admitted; controller state is no longer trusted |
| A controller-gated TUI notification arrives while 256 notifications await terminal processing or the corresponding start response | The adapter enters a fatal shutdown path | The attempted 257th notification is not admitted; proxy ordering is no longer trusted |
| App-server exceeds the reverse-handler concurrency limit | The app-server client terminates | The adapter enters a fatal shutdown path |
| App-server message is binary, oversized, malformed, duplicated, or uncorrelated | The app-server client terminates | The adapter enters a fatal shutdown path |
| App-server disconnects | The adapter fails and deregisters | The managed peer becomes unavailable |
| TUI disconnects | The downstream session ends and pending TUI reverse requests use their fallback path | The adapter, managed turn, queued deliveries, app-server connection, and listener remain active |
| A second TUI connects | The proxy returns HTTP status 409 | The existing TUI remains authoritative |
| A TUI does not complete initialize and resume within 30 seconds | The downstream session closes with a policy violation | The sole-session slot becomes available and the adapter remains active |
| A 65th forwarded TUI request arrives while 64 current or detached-session handlers remain active | The request receives error -32001 | The connection and existing requests remain active |
| TUI requests another thread or an ownership-changing operation other than rollback | The proxy returns a JSON-RPC parameter or invalid-request error | The binding and managed thread remain unchanged; client handling of the request error is client-defined |
| `codex-cli` 0.144.4 requests rollback | The proxy returns JSON-RPC error -32600; the TUI exits with status 1 | The adapter, app-server connection, binding, managed thread, active turn, and queued deliveries remain active; a later TUI can attach |
| `codex-cli` 0.144.4 submits Enter during an Intercom-owned turn | The proxy rejects `turn/steer` with JSON-RPC error -32600; the TUI exits with status 1 | The Intercom turn, adapter, app-server connection, and queued deliveries remain active; a later TUI can attach |
| TUI `turn/start` races an existing reservation | The later source does not start a second turn | A rejected TUI receives an active-turn error; a pending delivery waits |
| A local turn or second settings update races an accepted settings update | The settings request retains local admission through its upstream response or error | A delivery waits; a TUI request receives its active-turn or idle-only error; ordered app-server lifecycle remains authoritative |
| TUI turn start has an ambiguous outcome | The adapter fails and enters managed shutdown | No later delivery or TUI turn is admitted under uncertain ownership |
| Another forwarded TUI request exceeds its 30-second upstream deadline | The TUI receives error -32603 and the expired request ID is retained in bounded history | One late upstream response is ignored and the adapter remains active |
| Interactive TUI reverse request reaches its input deadline | The adapter applies the fixed headless handler | One later response is ignored while its remapped ID remains in the bounded expired-ID history |
| Accepted TUI reverse response cannot reach app-server within its fresh control deadline | The adapter fails and enters managed shutdown | The sole upstream responder can no longer complete the request reliably |
| TUI sends an unrelated or no-longer-tracked reverse-response ID | The downstream session is disconnected | The adapter and upstream app-server connection remain active |
| An app-server notification finds the TUI write queue or response-barrier queue full, or a downstream frame write exceeds 30 seconds | The slow downstream session is disconnected | The ordered upstream app-server reader and adapter remain active |
| TUI proxy listener stops | The adapter fails and deregisters | The live descriptor and managed peer become unavailable |
| Live-descriptor publication, readiness-output writing, or final startup release fails | The adapter removes any descriptor written by that attempt and fails startup | The broker peer deregisters and the launcher stops app-server; adoption or fork restores the prior binding or removes a replacement with no predecessor; a `--new` binding remains stored |
| Live descriptor names a missing PID | Attach reports stale state | A later publisher may atomically replace the descriptor |
| Another live PID owns the broker-and-peer descriptor key | Publication fails | No second attach target replaces the live owner |
| Another Intercom adapter holds the selected thread lock | Startup fails | The existing thread owner and saved binding remain authoritative |
| Another Intercom adapter holds the prior binding's thread lock during authorized replacement | Startup fails before app-server validation | The saved binding remains unchanged and no provisional replacement is written |
| Exact adoption races an ordinary Codex process that still uses the thread | Intercom cannot detect or prevent the external writer | Lifecycle, history, and tool ownership can become inconsistent; stopping the source is required |
| Session selection, source eligibility, fork ancestry, or required MCP-tool validation fails | Startup fails before the provisional binding write | The previous binding remains unchanged |
| An authenticated MCP call carries missing, unequal, or unauthorized thread or turn metadata | The tool fails and the adapter enters a fatal path | No broker operation occurs |
| An active Codex turn or an idle persistent-goal reservation produces no app-server activity before the watchdog deadline | The adapter fails and deregisters; cleanup interrupts the current turn when a turn ID is known | The inbound message is not retried |
| Codex turn reports `failed` or `interrupted` | The delivery is terminal; completion processing and the start response return the controller to idle | No automatic reply or retry occurs |
| A root Intercom tool call names another turn, or a foreign tool-call thread has no observed parent or fork ancestry to the managed root | The tool call returns failure and the adapter enters a fatal path | No broker operation occurs; a recognized descendant may use inherited Intercom tools |
| Unsupported app-server reverse request arrives | The adapter returns a denial, unavailable error, or method-not-found error according to request type | Dynamic tools remain Intercom-owned |
| A supported human-interaction reverse request names a thread outside the managed root and cached descendants, or carries malformed routing parameters | The adapter applies the applicable fixed headless policy without managed activity or watchdog accounting | The request does not reach the managed TUI; app-server receives the method-specific protocol response |
| Persisted Codex binding identity or contract is incompatible | Startup fails with an instruction to select `--new` where replacement is valid | The saved binding remains unchanged |
| Materialized Codex rollout is missing | Resume fails | The adapter does not replace the thread implicitly |
| The app-server session marker differs from the launcher's direct-child PID | Startup fails and cleanup is marked failed | Session cleanup still targets the expected PID and session ID; an otherwise successful exit becomes status 1 |
| A termination signal arrives before app-server session publication | The direct child receives `SIGTERM` while the launcher watches for the marker; a surviving unmarked child later receives `SIGKILL` | Publication switches to full session cleanup; publish-before-exec ordering excludes untracked app-server descendants before that point |
| App-server session members survive the `SIGTERM` deadline | A fresh monotonic `SIGKILL` phase begins | The diagnostic distinguishes a live leader from descendant-only survivors |
| Final app-server inspection, signaling, or survivor checks fail | The cleanup helper returns nonzero and the launcher attempts direct-child `SIGKILL` as a fallback | An otherwise successful exit becomes status 1; an existing nonzero or signal status is retained |
| The cleanup helper reports success while the app-server direct child remains live | The launcher reports the inconsistent result and sends direct-child `SIGKILL` | Cleanup is marked failed even when fallback stops the child |
| Private runtime-directory removal fails | The launcher reports the path and marks cleanup failed | An otherwise successful exit becomes status 1; an existing nonzero or signal status is retained |
| Another terminal signal arrives during launcher cleanup | The launcher and session-cleanup helper ignore it | The status selected by the first terminating event remains authoritative |

Send acknowledgement is a transport event. It is not an end-to-end processing receipt. Neither adapter sends acknowledgements back to the originating agent after model observation.

## SECURITY

Intercom's transport boundary is the local Unix host. The broker socket, broker lock, broker log, Codex binding, and Codex peer lock use owner-only modes when Intercom creates them. Intercom creates its state directories with owner-only mode when absent. It does not repair permissions on pre-existing directories.

The broker has no cryptographic authentication. Filesystem access to the Unix socket is the admission control. A process with socket access can register any free valid peer name and send broker frames. The `version` field in `hello` is diagnostic and does not authorize a client.

The broker supplies `from` from the registered connection, but registration does not prove that the process represents a particular project or human. Peer names are routing labels, not security principals.

Inbound message bodies are untrusted agent input. Claude Code receives them as channel content. Codex receives them as user-turn content under existing system and developer instructions. Neither representation grants message content a higher instruction priority.

Managed Codex threads begin and resume with approval policy `never`, approvals reviewer `user`, runtime workspace roots containing only the managed directory, and the configured workspace-write or `danger-full-access` sandbox. Every TUI and Intercom `turn/start` reasserts that complete service policy; TUI settings cannot expand it. For a request whose `threadId` names the managed root or a cached descendant, absence of a ready TUI causes the adapter to decline command and file-change approvals, return an empty turn-scoped permission grant, decline MCP elicitation, and reject interactive user input. With a ready TUI, those owned human-interaction requests are delegated to that local interface and its result is relayed to app-server. A supported human-interaction request for an unrelated thread receives the fixed headless-policy response without delegation to the managed TUI or managed activity accounting. Workspace-write permits actions allowed by app-server's returned sandbox policy. Danger-full-access removes the Codex sandbox and therefore gives model-initiated operations the operating-system permissions of the service account.

The dedicated app-server is an authorization premise for inherited Intercom tools. The controller verifies child ancestry from `thread/read` parent and fork links, caches successful paths, and also records explicit `thread/started` ancestry when app-server supplies it. The lower-level adapter must not connect to a shared app-server whose other clients can create threads with copied Intercom dynamic-tool or MCP definitions.

The MCP bridge uses both filesystem isolation and a random service-lifetime token. Its parent must be a real mode-0700 directory owned by the effective user, its socket is mode 0600, and each request is size-, time-, and concurrency-bounded. The token is process environment, not persistent state. These controls prevent accidental cross-instance routing; they do not defend against another process running as the same user that can inspect process state or access the private directory.

Intercom's thread locks coordinate Intercom processes that use one `CODEX_HOME`, including processes with different `INTERCOM_DIR` values. Replacement retains the prior binding lock until the replacement is committed or the prior state is restored, so another Intercom adapter cannot acquire the prior thread during the rollback window. Fork from that prior thread reuses the retained lock for source ownership and separately locks the returned fork. Codex itself does not acquire these locks, so exact adoption cannot exclude an ordinary Codex TUI or IDE process. The source process must stop before adoption and remain stopped for that thread while the managed service is live. Fork avoids continuing shared ownership by managing the distinct returned ID after commit.

The upstream app-server connection and downstream TUI proxy accept Unix endpoints only and cannot fall back to TCP. The project launcher places both endpoints in an owner-only private runtime directory and the proxy socket has mode 0600. Live descriptors and their registry are owner-only files and directories. A process with access under the same account can still read a descriptor, connect to a socket, or impersonate an eligible peer; the registry is discovery and ownership coordination rather than authentication.

“Local” describes Intercom routing and transport. Claude Code, Codex app-server, model providers, configured MCP servers, and agent tools may transmit message content or derived data according to their own configuration. Intercom does not impose an offline inference boundary.

## COMPATIBILITY

Intercom requires Unix-domain sockets and advisory file locking. The packaged targets are `x86_64-linux`, `aarch64-linux`, `x86_64-darwin`, and `aarch64-darwin`. The project launcher requires Bash, `setsid(2)`, `getsid(2)`, and standard Unix process utilities. The Linux and Darwin cleanup helper invokes `ps -A -o pid= -o stat=` for candidate enumeration; it does not depend on a `ps` session-ID or UID field. A non-Linux, non-Darwin build reports that process-session cleanup is unavailable.

The broker protocol has no negotiated protocol version. JSON decoders ignore unknown object fields, and the receiver accepts a delivery without an ID. These decoding properties do not establish general mixed-build compatibility; one broker socket should use one Intercom build. Frame kinds and required behavioral constraints remain fixed by [BROKER_PROTOCOL.md](BROKER_PROTOCOL.md).

The Claude shim implements a bounded MCP subset. It echoes a client-supplied MCP protocol version during initialization and uses `2025-11-25` when the client omits one. Claude channel delivery depends on support for the advertised `claude/channel` experimental capability.

Codex app-server exposes no feature or schema-version negotiation. The adapter's wire types originate from the experimental schema generated by Codex CLI `0.144.1`, which is the known minimum supported version. Startup accepts a semantic version at least `0.144.1`, then executes and validates the consumed initialize, thread-control, listing, fork, lifecycle, sandbox, dynamic-tool, MCP-configuration, and MCP-status contracts. Unknown additive fields in recognized objects are ignored. Missing fields, incompatible field values, malformed envelopes, invalid request correlation, invalid lifecycle state, and managed-thread invariant violations fail at the affected operation. A newer version number does not override runtime validation.

The downstream proxy requires the TUI client version to equal the currently running app-server version. Restarting the launcher after a Codex upgrade establishes a service and TUI endpoint for the upgraded version.

Codex binding schema version `1`, tool contract version `1`, tool transport, peer name, canonical directory, and `CODEX_HOME` are exact compatibility gates. A schema-1 binding with an omitted tool transport is normalized to `dynamic`. The saved app-server user agent and Codex version record the last successfully validated runtime and are refreshed atomically after successful resume validation. They do not require binding replacement when Codex changes. An incompatible exact binding field is not migrated in place. `--new` remains stored after its thread checks succeed. Adoption and fork provisionally write a replacement after their pre-publication checks and retain rollback state until startup reaches published readiness.

## SOURCE MAP

| Responsibility | Source |
|---|---|
| Command registration and broker binary selection | [`cmd/intercom/main.go`](../cmd/intercom/main.go) |
| Broker command and logging policy | [`cmd/intercom/broker_cmd.go`](../cmd/intercom/broker_cmd.go) |
| Peer-name diagnostic command | [`cmd/intercom/name_cmd.go`](../cmd/intercom/name_cmd.go) |
| Peer-list diagnostic command | [`cmd/intercom/peers_cmd.go`](../cmd/intercom/peers_cmd.go) |
| Claude shim command | [`cmd/intercom/shim_cmd.go`](../cmd/intercom/shim_cmd.go) |
| Codex adapter command | [`cmd/intercom/codex_cmd.go`](../cmd/intercom/codex_cmd.go) |
| Codex app-server process-session helper | [`cmd/intercom/codex_app_server_exec_cmd_unix.go`](../cmd/intercom/codex_app_server_exec_cmd_unix.go), [`cmd/intercom/codex_app_server_exec_cmd_unsupported.go`](../cmd/intercom/codex_app_server_exec_cmd_unsupported.go) |
| Codex process-session cleanup helper | [`cmd/intercom/codex_process_session_cleanup_cmd_unix.go`](../cmd/intercom/codex_process_session_cleanup_cmd_unix.go), [`cmd/intercom/codex_process_session_cleanup_getsid_linux.go`](../cmd/intercom/codex_process_session_cleanup_getsid_linux.go), [`cmd/intercom/codex_process_session_cleanup_getsid_darwin.go`](../cmd/intercom/codex_process_session_cleanup_getsid_darwin.go), [`cmd/intercom/codex_process_session_cleanup_cmd_unsupported.go`](../cmd/intercom/codex_process_session_cleanup_cmd_unsupported.go) |
| Broker registration, routing, idle exit, and shutdown | [`internal/broker/broker.go`](../internal/broker/broker.go) |
| Shared broker connection and auto-spawn behavior | [`internal/brokerclient/client.go`](../internal/brokerclient/client.go) |
| Broker frame types and codec | [`internal/wire/wire.go`](../internal/wire/wire.go) |
| Claude MCP-to-broker adapter | [`internal/shim/shim.go`](../internal/shim/shim.go) |
| MCP server subset | [`internal/mcp/mcp.go`](../internal/mcp/mcp.go) |
| Shared agent tool contract | [`internal/intercomtools/tools.go`](../internal/intercomtools/tools.go) |
| Codex controller and lifecycle | [`internal/codex/controller.go`](../internal/codex/controller.go) |
| Codex dynamic-tool, MCP-tool, and reverse-request handling | [`internal/codex/tools.go`](../internal/codex/tools.go) |
| Codex binding and lifetime lock | [`internal/codex/state.go`](../internal/codex/state.go) |
| Codex session discovery and terminal selection | [`internal/codexsession/session.go`](../internal/codexsession/session.go), [`internal/codexsession/picker.go`](../internal/codexsession/picker.go) |
| Codex private MCP bridge | [`internal/codexbridge/bridge.go`](../internal/codexbridge/bridge.go), [`internal/codexbridge/helper.go`](../internal/codexbridge/helper.go) |
| Codex app-server protocol types | [`internal/appserver/protocol.go`](../internal/appserver/protocol.go) |
| Codex app-server connection and dispatch | [`internal/appserver/client.go`](../internal/appserver/client.go) |
| Unix WebSocket transport | [`internal/appserver/unixws.go`](../internal/appserver/unixws.go) |
| Codex TUI proxy and downstream correlation | [`internal/appserverproxy/proxy.go`](../internal/appserverproxy/proxy.go) |
| Live Codex descriptor registry | [`internal/codexinstance/registry.go`](../internal/codexinstance/registry.go) |
| Runtime and state paths | [`internal/paths/paths.go`](../internal/paths/paths.go) |
| Peer-name resolution and validation | [`internal/peername/name.go`](../internal/peername/name.go) |
| Project launcher supervision and process-session orchestration | [`scripts/intercom-codex-project`](../scripts/intercom-codex-project) |

## SEE ALSO

- [README.md](../README.md) — public synopsis and quick start.
- [HANDBOOK.md](HANDBOOK.md) — installation and operating procedures.
- [REFERENCE.md](REFERENCE.md) — commands, tools, environment, files, limits, and errors.
- [BROKER_PROTOCOL.md](BROKER_PROTOCOL.md) — broker transport and frame contract.
- [DEVELOPMENT.md](DEVELOPMENT.md) — build and verification procedures.
