#!/usr/bin/env bash
# Mimir's test entry point.
#
#   scripts/test.sh                     operator unit tests — fast, no cluster
#   scripts/test.sh --integration       + provisioner tests against a real server
#   scripts/test.sh --e2e [suite...]    kuttl e2e in Docker, against a cluster
#
# The default is deliberately cluster-free, because this is what runs before a
# commit. The e2e suite creates namespaces and databases and takes minutes, so
# it is opt-in rather than something a routine `ws test mimir` drags in.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Go is frequently not on PATH in the shells this workspace runs from on
# Windows. Failing with "go: command not found" when the toolchain is sitting
# in the default install location helps nobody.
find_go() {
  if command -v go >/dev/null 2>&1; then
    command -v go
    return
  fi
  for candidate in "/c/Program Files/Go/bin/go" "/usr/local/go/bin/go" "$HOME/go/bin/go"; do
    if [ -x "$candidate" ]; then
      printf '%s' "$candidate"
      return
    fi
  done
  echo "go not found on PATH" >&2
  exit 1
}

mode="unit"
rest=()
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help)
      sed -n '2,10p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    --e2e) mode="e2e" ;;
    --integration) mode="integration" ;;
    *) rest+=("$1") ;;
  esac
  shift
done

case "$mode" in
  e2e)
    exec bash "$REPO/scripts/run-kuttl.sh" ${rest[@]+"${rest[@]}"}
    ;;
  unit)
    GO="$(find_go)"
    cd "$REPO/operator"
    exec "$GO" test ./... -count=1 ${rest[@]+"${rest[@]}"}
    ;;
  integration)
    # The provisioner tests need a real PostgreSQL: identifier quoting can be
    # unit-tested, but "does the server actually refuse a cross-tenant
    # connection after REVOKE CONNECT" is a property of the server.
    if [ -z "${MIMIR_TEST_PG:-}" ]; then
      echo "MIMIR_TEST_PG is not set — the integration tests would skip." >&2
      echo "Start a throwaway server and point at it:" >&2
      echo "  docker run -d --name mimir-pgtest -e POSTGRES_PASSWORD=testadminpw -p 15432:5432 postgres:15" >&2
      echo "  MIMIR_TEST_PG=localhost:15432 scripts/test.sh --integration" >&2
      exit 1
    fi
    GO="$(find_go)"
    cd "$REPO/operator"
    exec "$GO" test -tags integration ./... -count=1 ${rest[@]+"${rest[@]}"}
    ;;
esac
