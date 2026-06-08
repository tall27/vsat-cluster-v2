#!/usr/bin/env bash
# quickstart.sh — single-command install of VSAT Cluster v2 on a fresh Ubuntu host.
#
# Downloads the latest GitHub release (binary + scripts/), preps the host
# (LXD, vsat-nested profile, NAT) and installs the systemd service. Idempotent:
# safe to re-run (e.g. to pick up a newer release).
#
# Usage:   curl -fsSL https://raw.githubusercontent.com/tall27/vsat-cluster-v2/master/scripts/quickstart.sh | sudo bash
#      or: sudo ./quickstart.sh [PRIMARY_IP]
set -euo pipefail

REPO="tall27/vsat-cluster-v2"
log() { printf '[quickstart] %s\n' "$*"; }

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (use sudo)" >&2
  exit 1
fi

for bin in curl tar; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing required tool: $bin" >&2; exit 1; }
done

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

log "fetching latest release of ${REPO}"
RELEASE_JSON="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")"
BINARY_URL="$(printf '%s' "${RELEASE_JSON}" | grep -o '"browser_download_url": *"[^"]*vsat-webapp"' | head -n1 | cut -d'"' -f4)"
SCRIPTS_URL="$(printf '%s' "${RELEASE_JSON}" | grep -o '"browser_download_url": *"[^"]*scripts\.tar\.gz"' | head -n1 | cut -d'"' -f4)"
[ -n "${BINARY_URL}" ] && [ -n "${SCRIPTS_URL}" ] || {
  echo "could not find release assets for ${REPO} — has a release been published?" >&2
  exit 1
}

log "downloading vsat-webapp binary"
curl -fsSL -o "${WORK}/vsat-webapp" "${BINARY_URL}"
chmod +x "${WORK}/vsat-webapp"

log "downloading scripts"
curl -fsSL -o "${WORK}/scripts.tar.gz" "${SCRIPTS_URL}"
tar -C "${WORK}" -xzf "${WORK}/scripts.tar.gz"
chmod +x "${WORK}"/scripts/*.sh

# Run with stdin from /dev/null: when this script is invoked as
# `curl ... | sudo bash`, our stdin is the pipe carrying the rest of our own
# source. Without this, a subcommand that reads stdin when it's not a TTY
# (e.g. `lxc profile create` accepting YAML config) would swallow leftover
# script bytes and choke on them as malformed input. Redirecting only here
# (not for this whole script) keeps bash able to read its own piped source.
log "preparing the host (LXD, vsat-nested profile, NAT)"
"${WORK}/scripts/bootstrap-host.sh" "$@" </dev/null

log "installing the binary and systemd service"
"${WORK}/scripts/install.sh" "${WORK}/vsat-webapp" </dev/null

ADDR="$(curl -fsSL -4 ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}')"
log "done — browse to https://${ADDR}/ to accept the self-signed cert and complete /setup"
log "(open port 443 in your security group / firewall if you can't reach it)"
