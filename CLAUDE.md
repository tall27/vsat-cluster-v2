# CLAUDE.md

Guidance for Claude Code (claude.ai/code) when working in this repository.

## What this project is

`VSAT Cluster v2` is a single-binary Go **web app** that manages LXD containers on
one Linux host: add/remove containers (up to 4) and open an in-browser terminal
into each. It is the web successor to the terminal-only `lxd-vsat` CLI in the
sibling `../VSAT CLUSTER` project. It runs **on the same host** it manages and
drives `lxc` via local `os/exec` (optionally `sudo -n`), not over SSH.

Two host prerequisites come from the proven lab model and are encoded in
`scripts/bootstrap-host.sh`:
1. The `vsat-nested` LXD profile (privileged, nesting, kernel modules, `raw.lxc`
   apparmor/cgroup/proc-sys overrides) — required for stable nested k3s.
2. iptables SNAT from the `lxdbr0` CIDR to the host's primary IP, so containers
   reach the internet through the single public IP.
Each container also gets a `/dev/kmsg` `tmpfiles.d` workaround at add time
(`internal/lxdctl.Add`).

## Build, test, run

Go module `github.com/tall27/vsat-cluster-v2`, go 1.24. Entrypoint `cmd/vsat-webapp`.

```bash
go build ./cmd/vsat-webapp
go test ./...                                   # config, auth, lxdctl, httpserver
GOOS=linux GOARCH=amd64 go build -o out/vsat-webapp ./cmd/vsat-webapp

# run locally on the host
./vsat-webapp --addr 0.0.0.0:8443 --sudo --host <ip> --config-dir ./cfg
```

Key flags: `--addr`, `--host` (UI label), `--lxc-bin`, `--sudo`, `--config-dir`,
`--tls-cert`/`--tls-key` (else self-signed), `--http` (lab only), `--max-containers`,
`--prefix`.

## Architecture

`cmd/vsat-webapp/main.go` parses flags, builds a `lxdctl.Client` and an
`httpserver.Server`, and serves HTTPS (self-signed cert from `internal/selfsign`
unless `--tls-cert/--tls-key` are given).

Request flow is gated twice in `internal/httpserver/server.go`:
- `withConfigGate` — before setup, everything redirects to `/setup`; after, `/setup`
  redirects to `/`. Static assets and `/healthz` always pass.
- `protected` → `auth.SessionManager.RequireSession` — gates the dashboard,
  container actions and the terminal (including the WebSocket upgrade).

Packages:
- `internal/config` — AES-256-GCM-encrypted config blob (`config.enc`) with a 0600
  key file (`config.key`); holds the bcrypt password hash, the random session
  secret, prefix and max. `ErrNotConfigured` drives the first-run `/setup` flow.
- `internal/auth` — `HashPassword`/`VerifyPassword` (bcrypt) and a `SessionManager`
  issuing HMAC-SHA256-signed, expiring cookies.
- `internal/lxdctl` — `lxc` wrapper. `List` parses `lxc list --format json`; `Add`
  enforces the cap + duplicate check, launches `ubuntu:24.04 -p vsat-nested`, then
  applies the `/dev/kmsg` fix **with retry** (`kmsgRetryAttempts`/`kmsgRetryDelay`,
  ~6 attempts / 2 s) — `lxc launch` returns before the container's init can be
  `exec`'d into, and a one-shot attempt left two live containers created but
  missing the fix (see `docs/test-report.md`); `Remove` is `lxc delete --force`;
  `ShellArgs` builds the exec argv. Command execution is behind the `CommandRunner`
  interface — **mock it in tests** (see `lxdctl_test.go`), mirroring how the CLI
  mocked SSH/HTTP.
- `internal/webterm` — `Handler.ServeWS` upgrades the WebSocket (`gorilla/websocket`)
  and bridges it to a PTY. The PTY is platform-split: `pty_linux.go` (real, via
  `creack/pty`) and `pty_other.go` (stub) so `go build` works on dev machines while
  real terminals only run on Linux. Wire protocol: binary frames = raw stdin/stdout,
  text frames = JSON control (`{"type":"resize",...}`). The PTY command's env is
  forced to `TERM=xterm-256color` in `internal/httpserver/server.go` — without it
  `lxc exec` forwards `TERM=unknown` (no terminfo entry) and cursor-addressed TUIs
  like `vsatctl preflight`/`install` fail (see `docs/test-report.md`).
- `internal/httpserver` — `templates.go` parses one `html/template` set per page
  (layout + partials + page) plus a standalone `containers` fragment for htmx polling.
- `web/` — `embed.FS` for `templates/` and `static/`. Front-end libs (htmx, xterm.js,
  addon-fit) are vendored under `web/static/` so the binary is self-contained.

## Conventions

- **Reuse the host patterns from the sibling project** (`../VSAT CLUSTER`): the
  `vsat-nested` profile, NAT rules and container launch/kmsg steps were ported from
  `scripts/bootstrap-onebox.sh` and `standalone-clean-min/internal/vsat/add.go`.
  Prefer pure-vendor LXD/Go features over new dependencies.
- **Test by mocking the boundary**, not the network — inject `CommandRunner`,
  `config.Store` (temp dir), and drive handlers with `httptest`.
- The app talks to LXD **locally**; do not reintroduce SSH for steady-state ops.
  SSH belongs only to a future one-time remote-bootstrap helper.

## CI / deployment

- `scripts/bootstrap-host.sh` — idempotent host prep (LXD, profile, NAT, autostart).
- `scripts/install.sh` + `scripts/vsat-webapp.service` — install binary + systemd unit.
- No GitHub Actions workflow yet; `go test ./...` and a linux/amd64 cross-build are
  the gate.

## Backlog (not built yet)

- **Route 53 DNS sync** — per-container `A` records + boot-time public-IP refresh
  (host has no EIP). Would add `internal/dns` over `aws-sdk-go-v2` using the EC2
  instance role. See `docs/architecture.md`.
- Start/Stop container actions; a Go-native remote bootstrap (currently a shell script).
