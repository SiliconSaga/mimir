# DataService Vending — Design

**Status:** Draft, ready for review
**Date:** 2026-08-17
**Builds on:** [Vending Databases, Not Clusters](2026-08-17-vending-databases-not-clusters.md) (landscape + history) · [Shared Postgres investigation](2026-08-17-shared-postgres-design.md) (what was ruled out)

## The requirement

An app adds **one resource to its own namespace** and receives a working database inside an existing server, plus a generated Secret. The type of data service is **a value in that resource**, not a different API per engine. Whether the database lands in a shared cluster or a namespace-local one is **a property the app can toggle**, so the same manifest scales from homelab to GKE without changing shape.

Explicitly not wanted: a cluster per app. That is the current behaviour and it costs ~4 pods per consumer for databases measuring 60–77m CPU p95.

## Why an operator rather than something off the shelf

Established in the landscape investigation and not re-argued here:

- **Percona has no per-database primitive for any of its three engines.** That is the gap, and it is uniform — one consistent thing to build rather than three different partial implementations to paper over. It is also, on reflection, a reason Percona was a sensible choice: a clean space rather than a patchwork.
- CNPG's `Database` references an owner role it does not create, manages no credential, and its roles array is `atomic`. provider-sql covers PG and MySQL but not Mongo, and holds superuser credentials. Percona Everest provisions clusters with a unified API, not databases.
- **Strimzi already ships the target interface** — a namespaced CR, targeting a shared cluster, with the operator writing credentials to a Secret named in `status.secret`. That contract is what this design copies.

**Strimzi dependency risk was checked and is acceptable:** Apache 2.0, CNCF-hosted since 2019 and Incubating since Feb 2024, 1600+ contributors across 180+ organisations. CNCF hosting means the trademark sits with the Linux Foundation and existing code is irrevocably Apache 2.0 — the single-vendor relicensing pattern (Terraform, Elastic, Redis, MongoDB) structurally cannot happen here. Worst case is stagnation, which is a maintenance risk on forkable code.

## Which engines the abstraction covers — and which it must not

The open question was whether one kind with an `engine` field can express every data service, or whether some are too different. **Tested against the live CRDs, and two of the five are genuinely too different.**

| Engine | Unit being vended | Fits one abstraction? |
|---|---|---|
| **PostgreSQL** | database + owning role + credential | **Yes** |
| **MySQL** | database + user + grants | **Yes** |
| **MongoDB** | database (implicit) + user with roles scoped to it | **Yes** — the database is created on first write, so the reconciler's work is the user; that is an implementation difference, not an API one |
| **Kafka** | **topics (plural) + a separate identity with ACLs and quotas** | **No** |
| **Valkey** | — | **No — there is no sub-unit to vend** |

**Kafka does not fit.** `KafkaTopic` carries partitions, replicas and retention config; `KafkaUser` is a *separate* resource carrying an authentication type (`scram-sha-512` / `tls` / `tls-external`), a list of ACL rules over four resource types (`topic`, `group`, `cluster`, `transactionalId`) with eleven operations, and network-bandwidth and CPU quotas. An app normally wants several topics and one identity. None of that maps onto "a database with an owner", and forcing it through `databaseName` would lose expressiveness while gaining nothing — Strimzi's own API is already the right shape and is already installed.

**Valkey does not fit either, in the opposite direction.** The opstreelabs `Redis` CRD is an instance definition — TLS, storage, sidecars, affinity, resources — and the operator models no per-tenant object at all. There is no topic-or-database equivalent to hand out. Vending a Valkey "database" would mean either an instance per app (the measles this design exists to avoid) or inventing a keyspace-prefix and ACL convention the operator does not support and cannot enforce.

**So `engine` is an enum of the three databases: `postgres`, `mysql`, `mongodb`.** Kafka keeps Strimzi's native `KafkaTopic`/`KafkaUser`, which apps can use directly. Valkey keeps its current cluster claim until someone has a real multi-tenant requirement for it.

This is a *reduction* from "all five", and deliberately so — the enum can grow later, but a schema contorted to fit Kafka cannot easily be un-contorted.

## The API

```yaml
apiVersion: mimir.siliconsaga.org/v1alpha1
kind: DataService
metadata:
  name: forgejo
  namespace: forgejo
spec:
  engine: postgres              # postgres | mysql | mongodb
  placement: shared             # shared | dedicated
  databaseName: forgejo         # optional; otherwise derived from namespace + name
  # version is only legal when placement: dedicated — see below
  extensions: [pgcrypto]        # postgres only
status:
  phase: Ready
  secretName: forgejo-dataservice   # the Strimzi contract
  host: mimir-postgres-pgbouncer.mimir.svc
  port: 5432
```

