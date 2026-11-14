#!/usr/bin/env bash
# Offline render check for the shared/ overlays.
#
# Builds both overlays, then layers the gke overlay with the same patches the
# mimir-shared-clusters ApplicationSet injects (example-project / gke-prod
# substituted for the identity values) and asserts the project and per-cluster
# paths land where the patches say. No cluster, no credentials (uses standalone
# kustomize, or falls back to the offline `kubectl kustomize`).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  echo "render-check FAIL: $*" >&2
  exit 1
}

if command -v kustomize >/dev/null 2>&1; then
  BUILD=(kustomize build --load-restrictor LoadRestrictionsNone)
elif command -v kubectl >/dev/null 2>&1; then
  BUILD=(kubectl kustomize --load-restrictor LoadRestrictionsNone)
else
  fail "neither kustomize nor kubectl on PATH"
fi

homelab="$("${BUILD[@]}" "$REPO/shared/overlays/homelab")" || fail "homelab overlay does not build"
gke="$("${BUILD[@]}" "$REPO/shared/overlays/gke")" || fail "gke overlay does not build"

if grep -q 'gcs:' <<<"$homelab"; then
  fail "homelab build contains gcs:"
fi
grep -q 'repo2-gcs-key-type: auto' <<<"$gke" || fail "gke build lacks repo2-gcs-key-type: auto"

# Scratch kustomization: gke overlay plus the ApplicationSet's gke patches.
project="example-project"
cluster="gke-prod"
# Inside the repo so the overlay can be referenced relatively (kustomize rejects
# absolute paths under resources:).
scratch="$(mktemp -d "$REPO/.render-check.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT

# The patches come from the ApplicationSet itself, not a copy: take the
# `patches:` block of its templatePatch (up to the closing `{{- end }}`), strip
# the template's 10-space indent, and substitute the two template variables.
appset="$REPO/argocd/apps/shared-clusters.yaml"
{
  cat <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../shared/overlays/gke
EOF
  awk '/^          patches:$/ {on=1} /^    \{\{- end \}\}$/ {on=0} on' "$appset" \
    | sed -e 's/^          //' \
          -e "s/{{ \$project }}/${project}/g" \
          -e "s/{{ \$cluster }}/${cluster}/g"
} >"$scratch/kustomization.yaml"
grep -q '^patches:$' "$scratch/kustomization.yaml" || fail "could not extract patches from $appset"
if grep -q '{{' "$scratch/kustomization.yaml"; then
  fail "unsubstituted template variable in extracted patches"
fi

patched="$("${BUILD[@]}" "$scratch")" || fail "patched gke overlay does not build"

grep -q "example-project.iam.gserviceaccount.com" <<<"$patched" || fail "pgBackRest GSA not from identity project"
grep -q "bucket: example-project-pgbackrest" <<<"$patched" || fail "repo2 bucket not from identity project"
grep -q "repo2-path: /gke-prod/pgbackrest" <<<"$patched" || fail "repo2-path not per-cluster"
grep -q "bucket: example-project-mysql-backups/gke-prod" <<<"$patched" || fail "PXC bucket not per-cluster"
if grep -q teralivekubernetes <<<"$patched"; then
  fail "teralivekubernetes still present after patching"
fi
# The placeholder is a syntactically valid bucket name, so a leak would only
# surface at backup time.
if grep -q set-by-the-shared-clusters-applicationset <<<"$patched"; then
  fail "placeholder set-by-the-shared-clusters-applicationset survived patching"
fi

echo "render-check OK"
