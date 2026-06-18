# Architecture

## Goal

Give an operator a browser to manage up to four LXD "VSatellite" containers on a
single Linux host — add, remove, and get a real shell into each — behind one static
password, over HTTPS. The app runs **on the host it manages**.

## Component diagram

```
                         ┌──────────────────────── vsat-webapp (single Go binary) ───────────────────────┐
   browser               │                                                                               │
  ┌────────┐  HTTPS      │  httpserver ── withConfigGate ── protected(session) ── handlers               │
  │  htmx  │◄──────────► │      │                                            ├── lxdctl ─ os/exec ─ lxc ──┼──► LXD
  │xterm.js│  WebSocket  │      │                                            └── webterm ─ PTY ─ lxc exec │     containers
  └────────┘◄══════════► │   templates (embed)        config (AES-GCM)   auth (bcrypt + HMAC cookie)      │
                         └───────────────────────────────────────────────────────────────────────────────┘
```

## Request lifecycle

1. **TLS** terminates in the binary. With `--tls-cert/--tls-key` it loads a real
   keypair; otherwise `internal/selfsign` mints an in-memory ECDSA self-signed cert
   covering `localhost`, `127.0.0.1` and the `--host` value.
2. **`withConfigGate`** (`internal/httpserver/server.go`): if no config exists, all
   paths but `/setup` redirect to `/setup`; once configured, `/setup` → `/`.
   `/static/*` and `/healthz` always pass.
3. **`protected`** wraps the dashboard, container actions and terminal with
   `auth.SessionManager.RequireSession`. GET requests without a session redirect to
   `/login`; the WebSocket/non-GET requests get `401`.
4. **Handlers** (`internal/httpserver/handlers.go`) render `html/template` pages or
   call `lxdctl`/`webterm`.

## Data & secrets

- `internal/config` writes `config.enc` (AES-256-GCM) + `config.key` (32 random
  bytes, 0600). The blob holds the **bcrypt** password hash, a random 32-byte
  **session secret**, the name prefix and the container cap. Copying the blob alone
  reveals nothing without the sibling key — lab-grade protection, by design.
- Sessions are stateless: the cookie is `base64url(expiryUnix ‖ HMAC-SHA256(expiry))`
  signed with the session secret; `verify` checks the MAC in constant time and the
  expiry. No server-side session store.

## Container lifecycle (`internal/lxdctl`)

- **List** → `lxc list --format json`, parsed to `{Name, Status, IPv4}` (global,
  non-link-local IPv4 from `eth0`/any non-`lo` interface).
