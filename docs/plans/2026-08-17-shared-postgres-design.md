# Shared PostgreSQL Cluster — Design

**Status:** Draft, ready for plan
**Date:** 2026-08-17
**Related:** [Data Resiliency Plan](data-resiliency-plan.md) · [Requesting a PostgreSQL Database](../../percona/docs/get-postgres-db.md) · yggdrasil `docs/plans/2026-05-15-forgejo-day2-design.md`

## Overview

A `PostgreSQLInstance` claim currently provisions an **entire dedicated PerconaPGCluster** into the claiming namespace. This design changes it to provision **a database on one shared cluster**, which is what the claim's name and its own documentation already imply.

The claim API does not change. What changes is what it builds underneath — and, less obviously, **which operator builds it**.

## Motivation

The current model costs roughly four pods per consuming app — a Postgres instance, a pgBouncer, a pgBackRest repo-host, plus backup jobs — regardless of how little that app uses its database.

Measured on GKE, 2026-08-17:

| Claim | Namespace | Postgres CPU p95 | Memory p95 |
|---|---|---|---|
| `keycloak-postgres` | keycloak | 77m | 418Mi |
| `ting-pg` | ting | 70m | 336Mi |
| `harbor-postgres` | harbor | 60m | 487Mi |

Three databases, three full clusters, ~12 pods, none meaningfully busy. This cluster is meant to fit on three small nodes; it is currently on five with one at 99% of its CPU requests. Per-app clusters scale that overhead linearly with app count, which is the wrong direction for infrastructure that is sparse by design. Adding Forgejo would make it four clusters and ~16 pods.

## Goals

1. **One Postgres cluster, many databases.** A claim yields a database on the shared cluster, not a new cluster.
2. **Keep the claim API.** `PostgreSQLInstance` with `databaseName` stays the interface.
3. **Forgejo is the first customer**, and the design must be proven before Forgejo depends on it.
4. **No data migration.** Existing consumers are torn down and rebuilt, not migrated.
5. **Move off the placeholder API group.** `database.example.org` becomes `mimir.siliconsaga.org`, matching Kafka and Valkey.

## The central question

"Give me a database" has to become "a database on the one existing cluster". Something must watch those requests and act on the shared cluster. Three findings, all verified against the live cluster, decide what that something is.

### Finding 1 — Percona has no per-database primitive

The Percona PG operator ships exactly four CRDs: `PerconaPGCluster`, `PerconaPGBackup`, `PerconaPGRestore`, `PerconaPGUpgrade`. **There is no per-database resource.** The only way to add a database is to append to the shared cluster's `spec.users[]` array.

### Finding 2 — that array cannot be shared between claims

`PerconaPGCluster.spec.users` does declare `x-kubernetes-list-type: map` with `x-kubernetes-list-map-keys: ["name"]`, so in principle several field managers could each own their own entry.

But **`provider-kubernetes` v1.2.0's `Object` has no server-side-apply support** — its entire spec surface is `connectionDetails`, `deletionPolicy`, `forProvider`, `managementPolicies`, `providerConfigRef`, `readiness`, `references`, `watch`, `writeConnectionSecretToRef`. It applies whole manifests, so two Objects pointed at one `PerconaPGCluster` would each rewrite `spec.users` down to only their own entry and fight indefinitely.

Merge semantics are necessary but not sufficient: the applier must speak SSA, and this one does not. **On Percona, the shared model needs either a custom controller or a third-party SQL provider.**

### Finding 3 — CloudNativePG already has exactly this primitive

CloudNativePG ships `databases.postgresql.cnpg.io`, whose spec requires precisely three fields:

```
required: [cluster, name, owner]
```

*"Create database `name`, owned by `owner`, on cluster `cluster`."* Plus `ensure: present|absent`, `databaseReclaimPolicy` for end-of-life behaviour, `connectionLimit`, `allowConnections`, encoding and locale.

**This is the operator-reads-the-request model, natively.** No custom controller, no SQL provider holding superuser credentials, no server-side-apply problem — because each request is its own object rather than an entry in a shared array.

CNPG also ships `Pooler` (pgBouncer), `ScheduledBackup`, `Backup`, `Publication` and `Subscription`.

## The consequence: promote CNPG, retire Percona PG

The existing `tera-cnpg-cluster` has been running **571 days, 3 instances, "Cluster in healthy state"** — the oldest and healthiest Postgres on the platform. It has been mentally filed as legacy to be retired.

**That is backwards.** CloudNativePG is the operator with the right model, a CNCF project with a strong maintenance record, first-class object-store backups via Barman, and a per-database CRD that Percona simply does not have. Percona PG is the one that should go.

This is the single biggest decision in this design, and it was not the expected outcome — the investigation was scoped as "how do we make Percona do shared databases" and the honest answer turned out to be "use the operator that already does".

Percona's MongoDB and MySQL operators are unaffected and stay.

## Design

```text
  app namespace                       cnpg namespace
  ┌─────────────────────────┐         ┌────────────────────────────────┐
  │ PostgreSQLInstance      │         │ Cluster "mimir-postgres"       │
  │   databaseName: forgejo │         │   - N instances                │
  └───────────┬─────────────┘         │   - Pooler (pgBouncer)         │
              │ composed into          │   - ScheduledBackup (later)   │
              ▼                        └───────────────┬────────────────┘
  ┌─────────────────────────┐  cluster: mimir-postgres  │
  │ Database (cnpg)         │───────────────────────────┘
  │   name: forgejo         │      operator does CREATE DATABASE
  │   owner: forgejo        │
  └───────────┬─────────────┘
              │
              ▼
  ┌─────────────────────────┐
  │ Secret in app namespace │  host = pooler svc, db/user = forgejo
  └─────────────────────────┘
```

