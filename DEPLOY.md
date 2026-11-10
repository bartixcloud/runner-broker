# bcx-broker: deploy and operate

Ephemeral GitHub Actions runners on Incus for every bartixcloud tenant, behind one
GitHub App (`bartixcloud-runners`). Spec: `docs/superpowers/specs/2026-10-04-platform-design.md`
§3.3 (CI: App, dispatcher, per-tenant brokers) and §2.2 (L0 metal layer, which
installs all of this); contracts C5.

```
GitHub App webhook ─► https://hooks.example.org/github ─► host Traefik
  ─► bcx-dispatcher 172.17.0.1:9100 (HMAC, installation.id → tenant)
       ├─ workflow_job ─► /run/bcx-broker/<tenant>.sock ─► bcx-broker@<tenant>
       │                    JIT runner [self-hosted, <tenant>, <tier>],
       │                    VM in the tenant's Incus project on nat-ci
       └─ push refs/tags/v* on <owner>/infra ─► incus exec … nuk-converge (poke)
```

## Files

Every binary is a static Go program, built by `build.sh` from the checkout on metal
(`<checkout>`, pinned to a commit) and installed from its
`bin/`. `binaries.txt` is the one place that names them: `role installed-name go-package`.

| Binary | Installed at | Purpose |
|---|---|---|
| `bcx-dispatcher` | `/opt/bcx-broker/` | webhook endpoint, HMAC, routing by installation, poke |
| `bcx-broker` | `/opt/bcx-broker/` | per-tenant broker (`bcx-broker@<tenant>.service`) |
| `bcx-reap` | `/opt/bcx-broker/` | orphan reaper, a security control (`bcx-broker-reap@<tenant>.timer`) |
| `bcx-appauth` | `/opt/bcx-broker/` | App JWT and installation tokens: `bcx-appauth token --installation-id <id>` |
| `bcx-gen-tenant-env` | `/opt/bcx-broker/` | `tenants.yml` → `/etc/bcx-broker/<tenant>.env` + `dispatcher.json` |

| Source | Installed at | Purpose |
|---|---|---|
| `broker-firewall.sh` | `/opt/bcx-broker/` | ufw: edge network → 172.17.0.1:9100 |
| `nat-ci-firewall.sh` | `/opt/bcx-broker/` | nftables CI-ISOLATION for the `nat-ci` bridge |
| `tiers.yml` | `/etc/bcx-broker/tiers.yml` | tier shapes |
| `bcx-dispatcher.service`, `bcx-broker@.service`, `bcx-broker-reap@.service`, `bcx-broker-reap@.timer`, `broker-firewall.service`, `nat-ci-firewall.service` | `/etc/systemd/system/` | units |
| `traefik-hooks.yml` | `/opt/docker-volumes/traefik/config/dynamic/hooks.yml` | host-Traefik route; placed by hand at the cutover, not by `install.sh` |
| generated `<tenant>.env`, `dispatcher.json` | `/etc/bcx-broker/` (0600) | per-tenant broker config and the dispatcher's routing table |

`bcx-gen-tenant-env` still shells out to `python3` for the poke targets of
`tenants.yml`, so metal keeps `python3` and `python3-yaml` until that is ported.
Nothing else needs an interpreter.

Secrets on metal (never in git):
- `/etc/bartixcloud/app.pem`: App private key, root 0600.
- `/etc/bartixcloud/app.env`: `BCX_APP_ID=`.
- `/etc/bartixcloud/dispatcher.env`: `BCX_WEBHOOK_SECRET=` (at least 20 characters), 0600.
- Optional `/etc/bcx-broker/<tenant>.age`: tier secrets, age-encrypted to `/etc/bartixcloud/age.key`. Absent means none. A tier this tenant can use that needs secrets fails the broker's start.

## Hetzner Cloud (`backend: hcloud`)

An hcloud tier runs one-shottable Hetzner Cloud servers instead of Incus containers. It is
enabled per tier with `backend: hcloud`; absent means Incus. The tier takes exactly
`image, server_type, locations, runner_group, max_runners, allowlist, secrets` and optional
`max_age` (Go duration, default `2h`). It must not carry Incus' `network`, `cpu` or `mem`, and
its allowlist must not contain `*`. Example (the shipped `tiers.yml` enables it only in the
separate rollout step):

```yaml
e2e:
  backend: hcloud
  image: bcx-e2e-base          # snapshot labelled bcx-image=bcx-e2e-base, newest wins
  server_type: cpx42
  locations: [fsn1, nbg1, hel1]
  runner_group: e2e-ephemeral
  max_runners: 2
  max_age: 2h
  allowlist: ["kappa-org/app"]
  secrets: []
```

The token is project-scoped, root-owned, mode 0600, at
`/etc/bcx-broker/<tenant>.hcloud-token` (override with `BROKER_HCLOUD_TOKEN_FILE`). It is read
only when this tenant can use an hcloud tier (an allowlist owner equal to `BROKER_TARGET`;
never `*`). The broker then requires the file and one successful
`GET /servers?per_page=1` at startup, or it fails loudly. Deliver it over stdin, never argv:

