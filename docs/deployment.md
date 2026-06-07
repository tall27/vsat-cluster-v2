# Deployment

The app runs on the Linux host it manages. Target: Ubuntu 24.04/26.04 with LXD.

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
