# VSAT Cluster v2

A small, single-binary web app to manage **LXD containers** ("VSatellites") on one
Linux host: add and remove containers, and open a real **in-browser terminal** into
each one. Protected by a single static password and served over HTTPS.

It is the web successor to the terminal-only `lxd-vsat` CLI in the sibling
`VSAT CLUSTER` project, reusing the proven `vsat-nested` LXD profile and the
SNAT/iptables networking pattern that lets nested containers reach the internet
through the host's single public IP.

## What it does

- **Dashboard** — lists up to 4 containers with status and internal IP; auto-refreshes.
- **Add / Remove** — launch a container with the `vsat-nested` profile (enforces the
  4-container cap and the `/dev/kmsg` workaround) or force-delete one.
- **Web terminal** — a full `xterm.js` terminal streamed over a WebSocket to a host
  PTY running `lxc exec <name> -- bash`. No Guacamole, ttyd or gotty.
- **Monitoring** — a single `/monitoring` table shows host and every VSatellite
  side by side (CPU, memory, disk I/O, network), refreshed every 5 s, in the spirit
  of AWS CloudWatch's per-instance "Monitoring" tab. Polls LXD's own
  `lxc query /1.0/metrics` Prometheus endpoint plus host `/proc` — no
  Netdata/Prometheus/Grafana — and renders with plain HTML/CSS bars, so nothing new
  is vendored. See [docs/architecture.md](docs/architecture.md#monitoring).
- **Auth** — one static password (bcrypt-hashed), HMAC-signed session cookie, HTTPS.

> **Out of scope for this drop:** Route 53 DNS sync. See
> [docs/architecture.md](docs/architecture.md#backlog).

## Architecture at a glance

```
browser ──HTTPS──> vsat-webapp (Go) ──local exec──> lxc / lxc exec ──> LXD containers
   xterm.js  ◄─WebSocket─► PTY ◄──────────────────► bash in container
```

| Package | Responsibility |
|---|---|
| `cmd/vsat-webapp` | entrypoint, flags, TLS (self-signed by default) |
| `internal/config` | AES-GCM-encrypted config (password hash, session secret) |
| `internal/auth` | bcrypt + signed-cookie sessions + middleware |
| `internal/lxdctl` | `lxc` add/remove/list with the 4-container cap |
| `internal/webterm` | PTY ⇄ WebSocket bridge (Linux PTY, stub elsewhere) |
| `internal/metrics` | polls `lxc query /1.0/metrics`, derives per-container CPU/memory/network rates |
| `internal/httpserver` | routing, config/auth gates, handlers |
| `internal/selfsign` | in-memory self-signed TLS certificate |
| `web/` | embedded templates + static assets (htmx, xterm.js) |

## Build

Go 1.24+.

```bash
go build ./cmd/vsat-webapp           # host build
go test ./...                        # unit + handler tests
GOOS=linux GOARCH=amd64 go build -o out/vsat-webapp ./cmd/vsat-webapp   # release
```

The browser libraries (`htmx`, `xterm.js`) are vendored under `web/static/` and
embedded into the binary, so the single artifact is fully self-contained.

## Deploy (on the Linux host)

For AWS-side operations on this project, use the Venafi SE Basic Access profile:

```bash
aws <service> <operation> --profile Venafi-SE-Basic-Access-427380916706
```

Recommended AWS deployment is the nested CloudFormation package under
[`cloudformation/`](cloudformation/README.md). It follows the `onebox.v2` stack
structure, publishes templates to `s3://akush/vsat-cluster/`, creates an 8 GiB
root volume plus a separate raw gp3 EBS volume, and runs quickstart from UserData:

```bash
curl -fsSL https://raw.githubusercontent.com/tall27/vsat-cluster-v2/master/scripts/quickstart.sh | sudo VSAT_COW_DEVICE=/dev/nvme1n1 bash
```

Manual host prep also requires a dedicated unformatted block device for the LXD
`cow` btrfs pool. The app intentionally avoids LXD's slow `dir` storage for new
VSatellite launches.

```bash
# 1. Prepare a fresh host: installs LXD, creates cow on the raw disk,
#    configures the vsat-nested profile and NAT.
sudo VSAT_COW_DEVICE=/dev/nvme1n1 ./scripts/bootstrap-host.sh

# 2. Install the binary + systemd unit (serves :443, config in /etc/vsat-cluster).
sudo ./scripts/install.sh ./vsat-webapp
```

Then browse to `https://<host>/`, set the admin password on first run, and start
adding containers. See [docs/deployment.md](docs/deployment.md) for details,
including running as a non-root user with `--sudo`.

### Quick manual run (no systemd)

```bash
./vsat-webapp --addr 0.0.0.0:8443 --sudo --host <public-ip> --config-dir ./cfg
```

`--sudo` runs `lxc` via `sudo -n` so the app can stay a non-root user in the
`lxd`/sudo group. Drop `--http` in for plain HTTP in a throwaway lab only.

## Status

Verified end-to-end on an Ubuntu 26.04 AWS host: setup → add container (RUNNING) →
container reached the internet via NAT → web terminal (WebSocket + live PTY) →
remove. See [docs/test-report.md](docs/test-report.md), including two fixes found
during live VSatellite installs and folded back into the code:

- **Web terminal `TERM`** — `internal/httpserver` now exports `TERM=xterm-256color`
  to the `lxc exec` PTY command, so cursor-addressed TUIs (`vsatctl preflight`/`install`)
  render correctly instead of failing with "terminal not cursor addressable".
- **`/dev/kmsg` retry** — `internal/lxdctl.Add` now retries the `/dev/kmsg`
  workaround for a few seconds, because `lxc launch` returns before the container's
  init is ready for `lxc exec`, and a one-shot attempt could leave a container
  created but missing the fix it needs for stable nested k3s.
