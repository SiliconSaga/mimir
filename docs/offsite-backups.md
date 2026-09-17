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
| `repo1` | 40Gi PVC in-cluster | fast restore, cheap. 2 weekly fulls. |
| `repo2` | `gs://teralivekubernetes-pgbackrest` | survives losing the cluster or the zone. 4 weekly fulls. |

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

```
bucket   gs://teralivekubernetes-pgbackrest      us-east1, uniform access
GSA      pgbackrest@teralivekubernetes.iam.gserviceaccount.com
grant    roles/storage.objectAdmin, scoped to that bucket only
binding  roles/iam.workloadIdentityUser on the GSA, for TWO KSAs
```

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
`mimir-mysql-backup-s3`, with a recovery copy in the workspace `.env` because
OpenBao runs the manual Shamir seal posture. Full reasoning in
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
`Running` through a hard `garbd` failure once already. See the schedule-disabled
block in `shared/mysql-cluster.yaml`.

## Still owed

- **A restore drill.** Neither Postgres repo has been restored from, and neither
  has MySQL. Until that happens this is an untested backup, which is the only
  kind that fails when it matters.