**`placement` is the load-bearing addition.** `shared` vends a database into Mimir's shared cluster for that engine; `dedicated` provisions a cluster in the app's own namespace — which is exactly today's behaviour. So the existing design is not discarded, it becomes a value, and an app can be moved between the two without its manifest changing shape.

This also resolves a tension worth naming: a since-deleted `percona/docs/architecture.md` justified the current model as *"each database instance runs in its own namespace for security and resource isolation."* Under `placement`, that isolation remains available for anything that genuinely needs it, rather than being imposed on everything by default.

**Version is a platform property, not a request parameter.** The platform documents the version it serves for each engine — Postgres X, Mongo Y, MySQL Z — and `spec.version` is **rejected outright when `placement: shared`**. A shared cluster runs one major version for every tenant, so accepting a version field there would be a promise the API cannot keep; failing the request is honest, whereas silently ignoring it is the kind of dead config that looks correct for months. Under `placement: dedicated` the field is honoured, because there the cluster genuinely is the app's own.

That also gives the version-skew story a clean shape: an app that truly needs a different major version is telling you it needs `dedicated`, and the API says so.

**The Secret** carries `host`, `port`, `database`, `username`, `password`, and a ready-assembled connection URI. Its name is reported in `status.secretName` so consumers never guess.

**Optional namespace-local handle:** an `ExternalName` Service in the app's namespace pointing at the shared cluster's Service, so app config can reference a local name regardless of placement. Cheap, and it makes `shared` → `dedicated` a genuinely transparent switch.

## Shape of the implementation

One controller, one request kind, a **per-engine reconciler behind a common interface**:

```text
DataService (one CRD, engine is data)
        │
        ▼
  engine registry ──► postgres reconciler ──► shared PerconaPGCluster
                 ├──► mysql reconciler    ──► shared PerconaXtraDBCluster
                 └──► mongodb reconciler  ──► shared PerconaServerMongoDB
```

Each reconciler ensures: the database exists, the role/user exists with a generated password, grants are correct, the tenant cannot reach other tenants' databases, and the Secret is present and current. Adding an engine is adding a reconciler, not touching the API.

**The operator is a single writer** to each shared cluster's spec. That dissolves the contention problem that blocks every declarative alternative: Percona's `spec.users[]` is a merge-map but `provider-kubernetes` cannot server-side-apply, and CNPG's roles array is atomic and cannot be merged at all. A single writer makes list semantics irrelevant.

## Tenant isolation

Postgres permits any role to connect to any database by default. `REVOKE CONNECT ON DATABASE <db> FROM PUBLIC` must be part of the reconcile, not left to convention — it is the difference between shared-cluster and shared-data. Equivalent care is needed for MySQL grants and Mongo role scoping, and each deserves a test that asserts tenant A *cannot* read tenant B.

## First customer

Forgejo, with `engine: postgres, placement: shared`. The minimum to get there is the shared Postgres cluster, the Postgres reconciler, and the CRD. MySQL and Mongo reconcilers are schema headroom, not v1 work.

## Deferred, with the risk stated

Backups remain out of scope by direction, on the basis that nothing is live. Recorded rather than implied: Percona's pgBackRest writes only to a 1Gi local PVC; Velero's BackupStorageLocation has been `Unavailable` for 149 days; and two PVCs were lost in a single node roll on 2026-08-14, one of them unrecoverable. Consolidating concentrates that exposure from one app to all of them. **Forgejo becoming load-bearing for GitOps is the moment deferring stops being free.**

## Decided

- **The operator lives in the Mimir repo.** Vending data services is Mimir's whole purpose; a separate repo would split the thing from its reason to exist.
- **Version is platform-documented and rejected under `placement: shared`** (above).

## Open questions

1. **How is the shared cluster declared?** Presumably a plain manifest in Mimir, one per engine, with the operator discovering it by name or label rather than by hardcoded reference.
2. **Does the existing `PostgreSQLInstance` claim stay** as a thin alias over `DataService`, or is it retired with its three consumers rebuilt? It is the current cluster-per-app claim, so under the new API it is exactly `DataService{engine: postgres, placement: dedicated}` — which argues for retiring it rather than maintaining two spellings.
3. **Framework.** kubebuilder is the default for a Go controller-runtime operator; anything else needs a reason.
