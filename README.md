Nomad Systemd Driver Plugin
==========

A [Nomad task driver](https://developer.hashicorp.com/nomad/docs/drivers) that manages **existing**
systemd units on a Linux host.

The driver does not run or supervise processes itself. A task names a unit already installed on the
host; the driver starts it, tracks its state over DBus, streams its journal into the task's
stdout/stderr, and stops it when the allocation goes away.

## Security model

Read this before enabling the driver on a shared cluster.

Units live outside the allocation, so **the driver provides no isolation of any kind** — no
filesystem or network isolation, no signals, no `nomad alloc exec`. Any unit on the host is
reachable by name. Two rules limit the damage:

- A unit may be managed by at most one task at a time.
- `allowed_units` / `denied_units` restrict which units are eligible at all.

**Without `allowed_units` or `denied_units`, any job submitter can take over arbitrary host units,
including `nomad.service` itself.** Configure them.

## Requirements

- Linux with systemd, on the unified (v2) cgroup hierarchy — units start and stop on cgroup v1 or
  hybrid hosts, but resource statistics read as zeros. The driver's fingerprint reports which
  hierarchy it found.
- Nomad v1.10+
- Go 1.24+ and systemd development headers to build (`libsystemd-dev` on Debian/Ubuntu,
  `systemd-devel` on RHEL/Fedora). cgo is required.

## Building

On Linux:

```bash
make build
```

On macOS or Windows, build in a container — the plugin cannot be compiled natively:

```bash
make docker-build
```

The repository also ships a dev container (`.devcontainer/`) with Go, systemd and the headers
already in place.

## Installation

Copy the binary into Nomad's plugin directory:

```bash
sudo cp bin/nomad-driver-systemd /opt/nomad/plugins/
```

Configure the Nomad client. The plugin block is named after the binary:

```hcl
# /etc/nomad.d/systemd.hcl
plugin "nomad-driver-systemd" {
  config {
    allowed_units = ["^app-.*\\.service$"]
    denied_units  = ["^nomad\\.service$", "^sshd?\\.service$"]
  }
}
```

Restart Nomad and confirm the driver is detected:

```bash
nomad node status -self
```

### Driver configuration

| Option          | Type           | Description                                                                                                                           |
|-----------------|----------------|---------------------------------------------------------------------------------------------------------------------------------------|
| `allowed_units` | `list(string)` | Regexes. If non-empty, a task's unit must match at least one (allowlist mode).                                                         |
| `denied_units`  | `list(string)` | Regexes. A unit matching any of them is always rejected, even if it also matches `allowed_units`.                                      |
| `pprof_addr`    | `string`       | Address for a debug pprof server, e.g. `127.0.0.1:6061`. Empty disables it. Keep it on loopback — the profiles expose process memory.   |

## Job specification

```hcl
job "systemd-example" {
  group "g" {
    task "unit-task" {
      driver = "systemd"

      config {
        unit = "app-api.service"
      }
    }
  }
}
```

### Task configuration

| Option | Type     | Required | Description                                                                                                                             |
|--------|----------|----------|-----------------------------------------------------------------------------------------------------------------------------------------|
| `unit` | `string` | yes      | Name of an existing systemd unit, e.g. `nginx.service`. Must be a valid unit name of at most 255 characters ending in a known unit type. |

The unit must already be installed on the host; the driver never writes unit files. Task state
follows the unit's own state, and the task's exit result is derived from the unit's exit status, so
the unit's `Restart=` and `TimeoutStopSec=` settings govern its behaviour.

Working examples are in [`example/`](example/).

## Development

Tests need Linux and libsystemd, same as the build:

```bash
make test
```

From macOS, run them in a container:

```bash
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.24-bookworm sh -c "apt-get update -qq && apt-get install -y -qq libsystemd-dev && go test -race ./..."
```

Linting and formatting work anywhere, via golangci-lint:

```bash
make lint
```

```bash
make fmt
```

### Trying it out

Install a throwaway unit, then run a Nomad dev agent against the built plugin and submit the
example job:

```bash
sudo nomad agent -dev -config=example/agent.hcl -plugin-dir=$(pwd)/bin
```

```bash
nomad job run example/example.nomad
```

## License

Mozilla Public License 2.0 — see [LICENSE](LICENSE).
