# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Nomad task driver plugin (`driver = "systemd"`) that manages **pre-existing** systemd units on
the host over DBus. It does not spawn or supervise processes: a task names a unit, and the driver
starts/stops/monitors it and streams its journal into the task's stdout/stderr.

Consequences that shape the whole codebase:

- **No isolation of any kind.** No filesystem, no network namespace, no signals, no exec. Units
  live outside the allocation, so the driver enforces its own safety rules instead (see below).
- **Units are host-global.** At most one task may manage a given unit at a time
  (`Driver.unitOwners`), and `allowed_units`/`denied_units` regex lists gate which units are
  eligible at all. Without those lists any job submitter can take over arbitrary host units.
- **cgo + Linux only.** `sdjournal` links against libsystemd, and resource stats are read from the
  unified (v2) cgroup hierarchy only — on v1/hybrid hosts units still run but stats read as zeros
  (`Manager.CgroupV2Available` detects this and it is reported in the fingerprint).

## Building and testing

**The host is macOS; nothing here compiles or tests natively.** `go build ./...` fails on
`systemd/sd-journal.h not found`. Every build/vet/test must run in Linux with `libsystemd-dev`.

```bash
make docker-build
```

```bash
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.24-bookworm sh -c "apt-get update -qq && apt-get install -y -qq libsystemd-dev && go test -race ./..."
```

Single test / single package inside that container:

```bash
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.24-bookworm sh -c "apt-get update -qq && apt-get install -y -qq libsystemd-dev && go test -race -run TestHandler_HandleStateChange ./pkg/task/"
```

The dev container (`.devcontainer/`, Debian 12 + Go 1.24.4 + systemd headers + golangci-lint) is
the interactive equivalent; inside it `make test`, `make vet`, `make build`, `make lint` work
directly. `make vet` deliberately no-ops on non-Linux.

**Prefer the dev container image over the throwaway `golang:` containers above** — once an IDE has
built it, it already carries `libsystemd-dev`, the module cache and golangci-lint, so a run costs
seconds instead of an `apt-get`/`go install` round trip on every invocation:

```bash
docker run --rm -v "$PWD":/workspace -w /workspace -v nomad-systemd-driver-plugin-gomodcache:/go/pkg/mod "$(docker images --format '{{.Repository}}:{{.Tag}}' | grep -m1 nomad-systemd-driver-plugin)" bash -lc "make lint && make test"
```

Lint and format both go through golangci-lint v2 (`make lint`, `make fmt`, `make fmt-check`).
`make fmt` runs on macOS; **`make lint` does not** — it type-checks, so it fails on the same missing
`systemd/sd-journal.h` and has to run in Linux like everything else. Installing the linter from
source needs Go ≥ 1.25, newer than the Go in the dev container's own image.

`.golangci.yaml` is large and strict — notable enforced conventions:

- `gci` import order: standard → default → `github.com/webitel` → local module → blank → dot.
- `containedctx` is enabled; the three structs that legitimately hold a lifetime context carry
  `//nolint:containedctx // @kirychukyurii: <reason>`. Follow that `@kirychukyurii:` prefix format
  for any new nolint.
- revive `enforce-map-style`/`enforce-slice-style` require `make(...)`, not literals.
- `interface{}` is rewritten to `any` on format.

## Architecture

Three layers, each with a `doc.go` that states its contract — read those first.

```
main.go → plugin.Driver → systemd.Manager → DBus / journal / cgroupfs
                       ↘ task.Handler (one per task)
```

**`plugin/`** — the `drivers.DriverPlugin` implementation Nomad talks to, one file per concern:
`driver.go` holds the `Driver` type and the plugin-level RPCs (`SetConfig`, schemas,
`Capabilities`); `task.go` the per-task RPCs (`RecoverTask`…`TaskEvents`); `fingerprint.go` the
health loop; `about.go` plugin metadata and HCL specs; `config.go` `Config`/`TaskConfig` and
unit-name validation; `unit.go` unit ownership (`claimUnit`/`releaseUnit`) and the compiled
allow/deny regexes; `pprof.go` is an optional debug server, enabled by a non-empty `pprof_addr` and
reconfigured on config reload.