```bash
sops -d --extract '["HCLOUD_TOKEN"]' secrets/broker-hcloud-<tenant>.enc.env |
  ssh metal 'sudo install -m 0600 -o root -g root /dev/stdin /etc/bcx-broker/<tenant>.hcloud-token'
```

VMs get public IPv4 only (`enable_ipv6: false`), the rule-less firewall
`bcx-ci-deny-inbound`, no SSH key and no private network (decision D7). The JIT config is in
cloud-init `user_data`; the baked snapshot has Docker, the runner and the one-job unit.

Deploy in this order (spec §8):
1. Create the tenant's Hetzner project, a read/write token, and its runner group; check the
   project server limit is at least 4.
2. Put the token in sops, deliver it to metal, and bake/verify the snapshot. Both the token
   and the snapshot must exist before step 3.
3. Merge and deploy the broker binary **without** the hcloud tier; the existing Incus tiers
   must keep working.
4. Only then add the hcloud tier to `tiers.yml`, build/install, and restart the tenant's
   broker. Startup proves the token. A rollback removes the tier first and restarts; an old
   binary must never see `backend: hcloud`.

## Build

Metal builds it, from its own checkout. `go.mod` pins `go 1.27.1`, which Debian 13
does not package, so metal carries the upstream toolchain at `/usr/local/go`
(`/usr/local/bin/go` precedes `/usr/bin/go` on PATH):

```bash
sudo curl -fsSLO https://go.dev/dl/go1.27.1.linux-amd64.tar.gz
echo "63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445  go1.27.1.linux-amd64.tar.gz" | sha256sum -c -
sudo tar -C /usr/local -xzf go1.27.1.linux-amd64.tar.gz
sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go      # /etc/profile.d/go.sh does it for logins
go version                                              # go1.27.1 linux/amd64
```

A toolchain bump is a `go.mod` edit plus this install, in that order.

Deploy from the checkout at a pinned commit (a detached HEAD: a deploy host must not
follow a moving branch):

```bash
cd <checkout>
git fetch origin
git checkout --detach <commit>   # the commit under deploy; record it in the ticket
./build.sh                       # all five, linux/amd64, into bin/
sudo ./install.sh --tenants <infra>/tenants.yml --check
```

`build.sh` itself, unchanged, for a one-role build or another target:

```bash
./build.sh --only dispatcher  # one role from binaries.txt
BCX_GOOS=darwin BCX_GOARCH=arm64 ./build.sh --only dispatcher   # other targets
```

`CGO_ENABLED=0`, `-trimpath -buildvcs=false -ldflags='-s -w'`. `bin/` is git-ignored.
If a package directory does not exist yet, `build.sh` names it, builds nothing, and
exits 2. `-trimpath` plus a pinned commit makes the build reproducible: on 2026-10-07
the same commit built on the laptop and on metal produced byte-identical binaries, so
`sha256sum bin/* /opt/bcx-broker/*` is a valid deploy check. Building elsewhere and
copying `bin/` over still works — `install.sh --bin-dir` exists for that — it just
leaves the checkout and the installed binary free to disagree.

## Install (idempotent; P3 `metal/bootstrap.sh` calls this)

```bash
sudo ./install.sh --tenants infra/tenants.yml                  # install
sudo ./install.sh --tenants infra/tenants.yml --check          # drift: exit 1 + "would change:" lines
sudo ./install.sh --tenants infra/tenants.yml --bin-dir /path  # binaries from somewhere other than ./bin
```

| Exit | Meaning |
|---|---|
| 0 | installed, or `--check` found nothing to change |
| 1 | `--check` found drift (one `would change: <path>` per file, content or mode); outside `--check`, a write failed |
| 2 | nothing was touched: `tenants.yml` invalid, a binary or source file missing (`missing binary: <path> (run build.sh)`), or bad arguments |

Every binary in `binaries.txt` must be an executable file in the bin dir, or the run
exits 2 before rendering or writing anything, `--check` included. The generator renders
into a temporary directory first, so an invalid `tenants.yml` leaves the host as it
was. Each file is replaced by rename, so a running binary is never overwritten in
place.

`install.sh` never enables or starts a unit. After a real install it runs
`systemctl daemon-reload` if a unit file changed, and
`systemctl try-restart bcx-dispatcher.service 'bcx-broker@*.service'` if a binary,
`broker-firewall.sh`, `tiers.yml` or a generated file changed; `try-restart` leaves
stopped units stopped.

## Adding a tenant

A tenant is an owner whose repositories use `bcx-broker@<tenant>`. The GitHub App must be
installed on that owner and the tenant added to the platform `tenants.yml`; then, per owner:

```bash
sudo ./install.sh --tenants <platform tenants.yml>
sudo systemctl enable --now "bcx-broker@<tenant>.service" "bcx-broker-reap@<tenant>.timer"
```

Host-specific facts of the production deployment (hostnames, paths, installation ids, the
retirement of the old Python brokers) are not kept in this repository.

## Verify