- **Add** → validate name (must be `vsat-…`, LXD-legal), refuse duplicates, enforce
  the cap, then `lxc launch ubuntu:24.04 <name> -p vsat-nested -c limits.cpu=2 -c
  limits.memory=3GiB`, then run **one** combined post-launch `lxc exec` inside:
  the `/dev/kmsg` `tmpfiles.d` workaround, disabling journald's watchdog
  (`WatchdogSec=0` — a privileged nested container's heavy k3s/kubelet log volume
  can stall journald past its 3-minute watchdog, SIGABRT it, and trigger an
  apport crash-capture loop with no real hardware to protect), and installing a
  pinned version of [k9s](https://github.com/derailed/k9s) to `/usr/local/bin`
  for in-container cluster inspection.
  `lxc launch` returns once the container *starts* booting, not once its init is
  ready for `lxc exec`, so this step retries as a whole (`kmsgRetryAttempts` /
  `kmsgRetryDelay`, 15 attempts / 2 s apart, ~30 s budget) rather than risking a
  container that exists but never got the fixes — see [the live finding](test-report.md#follow-up-findings-from-real-vsatellite-installs).
  `Add` is fully synchronous: the HTTP response to `POST /containers` only
  returns once the container is genuinely ready, which the dashboard's "Add
  VSAT" modal reflects by staying open with a spinning hourglass for the
  duration instead of closing immediately.
- **Remove** → `lxc delete --force <name>`.
- Execution is behind `CommandRunner`, so production uses the `lxc` CLI (optionally
  `sudo -n`) and tests inject a fake.

## Web terminal (`internal/webterm`)

`GET /vsat/{name}/terminal` serves an `xterm.js` page; `…/ws` upgrades to a
WebSocket and starts a host PTY running `lxc exec <name> -- bash -l`. Bytes flow
both ways as binary frames; the browser sends `{"type":"resize",cols,rows}` text
frames on open/resize. The PTY layer is build-tagged: real on Linux (`creack/pty`),
a stub elsewhere so the package still compiles on dev machines.

The command factory in `internal/httpserver/server.go` explicitly sets
`TERM=xterm-256color` in the `lxc exec` process's environment. The systemd service
has no controlling terminal, so without this `lxc exec` forwards `TERM=unknown` —
which has no terminfo entry — and cursor-addressed TUIs inside the container
(`vsatctl preflight`/`install`) fail with "terminal not cursor addressable". The
container images already ship `ncurses-term`, which provides the `xterm-256color`
entry, so forcing the variable is sufficient (no extra package needed). See
[the live finding](test-report.md#follow-up-findings-from-real-vsatellite-installs).

## Monitoring (`internal/metrics`)

A single unified utilization table — host plus every VSatellite, side by side — in
the spirit of the AWS CloudWatch per-instance "Monitoring" tab the user wants to
mirror, accessed via the "Monitor" link on the dashboard (`GET /monitoring`).

**Data sources — pure vendor functionality, no new agents.**
- *Host*: read directly from `/proc/stat`, `/proc/meminfo`, `/proc/diskstats` and
  `/proc/net/dev` — the same counters `top`/`free`/`iostat` use, no agent needed.
- *Containers*: LXD already exposes a Prometheus-format metrics endpoint locally,
  `lxc query /1.0/metrics`. It returns per-instance CPU (`lxd_cpu_seconds_total`,
  `lxd_cpu_effective_total`), memory (`lxd_memory_MemTotal_bytes`/
  `MemAvailable_bytes`), disk (`lxd_disk_{read,write}_bytes_total`) and network
  (`lxd_network_{receive,transmit}_bytes_total{device=...}`) series labelled with
  `name=` and `type="container"`, with no extra LXD config.

Measured live on the test box: **~0.2-0.3 s and ~65 KB of text per poll**,
independent of how many containers exist — negligible CPU/RAM overhead even at a
5 s poll interval (well under 1% of a core; a few KB of RAM for the row cache).
This directly answered the "is it a heavy toll on the host's memory and CPU"
question — it isn't.

**Collector (`internal/metrics.Collector`)** mirrors `internal/lxdctl`'s
`CommandRunner`/`execRunner` shell-out pattern (mockable via a `Runner` in
`Options`, exercised with a `fakeRunner` in `metrics_test.go`). Every 5 s it:
1. Reads the host's `/proc` counters and runs `lxc query /1.0/metrics` (via the
   same `lxc-bin`/`--sudo` the rest of the app uses), parsing the Prometheus text
   with a small regex-based scanner that keeps only `type="container"` series and
   skips loopback network devices so internal `veth`/`cni`/`flannel` interfaces
   created by nested k3s aren't double-counted.
2. Derives **rates** from consecutive cumulative-counter samples:
   `(cur - prev) / dt`, clamped to ≥ 0 so a counter reset on container restart
   reports `0` for that interval instead of a bogus negative spike. The previous
   sample is always cached after the first poll — even before a rate can be
   derived — so the *second* poll (not some later one) is the first to produce a
   row's numbers.
3. Builds one `UtilizationRow` per host/container — CPU/memory/disk-IO/network
   utilization plus the underlying byte rates — and snapshots them sorted host
   -first, then containers by name.

**Wiring.** `httpserver.New` builds the collector and starts `Run(ctx)` in its own
goroutine. `GET /monitoring` renders `web/templates/monitoring.html` (both routes
sit behind the same session-auth gate as the terminal); the page polls
`GET /monitoring/data` (JSON snapshot of all rows) every 5 s and renders each
metric as a small HTML/CSS bar with a numeric readout — no charting library
vendored, matching the "keep the binary self-contained, minimal footprint" approach
already used for the web terminal (`xterm.js` is the only vendored front-end
dependency of substance; table icons are vendored from Lucide as inline SVG).
A row needs two polls (~10 s) before a rate can be derived; until then its metric
cells show `--`.

## Host prerequisites (`scripts/bootstrap-host.sh`)

Ported from the sibling project's `bootstrap-onebox.sh` and updated for the
dedicated-disk CloudFormation path:
- Install/init LXD; provision a required **btrfs `cow` pool** on a dedicated,
  unformatted block device such as `/dev/nvme1n1`, and point the `vsat-nested`
  profile's root device at it instead of the default `dir`-backed pool. `dir`
  does a full filesystem copy on every `lxc launch` — slow enough that
  back-to-back launches can miss the kmsg-fix retry window (see
  [the live finding](test-report.md#follow-up-findings-from-real-vsatellite-installs)).
  `btrfs` gives near-instant copy-on-write clones; confirmed live, exec-ready in
  ~1 s vs. routinely blowing a 12 s budget on `dir`, even with three containers
  launched concurrently. Bootstrap must fail fast when no dedicated COW disk is
  available; do not fall back to `dir`.
- Create the **`vsat-nested`** profile (`security.nesting`, `security.privileged`,
  kernel modules, `raw.lxc` apparmor=unconfined / no cap drop / full cgroup
  devices / rw proc+sys).
- `net.ipv4.ip_forward=1`, `FORWARD` accepts for `lxdbr0`, and a **SNAT** from the
  `lxdbr0` CIDR to the host's primary IP — the one-public-IP egress path.
- Set `boot.autostart=true` on existing containers.

## Backlog

- **Route 53 DNS sync** — the host has no EIP, so its public IP can change on reboot.
  A future `internal/dns` (over `aws-sdk-go-v2`, using the EC2 instance role) would
  upsert/delete `<name>.mimlab.io` A records on add/remove and refresh them at boot
  from instance metadata. Deliberately excluded from this first drop.
- Start/Stop actions; a Go-native remote installer to replace the shell scripts.
