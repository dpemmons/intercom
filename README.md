# intercom

A local-only chat bridge between Claude Code sessions. One Claude Code session
calls `send_message`; the other receives it as a `<channel>` tag in its
context, in real time, via the [Channels API](https://code.claude.com/docs/en/channels-reference).

Inspired by [`MuhammadTalhaMT/claude-intercom`](https://github.com/MuhammadTalhaMT/claude-intercom),
rewritten in Go with a single-binary, broker-plus-shim architecture optimized
for the realistic case of two or more local Claude Code sessions on the same
machine.

See [`DESIGN.md`](./DESIGN.md) for the full architecture, wire protocols, and
non-goals.

## Requirements

- macOS or Linux
- Go 1.25+ (to build)
- Claude Code v2.1.80+ (the Channels API is in research preview)

## Install

```sh
go install github.com/dpemmons/intercom/cmd/intercom@latest
```

Or build from a checkout:

```sh
go build -o /usr/local/bin/intercom ./cmd/intercom
```

## Configure (one time)

Add the shim to your **user-level** `~/.claude.json` so every Claude Code
session you start gets it for free:

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

No `env` block needed for the common case. The shim auto-derives a peer name
from the basename of the directory you started Claude Code in.

## Use

Start Claude Code with the channel allowlist override (required during the
research preview):

```sh
claude --dangerously-load-development-channels server:intercom
```

In one Claude Code window:

> "What sessions are online? Send a message to `<peer>` asking about the API
> for the dashboard."

The other session receives the message as a `<channel>` tag and can reply by
calling `send_message`.

## Subcommands

| Command            | What it does |
|--------------------|--------------|
| `intercom shim`    | The MCP server. Launched by Claude Code via `~/.claude.json`. |
| `intercom broker`  | The local router. Auto-spawned by the shim; can be run by hand for debugging. |
| `intercom name`    | Print the peer name the shim would register with for the current cwd. |
| `intercom peers`   | Connect to the broker and print the names of other connected peers. |
| `intercom version` | Print version + git SHA (also available via `--version`). |

## Configuration

| Env var               | Used by      | Default                          | Purpose |
|-----------------------|--------------|----------------------------------|---------|
| `INTERCOM_NAME`       | shim         | basename of cwd                  | This session's peer name. |
| `INTERCOM_SOCKET`     | shim, broker | `~/.claude-intercom/broker.sock` | Path to the Unix socket. |
| `INTERCOM_DIR`        | shim, broker | `~/.claude-intercom`             | Override the runtime directory entirely. |
| `INTERCOM_BROKER_BIN` | shim         | `os.Executable()`                | Override the binary used to auto-spawn the broker. |
| `INTERCOM_BROKER_LOG` | broker       | `~/.claude-intercom/broker.log`  | Where the broker writes structured logs. |

## How it works

```
Claude Code A ──stdio (MCP)──► intercom shim A ──┐
                                                  ├── unix socket ──► intercom broker
Claude Code B ──stdio (MCP)──► intercom shim B ──┘
```

- Each Claude Code session spawns its own `intercom shim` subprocess. The
  shim speaks MCP over stdio and declares the `claude/channel` capability,
  which is what gates the `<channel>` tag treatment in Claude's context.
- All shims connect to one shared `intercom broker` over a Unix socket. The
  broker is auto-spawned by the first shim and exits cleanly after 10 idle
  minutes.
- `send_message` in one shim becomes a `notifications/claude/channel` event
  on the destination shim, surfaced to Claude as a `<channel>` tag.

## Limitations

- **Single machine only.** The broker listens on a Unix socket; there is no
  TLS path. Cross-machine support is intentionally deferred (see
  [DESIGN.md → Future work](./DESIGN.md#future-work-out-of-scope-for-v1)).
- **No persistence.** Messages are routed in memory; the broker keeps no
  history. If a peer is offline when you send to it, the send fails with
  `no_such_peer`.
- **Same-project collisions are loud.** If you open two Claude Code windows
  on the same project, both auto-name to the project basename and the second
  fails to register. Workaround: set `INTERCOM_NAME` explicitly in one of
  them, e.g.:
  ```jsonc
  // project-local .mcp.json overrides ~/.claude.json
  {
    "mcpServers": {
      "intercom": {
        "command": "/usr/local/bin/intercom",
        "args": ["shim"],
        "env": { "INTERCOM_NAME": "myproj-window2" }
      }
    }
  }
  ```
- **macOS / Linux only.** Unix sockets are POSIX-only.

## Tests

```sh
go test ./... -race
```

The `e2e_test.go` at the repo root spins up an in-process broker and two
shims and exercises a full `send_message` → `notifications/claude/channel`
round trip.

## License

MIT
