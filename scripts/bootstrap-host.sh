#!/usr/bin/env bash
# bootstrap-host.sh — prepare a fresh Ubuntu host to run VSAT Cluster v2.
#
# Installs/initialises LXD, creates the `vsat-nested` profile required for stable
# nested-k3s VSatellites, and adds iptables SNAT so containers reach the internet
# through the host's single primary IP. Idempotent: safe to re-run.
#
# Usage:  sudo ./bootstrap-host.sh [PRIMARY_IP]
#   PRIMARY_IP  source address for SNAT (default: auto-detected primary IPv4)
set -euo pipefail

log() { printf '[bootstrap] %s\n' "$*"; }

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (use sudo)" >&2
  exit 1
fi

PRIMARY_IP="${1:-$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')}"
if [ -z "${PRIMARY_IP}" ]; then
  echo "could not determine primary IP; pass it as the first argument" >&2
  exit 1
fi
log "primary IP for SNAT: ${PRIMARY_IP}"

# --- LXD ------------------------------------------------------------------
if ! snap list lxd >/dev/null 2>&1; then
  log "installing LXD"
  snap install lxd --channel=latest/stable
fi
snap wait system seed.loaded >/dev/null 2>&1 || true
systemctl start snap.lxd.daemon.service || true
lxd waitready

if [ -z "$(lxc storage list --format csv -c n 2>/dev/null | head -n1)" ]; then
  log "initialising LXD (auto)"
  lxd init --auto
fi

POOL="$(lxc profile device get default root pool 2>/dev/null || true)"
[ -z "${POOL}" ] && POOL="$(lxc storage list --format csv -c n | head -n1)"

# --- vsat-nested profile --------------------------------------------------
if ! lxc profile show vsat-nested >/dev/null 2>&1; then
  log "creating vsat-nested profile"
  lxc profile create vsat-nested
fi
[ -z "$(lxc profile device get vsat-nested root path 2>/dev/null || true)" ] && \
  lxc profile device add vsat-nested root disk path=/ pool="${POOL}"
[ -z "$(lxc profile device get vsat-nested eth0 network 2>/dev/null || true)" ] && \
  lxc profile device add vsat-nested eth0 nic name=eth0 network=lxdbr0

lxc profile set vsat-nested security.nesting true
lxc profile set vsat-nested security.privileged true
lxc profile set vsat-nested linux.kernel_modules overlay,nf_conntrack,br_netfilter,iptable_nat,iptable_filter
lxc profile set vsat-nested raw.lxc 'lxc.apparmor.profile=unconfined
lxc.cap.drop=
lxc.cgroup.devices.allow=a
lxc.mount.auto=proc:rw sys:rw'

# Autostart existing containers after reboot.
for c in $(lxc list --format csv -c n 2>/dev/null); do
  lxc config set "$c" boot.autostart true || true
done

# --- NAT / forwarding -----------------------------------------------------
LXD_CIDR="$(lxc network get lxdbr0 ipv4.address | sed 's#/.*#/24#')"
log "lxdbr0 CIDR: ${LXD_CIDR}"
sysctl -w net.ipv4.ip_forward=1 >/dev/null
iptables -C FORWARD -i lxdbr0 -j ACCEPT 2>/dev/null || iptables -A FORWARD -i lxdbr0 -j ACCEPT
iptables -C FORWARD -o lxdbr0 -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
  iptables -A FORWARD -o lxdbr0 -m state --state RELATED,ESTABLISHED -j ACCEPT
iptables -t nat -C POSTROUTING -s "${LXD_CIDR}" ! -d "${LXD_CIDR}" -j SNAT --to-source "${PRIMARY_IP}" 2>/dev/null || \
  iptables -t nat -I POSTROUTING 1 -s "${LXD_CIDR}" ! -d "${LXD_CIDR}" -j SNAT --to-source "${PRIMARY_IP}"

log "host bootstrap complete. Next: install the app (see scripts/install.sh)."