The `Manager` and the compiled `unitPolicy` are `atomic.Pointer` fields because `SetConfig` writes
them while task RPCs and the fingerprint loop read them concurrently, with no ordering guarantee.
The driver fingerprints itself undetected until `SetConfig` establishes the DBus connection.

**`pkg/systemd/`** — `Manager` owns exactly one system-bus connection and reconnects on its own
when it drops, so callers retry rather than discard it. Key invariants:

- Per-unit state lives in one `map[string]*unitState` under one `unitsLock`. **No lock is ever
  held across a DBus call, a file read, or a channel send.**
- Callers pass plain contexts with no deadlines. `opContext` applies the budget appropriate to the
  operation (property lookups ~10s, start/stop jobs 2min, since `TimeoutStopSec` defaults to 90s),
  bounded also by the Manager's own shutdown. A caller's shorter deadline is still honored.
- State change delivery is **push-as-a-hint**: DBus `PropertiesChanged` merely wakes the per-unit
  `WakeChannel` (buffered by 1, so wakes coalesce), and the consumer re-reads state itself. The
  signal is documented lossy in go-systemd, hence the 30s safety-net ticker in `task.Handler`.
  Do not turn the wake into a carrier of state data.
- `systemd.go` holds the `Manager` itself: its lifetime, the connection, and the reconnect loop.
  `unit.go` holds everything unit-shaped — the systemd→Nomad translation (`UnitState` predicates,
  `ToTaskState`, `ExitResultFromStatus`), registration and wake channels, and the unit operations.
  `cgroup.go` and `journal.go` hold the two things read outside DBus: cgroup v2 accounting and the
  journal reader. `conn.go` is the `dbusConn` interface over `*dbus.Conn` that makes the Manager
  testable without a bus (see `fake_conn_test.go`).

**`pkg/task/`** — one `Handler` per Nomad task, held in a `Store` keyed by task ID. It watches one
unit, maps its systemd state to the task state Nomad expects, resolves the exit result once the
unit stops, and copies the journal to stdout/stderr. It depends on the `unitController` interface,
not on `*systemd.Manager` directly. `task.go` holds the `Handler` and its API, `state.go` the state
machine that watches the unit, `logs.go` the journal-to-FIFO copying.

**`pkg/logx/`** — typed-attribute wrapper over hclog, replacing alternating key/value pairs.
Every attribute key is named by a constructor in `pkg/logx/semconv/` (flat dot-separated
OpenTelemetry-style keys), so a rename is a one-file change. `semconv` must not import the plugin's
own packages — it takes interfaces or strings to avoid an import cycle.

## Conventions

- **Comments state the contract, not the rationale.** Doc comments describe what a caller can rely
  on; design rationale belongs in separate technical docs, not in function bodies. The exception,
  used consistently, is a comment on a struct field or invariant explaining *why* the
  synchronization is shaped that way — keep those.
- **Package docs live in `doc.go`**, including for `main`.
- **Declaration order within a file** follows: `const` → `type` → `var` → constructors → exported
  methods → exported functions → unexported methods → unexported helpers. Public API comes before
  implementation; a type's own constants stay next to the type, and helpers stay near their caller.
- **A change to `configSpec` or `taskConfigSpec` in `plugin/about.go` is not finished until the
  examples match it.** `example/agent.hcl` mirrors the plugin block (driver-level options) and
  `example/example.nomad` the task block; the README documents both. Adding, renaming, removing or
  changing the meaning of an option means updating those in the same change — a config surface
  nobody can copy from an example is effectively undocumented.
- Tests are table-driven with named cases and hand-written fakes (`fakeUnits`, `fakeConn`) whose
  unset function fields return a "not configured in this test" error rather than panicking. No
  mocking framework. ~300 cases across the tree; keep new tests in that style.
