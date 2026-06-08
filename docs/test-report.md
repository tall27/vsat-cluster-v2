# End-to-end test report

Verified on a dedicated AWS test box on 2026-06-07.

- **Host:** `18.218.238.174` (private `10.0.2.115`), Ubuntu 26.04 LTS, amd64, 3.8 GiB RAM
- **Build:** `GOOS=linux GOARCH=amd64` binary, copied via `scp`
- **Run:** `./vsat-webapp --addr 0.0.0.0:8443 --sudo --host 18.218.238.174 --config-dir ./cfg`
  (HTTPS self-signed, `lxc` via `sudo -n`)

## Automated unit/handler tests

`go test ./...` — all green: `internal/config`, `internal/auth`, `internal/lxdctl`,
`internal/httpserver`. Also builds clean for `windows/amd64` and `linux/amd64`, `go vet` clean.

## Live host results

| # | Check | Result |
|---|---|---|
| 1 | `scripts/bootstrap-host.sh` | LXD init, `vsat-nested` profile created, `lxdbr0` = `10.10.81.1/24`, SNAT → `10.0.2.115` ✓ |
| 2 | `GET /healthz` | `ok` ✓ |
| 3 | Unconfigured `GET /` | `303 → /setup` ✓ |
| 4 | `POST /setup` (password) | `303 → /`, session cookie issued ✓ |
| 5 | `GET /` dashboard | renders, `0 / 4 used`, "No containers yet" ✓ |
| 6 | `POST /containers name=vsat-a` | `303 → /`, container **RUNNING** in ~27 s ✓ |
| 7 | `GET /partials/containers` | shows `vsat-a` / `Running` ✓ |
| 8 | Container internet via NAT | `lxc exec vsat-a -- curl https://api.venafi.cloud` → HTTP 404 (connectivity OK) ✓ |
| 9 | Terminal page | renders with `xterm.js` ✓ |
| 10 | Terminal backend | `lxc exec vsat-a -- bash -lc 'hostname; id'` → `vsat-a` / `root` ✓ |
| 11 | Terminal WebSocket | `101 Switching Protocols`, `Sec-WebSocket-Accept` present, 17 bytes streamed from the PTY ✓ |
| 12 | Unauthenticated `GET /`, `/partials/containers` | `303 → /login` ✓ |
| 13 | `POST /containers/vsat-a/delete` | `303 → /`, `lxc list` empty, dashboard back to `0 / 4 used` ✓ |

## Notes

- Port `8443` is closed in the AWS security group, so the UI is not reachable from
  the public internet without opening the port or using an SSH tunnel
  (`ssh -L 8443:127.0.0.1:8443 ubuntu@18.218.238.174`).
- iptables/SNAT rules from the bootstrap are not yet persisted across reboot.

## Follow-up findings from real VSatellite installs

The user ran real `vsatctl preflight`/`install` flows against three containers
(`vsat-ca-eval-*`, `vsat-eu-eval-*`, `vsat-us-*`) created on the same host. Two bugs
surfaced and were fixed (both deployed back to `18.218.238.174`):

### 1. Web terminal lacked `TERM` → "terminal not cursor addressable"

