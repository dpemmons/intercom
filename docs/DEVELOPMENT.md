# INTERCOM DEVELOPMENT

## NAME

`intercom-development` — build and verification procedures for Intercom

## CONTENTS

- [Purpose](#purpose)
- [Requirements](#requirements)
- [Checkout build](#checkout-build)
- [Nix package build](#nix-package-build)
- [Verification tiers](#verification-tiers)
  - [Tier 1 — format, analysis, and package tests](#tier-1--format-analysis-and-package-tests)
  - [Tier 2 — race detector and coverage](#tier-2--race-detector-and-coverage)
  - [Tier 3 — repeated concurrency tests](#tier-3--repeated-concurrency-tests)
  - [Tier 4 — protocol fuzzing](#tier-4--protocol-fuzzing)
  - [Tier 5 — target builds](#tier-5--target-builds)
  - [Tier 6 — Nix flake](#tier-6--nix-flake)
  - [Tier 7 — real Codex app-server](#tier-7--real-codex-app-server)
  - [Tier 8 — stock Codex TUI attachment](#tier-8--stock-codex-tui-attachment)
- [Continuous integration](#continuous-integration)
  - [Triggers](#triggers)
  - [Jobs](#jobs)
- [Notes](#notes)
- [See also](#see-also)

## PURPOSE

This document defines the development environment, checkout build, verification
tiers, and continuous-integration contract for Intercom. Every command executes
from the repository root.

## REQUIREMENTS

| Component | Required interface | Required for |
|---|---|---|
| Linux or macOS | Unix domain sockets, process signals, and file locking | All builds and tests |
| Go | Version declared by [`go.mod`](../go.mod); the declared version is 1.25.5 | Go builds, analysis, tests, coverage, and fuzzing |
| Bash | Bash with job control, process substitution, and arithmetic expansion | `scripts/intercom-codex-project` and its tests |
| Host utilities | `chmod`, `mktemp`, `rm`, and `sleep` | Launcher execution and its tests |
| C toolchain | A compiler supported by the Go race detector | Race-enabled tests |
| Nix | A Nix installation with flake support | Nix package verification |
| Codex CLI | `codex-cli` 0.144.1 or later | Opt-in real app-server tests and stock-TUI verification |
| Codex authentication and model access | A working interactive configuration for the selected compatible Codex CLI | Opt-in stock-TUI verification only |

The Go toolchain downloads the modules declared by [`go.mod`](../go.mod) and
[`go.sum`](../go.sum) when they are absent from the module cache. Nix downloads
the locked inputs declared by [`flake.lock`](../flake.lock) when they are absent
from the Nix store. These operations require network access unless the
corresponding caches contain all inputs.

The Tier 7 real Codex tests require no OpenAI credentials and make no external
model request. They start an installed Codex executable with an isolated
`CODEX_HOME`. Tier 8 uses the normal authenticated Codex home and makes one
model request.

The flake development shell supplies the Go toolchain selected by `flake.nix`,
`gopls`, and `gotools`:

```sh
nix develop path:.
go version
```

## CHECKOUT BUILD

### Purpose

The checkout build produces the `intercom` executable and validates the launcher
syntax without installing either artifact.

### Inputs

| Input | Type | Mode | Default |
|---|---|---|---|
| `go.mod` | Go module definition | Read-only | None |
| `go.sum` | Module checksums | Read-only | None |
| `./cmd/intercom` | Go main package | Read-only | None |
| `scripts/intercom-codex-project` | Bash program | Read-only | None |
| `intercom` | Output file | Created or replaced | Repository root |

### Procedure

```sh
go build -o intercom ./cmd/intercom
bash -n scripts/intercom-codex-project
./intercom --version
```

The generated `intercom` file is ignored by Git.

### Verification

`go build` exits with status 0, `bash -n` emits no output, and
`./intercom --version` prints the build version and commit identity.

### Errors

- `go build` exits nonzero when dependency loading, compilation, or linking
  fails.
- `bash -n` exits nonzero when the launcher contains invalid Bash syntax.
- `./intercom --version` exits nonzero when the executable cannot start.

## NIX PACKAGE BUILD

### Purpose

The Nix build includes both `intercom` and `intercom-codex-project`:

### Inputs

| Input | Type | Mode | Default |
|---|---|---|---|
| `flake.nix` | Nix flake definition | Read-only | None |
| `flake.lock` | Locked flake inputs | Read-only | None |
| Working tree | Path-flake source | Read-only | Repository root |
| Nix store output | Package path | Created or reused | Nix store |

### Procedure

```sh
nix build path:.
./result/bin/intercom --version
./result/bin/intercom-codex-project --help
```

`path:.` copies the working tree, including untracked files, into the flake
source. A Git flake reference can omit untracked source files from a dirty
checkout.

### Verification

Both installed programs exit with status 0. `intercom --version` reports the
package version and source revision supplied by `flake.nix`.

### Errors

- `nix build` exits nonzero when flake evaluation, input fetching, vendor-hash
  verification, compilation, launcher installation, or package fixup fails.
- Either program exits nonzero when the package omits an artifact or the
  artifact cannot start.

## VERIFICATION TIERS

The tiers are cumulative for release verification. A lower tier remains
required when a higher tier applies.

| Change boundary | Required tiers |
|---|---|
| Documentation only | 1 |
| Go behavior without concurrency or protocol changes | 1–2 |
| Concurrency or lifecycle behavior | 1–3 |
| Broker frames, MCP messages, app-server messages, or tool schemas | 1–4 |
| Supported target or packaged artifact | 1–6 |
| Codex app-server protocol or managed-thread lifecycle | 1–7 |
| Stock Codex TUI attachment or downstream-proxy compatibility | 1–8 |

### Tier 1 — format, analysis, and package tests

#### Purpose

Tier 1 verifies source formatting, static analysis, unit tests, in-process
integration tests, command tests, and launcher tests. The real Codex tests skip
unless `INTERCOM_CODEX_SMOKE=1` is present.

#### Procedure

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 ./...
```

#### Coverage

| Package | Boundary exercised by `go test ./...` |
|---|---|
| `.` | Broker-to-shim end-to-end delivery and combined Claude/Codex adapter behavior with a simulated app-server |
| `./cmd/intercom` | Command registration, option precedence, endpoint validation, session listing and selection, live-descriptor publication, execution-policy inheritance, attach process replacement, readiness output, and signal handling |
| `./docs/examples` | Compilation of the standalone manual broker-framing example |
| `./internal/appserver` | Unix-WebSocket transport, request correlation, reverse requests, limits, cancellation, and protocol shapes |
| `./internal/appserverproxy` | Downstream Unix-WebSocket service, TUI initialization, request-ID remapping, notification forwarding, reverse-request relay, connection exclusion, queue limits, and disconnect recovery |
| `./internal/broker` | Registration, routing, ordering, shutdown, idle exit, locking, deadlines, and concurrent delivery |
| `./internal/brokerclient` | Connection, broker auto-start, request handling, reconnection, cancellation, and concurrent callers |
| `./internal/codex` | Managed-thread state, adoption, fork, transactional replacement, thread locking, lifecycle control, Intercom/TUI turn arbitration, execution-policy pinning, reverse routing, delivery serialization, dynamic and MCP tools, recovery, and simulated app-server integration |
| `./internal/codexbridge` | Private Unix listener and MCP helper authentication, framing, metadata preservation, limits, deadlines, concurrency, and cleanup |
| `./internal/codexinstance` | Live-descriptor validation, broker-scoped keys, atomic publication, cross-process exclusion, stale-PID handling, and nonce-checked cleanup |
| `./internal/codexsession` | Paged session discovery, eligibility filtering, explicit-ID resolution, deterministic ordering, terminal selection, and display sanitization |
| `./internal/intercomtools` | Shared tool schemas, strict argument decoding, size limits, result formatting, and fuzz seeds |
| `./internal/mcp` | MCP initialization, tool dispatch, notifications, errors, concurrency, and ping |
| `./internal/paths` | Environment overrides and derived runtime paths |
| `./internal/peername` | Peer-name precedence and validation |
| `./internal/shim` | Claude shim name resolution |
| `./internal/wire` | Broker framing, compatibility, limits, deadlines, concurrent writes, and peer-name grammar |
| `./scripts` | Per-instance app-server, client, and MCP-bridge path setup; explicit and interactive session selection; list mode; readiness-output preservation; supervision; signals; exit propagation; timeout validation; and cleanup |

#### Success

All three commands exit with status 0. The formatting command emits no output.
The Go test command can report the real Codex tests as skipped when verbose test
output is enabled.

#### Errors

- The formatting command exits with status 1 when `gofmt -l .` returns one or
  more file names.
- `go vet` exits nonzero and identifies the package and analyzer diagnostic when
  static analysis fails.
- `go test` exits nonzero when a package does not compile, a test fails, a test
  panics, or the Go test process terminates abnormally.

### Tier 2 — race detector and coverage

#### Purpose

Tier 2 executes every package test under the Go race detector, randomizes the
top-level test order, disables cached results, and writes atomic coverage data.

#### Procedure

```sh
go test -race -shuffle=on -count=1 \
  -covermode=atomic -coverprofile=coverage.out ./...
```

#### Coverage

The command exercises the same package set as Tier 1. The Go test driver writes
`coverage.out` in the repository root. Git ignores that file.

#### Success

The command exits with status 0 and reports the shuffle seed. No `DATA RACE`
report appears.

#### Errors

- The command exits nonzero under every Tier 1 test failure condition.
- The command exits nonzero when the race detector observes an unsynchronized
  conflicting memory access.
- The command exits nonzero when coverage data cannot be created or written.

### Tier 3 — repeated concurrency tests

#### Purpose

Tier 3 repeats the concurrency-sensitive packages under the race detector to
exercise scheduling-dependent behavior.

#### Procedure

```sh
go test -race -count=20 \
  ./internal/appserver \
  ./internal/appserverproxy \
  ./internal/broker \
  ./internal/brokerclient \
  ./internal/codex
```

#### Coverage

The command executes each test in the five listed packages 20 times. It covers
upstream and downstream WebSocket request correlation, TUI connection and
request arbitration, broker routing and shutdown, broker-client reconnection,
and managed Codex lifecycle serialization.

#### Success

The command exits with status 0 after all repetitions complete. No `DATA RACE`
report appears.

#### Errors

- The command exits nonzero when any repetition fails.
- The command exits nonzero when the race detector observes an unsynchronized
  conflicting memory access.

### Tier 4 — protocol fuzzing

#### Purpose

Tier 4 runs the five fuzz targets assigned to untrusted broker, MCP-tool, and
app-server input boundaries.

#### Procedure

```sh
go test -run=^$ -fuzz=FuzzConnRead -fuzztime=5s ./internal/wire
go test -run=^$ -fuzz=FuzzRequestIDJSON -fuzztime=5s ./internal/appserver
go test -run=^$ -fuzz=FuzzParseUnixEndpoint -fuzztime=5s ./internal/appserver
go test -run=^$ -fuzz=FuzzDecodeSendMessage -fuzztime=5s ./internal/intercomtools
go test -run=^$ -fuzz=FuzzDecodeListPeers -fuzztime=5s ./internal/intercomtools
```

#### Coverage

| Fuzz target | Input boundary |
|---|---|
| `FuzzConnRead` | Length-prefixed broker frames and JSON envelopes |
| `FuzzRequestIDJSON` | App-server request identifiers |
| `FuzzParseUnixEndpoint` | App-server Unix endpoint syntax |
| `FuzzDecodeSendMessage` | `send_message` JSON arguments and encoded-size limits |
| `FuzzDecodeListPeers` | `list_peers` JSON arguments |

`-run=^$` suppresses ordinary test execution. Each command assigns a
five-second fuzzing interval to the named target.

#### Success

Each command exits with status 0 after its fuzzing interval.

#### Errors

- A command exits nonzero when a fuzz input causes a panic, invariant failure,
  unexpected process termination, or test failure.
- The Go fuzz driver records a reproducing input under the affected package's
  `testdata/fuzz` directory when it can persist the failure.

### Tier 5 — target builds

#### Purpose

Tier 5 verifies compilation for every supported operating-system and
architecture pair without C dependencies.

#### Procedure

```sh
(
  set -e
  build_root=$(mktemp -d)
  trap 'rm -rf "$build_root"' EXIT

  for target in \
    linux/amd64 \
    linux/arm64 \
    darwin/amd64 \
    darwin/arm64
  do
    target_os=${target%/*}
    target_arch=${target#*/}
    CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch \
      go build -o "$build_root/intercom-$target_os-$target_arch" ./cmd/intercom
  done
)
```

#### Coverage

The command compiles `./cmd/intercom` for Linux and macOS on AMD64 and ARM64.
The temporary output directory is removed when the subshell exits.

#### Success

The subshell exits with status 0 after all four executables link.

#### Errors

- The subshell exits nonzero when any target fails dependency loading,
  compilation, or linking.
- `mktemp` exits nonzero when it cannot create the output directory.

### Tier 6 — Nix flake

#### Purpose

Tier 6 evaluates the flake and builds its declared check for the host system.

#### Procedure

```sh
nix flake check path:. --print-build-logs
```

#### Coverage

The flake check builds the `intercom` package for the host system. That package
contains the Go executable and the launcher. The flake check does not execute
the Go test suite.

#### Success

The command exits with status 0 after evaluation and package construction.

#### Errors

- The command exits nonzero when flake evaluation, dependency fetching, vendor
  hash verification, Go compilation, launcher installation, or package wrapping
  fails.
- The command exits nonzero when the host system has no declared flake output.

### Tier 7 — real Codex app-server

#### Purpose

Tier 7 verifies the consumed app-server contract against an installed Codex CLI
at or above the known minimum declared by
[`protocol.go`](../internal/appserver/protocol.go).

#### Procedure

```sh
codex --version
INTERCOM_CODEX_SMOKE=1 \
  go test -count=1 -v \
  -run '^TestCompatibleCodexAppServer(Schema|Smoke|LocalProviderE2E|ForkedSubagentDynamicToolE2E|AdoptOrdinarySessionMCPBridgeE2E)$' \
  ./internal/codex
```

The version command must print `codex-cli VERSION`, where `VERSION` is 0.144.1
or later. For example:

```text
codex-cli 0.144.1
```

`CODEX_BIN` selects a non-default executable:

```sh
CODEX_BIN=./codex INTERCOM_CODEX_SMOKE=1 \
  go test -count=1 -v \
  -run '^TestCompatibleCodexAppServer(Schema|Smoke|LocalProviderE2E|ForkedSubagentDynamicToolE2E|AdoptOrdinarySessionMCPBridgeE2E)$' \
  ./internal/codex
```

#### Coverage

`TestCompatibleCodexAppServerSchema` generates every experimental app-server
JSON schema, removes documentation-only schema annotations, canonicalizes the
complete schema set, and compares its structural fingerprint with the reviewed
baseline. Formatting and descriptions do not affect the fingerprint. Any
added, removed, or changed schema file, method, property, type, enum, union,
required field, nested definition, request, response, or notification fails the
test and requires contract review before the baseline changes.

`TestCompatibleCodexAppServerSmoke` performs the following operations against the
real executable:

1. Starts `codex app-server` on an isolated Unix socket with an isolated
   `CODEX_HOME`.
2. Enables the experimental app-server API, validates the minimum server
   version, and exercises the consumed initialize and managed-thread contract.
3. Starts a non-ephemeral thread with `approvalPolicy: never`, workspace-write
   sandboxing, developer instructions, and the Intercom dynamic tools.
4. Verifies the pre-materialization `thread/read` and `thread/resume` error
   contracts without starting a model turn.

`TestCompatibleCodexAppServerLocalProviderE2E` performs the following operations
against the real executable and a loopback Responses-compatible model server:

1. Starts and materializes a managed thread.
2. Executes `list_peers` as a dynamic tool and returns the result to the model
   provider.
3. Restarts app-server, resumes the thread, and executes the restored dynamic
   tool.
4. Kills app-server while a dynamic-tool request is outstanding.
5. Starts app-server again, resumes the thread, verifies the interrupted turn,
   and verifies that the outstanding request is not replayed.

`TestCompatibleCodexAppServerForkedSubagentDynamicToolE2E` materializes a root
thread, starts a full-history child thread through the real app-server, verifies
the child's root ancestry through `thread/read`, and executes an inherited
Intercom dynamic tool from the child.

`TestCompatibleCodexAppServerAdoptOrdinarySessionMCPBridgeE2E` materializes an
ordinary remote-TUI thread without Intercom dynamic tools, verifies its VS Code
source classification, adopts it through the controller, and executes
`list_peers` through the request-scoped managed MCP helper. It then restarts the
controller and app-server, resumes the saved binding, reinjects a fresh helper,
and executes the tool again. The loopback provider and in-process broker require
no credentials or external network access.

Tier 7 does not start the stock Codex TUI through the downstream proxy. The
proxy's initialization, remapping, reverse routing, turn arbitration,
disconnect, and exclusion contracts use simulated app-server and TUI peers in
`./internal/appserverproxy` and `./internal/codex`. Tier 8 supplies the real
client boundary.

#### Success

All five tests pass. No test reports a skip. All generated schemas, Codex
homes, sockets, model-server state, and project directories remain confined to
test temporary directories and are removed by the test harness.

#### Errors

- The tests fail when `codex` is absent from `PATH` and `CODEX_BIN` is unset.
- The tests fail when `CODEX_BIN` names an executable that cannot start.
- The tests fail when schema generation fails or the canonical structural
  fingerprint differs from the reviewed baseline.
- The tests fail when the app-server user agent does not identify a semantic
  version of 0.144.1 or later.
- The tests fail when app-server does not accept its Unix-WebSocket connection
  within five seconds.
- The tests fail when any consumed request, response, sandbox, lifecycle,
  persistence, dynamic-tool, MCP-configuration, adoption, or crash-recovery
  invariant differs.

### Tier 8 — stock Codex TUI attachment

#### Purpose

Tier 8 verifies the launcher, live descriptor, stock Codex TUI initialization
and resume, unmaterialized-thread attachment, model turn, detachment, and
reconnection as one interactive service.

#### Prerequisites

`intercom`, `intercom-codex-project`, and Codex CLI 0.144.1 or later must be
available in `PATH`. The launcher and attachment terminal must select the same
Codex CLI version. The normal Codex home must contain working authentication
and model configuration. Two interactive terminals are required.

#### Procedure

The service terminal creates isolated Intercom state and an empty project, then
starts the foreground launcher:

```sh
codex --version
smoke_root=$(mktemp -d)
mkdir -p "$smoke_root/state" "$smoke_root/project"
chmod 700 "$smoke_root" "$smoke_root/state" "$smoke_root/project"
INTERCOM_DIR="$smoke_root/state" \
  intercom-codex-project --name tui-smoke --cwd "$smoke_root/project"
launcher_status=$?
rm -rf "$smoke_root"
test "$launcher_status" -eq 130
```

The version output must identify version 0.144.1 or later. After readiness, the
attachment terminal executes the complete command printed beneath `Attach from
another terminal:`. That command preserves the launcher's `CODEX_BIN`
selection. The TUI submits this prompt:

```text
Reply with exactly TUI_SMOKE_OK. Do not call tools.
```

After the terminal answer reaches completion, an empty composer receives
`Ctrl-C` and the TUI process exits. The attachment terminal executes the same
printed attachment command again, verifies that the prior prompt and answer
appear in the resumed thread, and exits the TUI again. The service terminal
then receives `Ctrl-C`; the remaining cleanup commands remove the isolated
state and require launcher status 130.

#### Coverage

The procedure exercises a real stock client initialize and resume, the local
resume snapshot before first materialization, one authenticated model turn,
rollout materialization, downstream disconnect without service shutdown, live
descriptor reuse, and a second stock-client initialization against the same
service and thread.

This procedure does not exercise active-turn composer or rollback behavior.
During a TUI-owned turn, Enter submits a forwarded `turn/steer` request and Tab
queues the composer text locally until the active turn completes. During an
Intercom-owned turn, Enter submits a rejected `turn/steer` request. Rollback
submits rejected `thread/rollback`. Client handling of either request error is
version-dependent; `codex-cli` 0.144.4 exits with status 1. In both rejection
cases, the adapter, app-server connection, managed thread, active turn, and
queued deliveries remain active and a later TUI can attach. Simulated proxy
tests verify the JSON-RPC rejections and service survival; this interactive tier
verifies neither stock-client error path.

#### Success

The first attachment opens the managed thread before it has prior turns. The
model returns `TUI_SMOKE_OK`. TUI exit leaves the launcher running and the peer
registered. The second attachment opens the same thread and displays the first
turn. Launcher interruption returns status 130 and removes its private sockets.

#### Errors

- An app-server version earlier than 0.144.1 invalidates the verification.
- A TUI version different from the running app-server version fails proxy
  initialization and invalidates the verification.
- Missing Codex authentication or model access prevents the model turn.
- Missing readiness output identifies launcher, app-server, adapter, broker,
  proxy, descriptor, or output failure.
- A failed first attachment identifies descriptor discovery, executable,
  remote-socket, initialization, or synthetic-resume failure.
- A failed second attachment or missing first turn identifies detach,
  materialization, descriptor-reuse, or resume failure.
- A launcher status other than 130 identifies signal or child-status failure.

## CONTINUOUS INTEGRATION

The Forgejo workflow is defined by
[`../.forgejo/workflows/ci.yml`](../.forgejo/workflows/ci.yml).

### Triggers

The workflow runs for pushes to `main`, pull requests, and manual workflow
dispatches.

### Jobs

| Job | Timeout | Contract |
|---|---:|---|
| `verify` | 30 minutes | Formatting, vet, race tests with coverage, repeated concurrency tests, and all five fuzz targets |
| `build` | 15 minutes per matrix entry | `CGO_ENABLED=0` builds for Linux AMD64, Linux ARM64, macOS AMD64, and macOS ARM64 |
| `nix` | 30 minutes | Flake evaluation and package build |

The `verify` job executes these commands in order:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test -race -shuffle=on -count=1 -covermode=atomic -coverprofile=coverage.out ./...
go test -race -count=20 ./internal/appserver ./internal/appserverproxy ./internal/broker ./internal/brokerclient ./internal/codex
go test -run=^$ -fuzz=FuzzConnRead -fuzztime=5s ./internal/wire
go test -run=^$ -fuzz=FuzzRequestIDJSON -fuzztime=5s ./internal/appserver
go test -run=^$ -fuzz=FuzzParseUnixEndpoint -fuzztime=5s ./internal/appserver
go test -run=^$ -fuzz=FuzzDecodeSendMessage -fuzztime=5s ./internal/intercomtools
go test -run=^$ -fuzz=FuzzDecodeListPeers -fuzztime=5s ./internal/intercomtools
```

The job uploads `coverage.out` as the `coverage` artifact.

Each `build` matrix entry selects one of these equivalent commands:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/intercom
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/intercom
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/intercom
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/intercom
```

The `nix` job executes `nix flake check --print-build-logs` against a clean Git
checkout. The local dirty-checkout equivalent is:

```sh
nix flake check path:. --print-build-logs
```

The workflow does not set `INTERCOM_CODEX_SMOKE`. The real Codex tests skip
in continuous integration. The workflow establishes simulated app-server
coverage but does not establish compatibility with an installed Codex release.

## NOTES

`-count=1` disables reuse of cached Go test results. The race and fuzz tiers can
consume more CPU and memory than Tier 1.

The package test suite creates isolated broker sockets, state directories,
Codex homes, and launcher runtime directories. It does not use the default user
broker socket or default Intercom state directory.

The Codex minimum protocol version and the real Codex test requirement refer to
the same version constant in [`protocol.go`](../internal/appserver/protocol.go).
A minimum-version change requires schema review, simulated protocol tests, and
Tier 7 verification.

The Nix flake check verifies package construction only. A successful flake
check does not imply successful Go tests, race detection, fuzzing, or real
Codex compatibility.

## SEE ALSO

- [Project synopsis](../README.md)
- [User handbook](HANDBOOK.md)
- [Command and tool reference](REFERENCE.md)
- [Architecture](ARCHITECTURE.md)
- [Broker protocol](BROKER_PROTOCOL.md)
