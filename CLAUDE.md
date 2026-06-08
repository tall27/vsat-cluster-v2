# CLAUDE.md

Guidance for Claude Code (claude.ai/code) in this repo.

## What this project is

`VSAT Cluster v2`: single-binary Go **web app** managing LXD containers on
one Linux host: add/remove containers (up to 4), in-browser terminal into each.
Web successor to terminal-only `lxd-vsat` CLI in sibling `../VSAT CLUSTER` project.
Runs **on the same host** it manages, drives `lxc` via local `os/exec` (optionally
`sudo -n`), not over SSH.

Host prereqs from proven lab model, encoded in `scripts/bootstrap-host.sh`:
1. `vsat-nested` LXD profile (privileged, nesting, kernel modules, `raw.lxc`
   apparmor/cgroup/proc-sys overrides) — required for stable nested k3s.
2. iptables SNAT from `lxdbr0` CIDR to host's primary IP, so containers
   reach internet through single public IP.
3. Loop-file **btrfs `cow` pool** (20GB, when `/` has room) wired into
   `vsat-nested` as `root` device — copy-on-write launches are exec-ready in
   ~1s vs. routinely blowing the kmsg-retry budget on the default `dir` driver
   (full-copy per launch). Falls back to the default pool if space is tight.

At add time (`internal/lxdctl.Add`, one retried `lxc exec`, see below) each
container also gets: `/dev/kmsg` `tmpfiles.d` workaround, journald watchdog
disabled (`WatchdogSec=0` — avoids SIGABRT/apport churn under heavy k3s log
volume), and k9s (pinned version) installed to `/usr/local/bin`.

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

`cmd/vsat-webapp/main.go` parses flags, builds `lxdctl.Client` and
`httpserver.Server`, serves HTTPS (self-signed cert from `internal/selfsign`
unless `--tls-cert/--tls-key` given).

Request flow gated twice in `internal/httpserver/server.go`:
- `withConfigGate` — pre-setup, all paths redirect to `/setup`; post-setup, `/setup`
  redirects to `/`. Static assets and `/healthz` always pass.
- `protected` → `auth.SessionManager.RequireSession` — gates dashboard,
  container actions, terminal (incl. WebSocket upgrade).

Packages:
- `internal/config` — AES-256-GCM-encrypted config blob (`config.enc`) with 0600
  key file (`config.key`); holds bcrypt password hash, random session
  secret, prefix, max. `ErrNotConfigured` drives first-run `/setup` flow.
- `internal/auth` — `HashPassword`/`VerifyPassword` (bcrypt), `SessionManager`
  issuing HMAC-SHA256-signed, expiring cookies.
- `internal/lxdctl` — `lxc` wrapper. `List` parses `lxc list --format json`; `Add`
  enforces cap + duplicate check, launches `ubuntu:24.04 -p vsat-nested`, then runs
  ONE combined post-launch `lxc exec` (kmsg `tmpfiles.d` fix + journald watchdog
  disable + pinned-version k9s install) **with retry**
  (`kmsgRetryAttempts`=15 / `kmsgRetryDelay`=2s, ~30s budget) — `lxc launch`
  returns before container's init can be `exec`'d into; a one-shot attempt left
  live containers missing the fixes (see `docs/test-report.md`); `Remove` is
  `lxc delete --force`; `ShellArgs` builds exec argv. Command execution behind
  `CommandRunner` interface — **mock it in tests** (see `lxdctl_test.go`),
  mirrors how CLI mocked SSH/HTTP.
- `internal/webterm` — `Handler.ServeWS` upgrades WebSocket (`gorilla/websocket`),
  bridges to PTY. PTY platform-split: `pty_linux.go` (real, via
  `creack/pty`) and `pty_other.go` (stub) so `go build` works on dev machines while
  real terminals only run on Linux. Wire protocol: binary frames = raw stdin/stdout,
  text frames = JSON control (`{"type":"resize",...}`). PTY command's env
  forced to `TERM=xterm-256color` in `internal/httpserver/server.go` — without it
  `lxc exec` forwards `TERM=unknown` (no terminfo entry), cursor-addressed TUIs
  like `vsatctl preflight`/`install` fail (see `docs/test-report.md`).
- `internal/metrics` — `Collector` reads host's `/proc` counters, polls LXD's
  built-in `lxc query /1.0/metrics` Prometheus endpoint every 5 s (mirrors `lxdctl`'s
  `CommandRunner`/`execRunner` shell-out + mock-in-tests pattern), parses host and
  per-container CPU/memory/disk-IO/network counters, derives rates
  (`(cur-prev)/dt`, clamped ≥ 0 across counter resets — raw sample always
  cached after *first* poll so *second* poll is first to produce
  numbers), snapshots one `UtilizationRow` per host/container (host first, then
  containers by name) for monitoring table. Confirmed live: ~0.2-0.3 s / ~65 KB
  per poll — negligible overhead, no extra agent needed ("pure vendor functionality
  first").
- `internal/httpserver` — `templates.go` parses one `html/template` set per page
  (layout + partials + page) plus standalone `containers` fragment for htmx polling.
  Serves `GET /monitoring` (single host+container utilization table, one row each)
  + `…/monitoring/data` (JSON snapshot of all rows, polled client-side every 5 s,
  rendered as plain HTML/CSS bars — no charting lib, icons vendored from Lucide as
  inline SVG) alongside terminal routes, all behind `protected`. Terminal links
  open in a new tab (`target="_blank"`); the "Add VSAT" modal stays open with a
  spinning Lucide hourglass through the synchronous `POST /containers` (which
  blocks until the container is fully provisioned — see `internal/lxdctl.Add`
  above) instead of closing immediately and leaving a stale page.
- `web/` — `embed.FS` for `templates/` and `static/`. Front-end libs (htmx, xterm.js,
  addon-fit) vendored under `web/static/` so binary is self-contained.

## Conventions

- **Reuse host patterns from sibling project** (`../VSAT CLUSTER`): `vsat-nested`
  profile, NAT rules, container launch/kmsg steps ported from
  `scripts/bootstrap-onebox.sh` and `standalone-clean-min/internal/vsat/add.go`.
  Prefer pure-vendor LXD/Go features over new dependencies.
- **Test by mocking the boundary**, not network — inject `CommandRunner`,
  `config.Store` (temp dir), drive handlers with `httptest`.
- App talks to LXD **locally**; don't reintroduce SSH for steady-state ops.
  SSH belongs only to future one-time remote-bootstrap helper.

## CI / deployment

- `scripts/bootstrap-host.sh` — idempotent host prep (LXD, COW pool, profile, NAT, autostart).
- `scripts/install.sh` + `scripts/vsat-webapp.service` — install binary + systemd unit.
- No GitHub Actions workflow yet; `go test ./...` and linux/amd64 cross-build are
  the gate.

## Backlog (not built yet)

- **Route 53 DNS sync** — per-container `A` records + boot-time public-IP refresh
  (host has no EIP). Would add `internal/dns` over `aws-sdk-go-v2` using EC2
  instance role. See `docs/architecture.md`.
- Start/Stop container actions; Go-native remote bootstrap (currently shell script).
