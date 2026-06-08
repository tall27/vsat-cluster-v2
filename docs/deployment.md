# Deployment

The app runs on the Linux host it manages. Target: Ubuntu 24.04/26.04 with LXD.

## Quick start (recommended)

On a fresh host, as root:

```bash
curl -fsSL https://raw.githubusercontent.com/tall27/vsat-cluster-v2/main/scripts/quickstart.sh | sudo bash
```

This downloads the latest [GitHub release](https://github.com/tall27/vsat-cluster-v2/releases)
binary, runs `bootstrap-host.sh` (LXD, `vsat-nested` profile, NAT) and `install.sh`
(binary + systemd unit) in one shot, and prints the URL to finish setup at. It's
idempotent — re-run it to pick up a newer release. To pin the SNAT source IP, pass
it through: `... | sudo bash -s -- 10.0.2.115`.

The sections below describe what `quickstart.sh` does step by step — useful if you
want to run the steps individually, build from source, or debug a failure.

## 1. Build a release binary

On any machine with Go 1.24+:

```bash
GOOS=linux GOARCH=amd64 go build -o out/vsat-webapp ./cmd/vsat-webapp
```

Copy `out/vsat-webapp` and the `scripts/` folder to the host.

## 2. Prepare the host (once)

```bash
sudo ./scripts/bootstrap-host.sh            # auto-detects the primary IP for SNAT
# or pin the SNAT source IP:
sudo ./scripts/bootstrap-host.sh 10.0.2.115
```

This installs/initialises LXD, creates the `vsat-nested` profile, enables IP
forwarding + the SNAT rule for `lxdbr0`, and sets autostart on existing containers.
It is idempotent.

> The iptables rules are not persisted across reboot by this script. For a permanent
> setup, install `iptables-persistent` or re-run the bootstrap from a boot unit.

## 3. Install the service

```bash
sudo ./scripts/install.sh ./vsat-webapp
```

Installs the binary to `/usr/local/bin/vsat-webapp`, the config dir to
`/etc/vsat-cluster` (0700), and a systemd unit serving HTTPS on `:443`. The unit
runs as root (to bind 443 and drive `lxc`); switch to a non-root `User=` in the
`lxd` group plus `--sudo` if you prefer least privilege.

Check it:

```bash
systemctl status vsat-webapp
journalctl -u vsat-webapp -f
```

## 4. First run

Browse to `https://<host>/`, accept the self-signed certificate, and set the admin
password on the `/setup` page. After that, `/login` gates everything.

To use a real certificate instead of self-signed, pass `--tls-cert`/`--tls-key`
(edit `ExecStart` in the unit) — e.g. a Let's Encrypt pair.

## Viewing monitoring (CPU / memory / disk I/O / network)

Once logged in:

1. Click **Monitor** on the dashboard.
2. Or browse straight to `https://<host>/monitoring`
   (e.g. `https://18.218.238.174:8443/monitoring`).

The page shows a single live table — host plus every VSatellite, one row each —
with CPU, memory, disk I/O and network utilization bars (plus the underlying
byte rates), refreshed every 5 seconds, in the spirit of AWS CloudWatch's
per-instance "Monitoring" tab. A freshly-created container shows `--` for about
10 seconds (two poll cycles are needed to derive a rate from LXD's cumulative
counters) before its row populates. No extra setup is required — it reads
straight from the host's `/proc` counters and LXD's own `lxc query /1.0/metrics`
endpoint. See [docs/architecture.md](architecture.md#monitoring) for how it's
collected and why it's safe to leave running continuously (negligible CPU/RAM
cost, confirmed live).

## Running manually (no systemd)

```bash
# non-root user in the sudo/lxd group, lxc via sudo -n, HTTPS on 8443
./vsat-webapp --addr 0.0.0.0:8443 --sudo --host <public-ip> --config-dir ./cfg
```

## Troubleshooting

- **`vsatctl preflight`/`install` fails with "terminal not cursor addressable"** —
  fixed in `internal/httpserver/server.go`, which now forces `TERM=xterm-256color`
  on the web terminal's `lxc exec` process. Make sure you're running a build that
  includes this fix (anything built after 2026-06-07).
- **`vsatctl install` fails installing k3s (`sh: [: Illegal number:`)** — the
  container is missing the `/dev/kmsg` workaround. `internal/lxdctl.Add` now applies
  and retries this automatically for every container the app creates; if a container
  was created some other way, apply it by hand and restart the container:
  ```bash
  sudo lxc exec <name> -- bash -lc \
    "printf 'L /dev/kmsg - - - - /dev/console\n' > /etc/tmpfiles.d/kmsg.conf && \
     systemd-tmpfiles --create /etc/tmpfiles.d/kmsg.conf"
  sudo lxc restart <name>
  ```
  See [docs/test-report.md](test-report.md#follow-up-findings-from-real-vsatellite-installs)
  for the full investigation.

## Reaching the UI

If the host is in AWS, open the listening port (443 or 8443) in the **security
group**. Without that, tunnel over SSH:

```bash
ssh -L 8443:127.0.0.1:8443 ubuntu@<host>
# then browse https://localhost:8443/
```

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `--addr` | `:8443` | listen address |
| `--host` | hostname | label shown in the UI / cert SAN |
| `--lxc-bin` | `lxc` | path to the lxc binary |
| `--sudo` | false | run `lxc` via `sudo -n` |
| `--config-dir` | OS config dir | where `config.enc`/`config.key` live |
| `--tls-cert` / `--tls-key` | — | real TLS keypair (else self-signed) |
| `--http` | false | plain HTTP (throwaway lab only) |
| `--max-containers` | 4 | container cap |
| `--prefix` | `vsat` | required container-name prefix |