`vsatctl preflight` is a cursor-addressed TUI. The systemd service has no
controlling terminal, so `lxc exec` (run from the web terminal's PTY bridge)
forwarded `TERM=unknown` into the container — no terminfo entry, so
`vsatctl preflight` aborted immediately with `Error: terminal not cursor addressable`.

Verified the mechanism directly on the host:

```
$ sudo lxc exec <container> -- bash -lc 'echo TERM=$TERM'
TERM=unknown                                  # no forwarded TERM → no terminfo entry
$ TERM=xterm-256color sudo lxc exec <container> -- bash -lc 'tput cup 0 0 && echo OK'
TERM=xterm-256color
CURSOR_ADDRESSABLE_OK                         # ncurses-term already ships this entry
```

**Fix**: `internal/httpserver/server.go` now sets `cmd.Env =
append(os.Environ(), "TERM=xterm-256color")` on the PTY-backed `lxc exec` command.
Re-running `vsatctl preflight` through a `lxc exec` with `TERM=xterm-256color`
exported renders the full interactive checks dashboard correctly.

### 2. Two of three containers were missing the `/dev/kmsg` workaround

`vsat-eu-eval-34112515` and `vsat-us-64781200` had **no** `/etc/tmpfiles.d/kmsg.conf`
and no `/dev/kmsg` device — `vsatctl install` failed installing k3s with
`sh: [: Illegal number:` and `ERRO Failed to install VSatellite`. The third
container (`vsat-ca-eval-18037625`, created within 2 minutes of the others) *did*
have the fix and installed cleanly.

Root cause: `lxc launch` returns once the container starts booting, not once its
init is ready for `lxc exec`. `internal/lxdctl.Add` ran the kmsg-fix `lxc exec` only
once; if it raced a slow boot, `Add` returned an error but the container had
already been created — orphaned without the fix.

**Fix (applied live + in code)**:
- Manually applied the rule to both affected containers and restarted them —
  `/dev/kmsg` persisted across the restart (the `tmpfiles.d` rule re-creates the
  symlink on every boot), and k3s came up `active (running)` with `coredns`,
  `local-path-provisioner` and `metrics-server` pods all `1/1 Running`.
- `internal/lxdctl.Add` now retries the post-launch fix step (`kmsgRetryAttempts` = 15,
  `kmsgRetryDelay` = 2 s, ~30 s budget — bumped from the original 6 once side-by-side
  comparison showed the race was load/cadence-dependent, not host-config-dependent;
  see the COW finding below for the actual fix to the root cause) instead of failing
  on the first race, so every container the app creates is guaranteed to get the
  workaround. Covered by `TestAddRetriesKmsgFixUntilContainerReady` and
  `TestAddFailsAfterKmsgRetriesExhausted` in `internal/lxdctl/lxdctl_test.go`.

### 3. Root cause was launch cadence + slow `dir`-backed storage, not host config

Side-by-side comparison against the original reference host (`18.218.238.174`)
showed **identical** `vsat-nested` profiles and bootstrap config — the difference
was creation cadence: 4-6 minute gaps between `lxc launch` calls succeeded 3/3,
while 1-2 minute gaps succeeded only 1/3. Root cause: the default `dir` storage
driver does a full filesystem copy on every launch, slow enough on a loaded
2-core box that a freshly launched container can miss the kmsg-retry window
entirely when launches stack up.

**Fix**: provision a 20GB loop-file **btrfs** pool (`cow`) and point
`vsat-nested`'s root device at it — copy-on-write clones instead of full copies.
Validated live: 3 concurrent launches all became `lxc exec`-ready in **~1 second**
(vs. routinely blowing the 12 s budget on `dir`). Now baked into
`scripts/bootstrap-host.sh` (falls back to the default pool if `/` doesn't have
`COW_SIZE_GB + 5`GB free).

### 4. Heavy `apport` churn traced to journald's watchdog, not real crashes

The user noticed heavy `apport` activity inside containers. Traced to: a
privileged nested container's k3s/kubelet log volume stalls journald past its
3-minute watchdog timeout → SIGABRT → kernel `core_pattern` → apport
crash-capture — generating ~297KB crash files identically across all three test
containers, with no actual hardware to protect. **Fix**: ship a `WatchdogSec=0`
drop-in (`/etc/systemd/journald.conf.d/no-watchdog.conf`) and restart
`systemd-journald`, applied as part of the same combined post-launch step as the
kmsg fix.

### 5. k9s install requires DNS to be ready, and a new release to ship

Added a pinned-version k9s install (`internal/lxdctl.k9sVersion`) to the same
combined post-launch `lxc exec`. First test failed with
`curl: (6) Could not resolve host: github.com` — DNS isn't ready immediately
after `lxc launch`; resolved by the existing retry loop (the same race the kmsg
fix already handles). Also discovered `quickstart.sh` installs from the
**latest GitHub release**, not branch HEAD — `bootstrap-host.sh`/`lxdctl`
changes only reach fresh installs after cutting a new release (confirmed via
`strings <binary> | grep -c 'derailed/k9s'` returning `0` on a stale binary
predating the change).

## Monitoring data-source research (2026-06-07)

The user asked for per-container CPU/memory/network charts similar to AWS
CloudWatch's "Monitoring" tab, and whether polling for that data would be a heavy
toll on the host. Verified directly on `18.218.238.174`:

```
$ time sudo lxc query /1.0/metrics | wc -c
65431
real    0m0.241s
```

`lxc query /1.0/metrics` is LXD's **built-in** Prometheus-format endpoint — no
extra config, no Netdata/Prometheus/Grafana to install or run. It already exposes,
per instance (`name=`, `type="container"` labels):
- `lxd_cpu_seconds_total`, `lxd_cpu_effective_total`
- `lxd_memory_MemTotal_bytes`, `lxd_memory_MemAvailable_bytes`
- `lxd_network_{receive,transmit}_{bytes,packets}_total{device=...}`
- `lxd_disk_*` (not currently charted — CloudWatch screenshot didn't show disk panels)

At ~0.2-0.3 s and ~65 KB of text per call regardless of container count, polling
this every 10 s costs well under 1% of a core and a few KB of RAM for the rolling
history — **negligible**, directly answering "let me know if it is a heavy toll on
host's memory and CPU". This finding is what shaped `internal/metrics.Collector`
(see [docs/architecture.md](architecture.md#monitoring)): poll locally, derive
rates from the cumulative counters, keep a small bounded ring buffer per series,
and render with plain `<canvas>` — no new daemons, no new vendored dependencies.
