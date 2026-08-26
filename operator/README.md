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

## Naming and ownership

Two DataServices must never end up sharing one physical database. On a per-app cluster the object's name was unique enough because the cluster was; on a shared cluster nothing else disambiguates.

**When `spec.databaseName` is unset the name is derived from namespace *and* name** — `team-a/app` becomes `team_a_app`. Kubernetes names are DNS labels, so hyphens and dots become underscores and a leading digit gets a prefix. Long names are truncated with a hash of the *full* input, because plain truncation would reintroduce the collision the derivation exists to prevent.

An explicit `spec.databaseName` is still honoured, so that path is protected differently: **every provisioned database carries an ownership marker**, written as a `COMMENT ON DATABASE` of `mimir-dataservice:<namespace>/<name>`. Before touching anything, `Ensure` reads it back.

- Marker matches → proceed.
- Marker belongs to another DataService, or is absent because the database was made by hand → **refuse**, and report `Conflict`.

Refusing is the only safe answer. Adopting would hand one tenant another's data; dropping would destroy data the operator never created. It needs a person.

The ownership check runs **before any mutation**, and the order is load-bearing: an earlier version set the role password first and only then discovered the database was not ours, which broke a working tenant on the way to reporting the conflict.

## Extensions

`spec.extensions` is an **allowlist**, not free text. `CREATE EXTENSION` runs with administrative rights, and on a shared cluster that is a privilege boundary — several contrib extensions can read the filesystem or run code as the server user, reaching every other tenant.

Allowed: `btree_gin`, `btree_gist`, `citext`, `hstore`, `intarray`, `ltree`, `pg_stat_statements`, `pg_trgm`, `pgcrypto`, `unaccent`, `uuid-ossp`, `vector`.

Anything else is rejected with the list in the error. Adding to it is a deliberate review, in `allowedExtensions`.

## Deleting a DataService

The finalizer drops the database, and is **held if the drop fails** — an unreachable cluster leaves the object deleting rather than releasing. Releasing automatically would orphan the tenant's data on the shared cluster, where it then blocks the next request for that name with a conflict nobody can explain.

To delete anyway, accepting the orphan:

```bash
kubectl annotate dsvc <name> mimir.siliconsaga.org/force-delete=true
```

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
| `MIMIR_POSTGRES_POOLER_AUTH_ROLE` | `_crunchypgbouncer` | role the pooler authenticates as; `""` disables the bootstrap |

**Vended databases must admit the pooler's own role, or the published URI cannot connect.** pgBouncer does not verify passwords itself: it connects as `POOLER_AUTH_ROLE` into the database the client named and runs an `auth_query` there. A database this operator creates revokes `CONNECT` from `PUBLIC` — that revoke is what makes the shared cluster multi-tenant — and the revoke catches the pooler's role too. So each database is granted `CONNECT` for that role plus a `pgbouncer.get_auth()` lookup function, scoped to it alone.

Two things make that safe to default on. The bootstrap first checks whether the role exists and does nothing when it does not, so pointing this operator at a server with no such pooler is a no-op. And the function is `SECURITY DEFINER` living in a database the *tenant* owns, so it is created with `SET search_path = pg_catalog` and fully qualified references — without that, a tenant could shadow `pg_catalog` and have it run with admin rights.

The failure mode this fixes was silent: pgBouncer reports the failed lookup to the client as `permission denied for database "x"`, which is exactly what a correctly refused cross-tenant attempt looks like.

**Admin and consumer endpoints must differ when a pooler is in front.** `CREATE DATABASE` cannot run inside a transaction block, and a pooler in transaction mode wraps every statement in one — so admin traffic goes to the primary while consumers get the pooler. Pointing both at the pooler produces a confusing failure deep inside a reconcile.

An engine with no `HOST` set is simply not configured, and `shared` requests for it report `ClusterNotFound` rather than crashing the operator.

## Deployment

Two ArgoCD Applications, both registered in `argocd/kustomization.yaml`:

| Application | Path | Prune |
|---|---|---|
| `mimir-dataservice-operator` | `operator/config` | yes |
| `mimir-shared-clusters` | `shared` | **no** |

They are split so that pruning the operator can never reach a database. Under `prune: true`, deleting or renaming a file in `shared/` would delete a running cluster — and after consolidation that is not one app's data, it is every tenant's. `selfHeal` stays on for both, since correcting drift on an existing cluster is safe.

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

Generated output is **committed**, because ArgoCD applies `config/` directly and has no generation step. CI regenerates and diffs, so a hand-edited CRD that no longer matches the Go types fails the build rather than reaching the cluster.

## Building and releasing

`.github/workflows/operator.yml` builds `ghcr.io/siliconsaga/mimir-dataservice` on any push to `main` touching `operator/**`. Pull requests run the tests but publish nothing.

Three tags are pushed: the commit SHA, the contents of `VERSION`, and `latest`. **The SHA tag is the immutable one.** Pre-1.0 the version tag is republished in place whenever the operator changes, so it identifies a line of development rather than a fixed artifact — pin a SHA when a rollback has to be exact.

`config/deploy/operator.yaml` pins the `VERSION` tag, and CI fails if the two disagree. Releasing is therefore one commit that bumps both: the workflow publishes the new tag, ArgoCD sees the changed manifest, and the rollout happens because the *manifest* moved rather than because a tag quietly did.

```bash
docker build -t ghcr.io/siliconsaga/mimir-dataservice:dev .   # local check
```

The runtime image is `distroless/static:nonroot` — no shell, no package manager. `kubectl exec` into it will not work, which is intentional; diagnose from logs, events and `status.conditions`.

## Known gaps

- `placement: dedicated` is not implemented here.
- MySQL and MongoDB have no provisioner.
- The shared cluster's pgBackRest repo is a **local PVC with no offsite copy**. Consolidating concentrates that risk: what used to cost one app its database now costs all of them. Mimir Phase 2 is the fix, and it stops being optional once Forgejo is load-bearing for GitOps.
- The admin connection uses `sslmode=require`, not `verify-full` — encrypted, but the server identity is unverified because the operator does not carry the cluster's internal CA.
