# Mimir — Data Services (Tier 2 component)

Mimir provisions managed data services (PostgreSQL, MySQL, MongoDB, Kafka, Valkey)
via Crossplane Compositions + Percona operators. Deployed by Nidavellir.

**Full agent context:** [`yggdrasil/CLAUDE.md`](../yggdrasil/CLAUDE.md) and
[`yggdrasil/docs/ecosystem-architecture.md`](../yggdrasil/docs/ecosystem-architecture.md)

**Human-readable docs:** [`README.md`](README.md) and [`docs/cluster-setup.md`](docs/cluster-setup.md)

---

## Key Gotchas

- **Compositions omit `storageClassName`** — uses cluster default (local-path on k3d
  and on homelab, standard-rwo on GKE). Never hardcode a storage class. Longhorn was
  retired on homelab 2026-08-26 and nothing replaced it, so **assume no size
  enforcement there**: local-path ignores a PVC's request and reports the node
  filesystem as its capacity, which is why PVC-fill alerts cannot see a mimir volume
  fill (see `shared/postgres-cluster.yaml`).
- **PG operator `runAsNonRoot`**: chart 2.8.2 hardcodes it with no Helm override.
  Fix is in `argocd/apps/percona-pg-operator/kustomization.yaml` (Kustomize helmCharts
  + JSON patch). Requires `--enable-helm` in ArgoCD kustomize.buildOptions.
- **`ServerSideApply=true`**: Required on all operator ArgoCD Applications — several
  operator CRDs exceed the 262KB annotation limit for client-side apply.
- **`includeCRDs: true`**: Required in the PG operator's Kustomize helmCharts config —
  Kustomize does not include Helm CRDs by default (unlike `helm install`).
- **Valkey operator naming**: The OT-Container-Kit chart is called `redis-operator`
  (it supports both Redis and Valkey). Our ArgoCD app is `mimir-valkey-operator`,
  namespace is `valkey`, Helm release name is `redis-operator`.
