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

# staticcheck on top of vet, for the class of defect this component keeps
# producing: an assertion that cannot fail.
#
# A test here compared a derived name against itself — necessarily equal, so it
# asserted nothing — and it took a human reading the diff to notice. `go vet` is
# silent on that shape; staticcheck reports it as SA4000. Confirmed both ways
# before adding this, rather than assumed.
#
# It cannot catch the semantic version of the same mistake — "this counter can
# actually move", "this refusal is for the right reason" — which is why the
# integration tests carry explicit negative controls. This closes the half a
# machine can see.
#
# Pinned rather than @latest so a new release cannot fail the build on a day
# nobody changed the code. `go run` caches the module after the first fetch.
STATICCHECK="honnef.co/go/tools/cmd/staticcheck@2025.1.1"
"$GO" run "$STATICCHECK" ./...
"$GO" run "$STATICCHECK" -tags integration ./...

echo "ok: gofmt clean, go vet + staticcheck clean (both build tags)"
