#!/usr/bin/env bash
# probe-lxd-images.sh — launch candidate LXD images and record size/runtime facts.
#
# Intended for an existing LXD host. The script keeps launched containers by
# default so you can manually try the VSatellite installer in each one.
#
# Usage:
#   sudo ./scripts/probe-lxd-images.sh
#   sudo ./scripts/probe-lxd-images.sh --cleanup
#   sudo ./scripts/probe-lxd-images.sh --profile default images:alpine/3.23 ubuntu-minimal:24.04
set -uo pipefail

PROFILE="vsat-nested"
PREFIX="vsat-img"
TIMEOUT=90
CLEANUP=0
REPORT=""

DEFAULT_IMAGES=(
  "images:busybox/1.36.1"
  "images:alpine/3.23"
  "images:alpine/3.23/cloud"
  "images:debian/trixie"
  "images:debian/trixie/cloud"
  "ubuntu-minimal:24.04"
  "ubuntu:24.04"
)

usage() {
  cat <<EOF
Usage: $0 [options] [image ...]

Options:
  --profile NAME   LXD profile to apply (default: ${PROFILE})
  --prefix NAME    container name prefix (default: ${PREFIX})
  --timeout SEC    seconds to wait for lxc exec readiness (default: ${TIMEOUT})
  --report PATH    TSV report path (default: ./lxd-image-probe-<timestamp>.tsv)
  --cleanup        delete containers after probing
  -h, --help       show this help

If no images are provided, the script probes:
  ${DEFAULT_IMAGES[*]}

The script prints a TSV report with:
  image, remote_size, container, launch, exec_ready, bash, systemd, curl, tar, network, notes
EOF
}

images=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --profile)
      PROFILE="${2:-}"
      shift 2
      ;;
    --prefix)
      PREFIX="${2:-}"
      shift 2
      ;;
    --timeout)
      TIMEOUT="${2:-}"
      shift 2
      ;;
    --report)
      REPORT="${2:-}"
      shift 2
      ;;
    --cleanup)
      CLEANUP=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      while [ "$#" -gt 0 ]; do images+=("$1"); shift; done
      ;;
    -*)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      images+=("$1")
      shift
      ;;
  esac
done

if [ "${#images[@]}" -eq 0 ]; then
  images=("${DEFAULT_IMAGES[@]}")
fi

if [ -z "${REPORT}" ]; then
  REPORT="./lxd-image-probe-$(date -u +%Y%m%dT%H%M%SZ).tsv"
fi

log() {
  printf '[probe] %s\n' "$*" >&2
}

have() {
  command -v "$1" >/dev/null 2>&1
}

require_tools() {
  local missing=0
  for bin in lxc awk sed date tr; do
    if ! have "$bin"; then
      echo "missing required tool: $bin" >&2
      missing=1
    fi
  done
  [ "$missing" -eq 0 ] || exit 1
}

ensure_remote() {
  local name="$1"
  local url="$2"
  if lxc remote list --format csv 2>/dev/null | awk -F, '{print $1}' | grep -qx "$name"; then
    return 0
  fi
  log "adding LXD remote '${name}' (${url})"
  lxc remote add "$name" "$url" --protocol=simplestreams >/dev/null 2>&1
}

ensure_known_remotes() {
  local img
  for img in "${images[@]}"; do
    case "$img" in
      images:*)
        ensure_remote "images" "https://images.lxd.canonical.com" || \
          log "warning: could not add remote 'images'; probe may fail for ${img}"
        ;;
      ubuntu-minimal:*)
        ensure_remote "ubuntu-minimal" "https://cloud-images.ubuntu.com/minimal/releases/" || \
          log "warning: could not add remote 'ubuntu-minimal'; probe may fail for ${img}"
        ;;
    esac
  done
}

image_size() {
  local image="$1"
  local info
  info="$(lxc image info "$image" 2>/dev/null)" || {
    printf 'unknown'
    return
  }
  printf '%s' "$info" | awk -F': ' '/^Size:/ {print $2; found=1; exit} END {if (!found) print "unknown"}'
}

container_name() {
  local idx="$1"
  local image="$2"
  local slug
  slug="$(printf '%s' "$image" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]/-/g; s/--*/-/g; s/^-//; s/-$//')"
  slug="${slug:0:34}"
  printf '%s-%02d-%s' "$PREFIX" "$idx" "$slug"
}

exec_ready() {
  local name="$1"
  local start now
  start="$(date +%s)"
  while true; do
    if lxc exec "$name" -- true >/dev/null 2>&1; then
      return 0
    fi
    now="$(date +%s)"
    if [ "$((now - start))" -ge "$TIMEOUT" ]; then
      return 1
    fi
    sleep 2
  done
}

