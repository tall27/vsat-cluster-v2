#!/usr/bin/env bash
# run-lxd-image-probe-remote.sh — copy and run the LXD image probe on a test host.
#
# Reads .env from the repository root. Expected variables:
#   SSH_IP=1.2.3.4
#   SSH_USER=ubuntu
#   SSH_KEy=/path/to/key.pem   # existing mixed-case name supported
# Optional spelling also supported:
#   SSH_KEY=/path/to/key.pem
#
# Usage:
#   ./scripts/run-lxd-image-probe-remote.sh
#   ./scripts/run-lxd-image-probe-remote.sh -- --cleanup
#   ./scripts/run-lxd-image-probe-remote.sh -- --profile default images:alpine/3.23
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ROOT}/.env"
PROBE_SCRIPT="${ROOT}/scripts/probe-lxd-images.sh"
REMOTE_SCRIPT="/tmp/vsat-lxd-image-probe.sh"

if [ ! -f "${ENV_FILE}" ]; then
  echo "missing .env at ${ENV_FILE}" >&2
  exit 1
fi

if [ ! -f "${PROBE_SCRIPT}" ]; then
  echo "missing probe script at ${PROBE_SCRIPT}" >&2
  exit 1
fi

env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
      value=$0
      sub(/^[^=]*=/, "", value)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
      gsub(/^["\047]|["\047]$/, "", value)
      print value
      exit
    }
  ' "${ENV_FILE}"
}

SSH_IP="$(env_value SSH_IP)"
SSH_USER="$(env_value SSH_USER)"
SSH_KEY_PATH="$(env_value SSH_KEY)"
if [ -z "${SSH_KEY_PATH}" ]; then
  SSH_KEY_PATH="$(env_value SSH_KEy)"
fi

if [ -z "${SSH_IP}" ] || [ -z "${SSH_USER}" ]; then
  echo ".env must define SSH_IP and SSH_USER" >&2
  exit 1
fi

ssh_args=(-o StrictHostKeyChecking=accept-new)
scp_args=(-o StrictHostKeyChecking=accept-new)
if [ -n "${SSH_KEY_PATH}" ]; then
  ssh_args+=(-i "${SSH_KEY_PATH}")
  scp_args+=(-i "${SSH_KEY_PATH}")
fi

target="${SSH_USER}@${SSH_IP}"

if [ "${1:-}" = "--" ]; then
  shift
fi

printf '[remote-probe] copying probe script to %s:%s\n' "${target}" "${REMOTE_SCRIPT}" >&2
scp "${scp_args[@]}" "${PROBE_SCRIPT}" "${target}:${REMOTE_SCRIPT}" >/dev/null

printf '[remote-probe] running probe on %s\n' "${target}" >&2
remote_cmd="chmod +x '${REMOTE_SCRIPT}' && sudo '${REMOTE_SCRIPT}'"
for arg in "$@"; do
  remote_cmd+=" $(printf '%q' "$arg")"
done
ssh "${ssh_args[@]}" "${target}" "${remote_cmd}"
