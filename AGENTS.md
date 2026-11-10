# runner-broker — agent instructions

Go only. The broker is a root systemd service that shells out to `incus` and `age`
and handles attacker-influenced webhook payloads (GitHub `workflow_job`), so it is a
set of static binaries with no interpreter on the host (one exception: `gen` calls
`python3 fleet/tenants.py poke-targets` until P3 ports it).

## Type safety (non-negotiable, project standard)

- `struct` at every boundary — no `map[string]interface{}` crossing a public
  function signature. Decode external payloads (webhook JSON, YAML, API
  responses) into typed structs; `interface{}` is confined to the decode step.
- Closed value sets are `iota` enums (or `string` constants) with an exhaustive
  `switch` — never bare strings. Every `switch` on a tier/kind/verdict must be
  exhaustive (default branch = `panic`/error, never silent).
- No `any` in exported signatures. No unchecked type assertions without the
  `, ok` form (`forcetypeassert` enforces it).
- Config errors fail startup, loudly and completely. A value that does not parse
  never becomes a zero value (a floor of 0 admits everything).
- Never weaken the linter to make a change pass; scope exceptions with a
  `//nolint` and a reason.

## Gate

```bash
gofmt -l .                # must print nothing
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...   # .golangci.yml: standard + forcetypeassert
```

## Layout

| Path | Installed as | Responsibility |
|---|---|---|
| `mem/` | library | memory-size parsing, `MemAvailable` |
| `appauth/` | library | GitHub App RS256 JWT, installation-token cache |
| `dispatcher/` | library | HMAC check, routing by installation id, `v*` tag poke; org mode (`org` section: backup, detection port, mirror dispatch) |
| `cmd/bcx-dispatcher/` | `bcx-dispatcher` | webhook endpoint on `172.17.0.1:9100` |
| `cmd/bcx-broker/` | `bcx-broker` | per-tenant broker on `/run/bcx-broker/<tenant>.sock` |
| `cmd/bcx-reap/` | `bcx-reap` | orphan reaper (oneshot, timer-driven) |
| `cmd/bcx-gen-tenant-env/` | `bcx-gen-tenant-env` | `tenants.yml` → per-tenant env files + `dispatcher.json` |
| `cmd/bcx-appauth/` | `bcx-appauth` | `token --installation-id <int> [--key] [--app-env]` for P3's fetch step and the org's second App |
| `deploytest/` | tests only | `build.sh`, `install.sh`, units, firewall, tiers |

`binaries.txt` is the one manifest of binary names (`role installed-name go-package`);
every `package main` lives under `cmd/`, and `deploytest` enforces it.

## Build & deploy

Metal builds it: Go 1.27.1 at `/usr/local/go`, `./build.sh` in the checkout at
`<checkout>` (pinned to a commit), then `sudo ./install.sh
--tenants <tenants.yml>`; see `DEPLOY.md`. `install.sh` fails closed on a missing
binary and never enables or starts a unit. Building off-host and shipping `bin/` is
still supported (`--bin-dir`) but no longer the path.
