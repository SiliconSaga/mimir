#!/usr/bin/env bash
# Static checks for the operator — the same two CI enforces, so a green run
# here means CI will not fail on formatting or vet.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

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

GO="$(find_go)"
GOFMT="$(dirname "$GO")/gofmt"
cd "$REPO/operator"

unformatted="$("$GOFMT" -l .)"
if [ -n "$unformatted" ]; then
  echo "gofmt would rewrite:" >&2
  echo "$unformatted" >&2
  exit 1
fi

# Both build tags, because the integration tests are a third of the suite and
# are otherwise never compiled — a broken one surfaces only when someone has a
# server to run them against.
"$GO" vet ./...
"$GO" vet -tags integration ./...

echo "ok: gofmt clean, go vet clean (both build tags)"
