# Offsite backups for the shared Postgres cluster

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

## Still owed

- **A restore drill.** Neither repo has been restored from. Until that happens
  this is an untested backup, which is the only kind that fails when it matters.
