# Shared Database Provisioning — Investigation

**Status:** Investigation, NOT a decision. Supersedes the CloudNativePG recommendation in this file's first revision, which was wrong.
**Date:** 2026-08-17
**Related:** [Data Resiliency Plan](data-resiliency-plan.md) · [Requesting a PostgreSQL Database](../../percona/docs/get-postgres-db.md) · yggdrasil `docs/plans/2026-05-15-forgejo-day2-design.md`

## What this document is now

The first revision recommended moving shared Postgres to CloudNativePG on the strength of its `Database` CRD. **That recommendation was withdrawn after checking the part that mattered.** This revision records the evidence — including the evidence against — so the question is not re-opened from scratch a third time.

A prior assessment (months ago, when Mimir was first designed) already compared CNPG against Percona and concluded neither was an ideal fit, with Percona chosen because it covers PostgreSQL, MongoDB and MySQL under one operator family with shared observability and backup tooling. **That conclusion still stands.** Nothing found here overturns it.

## The actual goal

An arbitrary app in the ecosystem needs a database — Postgres, Mongo, MySQL, possibly Kafka or Valkey. It adds **one resource to its own namespace**, and gets back a working database plus either a generated secret or one configured in that same resource.

That is the requirement. Any evaluation has to be judged against it, not against "does Postgres have a per-database CRD".

## Motivation for changing anything at all

A `PostgreSQLInstance` claim currently provisions an **entire dedicated PerconaPGCluster** — roughly four pods per consuming app — regardless of usage. Measured on GKE, 2026-08-17:

| Claim | Namespace | Postgres CPU p95 | Memory p95 |
|---|---|---|---|
| `keycloak-postgres` | keycloak | 77m | 418Mi |
| `ting-pg` | ting | 70m | 336Mi |
| `harbor-postgres` | harbor | 60m | 487Mi |

Three databases, three full clusters, ~12 pods, none meaningfully busy, on a platform intended to fit three small nodes. Forgejo would make it four clusters and ~16 pods. The shared-cluster direction is right; the question is only how to implement it.

## Evidence

All verified against the live cluster on 2026-08-17.

### Percona has no per-database primitive

Four CRDs only: `PerconaPGCluster`, `PerconaPGBackup`, `PerconaPGRestore`, `PerconaPGUpgrade`. Databases and users are declared inside `PerconaPGCluster.spec.users[]`.

That array does declare `x-kubernetes-list-type: map` with `x-kubernetes-list-map-keys: ["name"]`, so in principle independent field managers could each own an entry.

**But `provider-kubernetes` v1.2.0's `Object` has no server-side-apply and no fieldManager** — spec is only `connectionDetails`, `deletionPolicy`, `forProvider`, `managementPolicies`, `providerConfigRef`, `readiness`, `references`, `watch`, `writeConnectionSecretToRef`. Two Objects on one cluster would each rewrite `spec.users` to their own entry and fight. Merge semantics are necessary but not sufficient — the applier must speak SSA.

### CloudNativePG solves the database half — and only that half

CNPG 1.25.0 ships `databases.postgresql.cnpg.io`, required fields `cluster, name, owner`. That looks like the answer, and the first revision of this document treated it as one.

It is not, for three reasons found on closer inspection:

| Check | Result |
|---|---|
| Is there a per-**role** CRD? | **No.** CNPG ships databases, publications, subscriptions, poolers, backups, scheduledbackups, imagecatalogs. No Role. |
| Where do roles live? | `Cluster.spec.managed.roles` — an array with **`x-kubernetes-list-type` unset, i.e. atomic**. |
| Does `Database` create the owner role? | **No.** `owner` "maps to the `OWNER` parameter of `CREATE DATABASE`" — the role must already exist. |
| Does `Database` manage a credential? | **No.** Its entire spec is Postgres DDL options: `allowConnections, builtinLocale, cluster, collationVersion, connectionLimit, databaseReclaimPolicy, encoding, ensure, icuLocale, icuRules, isTemplate, locale, localeCType, localeCollate, localeProvider, name, owner, tablespace, template`. |

