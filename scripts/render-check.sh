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

cat >"$scratch/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../shared/overlays/gke
patches:
  - target: {group: pgv2.percona.com, version: v2, kind: PerconaPGCluster, name: mimir-postgres}
    patch: |-
      - op: replace
        path: /spec/metadata/annotations/iam.gke.io~1gcp-service-account
        value: pgbackrest@${project}.iam.gserviceaccount.com
      - op: replace
        path: /spec/backups/pgbackrest/repos/1/gcs/bucket
        value: ${project}-pgbackrest
      - op: add
        path: /spec/backups/pgbackrest/global/repo2-path
        value: /${cluster}/pgbackrest
  - target: {group: pxc.percona.com, version: v1, kind: PerconaXtraDBCluster, name: mimir-mysql}
    patch: |-
      - op: replace
        path: /spec/backup/storages/gcs/s3/bucket
        value: ${project}-mysql-backups/${cluster}
EOF

patched="$("${BUILD[@]}" "$scratch")" || fail "patched gke overlay does not build"

grep -q "example-project.iam.gserviceaccount.com" <<<"$patched" || fail "pgBackRest GSA not from identity project"
grep -q "bucket: example-project-pgbackrest" <<<"$patched" || fail "repo2 bucket not from identity project"
grep -q "repo2-path: /gke-prod/pgbackrest" <<<"$patched" || fail "repo2-path not per-cluster"
grep -q "bucket: example-project-mysql-backups/gke-prod" <<<"$patched" || fail "PXC bucket not per-cluster"
if grep -q teralivekubernetes <<<"$patched"; then
  fail "teralivekubernetes still present after patching"
fi

echo "render-check OK"