**The shared cluster** is one CNPG `Cluster` owned by Mimir and declared in its own manifests — not composed per claim. It absorbs the resources three separate clusters used to hold, which is a net reduction rather than a like-for-like move.

**The claim composition** stops creating a cluster. It composes a CNPG `Database` naming the shared cluster, a role for the owner, and a `Secret` projected into the claiming namespace carrying host, port, database, user and password. Each claim owns its own `Database` object, so there is no shared mutable array and no contention.

**Connections** go through the shared `Pooler`. Worth noting for whichever pooler ends up in front: the Percona-generated pgBouncer config uses a wildcard route (`* = host=<primary>`) with `auth_query` resolved live against the database, so a pooler does not need to be told about individual tenant databases — routing and auth work as soon as the database and role exist.

### Alternatives considered

| Approach | Verdict |
|---|---|
| **CNPG `Cluster` + per-claim `Database`** | **Chosen.** Native, no custom code, no extra credentials, proper lifecycle via `ensure`/`databaseReclaimPolicy`. |
| Custom config operator watching claims, owning Percona's `users[]` | Correct and was the prior direction — a single writer sidesteps the SSA problem entirely. Rejected only because CNPG makes it unnecessary: this is custom code to reimplement a CRD that already exists. |
| `provider-sql` `Database`/`Role`/`Grant` against Percona | Workable, but adds a thinner-maintained contrib provider holding superuser credentials, and diverges `spec.users` from reality. |
| Append to `spec.users[]` via SSA | Blocked — provider-kubernetes cannot server-side-apply. |
| Declare all databases in Mimir's values | Simple and contention-free, but retires self-service: adding a database becomes a git change to Mimir. Reasonable fallback. |

## Forgejo as first customer

Forgejo is a good proving case: it needs a real database, it is being built now, and it has no legacy data. The Forgejo day-2 design already says "Postgres — Mimir-vended via Crossplane claim", so that design does not change; it simply receives a database on the shared cluster.

Sequence: size and stand up the shared CNPG cluster → cut the composition over → prove it with Forgejo → then rebuild keycloak, ting and harbor against it.

## Rebuild, don't migrate

Existing consumers are rebuilt rather than migrated, on the explicit basis that nothing is live. For each: delete the claim, let its dedicated cluster tear down, re-create against the new composition, let the app re-initialise its schema.

Harbor went through exactly this cycle on 2026-08-17 — claim deleted, cluster and PVCs torn down, ArgoCD rebuilt it to 11 pods and 5 PVCs — so the pattern is proven before it is relied upon.

## Deferred, with the risk stated

Backups are **not** part of this design, by explicit direction, on the grounds that nothing is live and the architecture matters more right now. That is defensible, but the exposure should be recorded rather than left implicit:

- Percona's pgBackRest currently writes to **`repo1`, a 1Gi local PVC** — no offsite copy. Verified live.
- **Velero's BackupStorageLocation has been `Unavailable` for 149 days**; it was never finished on either environment.
- **Two PVCs were lost in a single node roll on 2026-08-14** — OpenBao's (unrecoverable, forced a full re-init) and Harbor's Postgres. Not hypothetical: it happened twice this week.

Consolidating **concentrates** that exposure — today a lost PVC costs one app its database; afterwards it costs all of them. Fair while nothing is live, and it becomes the strongest argument for finishing Mimir Phase 2 before anything real lands. **Forgejo becoming load-bearing for GitOps is precisely that moment**, since a Forgejo outage takes GitOps with it.

Choosing CNPG helps here too: Barman object-store backup is native and considerably better-trodden than the Percona path, so Phase 2 gets easier rather than harder.

Percona's own resiliency behaviour has never been inspected; that todo is superseded for Postgres if Percona PG retires, but still stands for MongoDB and MySQL.

## Follow-ups

- Move the XRD from `database.example.org` to `mimir.siliconsaga.org`, matching `xkafkaclusters` and `xvalkeyclusters`.
- Retitle the docs: "Requesting a PostgreSQL Database" currently describes provisioning a cluster.
- Retire the Percona PG operator once its three consumers are rebuilt on CNPG. Keep PSMDB and PXC.
- Fold the bundled Postgres instances (Gitea, Artifactory, Backstage, Nautobot, Sonar) onto the shared cluster where the chart supports an external database. Gitea's is the one that matters, since Forgejo replaces it.
- Consider the same shared-instance treatment for MongoDB and MySQL — but check first whether their operators have a per-database primitive, because that is what decided this one.

## Open questions

1. **Shared cluster sizing.** Three dedicated clusters request 100m/256Mi each for their instance. The shared one needs headroom for the sum of tenants plus connection overhead, not simply one instance's worth. Settle against measured p95 in the plan.
2. **Reuse `tera-cnpg-cluster` or stand up a fresh one?** It is healthy and 571 days old, but it is named for a specific consumer and its provenance predates this design. Leaning towards a fresh, properly named cluster so the shared one is deliberate rather than inherited.
3. **Namespace.** `mimir` alongside the vending layer, or a dedicated `postgres`/`cnpg` namespace?
4. **Tenant isolation.** Postgres lets any role connect to any database by default. `REVOKE CONNECT ON DATABASE ... FROM PUBLIC` needs to be part of the composition rather than left to convention — check whether CNPG's `Database` exposes this or whether it needs a follow-on step.
5. **Version.** Existing clusters are PostgreSQL 15. Pick the shared cluster's major version deliberately, since moving it later affects every tenant at once.