check_cmd() {
  local name="$1"
  local cmd="$2"
  if lxc exec "$name" -- sh -lc "command -v ${cmd} >/dev/null 2>&1" >/dev/null 2>&1; then
    printf 'yes'
  else
    printf 'no'
  fi
}

check_systemd() {
  local name="$1"
  if lxc exec "$name" -- sh -lc 'test -d /run/systemd/system && command -v systemctl >/dev/null 2>&1' >/dev/null 2>&1; then
    printf 'yes'
  else
    printf 'no'
  fi
}

check_network() {
  local name="$1"
  local script
  script='
route_ok=0
dns_ok=0
egress_ok=0
(command -v ip >/dev/null 2>&1 && ip -4 route get 1.1.1.1 >/dev/null 2>&1) && route_ok=1
(command -v getent >/dev/null 2>&1 && getent hosts example.com >/dev/null 2>&1) && dns_ok=1
(command -v nslookup >/dev/null 2>&1 && nslookup example.com >/dev/null 2>&1) && dns_ok=1
(command -v curl >/dev/null 2>&1 && curl -fsS --max-time 8 https://example.com >/dev/null 2>&1) && egress_ok=1
(command -v wget >/dev/null 2>&1 && wget -q -T 8 -O /dev/null https://example.com >/dev/null 2>&1) && egress_ok=1
(command -v ping >/dev/null 2>&1 && ping -c 1 -W 3 1.1.1.1 >/dev/null 2>&1) && egress_ok=1
[ "$egress_ok" -eq 1 ]
'
  if lxc exec "$name" -- sh -lc "$script" >/dev/null 2>&1; then
    printf 'yes'
  else
    printf 'no'
  fi
}

write_row() {
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$@" | tee -a "$REPORT"
}

probe_image() {
  local idx="$1"
  local image="$2"
  local name size launch_status exec_status bash_status systemd_status curl_status tar_status net_status notes launch_out

  name="$(container_name "$idx" "$image")"
  size="$(image_size "$image")"
  launch_status="fail"
  exec_status="no"
  bash_status="n/a"
  systemd_status="n/a"
  curl_status="n/a"
  tar_status="n/a"
  net_status="n/a"
  notes=""

  log "probing ${image} as ${name} (remote size: ${size})"

  if lxc info "$name" >/dev/null 2>&1; then
    notes="container already exists"
    write_row "$image" "$size" "$name" "$launch_status" "$exec_status" "$bash_status" "$systemd_status" "$curl_status" "$tar_status" "$net_status" "$notes"
    return
  fi

  launch_out="$(lxc launch "$image" "$name" -p "$PROFILE" -c limits.cpu=2 -c limits.memory=3GiB 2>&1)"
  if [ "$?" -ne 0 ]; then
    notes="$(printf '%s' "$launch_out" | tr '\t\r\n' ' ' | sed 's/  */ /g')"
    write_row "$image" "$size" "$name" "$launch_status" "$exec_status" "$bash_status" "$systemd_status" "$curl_status" "$tar_status" "$net_status" "$notes"
    return
  fi

  launch_status="ok"
  if exec_ready "$name"; then
    exec_status="yes"
    bash_status="$(check_cmd "$name" bash)"
    systemd_status="$(check_systemd "$name")"
    curl_status="$(check_cmd "$name" curl)"
    tar_status="$(check_cmd "$name" tar)"
    net_status="$(check_network "$name")"
  else
    notes="lxc exec not ready after ${TIMEOUT}s"
  fi

  write_row "$image" "$size" "$name" "$launch_status" "$exec_status" "$bash_status" "$systemd_status" "$curl_status" "$tar_status" "$net_status" "$notes"

  if [ "$CLEANUP" -eq 1 ]; then
    lxc delete --force "$name" >/dev/null 2>&1 || true
  fi
}

main() {
  require_tools
  ensure_known_remotes

  : > "$REPORT"
  write_row "image" "remote_size" "container" "launch" "exec_ready" "bash" "systemd" "curl" "tar" "network" "notes" >/dev/null

  local idx=1
  local image
  for image in "${images[@]}"; do
    probe_image "$idx" "$image"
    idx=$((idx + 1))
  done

  log "report written to ${REPORT}"
  if [ "$CLEANUP" -eq 0 ]; then
    log "containers were kept for manual testing; delete them with: lxc delete --force ${PREFIX}-*"
  fi
}

main "$@"