```bash
curl -s http://172.17.0.1:9100/healthz                                       # {"status":"ok"}
sudo curl -s --unix-socket /run/bcx-broker/<tenant>.sock http://x/healthz    # {"status":"ok"}
journalctl -u bcx-dispatcher -u 'bcx-broker@*' -f
sudo /opt/bcx-broker/broker-firewall.sh --check
```

## Org instance

A second `bcx-dispatcher` for the organisation's own GitHub App (`akd-leitstand`,
spec K1/K8). It is a separate process, not a mode of the bartixcloud one: own listen
port, own `BCX_WEBHOOK_SECRET`, own `dispatcher.json`. Run it from its own unit (a copy
of `bcx-dispatcher.service` with its own `EnvironmentFile=` and a longer
`TimeoutStopSec=`: running plans get 25 s to finish on a stop, then they are cancelled
and recorded as findings).

The `org` section of its `dispatcher.json` switches org mode on. `installations` must
hold the org's installation (`"tenant": null`); `pokes` may stay empty:

```json
{
  "installations": {"<installation id>": {"owner": "ekd-akd", "tenant": null}},
  "pokes": {},
  "org": {
    "owner": "ekd-akd",
    "spiegel_repo": "ekd-akd/spiegel",
    "spiegel_installation": 0,
    "spiegel_workflow": "fakten.yml",
    "spiegel_org_workflow": "org.yml",
    "spiegel_ref": "main",
    "sicherung_dir": "/usr/local/lib/sicherung",
    "sicherung_wurzel": "/srv/sicherung",
    "befund_datei": "/var/lib/sicherung/befunde.jsonl",
    "schwellen": {"geloeschte_dateien": 50, "geloeschte_zeilen": 5000},
    "sicherung_timeout": "10m",
    "restic_timeout": "6h"
  }
}
```

Required: `owner`, `spiegel_repo`, `spiegel_installation` (the `akd-spiegel` App's
installation id on the mirror repo, a positive integer; the `0` above is a placeholder
that fails the start). Everything else shows its default. Any invalid value, unknown
key or missing credential file stops the start.

Environment (besides `BCX_WEBHOOK_SECRET`, `BCX_DISPATCHER_HOST`, `BCX_DISPATCHER_PORT`,
`BCX_DISPATCHER_MAP`): the `akd-spiegel` App is configured by two paths, never by value.

| Variable | Default | Content |
|---|---|---|
| `AKD_SPIEGEL_APP_ENV` | `/etc/akd-spiegel/app.env` | file with `BCX_APP_ID=<numeric app id>` (the process's own `BCX_APP_ID` is ignored) |
| `AKD_SPIEGEL_APP_KEY` | `/etc/akd-spiegel/app.pem` | the App's RSA private key, root 0600 |

Optionally the findings are also sent to a Telegram chat, both variables or neither:

| Variable | Default | Content |
|---|---|---|
| `BCX_TELEGRAM_TOKEN_FILE` | unset | absolute path of a file holding the bot token (0600; the token itself never goes into the environment) |
| `BCX_TELEGRAM_CHAT_ID` | unset | target chat: a numeric id (groups are negative) or `@channelname` |

One message per event, sent right after the findings are appended to the befund file and
before restic starts, so a slow snapshot never delays the alert. A failed message is
logged and does not stop the plan; the token never appears in a log line.

All of these are read only in org mode. The scripts `sichern.sh` and `restic-sichern.sh` run
with `SICHERUNG_WURZEL` set from the config; their own environment (`RESTIC_*`,
`SICHERUNG_MOUNT`, `SICHERUNG_TOKEN_DATEI`, ...) is inherited from the unit's
`EnvironmentFile=`. The unit's user needs write access to `befund_datei`'s directory and
the clone root.

Per event (after HMAC, installation and owner checks; a signed `ping` is answered
first, a new `v*` tag on a poke repo still pokes): `push` and `delete` back up and rate;
`repository` rates only (and the mirror follows); a push to the default branch,
`pull_request`, completed `check_suite` / `workflow_run`, `release` and repository
changes dispatch `fakten.yml` for the repo, a deleted repo dispatches `org.yml`. The
mirror repo's own events never dispatch anything. Replies are immediate (202); plans run
in the background, at most 4 at once, one repo at a time. A failed backup or snapshot is
a finding of kind `unpruefbar`; a failed dispatch is logged. Findings are one JSON line
each in `befund_datei`.

A second token for the clone fetch (the `akd-leitstand` App) is minted with
`bcx-appauth token --installation-id <int> --key <pem> --app-env <env file>`; with
`--app-env` the id comes from that file only.

## Security invariants

- One HMAC check, in the dispatcher. Brokers accept only their 0600 root socket.
- Unknown `installation.id` → 403 and a log line.
- No PAT. Tokens are minted per installation and live ≤ 1 h.
- A JIT runner carries `[self-hosted, <tenant>, <tier>]`. Another tenant's label, two tiers, or a stray label → DENY.
- CI VMs live in the tenant's Incus project on `nat-ci`. `TENANT-ISOLATION` (P3, nftables on metal) lets them reach `ingress:443` and the internet only.
