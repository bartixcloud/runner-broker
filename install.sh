#!/usr/bin/env bash
# install.sh - install the bartixcloud broker layer on metal (spec 2.2, L0).
# Called by infra metal/bootstrap.sh; idempotent. Never enables or starts a
# unit: switching from the old brokers is P5 step 5 (contract C5).
#
#   install.sh [--check] [--tenants <tenants.yml>] [--root <dir>] [--bin-dir <dir>]
#
#   --check    change nothing; print `would change: <path>` per differing file
#              (content or mode); exit 1 if any, else 0
#   --tenants  default: <superproject>/infra/tenants.yml
#   --root     prefix for every target path (tests)
#   --bin-dir  built binaries to install; default: <this dir>/bin (see build.sh).
#              Every binary named in binaries.txt must be an executable file there.
#   -h         print this header and exit 0
#
# Exit codes
#   0   installed, or --check found nothing to change
#   1   --check found drift; outside --check, a file could not be written
#   2   nothing was written: tenants.yml invalid (the generator refused it), a
#       binary or source file is missing, or bad arguments
#
# Environment
#   SYSTEMCTL  systemctl to call (default: systemctl). With --root, systemctl is
#              called only when this is set, so tests can record the calls.
#
# After a real install: `systemctl daemon-reload` if a unit file changed, and
# `systemctl try-restart` of the dispatcher and the tenant brokers if a binary,
# script, tiers.yml or generated file changed. try-restart touches only units that
# are already running.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TENANTS="$HERE/../infra/tenants.yml"
BIN_DIR="$HERE/bin"
ROOT=""
CHECK=0
CTL_SEAM="${SYSTEMCTL:-}"

# Sources that live next to this script, besides the binaries.
UNITS="bcx-dispatcher.service bcx-broker@.service bcx-broker-reap@.service bcx-broker-reap@.timer broker-firewall.service nat-ci-firewall.service"
SOURCES="binaries.txt tiers.yml broker-firewall.sh nat-ci-firewall.sh $UNITS"

usage() {
  local line
  while IFS= read -r line; do
    case "$line" in
      '#!'*) ;;
      '#') echo ;;
      '# '*) printf '%s\n' "${line#\# }" ;;
      *) break ;;
    esac
  done < "$0"
}

die() {
  echo "install.sh: $*" >&2
  exit 2
}

# An empty value would turn --root "" into a real install on /.
need_value() {
  [ -n "${2:-}" ] || die "$1 needs a value"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check)   CHECK=1; shift ;;
    --tenants) need_value "$@"; TENANTS="$2"; shift 2 ;;
    --root)    need_value "$@"; ROOT="$2"; shift 2 ;;
    --bin-dir) need_value "$@"; BIN_DIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

# Preflight 1: everything this script installs must exist before anything is written.
for f in $SOURCES; do
  [ -f "$HERE/$f" ] || die "missing source: $HERE/$f"
done

# binaries.txt is the single source of installed names.
BIN_NAMES=""
GEN_NAME=""
while read -r role name pkg extra || [ -n "$role" ]; do
  case "$role" in ''|'#'*) continue ;; esac
  if [ -z "$name" ] || [ -z "$pkg" ] || [ -n "$extra" ]; then
    die "bad line in $HERE/binaries.txt: $role $name $pkg $extra"
  fi
  BIN_NAMES="$BIN_NAMES $name"
  if [ "$role" = generator ]; then GEN_NAME="$name"; fi
done < "$HERE/binaries.txt"
[ -n "$GEN_NAME" ] || die "$HERE/binaries.txt has no generator role"

# Preflight 2: every binary, reported all at once. Also under --check, so a missing
# build is never mistaken for a clean host.
missing=0
for n in $BIN_NAMES; do
  if [ ! -f "$BIN_DIR/$n" ] || [ ! -x "$BIN_DIR/$n" ]; then
    echo "missing binary: $BIN_DIR/$n (run build.sh)" >&2
    missing=1
  fi
done
if [ "$missing" = 1 ]; then exit 2; fi

# Render first: an invalid tenants.yml must stop us before anything is touched.
gen="$(mktemp -d "${TMPDIR:-/tmp}/bcx-install.XXXXXX")"
trap 'rm -rf "$gen"' EXIT
rc=0
# stdout is discarded on purpose; stderr carries the generator's refusal reason.
"$BIN_DIR/$GEN_NAME" "$TENANTS" --out "$gen" --no-incus-check >/dev/null || rc=$?
if [ "$rc" != 0 ]; then
  echo "install.sh: $GEN_NAME refused $TENANTS (exit $rc)" >&2
  exit 2
fi
# Without the table the dispatcher would keep serving a stale one.
[ -f "$gen/dispatcher.json" ] || die "$GEN_NAME wrote no dispatcher.json"

changed=0
units_changed=0
runtime_changed=0

# put <src> <path under root> <mode> <unit|runtime>
# A file is replaced by rename, never rewritten: a running binary must not be
# overwritten in place, and a reader never sees a half-written env file.
put() {
  local src="$1" rel="$2" mode="$3" kind="$4"
  local dest="$ROOT$rel"
  local tmp="$dest.new.$$"
  # A symlink is drift: install -m replaces it by a regular file.
  if [ -f "$dest" ] && [ ! -L "$dest" ] && cmp -s "$src" "$dest" \
     && [ -n "$(find "$dest" -maxdepth 0 -perm "$mode")" ]; then
    return 0
  fi
  changed=1
  case "$kind" in
    unit) units_changed=1 ;;
    runtime) runtime_changed=1 ;;
    *) die "internal error: unknown kind $kind" ;;
  esac
  if [ "$CHECK" = 1 ]; then
    echo "would change: $rel"
    return 0
  fi
  mkdir -p "$(dirname "$dest")"
  if ! install -m "$mode" "$src" "$tmp" || ! mv -f "$tmp" "$dest"; then
    rm -f "$tmp"
    echo "install.sh: could not write $dest" >&2
    exit 1
  fi
  echo "installed: $rel"
}

for n in $BIN_NAMES; do
  put "$BIN_DIR/$n" "/opt/bcx-broker/$n" 0755 runtime
done
put "$HERE/broker-firewall.sh" /opt/bcx-broker/broker-firewall.sh 0755 runtime
put "$HERE/nat-ci-firewall.sh" /opt/bcx-broker/nat-ci-firewall.sh 0755 runtime
put "$HERE/tiers.yml" /etc/bcx-broker/tiers.yml 0644 runtime
for u in $UNITS; do
  put "$HERE/$u" "/etc/systemd/system/$u" 0644 unit
done
for f in "$gen"/*; do
  [ -f "$f" ] || continue
  put "$f" "/etc/bcx-broker/$(basename "$f")" 0600 runtime
done

if [ "$CHECK" = 1 ]; then
  exit "$changed"
fi

# With --root the host's systemd is not the target, so stay off it unless a test
# points SYSTEMCTL at a stub.
if [ -z "$ROOT" ] || [ -n "$CTL_SEAM" ]; then
  ctl="${CTL_SEAM:-systemctl}"
  if [ "$units_changed" = 1 ]; then
    "$ctl" daemon-reload
  fi
  # try-restart only restarts what already runs; nothing is enabled or started here.
  # nat-ci-firewall is a security control (CI-ISOLATION): a changed script must be
  # re-applied, not left in memory as the old ruleset.
  if [ "$runtime_changed" = 1 ]; then
    "$ctl" try-restart bcx-dispatcher.service 'bcx-broker@*.service' nat-ci-firewall.service
  fi
fi
exit 0
