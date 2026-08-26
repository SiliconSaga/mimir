#!/usr/bin/env bash
# Run the kuttl e2e suite inside Docker, against a local cluster.
#
# kuttl ships no native Windows binary, so the tests run in the official image
# with the repo mounted. This is the portable companion to test.ps1 — same
# approach, runnable from bash on Windows, macOS and Linux.
#
# The kubeconfig needs three edits before the container can use it:
#
#   * 127.0.0.1/localhost rewritten to host.docker.internal, so the container
#     reaches the host's API server rather than its own loopback
#   * the CA dropped, because the server certificate does not cover the
#     rewritten hostname
#   * flattened, since the container cannot follow file references out of it
#
# Usage:
#   scripts/run-kuttl.sh                          # whole suite
#   scripts/run-kuttl.sh dataservice-isolation    # one suite, by directory name
#   scripts/run-kuttl.sh --test=dataservice-isolation
#   scripts/run-kuttl.sh --context=k3d-dev --skip-delete
#
# Environment:
#   KUTTL_CONTEXT       kubeconfig context to target (default: current)
#   KUTTL_ALLOW_REMOTE  set to 1 to permit a non-local cluster (see below)
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CTX="${KUTTL_CONTEXT:-}"
args=()

while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help)
      sed -n '2,26p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    --context=*) CTX="${1#*=}" ;;
    --context)
      CTX="${2:-}"
      shift
      ;;
    # A bare directory name under tests/e2e is the common case, and typing
    # `--test` for it every time is friction with no upside.
    -*) args+=("$1") ;;
    *)
      if [ -d "$REPO/tests/e2e/$1" ]; then
        args+=(--test "$1")
      else
        args+=("$1")
      fi
      ;;
  esac
  shift
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

kubectl config view --raw --flatten >"$WORK/full"
export KUBECONFIG="$WORK/full"

if [ -z "$CTX" ]; then
  CTX="$(kubectl config current-context)"
fi
kubectl config use-context "$CTX" >/dev/null

# Refuse a cluster this script cannot legitimately be pointed at.
#
# Two independent reasons, and either alone would be enough. The mechanical
# one: rewriting the API server to host.docker.internal only works when the
# server is on this host, so a remote cluster cannot be reached this way in the
# first place. The one that matters more: this suite creates and destroys
# namespaces, and a workstation commonly has a production context selected
# while local work happens elsewhere. Defaulting to "whatever is current" and
# discovering the mistake afterwards is not a recoverable error.
SERVER="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
case "$SERVER" in
  # The brackets around ::1 are escaped: unescaped they would be read as a
  # bracket expression matching a single ':' or '1', which quietly refuses a
  # perfectly local IPv6 loopback.
  *//127.0.0.1:*|*//localhost:*|*//\[::1\]:*|*//0.0.0.0:*|*//host.docker.internal:*) ;;
  *)
    if [ "${KUTTL_ALLOW_REMOTE:-}" != "1" ]; then
      echo "refusing to run e2e against a non-local cluster" >&2
      echo "  context: $CTX" >&2
      echo "  server:  $SERVER" >&2
      echo "This suite creates and destroys namespaces. Pass --context=<local>," >&2
      echo "set KUTTL_CONTEXT, or set KUTTL_ALLOW_REMOTE=1 if you truly mean it." >&2
      exit 1
    fi
    echo "WARNING: KUTTL_ALLOW_REMOTE=1 — running against $SERVER" >&2
    ;;
esac

echo "target: context=$CTX server=$SERVER"
echo "nodes:  $(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}')"

kubectl config view --minify --flatten >"$WORK/kubeconfig"
sed -i -e 's/127\.0\.0\.1/host.docker.internal/g' \
       -e 's/localhost/host.docker.internal/g' \
       -e 's/certificate-authority-data:.*/insecure-skip-tls-verify: true/' \
       "$WORK/kubeconfig"

# Git Bash rewrites anything that looks like a POSIX path in a Windows process's
# argv, which mangles the container-side halves of these -v pairs. cygpath
# converts the host-side halves; MSYS_NO_PATHCONV leaves the rest alone. Both
# are no-ops off Windows.
hostpath() { cygpath -w "$1" 2>/dev/null || printf '%s' "$1"; }

# kuttl resolves testDirs relative to CWD, and a symlinked tree keeps the
# container from writing into the mounted repo.
MSYS_NO_PATHCONV=1 docker run --rm \
  -v "$(hostpath "$WORK/kubeconfig")":/kubeconfig \
  -v "$(hostpath "$REPO")":/workspace \
  -e KUBECONFIG=/kubeconfig \
  --add-host host.docker.internal:host-gateway \
  --entrypoint /bin/sh \
  kudobuilder/kuttl:latest \
  -c "mkdir -p /tmp/work && cp /workspace/kuttl-test.yaml /tmp/work/ && ln -s /workspace/tests /tmp/work/tests && ln -s /usr/bin/kubectl /tmp/work/kubectl && cd /tmp/work && kubectl-kuttl test --config kuttl-test.yaml ${args[*]+${args[*]}}"
