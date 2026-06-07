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
  limits.memory=3GiB`, then apply the `/dev/kmsg` `tmpfiles.d` workaround inside.
  `lxc launch` returns once the container *starts* booting, not once its init is
  ready for `lxc exec`, so the kmsg step retries (`kmsgRetryAttempts` /
  `kmsgRetryDelay`, ~6 attempts / 2 s apart) rather than risking a container that
  exists but never got the fix — see [the live finding](test-report.md#follow-up-findings-from-real-vsatellite-installs).
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

## Host prerequisites (`scripts/bootstrap-host.sh`)

Ported from the sibling project's `bootstrap-onebox.sh`:
- Install/init LXD; create the **`vsat-nested`** profile (`security.nesting`,
  `security.privileged`, kernel modules, `raw.lxc` apparmor=unconfined / no cap drop
  / full cgroup devices / rw proc+sys).
- `net.ipv4.ip_forward=1`, `FORWARD` accepts for `lxdbr0`, and a **SNAT** from the
  `lxdbr0` CIDR to the host's primary IP — the one-public-IP egress path.
- Set `boot.autostart=true` on existing containers.

## Backlog

- **Route 53 DNS sync** — the host has no EIP, so its public IP can change on reboot.
  A future `internal/dns` (over `aws-sdk-go-v2`, using the EC2 instance role) would
  upsert/delete `<name>.mimlab.io` A records on add/remove and refresh them at boot
  from instance metadata. Deliberately excluded from this first drop.
- Start/Stop actions; a Go-native remote installer to replace the shell scripts.
