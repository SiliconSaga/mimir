# Offsite backups for the shared data clusters

Both shared clusters now reach GCS, but by **different mechanisms**, and the
difference is not a style choice — see
[MySQL](#offsite-backups-for-the-shared-mysql-cluster) below.

| Cluster | Off-site target | Auth | Scheduled? |
| --- | --- | --- | --- |
| Postgres | `gs://teralivekubernetes-pgbackrest` | Workload Identity, keyless | yes, weekly full |
| MySQL | `gs://teralivekubernetes-mysql-backups` | **HMAC key** via S3 interop | yes, daily 02:00, keep 7 |

## Offsite backups for the shared Postgres cluster

The shared cluster (`shared/postgres-cluster.yaml`) backs up to two pgBackRest
repositories:

| Repo | Where | Why |
| --- | --- | --- |
| `repo1` | 40Gi PVC in-cluster | fast restore, cheap. 1 weekly full + the latest diff; point-in-time recovery back 0–7 days. |
| `repo2` | `gs://teralivekubernetes-pgbackrest` | survives losing the cluster or the zone. 2 weekly fulls + 3 diffs; point-in-time recovery back 7–14 days. |

repo1 alone is not a backup story. Consolidating every tenant onto one shared
Postgres concentrates the blast radius: what used to cost one app its database
now costs all of them, and a PVC does not outlive the cluster it lives in.

repo2 is written by pgBackRest itself on its own schedule — deliberately *not*
the Valheim shape of a separate job that copies files to a bucket. A second
scheduler cannot see the first, which is exactly how the Valheim off-cluster
uploads silently did nothing for months.

## What is NOT in Git

The manifest is declarative, but the GCP side of repo2 is not. These resources
are created by hand and a cluster rebuild will not recreate them. If repo2 ever
reports `error (missing stanza path)` on a fresh cluster, start here.

The project is not in Git either: the `mimir-shared-clusters` ApplicationSet
reads it from the cluster identity (the `nordri.siliconsaga.org/gcp-project`
annotation on the `in-cluster` Secret, i.e. the identity's `gcpProject`) and
patches the GSA, the repo2 bucket and the MySQL bucket. Names below use
`teralivekubernetes` as the current project. Paths are per cluster:
`/<cluster>/pgbackrest` inside the Postgres bucket (`repo2-path`) and
`<bucket>/<cluster>` for MySQL.

```
bucket   gs://teralivekubernetes-pgbackrest      us-east1, uniform access
GSA      pgbackrest@teralivekubernetes.iam.gserviceaccount.com
grant    roles/storage.objectAdmin, scoped to that bucket only
binding  roles/iam.workloadIdentityUser on the GSA, for TWO KSAs
```

### After a repo2-path change

On gke-prod only `repo2-path` moves; the bucket stays. Whether the operator
re-runs `stanza-create` for the new path is **not verified** — it may only do so
when the repo spec (the bucket) changes, not when a `global` option does. Check
rather than assume, because an absent stanza is not cosmetic: `archive-push`
fails for repo2, Postgres retains every unpushed WAL segment in `pg_wal`, and the
PVC slowly fills until the shared database cluster goes down.

```bash
ws k8s exec -n mimir mimir-postgres-repo-host-0 -c pgbackrest -- pgbackrest info --repo=2
```

If the stanza is absent for the new path, create it (on the repo host, or the
instance pod, whichever has the repo2 config):

```bash
pgbackrest stanza-create --stanza=db --repo=2
```

If `info` already shows the stanza, do nothing. Either way, watch the size of
`pg_wal` on the instance pod for a day or so, and watch for an
`UnableToCreateStanzas` event on the PerconaPGCluster. Run this check before
deleting anything from the old locations.

### The two-KSA trap

This is the part that is easy to get wrong, and it fails in a way that looks
like success.

`spec.metadata.annotations` in the manifest puts the
`iam.gke.io/gcp-service-account` annotation on every object the operator
manages. Two of the resulting KSAs actually use it:

- **`mimir-postgres-pgbackrest`** — the repo host. Runs the backups.
- **`mimir-postgres-instance`** — the Postgres pod. Runs `archive-push`, so
  every WAL segment reaches GCS from here, and it is also where the operator
  execs `stanza-create`.

Bind only the first and everything *looks* fine: a hand-run
`pgbackrest stanza-create` on the repo host succeeds, the stanza objects appear
in the bucket, and `pgbackrest info` reports repo2 healthy. But WAL never
arrives, so every scheduled backup fails with

```
ERROR: [099]: ... IAM returned 403 Forbidden:
Permission 'iam.serviceAccounts.getAccessToken' denied on resource
```

surfacing as a `UnableToCreateStanzas` event on the PerconaPGCluster and a
PerconaPGBackup stuck in `Starting` forever.

Bind both:

```bash
for ksa in mimir-postgres-pgbackrest mimir-postgres-instance; do
  gcloud iam service-accounts add-iam-policy-binding \
    pgbackrest@teralivekubernetes.iam.gserviceaccount.com \
    --project=teralivekubernetes \
    --role=roles/iam.workloadIdentityUser \
    --member="serviceAccount:teralivekubernetes.svc.id.goog[mimir/$ksa]"
done
```

Allow a couple of minutes for the binding to reach the metadata server — the
pods pick it up without a restart, but not instantly.

### Bucket is separate from Velero's on purpose

Different retention, different lifecycle, and a mistake in one cannot reach the
other. Velero backs the cluster's *objects* up; this backs the *database* up,
and the database is the thing that cannot be reconstructed from Git.

## Verifying

`Succeeded` on a PerconaPGBackup is necessary but not sufficient — check that
objects actually landed, the same discipline the Velero rollout used:

```bash
ws k8s exec -n mimir mimir-postgres-repo-host-0 -c pgbackrest -- pgbackrest info
gcloud storage ls -r gs://teralivekubernetes-pgbackrest --project=teralivekubernetes
```

A healthy `info` shows `repo1: ok` and `repo2: ok` under `status`, and lists
backups with a `repo2:` size line. To force a backup rather than wait for the
schedule:

```bash
ws k8s apply -f - <<'YAML'
apiVersion: pgv2.percona.com/v2
kind: PerconaPGBackup
metadata:
  name: repo2-verify
  namespace: mimir
spec:
  pgCluster: mimir-postgres
  repoName: repo2
  options:
    - "--type=full"
YAML
```

Note `options` is a **list**, not a string; the CRD rejects a bare string.

## Schedules

repo2 runs two hours after repo1 (03:00 vs 01:00 UTC). Backing both repos up at
the same instant means two pgBackRest processes competing for the same WAL and
the same disk, on a cluster already tight on CPU requests.

## Offsite backups for the shared MySQL cluster

`shared/mysql-cluster.yaml` backs up to `gs://teralivekubernetes-mysql-backups`
via a single `s3`-type storage named `gcs`. Three things about it differ from the
Postgres story above, all of them deliberate.

### It is not keyless, and it cannot be

The PXC operator supports exactly `filesystem`, `s3`, `azure`. There is **no
native GCS backend**, unlike the Postgres operator's `gcs:` repo with
`gcs-key-type: auto`. So MySQL reaches GCS over the S3-interoperability endpoint
`https://storage.googleapis.com`, which authenticates with **HMAC keys** —
something Workload Identity cannot issue. Replacing `credentialsSecret` with an
`iam.gke.io/gcp-service-account` annotation will not work; `xbcloud` 403s.

Blast radius is bounded instead of eliminated: the GSA `mysql-backup@…` holds
`roles/storage.objectAdmin` on that one bucket and nothing else. The key lives in
OpenBao at `secret/mimir-mysql-backup`, materialized by ESO into
`mimir-mysql-backup-s3`, with a recovery copy in the workspace `.env` from the
days when OpenBao ran the manual Shamir posture. Since 2026-09-17 OpenBao on
GKE auto-unseals through Cloud KMS and is itself snapshotted daily to GCS, so
the `.env` copy is a convenience, not the custody. Full reasoning in
`nidavellir/docs/secrets-management.md`.

The operator insists the Secret keys be named `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` whatever the store actually is. They are AWS-shaped names
holding Google values.

### There is no local tier, and adding one would be a bug

Postgres gets `repo1` (local, fast restore) plus `repo2` (offsite). That split
does **not** port to MySQL, for a mechanical reason: pgBackRest reuses one volume
and rotates retention inside it, so `repo1` costs a fixed 40Gi. PXC's
`filesystem` storage provisions a **new PVC per run**, and `keep:` prunes only
*successful* backups — so a local MySQL tier is a disk leak with a retention
setting painted on it. That already happened: 320Gi of orphaned volumes took the
regional SSD quota to 976 of 1000 and blocked disk provisioning cluster-wide.

So if you are tempted to add a local tier back for restore speed: it is not a
tradeoff, it is the outage. The PVCs also carry no `ownerReferences`, so they do
not disappear when the backup CR does.

### What is NOT in Git

Same shape as the Postgres section above — a cluster rebuild will not recreate
these:

```
bucket   gs://teralivekubernetes-mysql-backups   us-east1, uniform access
GSA      mysql-backup@teralivekubernetes.iam.gserviceaccount.com
grant    roles/storage.objectAdmin, scoped to that bucket only
HMAC     one ACTIVE key for that GSA, value in OpenBao + .env
```

Note there is **no** Workload Identity binding here, and no two-KSA trap — that
whole class of problem belongs to the keyless path and does not apply.

### Homelab is not covered

The bucket and endpoint are GKE-specific. Garage is the homelab object store and
is an in-cluster Service that does not resolve on GKE, so this cannot be one
fixed value for both. Running the shared MySQL cluster on homelab needs the
storage parameterised per environment first, the way nordri patches `velero-gke`
against `velero-homelab`.

### Verifying

`Succeeded` is necessary but not sufficient — the same discipline as Postgres,
and doubly so here because this cluster has already produced a backup that
reported success while writing nothing:

```bash
ws k8s get pxc-backup -n mimir
gcloud storage ls -r gs://teralivekubernetes-mysql-backups --project=teralivekubernetes
```

An empty bucket beside a `Succeeded` CR is the failure mode to expect from a
credential or endpoint problem. To force a backup rather than wait:

```bash
ws k8s apply -f - <<'YAML'
apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBClusterBackup
metadata:
  name: gcs-verify
  namespace: mimir
spec:
  pxcCluster: mimir-mysql
  storageName: gcs
YAML
```

If it fails, read the backup **pod's logs**, not the CR status — the CR sat at
`Running` through a hard `garbd` failure once already. The debugging order is in
the "THE STATUS LIED" notes beside the schedule in `shared/mysql-cluster.yaml`.

## Still owed

- **A restore drill.** Neither Postgres repo has been restored from, and neither
  has MySQL. Until that happens this is an untested backup, which is the only
  kind that fails when it matters.
- **Old-path restore sources.** `gke-prod`'s pre-3a pgBackRest repository and its
  MySQL backup folders at the bucket root are not moved or deleted by the move to
  per-cluster paths; new writes go only to `/<cluster>/pgbackrest` and
  `<bucket>/<cluster>`. The two engines age differently:
  - **Postgres: the old repository does not expire.** pgBackRest `expire` acts
    only on the repo path it is currently configured for, so once `repo2-path`
    moves the old repository is never expired again. It stays until someone
    deletes it or a GCS lifecycle rule removes it. The default cloud repo path in
    the Crunchy-derived operator is probably `/pgbackrest/repo2`
    (`/pgbackrest/<repoName>`), not the bare `/pgbackrest` — confirm the exact
    string with `gcloud storage ls gs://teralivekubernetes-pgbackrest/` before
    relying on it. To restore from it, select the repository with `repoName` and
    pass the old path explicitly in a `PerconaPGRestore` (the operator refuses a
    `--repo=` entry in `options` and never builds the restore Job; `repoName` is
    the only accepted selector):

    ```yaml
    repoName: repo2
    options:
      - "--repo2-path=<old path, as confirmed above>"
      - "--type=time"
      - "--target=<timestamp>"
    ```
  - **MySQL: the old backups lapse with retention.** `keep: 7` deletes old backup
    CRs and their objects at the destinations those CRs recorded, so a restore
    goes by the destination recorded on the backup CR. Expect the old root-level
    folders to be usable for about 7 days after the switch.
  - **MySQL: change the storage path only between backups.** The operator keeps
    a running backup Job as it is while recomputing the destination from the
    current storage spec on every reconcile, so a path change landing mid-run
    records a destination the Job never wrote to. Before the sync that changes
    `bucket` (the migration to per-cluster paths), confirm
    `kubectl get perconaxtradbclusterbackup -n mimir` shows nothing in a
    non-terminal state (`Starting`, `Running`); the daily runs at 02:00 UTC, so
    any other hour is clear.