So an app still cannot express its need in one namespaced resource. It needs a `Database` **plus** an entry appended to an array on the shared `Cluster` **plus** a Secret managed separately. And that array is **atomic**, which is strictly worse than Percona's — an atomic list cannot be merged per-entry by SSA even by an applier that supports it.

**CNPG is closer than Percona on the database axis and no better on the role axis.** It is not the out-of-the-box model.

### The multi-engine axis, which decides it

CNPG is PostgreSQL only. The requirement spans Postgres, Mongo and MySQL, possibly Kafka and Valkey. Even had the `Database` CRD been everything it first appeared, it would have addressed one engine of three-to-five, while splitting the platform across two Postgres operators and forfeiting the shared observability and backup tooling that motivated choosing Percona originally.

**Adopting CNPG would trade a whole-ecosystem story for a partial win on one engine.** That is the wrong trade, and it is why the earlier assessment's conclusion holds.

## Where that leaves us

No operator surveyed here provides "one namespaced resource → database + credential", for any engine. Percona has no per-database resource at all; CNPG has one that covers the database but not the role or the secret.

**This is why the custom-operator POC exists, and the reasoning behind it looks sound.** A small controller that watches one request CR per app namespace and reconciles database + role + grants + Secret against a shared cluster:

- is a **single writer**, which dissolves the array-contention problem entirely regardless of which operator's arrays it touches — atomic or not;
- can present **one consistent request API across engines**, which no upstream operator does;
- keeps Percona as the substrate, preserving the multi-engine, observability and backup rationale;
- is genuinely small — the hard part is the reconcile loop's idempotency, not the SQL.

The first revision of this document dismissed that idea as "custom code reimplementing a CRD that already exists". **That dismissal was based on a CRD that does not, in fact, do the job.**

## Open questions

1. **Does the earlier POC still exist,** and how far did it get? That should be recovered before anything is re-derived.
2. **Which engines does v1 cover?** Postgres alone is enough to unblock Forgejo; Mongo and MySQL can follow the same shape.
3. **Where does the operator run and what credentials does it hold?** It needs admin access to each shared cluster, which is the main security surface to design deliberately.
4. **Shared cluster sizing**, once the provisioning mechanism is settled — three dedicated clusters request 100m/256Mi each; the shared one needs headroom for the sum of tenants plus connection overhead.
5. **Tenant isolation.** Postgres lets any role connect to any database by default; `REVOKE CONNECT ON DATABASE … FROM PUBLIC` has to be part of the reconcile, not left to convention.
6. **Move the XRD off `database.example.org`** to `mimir.siliconsaga.org` regardless of which path is taken.

## Deferred, with the risk stated

Backups remain out of scope by direction, on the basis that nothing is live. Recorded precisely rather than implied:

- Percona's pgBackRest writes to **`repo1`, a 1Gi local PVC** — no offsite copy. Verified live.
- **Velero's BackupStorageLocation has been `Unavailable` for 149 days**, never finished on either environment.
- **Two PVCs were lost in a single node roll on 2026-08-14** — OpenBao's, unrecoverable, forcing a full re-init; and Harbor's Postgres. Not hypothetical.

Consolidating onto one cluster **concentrates** that exposure: a lost PVC costs one app its database today, and all of them afterwards. Fair while nothing is live, and it is the strongest argument for finishing Mimir Phase 2 before Forgejo becomes load-bearing for GitOps.

## A note on how the first revision went wrong

Worth recording, because the same trap reportedly caught the original Mimir design.

The CNPG `Database` CRD was found, its name and required fields (`cluster, name, owner`) read as exactly the desired model, and it was written up as "the operator-reads-the-request model, natively" — a strong claim, on the strength of a CRD's existence and schema. What was **not** checked before recommending it: whether the owner role is created or merely referenced, whether any credential is managed, whether roles have their own resource, and how any of it interacts with the multi-engine requirement that drove the original operator choice.

The schema was real. The conclusion drawn from it was not supported by it.
