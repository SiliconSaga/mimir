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

# gofmt via GOROOT, not as a sibling of the `go` binary.
#
# `dirname "$GO"` is only right when `go` sits directly in its distribution's
# bin/. It is wrong whenever `go` is reached through a symlink or a shim — a
# version manager, or a plain `ln -s ~/.local/go/bin/go ~/.local/bin/go`, which
# is how a machine with no Go package installs one. There is no gofmt beside the
# link, and the script dies with "No such file or directory" pointing at a path
# nobody chose. GOROOT is what the toolchain itself says, so it survives both.
find_gofmt() {
  local goroot
  goroot="$("$GO" env GOROOT 2>/dev/null || true)"
  if [ -n "$goroot" ] && [ -x "$goroot/bin/gofmt" ]; then
    printf '%s' "$goroot/bin/gofmt"
    return
  fi
  if [ -x "$(dirname "$GO")/gofmt" ]; then
    printf '%s' "$(dirname "$GO")/gofmt"
    return
  fi
  if command -v gofmt >/dev/null 2>&1; then
    command -v gofmt
    return
  fi
  echo "gofmt not found (looked in GOROOT, beside $GO, and on PATH)" >&2
  exit 1
}

GOFMT="$(find_gofmt)"
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
#
# The pin has a ceiling as well as a floor, which is less obvious: staticcheck
# reads the toolchain's export data, and a release predating a Go version cannot
# parse it. 2025.1.1 against Go 1.27 does not report "unsupported" — it emits a
# wall of `internal error in importing "container/list" (export data version 4
# is greater than maximum supported version 2)` for stdlib packages, which reads
# like the repo is broken. CI pins Go from go.mod so it never saw this; a
# workstation on a newer toolchain does. Bumped to 2026.2.1, which handles 1.27
# and still reports SA4000 — both verified rather than assumed.
#
# So this pin tracks the newest Go anyone builds with, not just the oldest.
STATICCHECK="honnef.co/go/tools/cmd/staticcheck@2026.2.1"
"$GO" run "$STATICCHECK" ./...
"$GO" run "$STATICCHECK" -tags integration ./...

echo "ok: gofmt clean, go vet + staticcheck clean (both build tags)"
