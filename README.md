# runner-broker

Webhook-driven ephemeral CI runners on Incus for every bartixcloud tenant, behind one
GitHub App. A dispatcher verifies the webhook HMAC once and routes by installation id
to a per-tenant broker, which mints a single-use JIT runner and launches it in a VM:
one job, then the VM is gone.

Operating it (build, install, cutover, verify): [`DEPLOY.md`](DEPLOY.md).
Working on it (type-safety rules, layout, gate): [`AGENTS.md`](AGENTS.md).

```bash
go test -race ./...     # includes deploytest: build.sh, install.sh, units, firewall
./build.sh              # five static linux/amd64 binaries into bin/
```

## Tier resolution

A job's labels, once the GitHub-assigned ones and the tenant's own are stripped, must
resolve to exactly one known tier from `tiers.yml`. Extra labels, more than one tier or
an unknown tier is a hard deny: the run is cancelled and logged, never ignored or
downgraded to a default. Bare `self-hosted` jobs (no known tier at all) are ignored.
A tier is entitled by its allowlist; a tenant whose owner cannot use a tier never
loads that tier's secrets.

## Org mode

The same binary can serve an organisation's GitHub App (spec K1/K8) as a second
instance: its own port, HMAC secret and `dispatcher.json`, switched on by an optional
`org` section in that file. Without the section nothing changes. Per delivery it backs
up the repo (`sichern.sh`), rates destructive events (a Go port of `erkennung.py`,
checked against shared golden vectors in `dispatcher/testdata/sicherung/`), snapshots
with `restic-sichern.sh` on a critical finding, appends the findings as JSON lines, and
dispatches the mirror repo's `fakten.yml` / `org.yml`. Events of the mirror repo itself
never start a mirror job (loop protection). Setup: [`DEPLOY.md`](DEPLOY.md#org-instance).

## Invariants

- One HMAC check, in the dispatcher. A broker accepts only its 0600 socket in a 0700
  directory; an unknown installation id is a 403.
- No PAT: installation tokens are minted per use and live at most an hour.
- Every `incus` call is scoped to the tenant's project (`BROKER_INCUS_PROJECT`, no
  default), so one tenant's broker and reaper cannot see another's instances.
- The reaper is a security control: it sweeps orphans left by a crashed injection on
  an idle TTL and a hard ceiling.
- Deny-path caveat: cancelling a denied job cancels the whole GitHub Actions run (the
  API has no per-job cancel for a queued job). A run with a legitimate job beside a
  mislabelled one loses both. Accepted; worth knowing before an incident.
