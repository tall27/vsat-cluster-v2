#!/usr/bin/env bash
# install.sh — install the vsat-webapp binary + systemd unit on the host.
#
# Run from the directory containing the `vsat-webapp` binary and this scripts/
# folder, e.g. after scp-ing the release. Idempotent.
#
# Usage: sudo ./install.sh [path-to-binary]
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (use sudo)" >&2
  exit 1
fi

BIN="${1:-./vsat-webapp}"
[ -x "${BIN}" ] || { echo "binary not found/executable: ${BIN}" >&2; exit 1; }
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

install -m 0755 "${BIN}" /usr/local/bin/vsat-webapp
install -d -m 0700 /etc/vsat-cluster
install -m 0644 "${SCRIPT_DIR}/vsat-webapp.service" /etc/systemd/system/vsat-webapp.service

systemctl daemon-reload
systemctl enable --now vsat-webapp.service
echo "installed. Service status:"
systemctl --no-pager --full status vsat-webapp.service | head -n 8 || true
