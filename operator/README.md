# DataService operator

Vends a **database inside an existing server** — not a server, and not a cluster.

An app declares one resource in its own namespace and gets back a database, an owning role, and a Secret:

```yaml
apiVersion: mimir.siliconsaga.org/v1alpha1
kind: DataService
metadata:
  name: forgejo
  namespace: forgejo
spec:
  engine: postgres
  placement: shared
  databaseName: forgejo
```

```console
$ kubectl get dsvc -n forgejo
NAME      ENGINE     PLACEMENT  DATABASE  SECRET               PHASE  AGE
forgejo   postgres   shared     forgejo   forgejo-dataservice  Ready  30s
```

The Secret carries `host`, `port`, `database`, `username`, `password` and a ready-assembled `uri`. Its name is reported in `status.secretName`, so consumers never guess — the same contract Strimzi's `KafkaUser` uses.

## Why this exists

Every Mimir claim before this one provisioned a whole cluster: roughly four pods per consuming app, regardless of use. Three Postgres consumers meant three clusters and about twelve pods, for databases measuring 60–77m CPU at p95.

Nothing upstream closes the gap. Percona ships no per-database resource for any of its three engines. CloudNativePG's `Database` creates a database but not the owning role and no credential, and its roles live in an atomic array. `provider-sql` covers Postgres and MySQL but not MongoDB. The full landscape is in [`docs/plans/2026-08-17-vending-databases-not-clusters.md`](../docs/plans/2026-08-17-vending-databases-not-clusters.md); the design is [`2026-08-17-dataservice-vending-design.md`](../docs/plans/2026-08-17-dataservice-vending-design.md).

## Placement

| Value | Meaning |
|---|---|
| `shared` (default) | a database on the platform's shared cluster for that engine |
| `dedicated` | a cluster in the app's own namespace — the pre-existing behaviour |

`dedicated` is accepted by the API but not yet implemented here; it still runs through the `PostgreSQLInstance` claim. The value exists so an app's manifest does not change shape when it moves between the two.

## Version is a platform property

`spec.version` is **rejected** under `placement: shared`, not ignored.

A shared cluster runs one major version for every tenant, so honouring a per-request version there is impossible. Failing the request is honest; silently ignoring it is the dead-config failure mode that looks correct for months. An app that genuinely needs a different major version is telling you it needs `dedicated`, and the API says so.

The platform documents what it serves: **PostgreSQL 15** today.

## Engines

`postgres`, `mysql`, `mongodb` — the three that share a model of *a named database with an owning identity and a credential*.

Only `postgres` has a provisioner in this build. The others are valid enum values that report cleanly rather than crashing, so adding one is implementing `engine.Provisioner` and registering it. The `DataService` API does not change.

**Kafka and Valkey are deliberately excluded.** Kafka vends topics — plural — plus a separate identity carrying ACLs over four resource types and bandwidth quotas; Strimzi's `KafkaTopic`/`KafkaUser` already model that properly. The Valkey operator models no per-tenant object at all, so there is nothing to hand out short of an instance. Forcing either through `databaseName` would lose expressiveness for nothing.

## Tenant isolation

PostgreSQL lets **any role connect to any database by default**. Without intervention, a shared cluster is shared *data*.

Every reconcile therefore runs `REVOKE CONNECT ON DATABASE … FROM PUBLIC` alongside the grant to the owning role. `tests/e2e/dataservice-isolation` asserts it end to end, and insists on the *right* failure — a wrong hostname also fails, and would otherwise look like a pass.

## Configuration

Shared clusters are supplied by environment rather than discovered, so the operator has no opinion about how they are deployed:

| Variable | Default | Notes |
|---|---|---|
| `MIMIR_POSTGRES_HOST` | required | what **consumers** connect to (the pooler) |
| `MIMIR_POSTGRES_PORT` | `5432` | |
| `MIMIR_POSTGRES_ADMIN_HOST` | `=HOST` | where **DDL** runs — the primary |
| `MIMIR_POSTGRES_ADMIN_PORT` | `=PORT` | |
| `MIMIR_POSTGRES_ADMIN_SECRET` | required | `namespace/name` |
| `MIMIR_POSTGRES_ADMIN_USER_KEY` | `user` | |
| `MIMIR_POSTGRES_ADMIN_PASSWORD_KEY` | `password` | |
| `MIMIR_POSTGRES_ADMIN_DATABASE` | `postgres` | |
| `MIMIR_POSTGRES_TLS` | `true` | Percona serves `hostssl` only |

**Admin and consumer endpoints must differ when a pooler is in front.** `CREATE DATABASE` cannot run inside a transaction block, and a pooler in transaction mode wraps every statement in one — so admin traffic goes to the primary while consumers get the pooler. Pointing both at the pooler produces a confusing failure deep inside a reconcile.

An engine with no `HOST` set is simply not configured, and `shared` requests for it report `ClusterNotFound` rather than crashing the operator.

## Developing

```bash
go build ./...
go test ./...

# regenerate after changing api/v1alpha1
controller-gen object paths=./api/v1alpha1/...
controller-gen crd:generateEmbeddedObjectMeta=false paths=./api/v1alpha1/... output:crd:artifacts:config=config/crd
controller-gen rbac:roleName=mimir-dataservice paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
```

Written against controller-runtime directly rather than scaffolded with the kubebuilder CLI, which is awkward on Windows. The layout is the same one kubebuilder produces; `controller-gen` does the generation and works fine.

## Known gaps

- `placement: dedicated` is not implemented here.
- MySQL and MongoDB have no provisioner.
- The shared cluster's pgBackRest repo is a **local PVC with no offsite copy**. Consolidating concentrates that risk: what used to cost one app its database now costs all of them. Mimir Phase 2 is the fix, and it stops being optional once Forgejo is load-bearing for GitOps.
- The admin connection uses `sslmode=require`, not `verify-full` — encrypted, but the server identity is unverified because the operator does not carry the cluster's internal CA.
