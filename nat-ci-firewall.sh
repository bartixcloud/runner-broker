#!/usr/bin/env bash
# infra/runner-broker/nat-ci-firewall.sh
#
# Installs the CI-ISOLATION nftables chain and its jump from Docker's
# DOCKER-USER hook. Idempotent — safe to re-run. Must run at boot: this rule
# currently lives ONLY in the running kernel ruleset (nftables.service is
# disabled on this host, /etc/nftables.conf is the stock empty template) and
# does not survive a reboot today.
#
# DOCKER-USER is Docker's documented hook point for user forwarding rules:
# dockerd creates the chain (empty) on daemon start and never touches rules
# already present in it. This script must run AFTER dockerd has created that
# chain, hence the poll loop below rather than trusting unit ordering alone.
set -euo pipefail

for _ in $(seq 1 30); do
  nft list chain ip filter DOCKER-USER >/dev/null 2>&1 && break
  sleep 1
done
nft list chain ip filter DOCKER-USER >/dev/null 2>&1 || {
  echo "DOCKER-USER chain never appeared — is docker.service up?" >&2
  exit 1
}

# CI-ISOLATION: from nat-ci, allow established/return traffic and outbound
# internet; drop anything destined for RFC1918 (the fleet, any
# other private-net host). Flush+re-add makes re-running this safe — done as
# one `nft -f` transaction (not separate `nft add rule` calls) so the chain
# is never briefly empty mid-run: a restart of this unit while runners are
# live must not leave a window where nat-ci traffic falls through unfiltered.
nft add table ip filter 2>/dev/null || true
nft add chain ip filter CI-ISOLATION 2>/dev/null || true
nft -f - <<'NFT'
flush chain ip filter CI-ISOLATION
add rule ip filter CI-ISOLATION oifname "nat-ci" ct state related,established accept
add rule ip filter CI-ISOLATION ip daddr 10.0.0.0/8 iifname "nat-ci" drop
add rule ip filter CI-ISOLATION ip daddr 172.16.0.0/12 iifname "nat-ci" drop
add rule ip filter CI-ISOLATION ip daddr 192.168.0.0/16 iifname "nat-ci" drop
add rule ip filter CI-ISOLATION iifname "nat-ci" accept
NFT

# Jump into it from DOCKER-USER, first in the chain (matches where it was
# manually inserted during the original P2 testing) — only if not already
# present, so re-running this script never stacks duplicate jumps.
if ! nft list chain ip filter DOCKER-USER | grep -q 'jump CI-ISOLATION'; then
  nft insert rule ip filter DOCKER-USER jump CI-ISOLATION
fi

echo "CI-ISOLATION installed and wired into DOCKER-USER"
