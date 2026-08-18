# Vending Databases, Not Clusters — Landscape Investigation

**Status:** Investigation. No decision proposed.
**Date:** 2026-08-17
**Supersedes framing in:** [2026-08-17-shared-postgres-design.md](2026-08-17-shared-postgres-design.md)

## The requirement, stated once

An arbitrary app in the ecosystem needs a data service — Postgres, MongoDB, MySQL, possibly Kafka or Valkey. It adds **one resource to its own namespace** and receives a working **database inside an existing server/cluster**, plus a generated Secret (or supplies its own config in that same resource).

Not a cluster. Not a server. A *database instance inside* one.

## What Mimir vends today

Every Mimir claim is cluster-shaped. Verified from the XRDs and the live CRDs:

| Claim kind | Group | What it actually provisions |
|---|---|---|
| `PostgreSQLInstance` | `database.example.org` | a whole `PerconaPGCluster` in the app's namespace |
| `MongoDBInstance` | `database.example.org` | a whole `PerconaServerMongoDB` |
| `MySQLInstance` | `database.example.org` | a whole `PerconaXtraDBCluster` |
| `KafkaCluster` | `mimir.siliconsaga.org` | a whole Strimzi `Kafka` |
| `ValkeyCluster` | `mimir.siliconsaga.org` | a whole Valkey cluster |

Only the Postgres XRD even has a `databaseName` parameter; Mongo and MySQL have no database concept in their claim at all — they expose `storageSize`, `version`, `replicas`.

So Mimir today is **a consistent, tested, Crossplane-fronted way to provision clusters**, with kuttl e2e coverage for all five services and measured provisioning times. That is real and it works. **It is not database vending, and the README's "standard interfaces for Databases, Caches, and Event Buses" overstates what the interfaces currently do.** Three of the five claims still sit on the placeholder `database.example.org` group.

Live cost of the cluster-shaped model, measured 2026-08-17: three Postgres consumers, three full clusters, ~12 pods, at 60–77m CPU p95 each.

## The landscape

Checked against the live cluster where the CRDs are installed, and upstream where they are not.

| Engine | Is there a per-database / per-tenant primitive? | Where it lives |
|---|---|---|
| **Kafka** | **Yes, complete.** `KafkaTopic` (partitions, replicas, config) and `KafkaUser` — and `KafkaUser.status.secret` is literally *"the name of `Secret` where the credentials are stored"* | **Strimzi — already installed here** |
| **PostgreSQL** | **Partial.** CNPG `Database` covers the database but *not* the role or any credential | CloudNativePG |
| **PostgreSQL / MySQL / MSSQL** | **Yes.** `Database`, `Role`/`User`, `Grant`, plus `Extension` and `DefaultPrivileges` on PG | `crossplane-contrib/provider-sql` |
| **MongoDB** | **Yes.** `MongoDBUser`, and the operator writes a connection-string Secret into the resource's namespace | MongoDB's own Enterprise/official operator |
| **Percona (PG, MySQL, Mongo)** | **No. None, for any engine.** Databases and users are fields inside the cluster CR's `spec.users[]` | — |
| **Percona Everest / OpenEverest** | **No.** Its CRDs are `DatabaseCluster`, `DatabaseClusterBackup`, `DatabaseClusterRestore` — a unified *API* across engines, still provisioning clusters | Percona |

### The finding that matters

**Percona — the family chosen precisely because it covers PG, Mongo and MySQL together — is the one family with no per-database vending for any of them.** Every other ecosystem here has some form of it; Strimzi has the complete form and is already running.

That is not a Mimir failure. Mimir was built on the operator that gave the best multi-engine, observability and backup story, and that operator simply does not model a database as an addressable object. The gap is upstream, not local.

Percona's own answer to the DBaaS question, Everest/OpenEverest, confirms the direction: it makes provisioning *clusters* pleasant and consistent across engines rather than making databases addressable. Worth watching — it is heading to the CNCF — but it does not close this gap.

## Strimzi is the reference design, and it is already here

`KafkaTopic` and `KafkaUser` are exactly the shape wanted, in production, in this cluster:

- a **namespaced** CR the app owns,
- targeting a shared cluster (by `strimzi.io/cluster` label),
- carrying only the tenant's own concerns (partitions, ACLs) — not cluster shape,
- with the operator writing credentials to a Secret and reporting its name in `status.secret`.

Any custom operator should copy this interface rather than invent one. It also means **Kafka needs no new work** — Mimir just vends the wrong thing for it today, exposing `KafkaCluster` where `KafkaTopic`/`KafkaUser` would serve.

## What the options actually are

1. **Custom operator (the existing POC direction).** One request CR per app namespace, reconciling database + role + grants + Secret against shared clusters. A **single writer**, so array contention on `spec.users[]` stops mattering — which is decisive, because CNPG's roles array is `atomic` and cannot be merged per-entry by anything. It is the only path that offers **one API across engines** while keeping Percona underneath. Cost: code to own.

2. **provider-sql for the SQL engines.** Covers PG and MySQL properly today — `Database` + `Role`/`User` + `Grant` as independent managed resources, no contention, reconciled every ~10 minutes. v0.9.0, 481 commits, active-ish contrib project. Cost: it holds superuser credentials, it is crossplane-contrib rather than first-party, and **it does nothing for MongoDB** — so this is two of three engines and a different mechanism per engine.

3. **Hybrid.** provider-sql for PG/MySQL now, Strimzi's native CRs for Kafka, custom operator only for Mongo and only if Mongo is actually needed. Fewest lines written; most mechanisms to understand.

4. **Stay cluster-per-app.** Costs ~4 pods per consumer, which is the thing that prompted this.

## Before deciding

- **Recover the earlier POC.** It reportedly exists; it should be read before any of this is re-derived, and its API compared against Strimzi's.
- **Confirm which engines v1 must cover.** If it is Postgres alone for Forgejo, option 2 is materially cheaper. If the goal is genuinely "any app, any engine, one resource", option 1 is the only one that gets there.
- **Decide whether Mongo is real.** Percona PSMDB has no user CRD; MongoDB's official operator does. If Mongo matters, that either drives the custom operator or a second Mongo operator.

## Corrections to the record

The previous document in this directory recommended CloudNativePG on the strength of its `Database` CRD. That recommendation is withdrawn: `Database` references an owner role it does not create, manages no credential, and CNPG has no Role CRD — roles live in an `atomic` array on the shared `Cluster`. It also would have addressed one engine of three, against a multi-engine requirement.
