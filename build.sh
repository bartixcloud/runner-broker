#!/usr/bin/env bash
# build.sh - build the bcx binaries named in binaries.txt into bin/, statically,
# off the metal host; install.sh then ships bin/ to it.
#
#   build.sh [--only <role>]
#
#   --only  build just the binary with this role in binaries.txt
#   -h      print this header and exit 0
#
# Environment
#   BCX_GOOS, BCX_GOARCH  build target; default linux / amd64 (the metal host)
#
# Exit codes
#   0   every selected binary was built
#   2   bad arguments, or a package directory does not exist yet; nothing was built
#   *   go build's own exit code when a build fails
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ONLY=""

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
  echo "build.sh: $*" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --only)
      [ -n "${2:-}" ] || die "--only needs a role"
      ONLY="$2"
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

# go build resolves ./cmd/... and bin/ relative to the module root.
cd "$HERE"

ROLES=""
SELECTED=""
NL='
'
while read -r role name pkg extra || [ -n "$role" ]; do
  case "$role" in ''|'#'*) continue ;; esac
  if [ -z "$name" ] || [ -z "$pkg" ] || [ -n "$extra" ]; then
    die "bad line in binaries.txt: $role $name $pkg $extra"
  fi
  ROLES="$ROLES $role"
  if [ -z "$ONLY" ] || [ "$ONLY" = "$role" ]; then
    SELECTED="$SELECTED$name $pkg$NL"
  fi
done < binaries.txt

if [ -n "$ONLY" ]; then
  case " $ROLES " in
    *" $ONLY "*) ;;
    *) die "unknown role: $ONLY (roles:$ROLES)" ;;
  esac
fi

# All-or-nothing: a half-built bin/ would install a mix of old and new binaries.
missing=0
while read -r name pkg; do
  [ -n "$name" ] || continue
  if [ ! -d "$pkg" ]; then
    echo "build.sh: package directory not found: $pkg (needed for $name)" >&2
    missing=1
  fi
done <<< "$SELECTED"
if [ "$missing" = 1 ]; then exit 2; fi

GOOS_T="${BCX_GOOS:-linux}"
GOARCH_T="${BCX_GOARCH:-amd64}"
while read -r name pkg; do
  [ -n "$name" ] || continue
  echo "build: bin/$name <- $pkg ($GOOS_T/$GOARCH_T)"
  # go must not read the here-string that feeds this loop.
  CGO_ENABLED=0 GOOS="$GOOS_T" GOARCH="$GOARCH_T" \
    go build -trimpath -buildvcs=false -ldflags='-s -w' -o "bin/$name" "$pkg" < /dev/null
done <<< "$SELECTED"
