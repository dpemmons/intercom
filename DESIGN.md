# intercom — design

A Go re-implementation of [`MuhammadTalhaMT/claude-intercom`](https://github.com/MuhammadTalhaMT/claude-intercom),
scoped down for the realistic use case of **two (or more) Claude Code sessions running on the same
machine talking to each other in real time**.

## Goals

- Two or more local Claude Code sessions can exchange messages, with inbound messages surfaced via
  the [Channels API](https://code.claude.com/docs/en/channels-reference) (i.e. delivered as
  `<channel>` tags in Claude's context, not as polled tool output).
- Single Go binary, multiple subcommands. `intercom shim` runs as the per-session MCP server;
  `intercom broker` is the local router; small utility subcommands (`intercom name`,
  `intercom peers`, `intercom version`) for debugging.
- N peers, addressed by name. `send_message` takes a `to` field.
- **One-time setup.** Add intercom to user-level `~/.claude.json` once; every project gets a peer
  name automatically (defaults to the project directory basename).
- No ports, no shared secrets, no manual broker startup.
- Fix the issues listed under [What we're fixing from the reference](#what-were-fixing-from-the-reference).

## Non-goals

- Cross-machine. Out of scope for v1; the broker listens on a Unix socket only. (See
  [Future work](#future-work).)
- Windows. Unix sockets are POSIX-only. macOS and Linux are supported.
- Permission relay (`claude/channel/permission`). Possible later; not v1.
- Persistence. No SQLite, no on-disk message log. Broker state is in-memory only.
- Authentication on the wire. Unix socket file mode (0600, owned by the user) is the boundary.
- A pairing/onboarding flow. Local-only, single-user — not needed.
- Two Claude Code sessions in the *same* project talking to each other. Both would auto-name to the
  same project basename; the second would fail to register. Workaround: set `INTERCOM_NAME`
  explicitly in one of them.

## Architecture

```
Claude Code A ──stdio (MCP)──► intercom shim A ──┐
                                                  ├── unix socket ──► intercom broker
Claude Code B ──stdio (MCP)──► intercom shim B ──┘                    (single process,
                                                                       in-memory router)
```

- Each Claude Code session spawns its own `intercom shim` subprocess via `.mcp.json`. This is required
  because the Channels API only delivers `notifications/claude/channel` to stdio-spawned MCP
  servers — the `<channel>` tag treatment is gated to subprocesses Claude Code itself launched.
- The shim opens a single connection to the broker over a Unix domain socket. If the socket isn't
  there, it spawns the broker and retries.
- The broker is a tiny in-memory router. It tracks connected shims by name and forwards messages
  between them. No history, no on-disk state.

### Why a broker rather than peer-to-peer or single-HTTP-MCP

| Alternative | Why not |
|---|---|
| Single MCP server over HTTP (one binary, both Claudes connect as MCP clients) | Channels API requires stdio. Without it, inbound delivery becomes poll-based via a tool call — meaningfully worse UX. |
| Peer-to-peer Unix sockets with a shared rendezvous directory | Each shim becomes both client and server, has to handle stale socket files, scan to discover peers. The broker centralizes that. |
| Two binaries (separate `intercom-shim` and `intercom-broker`) | More to ship and document. Single binary with subcommands is the standard Go pattern. |

## What we're fixing from the reference

The reference (`intercom.ts`) is a good 200-line starting point but has issues we'll address:

| Reference issue | Fix in this design |
|---|---|
| Default secret `"change-me-in-production"` accepted silently | N/A — no shared secret. Auth is Unix socket file mode. |
| `REMOTE_HOST.includes('ngrok')` to pick HTTP vs HTTPS | N/A — local socket, no scheme. |
| No outbound HTTP timeout — `send_message` can hang forever | Writes and request/reply round-trips have deadlines (default 5s). The steady-state inbound read loop has none — it's expected to block. |
| No request size cap on inbound POST | Wire framing has a max frame size (default 256 KiB). The shim also rejects oversized `send_message` arguments before serializing. |
| No graceful shutdown | Both shim and broker handle `SIGTERM`/`SIGINT`. Shim: stop accepting new tool calls, finish any in-flight, then exit. Broker: stop accepting new connections, close existing ones with a `goodbye` frame, unlink socket file. |
| No body validation | Wire messages are length-prefixed JSON parsed into typed structs; malformed messages are rejected with an error response, never forwarded blindly. |
| Strictly point-to-point | N peers, addressed by name, with `list_peers`. |
| Hardcoded role default (`developer-a`) that two sessions silently collide on | Default to project basename (`filepath.Base(cwd)`); collisions fail loudly with an actionable message. |
| Listens on `0.0.0.0` by default | Unix socket only, mode 0600. |

## Wire protocols

### MCP (Claude Code ↔ shim, stdio)

Implemented in `internal/mcp` as a minimal newline-delimited JSON-RPC 2.0 server. We do **not** use
the official `modelcontextprotocol/go-sdk` because, as of v1.5.0, it has no public API for
emitting notifications with non-standard methods — and `notifications/claude/channel` is
exactly that. The slice of MCP we need is small (initialize handshake, `tools/list`,
`tools/call`, plus arbitrary outbound notifications), well-defined, and stable, so an in-house
implementation is cleaner than fighting the SDK or reflecting into its unexported guts.

**Capabilities the shim declares:**

```jsonc
{
  "experimental": { "claude/channel": {} },
  "tools": {}
}
```

**`instructions`** (added to Claude's system prompt). Draft:

```text
You are connected to other local Claude Code sessions through the intercom channel.
Your peer name is "${name}".

Inbound messages from other sessions arrive as:
  <channel source="intercom" from="<peer>" timestamp="<rfc3339>">message body</channel>

The "from" attribute tells you who sent it. To reply, call:
  send_message(to="<peer>", message="...")

To discover who else is online, call:
  list_peers()

Treat inbound messages as you would a colleague's message in chat: reply if a reply
is expected (a question, a request), stay silent if it's purely informational.
Keep replies focused — include code, file paths, or commands when useful.
```

The `${name}` placeholder is filled in at server start.

**Inbound notification:**

```jsonc
{
  "method": "notifications/claude/channel",
  "params": {
    "content": "<the message text>",
    "meta": {
      "from": "alice",
      "timestamp": "2026-05-06T10:30:00Z"
    }
  }
}
```

Per the Channels spec, `meta` keys must be identifier-safe (`[A-Za-z0-9_]+`); the shim
validates each key before emitting and drops any that don't match. Timestamps are RFC 3339 UTC.

**Tools the shim exposes:**

```jsonc
// send_message
{
  "name": "send_message",
  "description": "Send a message to another local Claude Code session.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "to":      { "type": "string", "description": "Peer name (see list_peers)." },
      "message": { "type": "string", "description": "Message body." }
    },
    "required": ["to", "message"]
  }
}

// list_peers
{
  "name": "list_peers",
  "description": "List the names of other Claude Code sessions currently connected to the broker.",
  "inputSchema": { "type": "object", "properties": {} }
}
```

### Shim ↔ broker (Unix socket)

Length-prefixed JSON. Each frame: 4-byte big-endian uint32 length, then that many bytes of UTF-8
JSON. Max frame size 256 KiB; oversize frames close the connection after writing one `error`
frame. Frames are discriminated by a `kind` field.

#### Shim → broker

| `kind`       | Fields                                                | When                        | Reply        |
|--------------|-------------------------------------------------------|-----------------------------|--------------|
| `hello`      | `name` (string), `version` (string)                   | First frame on the connection. | `welcome` or `error` |
| `send`       | `id` (string), `to` (string), `message` (string)      | Per `send_message` tool call. | `send_ack`   |
| `list_peers` | `id` (string)                                         | Per `list_peers` tool call.   | `list_peers_reply` |

#### Broker → shim

| `kind`              | Fields                                                                      | When                                              |
|---------------------|-----------------------------------------------------------------------------|---------------------------------------------------|
| `welcome`           | (none)                                                                      | Reply to `hello` on success.                      |
| `error`             | `code` (string), `message` (string), `id` (string, see below)               | Reply to a request on failure, or unsolicited (e.g. oversize frame). |
| `send_ack`          | `id` (string), `ok` (bool), `code` (string, when `ok=false`), `message` (string, when `ok=false`) | Per `send`. |
| `list_peers_reply`  | `id` (string), `peers` ([]string, sorted lexicographically)                 | Per `list_peers`. Excludes the requester's own name. |
| `deliver`           | `from` (string), `message` (string), `timestamp` (RFC 3339)                 | Server-pushed when another peer sends to this one. |
| `goodbye`           | `reason` (string)                                                           | Just before broker closes the connection (broker shutdown, idle exit). |

`error.id` is **required** when responding to a `send` or `list_peers` request (so the shim can fail
the right pending call), and **omitted** for unsolicited errors (`oversize`, `bad_frame` on a frame
we couldn't parse, `hello_timeout`).

#### Error codes

| Code             | Direction       | Meaning                                                       |
|------------------|-----------------|---------------------------------------------------------------|
| `bad_hello`      | broker → shim   | First frame wasn't `hello`, or required fields missing.       |
| `bad_name`       | broker → shim   | `name` is empty or contains characters outside `[A-Za-z0-9_-]`. |
| `name_taken`     | broker → shim   | Another peer is already registered with this name.            |
| `hello_timeout`  | broker → shim   | Connection didn't send `hello` within 5s; broker closes the connection. |
| `no_such_peer`   | broker → shim (in `send_ack`) | `send.to` didn't match any connected peer at lookup time. |
| `no_self_send`   | broker → shim (in `send_ack`) | `send.to` equals the sender's own registered name. |
| `deliver_failed` | broker → shim (in `send_ack`) | The destination peer was registered at lookup, but writing the `deliver` frame to it failed (slow consumer, write-deadline exceeded, peer disconnected mid-route). The destination is also dropped from the registry. |
| `oversize`       | both directions | Frame exceeds 256 KiB. Connection closes. |
| `bad_frame`      | both directions | Malformed JSON or unknown `kind`. Connection closes. |

#### Correlation, concurrency, framing

- `id` is generated by the shim using `crypto/rand` (16 hex chars). Echoed unchanged in `send_ack`,
  `list_peers_reply`, and request-correlated `error` frames. Not present on `deliver`, `welcome`,
  `goodbye`, or unsolicited `error`.
- The broker validates `hello.name` against `[A-Za-z0-9_-]+` and rejects with `bad_name` /
  `bad_hello` if it fails. Defense in depth — the shim validates too, but the broker doesn't
  trust it because the validated `name` becomes the `from` value in `deliver` frames (and thus a
  `meta` value in the channel notification).
- `hello.version` is the shim's version string. The broker logs it on connect; no enforcement, no
  rejection on mismatch. Useful for debugging mixed-version setups during upgrades.
- The broker imposes a 5s deadline on receiving `hello` after `accept`. Connections that miss it
  get a `hello_timeout` error frame and are closed.
- The broker writes `deliver` frames with a 5s write deadline. A peer that can't drain a small
  JSON frame within 5s is treated as dead: the destination is dropped from the registry, and the
  sender's `send_ack` carries `deliver_failed`.
- A `send` to the sender's own name (`to == name`) is rejected with `no_self_send` rather than
  attempted. Self-loop would risk infinite Claude → Claude reflection bugs and has no real use.
- MCP allows concurrent tool calls. Both shim and broker hold a per-connection write mutex so
  frames don't interleave; the read path is single-goroutine per connection. Per-sender → per-
  receiver ordering is preserved as a side effect (single goroutine per receiver writes under
  that receiver's write mutex).
- No heartbeat. Both sides rely on EOF detection. If the broker dies, the shim's read returns EOF
  and the shim transitions to "disconnected" (see [Reconnect behavior](#reconnect-behavior)).
- A `goodbye` frame from the broker is treated like EOF on the shim side; the `reason` field is
  surfaced verbatim in the user-facing error attached to the next failing tool call.

## Components

### `cmd/intercom`

Single entry point built on [cobra](https://github.com/spf13/cobra). Subcommands:

- `intercom shim` — runs the per-Claude MCP server. (Invoked by Claude Code, not by users
  directly.)
- `intercom broker` — runs the router. Usually auto-spawned by the shim; can be run manually for
  debugging.
- `intercom name` — prints the peer name the shim would register with for the current working
  directory. No broker contact; pure local resolution.
- `intercom peers` — connects to the broker and prints currently-connected peer names.
- `intercom version` — prints binary version + git SHA. (Implemented as a subcommand for
  symmetry; cobra also exposes the same string via `--version` on the root.)

Cobra is overkill for five subcommands today, but it gives us help text, flag parsing,
shell-completion generation, and room to grow without restructuring later.

### `internal/shim`

- Boots the MCP server on stdio (registers the `claude/channel` capability, the `send_message`
  and `list_peers` tools, and the `instructions` string with the resolved peer name interpolated).
- Connects to broker (auto-spawning it if needed; see [Shim startup](#shim-startup)).
- Maintains a `pending` map keyed by `id`: each in-flight `send` / `list_peers` parks a channel
  there waiting for the broker's reply.
- Maintains a single read goroutine that reads broker frames and either:
  - Resolves a pending entry (`send_ack`, `list_peers_reply`, request-correlated `error`), or
  - Translates a `deliver` into a `notifications/claude/channel` (with `from` and `timestamp`
    populated as `meta`), or
  - Treats `goodbye` and EOF identically: closes the connection, fails all pending entries with
    a clear error message.
- Translates MCP `CallTool` invocations into broker `send` / `list_peers` frames; reconnects
  on-demand if disconnected.

### `internal/broker`

- Listens on `~/.claude-intercom/broker.sock` (path overridable by env).
- Per-connection goroutine: enforces the 5s `hello` deadline, validates `hello` (`bad_hello` /
  `bad_name` / `name_taken` outcomes), then loops reading incoming frames.
- In-memory `map[string]*conn` of registered peers, guarded by a `sync.RWMutex`. Names are
  re-validated at registration; this is the gate that makes `from` values in `deliver` frames
  safe to use as channel `meta` values downstream.
- Each `*conn` has its own write mutex; multiple goroutines (the read goroutine emitting replies,
  other peers' goroutines emitting `deliver` frames) coordinate writes via that mutex.
- Routes `send` by: rejecting self-loop (`no_self_send`), looking up `to` under the registry read
  lock, writing a `deliver` to the destination with a 5s write deadline. On write failure, drops
  the destination from the registry and acks the sender with `deliver_failed`.
- On connection close (clean or EOF), removes the peer from the map.
- Idle exit: if the peer map has been empty for 10 minutes, the broker writes a `goodbye` to any
  late arrivals and exits cleanly. (Configurable; set to 0 to disable.)

### `internal/wire`

Frame codec (length-prefixed JSON, max-size enforcement), message type structs, error code
constants, peer-name validation regex, request `id` generation (`crypto/rand` → 16 hex chars).
Shared by shim and broker.

### `internal/mcp`

Minimal MCP server: stdio transport, newline-delimited JSON-RPC 2.0 framing, the
initialize/initialized handshake, `tools/list` and `tools/call` dispatch, and a public
`Notify(method, params)` for sending arbitrary notifications (which is what makes our use of
`notifications/claude/channel` straightforward). Used only by the shim.

## Configuration

### Initial setup (one time)

Add intercom to **user-level** `~/.claude.json` so every Claude Code session you start gets it for
free:

```jsonc
{
  "mcpServers": {
    "intercom": {
      "command": "/usr/local/bin/intercom",
      "args": ["shim"]
    }
  }
}
```

No `env` block needed for the common case. With this in place:

- Open Claude Code in `~/src/foo` → shim registers as peer name `foo`.
- Open another in `~/src/bar` → shim registers as peer name `bar`.
- Either Claude can discover the other with `list_peers` and message it with `send_message`.

### Name resolution

The shim picks its peer name in this order:

1. `$INTERCOM_NAME` if set (explicit override).
2. Otherwise, `filepath.Base(cwd)` — the project directory basename.

So no per-project setup is needed unless you have two projects with the same basename open at the
same time (see [Collision handling](#collision-handling) below).

### Logging

Two destinations:

- **Shim**: writes to its own stderr. Claude Code already captures subprocess stderr to its
  per-session debug log (`~/.claude/debug/<session-id>.txt`); piggybacking on that avoids a second
  source-of-truth.
- **Broker**: detached process, no parent capturing stderr. Writes to `INTERCOM_BROKER_LOG`
  (default `~/.claude-intercom/broker.log`) with `O_APPEND` so concurrent `write(2)`s don't
  interleave at the kernel level.

### Env vars

| Env var | Used by | Default | Purpose |
|---|---|---|---|
| `INTERCOM_NAME` | shim | project basename (`filepath.Base(cwd)`) | This session's peer name. Override when you want something other than the directory name. |
| `INTERCOM_SOCKET` | shim, broker | `~/.claude-intercom/broker.sock` | Path to the Unix socket. |
| `INTERCOM_BROKER_BIN` | shim | path of the running shim binary (`os.Executable()`) | Used to auto-spawn the broker. Override only if you want to pin a different broker version. |
| `INTERCOM_IDLE_EXIT` | broker | `10m` | Broker exits after this long with zero peers. `0` disables. |
| `INTERCOM_BROKER_LOG` | broker | `~/.claude-intercom/broker.log` | Plain-text log file. Append-only (`O_APPEND`). |

### Collision handling

If two shims try to register the same name (e.g. you have `~/work/intercom` and `~/personal/intercom`
both open in Claude Code), the second one gets `error: name_taken` from the broker and exits with
a clear message:

```
peer name "intercom" is already in use; set INTERCOM_NAME in .mcp.json
or your shell to override (e.g. INTERCOM_NAME=intercom-personal)
```

We deliberately do **not** auto-suffix to `intercom-2`, because the other Claude has to be told a
name to send to — auto-generated names defeat the point.

To fix a collision, add `env: { "INTERCOM_NAME": "..." }` to the project's local `.mcp.json` (which
overrides the user-level config), or export `INTERCOM_NAME` in your shell before launching Claude
Code.

## Lifecycles

### Shim startup

1. Resolve peer name: `$INTERCOM_NAME` if set, else `filepath.Base(cwd)`. Validate against
   `[A-Za-z0-9_-]+` and reject empty; exit non-zero with a clear stderr message if invalid
   (e.g. cwd is `/`).
2. The `shim` subcommand handler sets up the MCP server on stdio (declares capabilities,
   registers `send_message` and `list_peers`, sets `instructions` with the resolved name
   interpolated).
3. Try to `net.Dial("unix", socketPath)`.
   - On success, send `hello`.
   - On failure (`ENOENT` or `ECONNREFUSED`): spawn the broker, then make up to 4 retry dials
     with sleeps of `100ms, 300ms, 1s, 3s` between them (5 attempts total, ~4.4s of total wall
     time in the worst case). Spawn uses `INTERCOM_BROKER_BIN` if set, otherwise
     `os.Executable()`. If `os.Executable()` returns an unusable path, exit with an actionable
     error.
4. On `welcome`, the shim is ready and the read goroutine starts.
5. On `error: name_taken` or `error: bad_name`, exit non-zero with a clear stderr message.

### Reconnect behavior

If the read goroutine sees EOF (broker died, was restarted, etc.):

- Mark the connection as disconnected. Don't auto-respawn the broker in the background.
- In-flight tool calls awaiting a `send_ack` / `list_peers_reply` resolve with an `isError: true`
  content describing the broker disconnect.
- The next `send_message` or `list_peers` tool call triggers the same dial-and-spawn sequence as
  startup (step 3 above). If it succeeds, the shim re-registers and the call proceeds.
- We deliberately don't poll in the background: idle reconnection costs nothing for the user, and
  a working reconnect on next use is good enough.

### Broker startup

1. Acquire an exclusive flock on a sentinel file (`broker.sock.lock`). If held, another broker is
   already running — exit silently with code 0 (the shim will retry the dial).
2. Unlink any stale socket file at `socketPath`.
3. Listen, set socket mode 0600.
4. Accept loop until `SIGTERM` / `SIGINT` / idle exit.
5. On shutdown: close listener, unlink socket, release flock.

### Failure modes

| Scenario | Handling |
|---|---|
| Shim's broker connection drops mid-conversation | Pending tool calls resolve with `isError: true`. Shim moves to disconnected; reconnects on next tool call (see [Reconnect behavior](#reconnect-behavior)). |
| Send to a peer that just disconnected | `send_ack` with `code: no_such_peer`. The MCP tool returns that as `isError` content. |
| Broker can't bind socket (perms / busy / EADDRINUSE after stale unlink) | Logs to broker log and exits non-zero. Shim's retry exhausts and surfaces a clear error to Claude. |
| Two shims try to register the same name | Second one gets `error: name_taken`, exits non-zero. Claude Code's `/mcp` shows the failure. |
| Frame larger than max | Receiver writes one `error: oversize` frame and closes the connection. |
| Malformed JSON or unknown `kind` | Receiver writes one `error: bad_frame` and closes the connection. |
| Claude Code crashes / shim's stdin EOFs | Shim exits cleanly. Its broker connection closes; broker drops it from the registry. Other peers' subsequent `send` calls get `no_such_peer`. |
| Broker's accept loop fails | Logs and exits. Next shim invocation auto-spawns a fresh broker. |
| Oversized `send_message` argument from Claude | Shim returns `isError: true` content explaining the limit, before any wire I/O. |
| Shim is mid-receiving a `deliver` and gets `SIGTERM` | Shim exits; the in-flight delivery is dropped. The sender's `send_ack` was already `ok=true` (broker write succeeded), so from the sender's perspective the message was delivered. Acceptable for v1; a redelivery story would require persistence. |
| Self-send (`send_message(to=name, ...)`) | Broker rejects with `no_self_send`; tool returns `isError: true`. |
| Broker idle-exit fires while a shim is mid-call | The pending call resolves with the broker-disconnect error; next tool call auto-respawns the broker. |

## Implementation plan

1. `internal/wire`: frame codec (length-prefixed JSON, max-size enforcement), message types,
   `id` generation, name validation. Tests for: short reads, oversize frames, malformed JSON,
   unknown `kind`, all field validations.
2. `internal/broker`: listener, peer registry, routing, idle-exit timer, flock-based startup.
   Tests with a fake clock and raw-socket clients for: handshake happy path, `bad_hello`,
   `name_taken`, `bad_name`, `hello_timeout`, `send` to present peer, `send` to absent peer
   (`no_such_peer`), self-send (`no_self_send`), `deliver_failed` when the destination's read
   side is plugged, `list_peers` excludes self and is sorted, concurrent `send`s to the same
   destination preserve order, peer disconnect cleanup, idle exit, `goodbye` on shutdown.
3. `internal/shim`: MCP server wrapper + broker client + auto-spawn logic + reconnect on
   next-call. Tests with a mock broker socket: tool call → wire frame → reply → MCP response,
   broker-disconnect path, oversized message rejection, concurrent in-flight calls.
4. `cmd/intercom`: cobra root + subcommands (`shim`, `broker`, `name`, `peers`, `version`).
5. End-to-end smoke test (top-level `e2e_test.go`, build-tag-gated): spawn broker, spawn two shims
   with stdio mocked via pipes, drive a `send_message` and assert the `notifications/claude/channel`
   appears on the other side. Also covers self-send rejection and broker-restart reconnect.
6. README with `.mcp.json` snippet and the two-Claude walkthrough.

## Future work (out of scope for v1)

- Cross-machine: a separate `intercom relay` mode that exposes the broker over TLS (or a tunnel
  someone else manages, like Tailscale). Would add real auth — likely the krb5/SPNEGO path discussed
  earlier.
- Permission relay (`claude/channel/permission`). The broker doesn't need to change much; the shim
  would gain a notification handler that turns inbound permission requests into peer messages with
  a structured prefix, and parses verdict replies the same way.
- Recent-history ring buffer per peer, so a late-joining session sees the last N messages. Still
  in-memory, no DB.
- A `/health` HTTP endpoint on the broker for monitoring.
