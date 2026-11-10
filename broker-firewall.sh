#!/usr/bin/env bash
# broker-firewall.sh - admit the host Traefik to bcx-dispatcher (contract C5).
#
# The dispatcher binds 172.17.0.1:9100 (docker0's address) because the host
# Traefik is a bridge-network container and cannot reach metal's loopback. Its
# traffic arrives from the `edge` docker network, so ufw must admit exactly that
# subnet to exactly 172.17.0.1:9100. Tenant brokers listen on unix sockets and
# need no rule; the old per-tenant 9010+ rules are removed at the switch (P5
# step 5), not by this script.
#
# The source subnet is DISCOVERED from the live `edge` network, so re-creating
# it with another subnet cannot silently orphan the rule. Override with --source
# for the post-DNAT topology.
#
#   broker-firewall.sh                    ensure the rule (idempotent: ufw skips
#                                         a rule identical to one already added)
#   broker-firewall.sh --check            change nothing; exit 1 if the rule is
#                                         missing
#   broker-firewall.sh --source <cidr>    admit this subnet instead of the
#                                         discovered one
#   broker-firewall.sh -h                 print this header and exit 0
#
# Exit codes
#   0   rule ensured, or --check found it
#   1   --check: rule missing or ufw cannot list rules; the edge subnet cannot
#       be discovered
#   2   bad arguments
set -euo pipefail

PORT=9100
DEST=172.17.0.1
COMMENT="Traefik edge network -> bcx-dispatcher"
SOURCE=""
CHECK=0

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
  echo "broker-firewall.sh: $*" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check)   CHECK=1; shift ;;
    # An empty value would fall back to discovery and admit a subnet nobody named.
    --source)  [ -n "${2:-}" ] || die "--source needs a CIDR"; SOURCE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

discover() {
  docker network inspect edge --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null || true
}

if [ -z "$SOURCE" ]; then
  if [ "$CHECK" = 1 ]; then
    # A health report, not a boot path: no polling, a missing network is a finding.
    SOURCE="$(discover)"
  else
    # At boot this may run before `edge` exists; poll instead of trusting unit order.
    for _ in $(seq 1 30); do
      SOURCE="$(discover)"
      [ -n "$SOURCE" ] && break
      sleep 2
    done
  fi
  if [ -z "$SOURCE" ]; then
    echo "cannot discover the edge network subnet - is docker up and 'edge' created?" \
         "(override with --source)" >&2
    exit 1
  fi
fi

if [ "$CHECK" = 1 ]; then
  # Exact-line match against ufw's own rendering verifies source, destination,
  # port and comment, so a stale rule is reported rather than counted as present.
  expected="ufw allow from $SOURCE to $DEST port $PORT proto tcp comment '$COMMENT'"
  # Listing first, then matching without a pipe: under pipefail a `grep -q`
  # that exits early would turn a match into a SIGPIPE failure of the lister.
  if ! added="$(ufw show added)"; then
    echo "FAIL - cannot list ufw rules (ufw show added failed)" >&2
    exit 1
  fi
  if grep -qxF "$expected" <<<"$added"; then
    echo "ok   - bcx-dispatcher ($DEST:$PORT) admitted from $SOURCE"
    exit 0
  fi
  echo "FAIL - bcx-dispatcher not admitted from $SOURCE; expected: $expected" >&2
  exit 1
fi

# argv form, never eval: SOURCE reaches ufw as one word however it was spelled.
ufw allow from "$SOURCE" to "$DEST" port "$PORT" proto tcp comment "$COMMENT" >/dev/null
echo "ensured: bcx-dispatcher ($DEST:$PORT) admitted from $SOURCE"
