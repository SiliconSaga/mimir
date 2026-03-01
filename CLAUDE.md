# Mimir — Data Services (Tier 2 component)

Mimir provisions managed data services (PostgreSQL, MySQL, MongoDB, Kafka, Valkey)
via Crossplane Compositions + Percona operators. Deployed by Nidavellir.

**Full agent context:** [`yggdrasil/CLAUDE.md`](../yggdrasil/CLAUDE.md) and
[`yggdrasil/docs/ecosystem-architecture.md`](../yggdrasil/docs/ecosystem-architecture.md)

---

## Active GKE Blockers

Mimir does not currently run on GKE. Two issues must be resolved first:

- **mimir#1** — Remove hardcoded `storageClassName: local-path` from all Compositions;
  use cluster default instead. Exceptions: workloads needing explicit node-local storage
  (OpenClaw, Obsidian) keep `storageClassName: local-path`.
- **mimir#2** — Add Kustomize post-renderer to the ArgoCD Application to patch out
  `runAsNonRoot: true` from the Percona PG operator Helm chart (hardcoded, no values
  override). MySQL and MongoDB operators are not affected.

`mimir-app.yaml` is commented out in `nidavellir/apps/kustomization.yaml` until resolved.

---

## Key Gotchas

- **Storage class**: GKE default is `standard`/`standard-rwo`; homelab default should be
  Longhorn (k3s ships `local-path` as default — may need to patch Longhorn as default and
  unset local-path).
- **Percona operators**: only the PG operator has the `runAsNonRoot` issue; MySQL/MongoDB
  operators are unaffected.
