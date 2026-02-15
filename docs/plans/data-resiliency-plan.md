# Mimir Data Resiliency Plan

Created: 2026-02-10
Status: Draft — awaiting approval

## Goal

Make Mimir's 5 data services production-ready with respect to backup, restore, and disaster recovery. Target SLA: recover from total cluster loss with less than 24 hours of data loss, using a combination of application-level backups (Percona/Strimzi-native) and infrastructure-level backups (Velero + Longhorn snapshots).

Nothing is live right now — no data to lose. This is greenfield hardening work.

## Current State Assessment

### Mimir Data Services — Backup Status

| Service | Backup Configured? | Storage | Problem |
|---------|-------------------|---------|---------|
| **PostgreSQL** | Yes (pgBackRest) | Local PVC (1Gi) | No offsite copy, PVC lost if cluster dies |
| **MongoDB** | Yes (daily logical) | Ephemeral `/backup` | **CRITICAL**: backup dir is in-container, lost on pod restart |
| **MySQL** | **None** | N/A | Zero backup configuration — relies solely on Galera replication |
| **Kafka** | N/A (event bus) | Persistent claims | Topic replication provides durability; no traditional backup needed |
| **Valkey** | N/A (cache) | Optional persistence | Cache layer; backup not critical |

### Nordri/refr-k8s Infrastructure — Backup Stack

| Component | Version | Status |
|-----------|---------|--------|
| Crossplane | 2.1.3 | Working |
| Velero | 1.17.1 (chart 11.3.1) | Installed but **not operational** — Garage layout not initialized, no bucket, no credentials, no schedules |
| Garage | v2.1.0 (chart 0.9.1) | Deployed as StatefulSet (3 replicas) but **layout not initialized** — needs `garage layout assign` + `garage layout apply` |
| Longhorn | 1.10.1 | Working (homelab only), no backup target configured |

### Crossplane Version Mismatch (Nordri vs Mimir)

| Component | Nordri (refr-k8s) | Mimir (setup.sh) | Action |
|-----------|-------------------|-------------------|--------|
| Crossplane core | 2.1.3 | 2.1.4 | Align to 2.1.4 |
| provider-kubernetes | v1.2.0 | v1.1.0 | Align to v1.2.0 (Nordri's is newer) |
| provider-helm | v1.0.0 | v0.18.0 | Align to v1.0.0 (Nordri's is newer) |
| function-go-templating | not installed | v0.4.0 | Add to Nordri |
| function-auto-ready | not installed | v0.2.1 | Add to Nordri |

**Note**: Mimir's `docs/cluster-setup.md` references `provider-helm:v0.19.0` and `function-go-templating:v0.7.0` which differ from `setup.sh` (v0.18.0 and v0.4.0). The setup.sh versions are the validated ones; the docs drifted. Fix docs as part of true-up.

### k3d Cluster Inventory (Local)

| Cluster | Agents | Purpose | Action |
|---------|--------|---------|--------|
| `mimir-test` | 2 | Mimir standalone testing | **Keep** (or recreate fresh) |
| `refr-k8s` | 2 | Nordri base infra | **Keep** (or recreate fresh) |
| `percona-kubedb-test` | 0 | Legacy experiment | **Delete** |

---

## Phased Plan

### Phase 0: Cleanup & Foundation (prerequisite for all other phases)

**Goal**: Clean slate — fresh clusters, aligned versions, Mimir running atop Nordri's Crossplane.

#### 0.1 Delete stale clusters
```bash
k3d cluster delete percona-kubedb-test
# Optionally delete and recreate mimir-test and refr-k8s for a clean start
```

#### 0.2 True-up Crossplane versions
**In Nordri (refr-k8s):**
- Update `platform/fundamentals/apps/crossplane.yaml` from 2.1.3 to **2.1.4**
- Update `bootstrap.sh` Crossplane version from 2.1.3 to **2.1.4**
- Update `platform/fundamentals/manifests/crossplane-providers.yaml`:
  - Keep provider-kubernetes at v1.2.0 (already newer)
  - Keep provider-helm at v1.0.0 (already newer)
  - **Add** function-go-templating v0.4.0
  - **Add** function-auto-ready v0.2.1
- Add ProviderConfig manifests for both providers (Nordri currently doesn't have these — they live in Mimir's `provider-configs.yaml`)

**In Mimir:**
- Update `platform.yaml` provider-kubernetes from v1.1.0 to **v1.2.0**
- Update `setup.sh` provider-helm from v0.18.0 to **v1.0.0**
- Update `docs/cluster-setup.md` to match setup.sh versions
- Test that all 5 kuttl tests still pass with newer provider versions

**Decision needed**: Should Nordri own the ProviderConfigs, or should Mimir continue to apply its own? If Mimir runs with `--skip-crossplane` atop Nordri, someone needs to install the ProviderConfigs. Options:
1. Nordri installs ProviderConfigs (simpler for Mimir, but Nordri needs to know about provider-kubernetes SA + RBAC)
2. Mimir always applies provider-configs.yaml even in `--skip-crossplane` mode (current behavior — setup.sh skips the entire Crossplane block including configs)
3. Split: `--skip-crossplane` only skips core install + providers, still applies configs

Recommendation: Option 3 — refactor setup.sh to have `--skip-crossplane-core` that skips Helm install + provider/function manifests but still applies provider-configs.yaml and RBAC.

#### 0.3 Stand up Nordri → Mimir stack
1. Create fresh `refr-k8s` cluster with Nordri bootstrap
2. Verify Nordri validate.py passes
3. Run `./setup.sh --skip-crossplane` in Mimir context (pointed at refr-k8s cluster)
4. Run `kubectl kuttl test tests/e2e/` — all 5 pass

#### 0.4 Add Nordri kuttl tests
Create `tests/e2e/` in refr-k8s with per-component tests:

| Test Suite | What It Validates |
|-----------|-------------------|
| `crossplane-health/` | Crossplane pods running, providers Healthy, functions Healthy |
| `longhorn-storage/` | PVC creation + binding with longhorn storageClass |
| `garage-s3/` | Garage pods running, S3 API reachable, bucket create/list/delete |
| `velero-health/` | Velero pods running, BSL available (Phase 3 prerequisite) |
| `traefik-ingress/` | IngressRoute connectivity to ArgoCD, Gitea |

These overlap with validate.py but provide declarative, reproducible testing via kuttl.

---

### Phase 1: Fix Mimir Backup Gaps

**Goal**: Every stateful service has working local backups that survive pod restarts.

#### 1.1 MongoDB — Fix ephemeral backup storage
**File**: `percona/MongoComp.yaml`

Change backup storage from in-container filesystem to PVC-backed:
```yaml
backup:
  enabled: true
  image: percona/percona-backup-mongodb:2.11.0
  storages:
    local-storage:
      type: filesystem
      filesystem:
        path: /backup
      # ADD: volumeSpec for persistent storage
      volumeSpec:
        persistentVolumeClaim:
          resources:
            requests:
              storage: 2Gi
          storageClassName: local-path
  tasks:
  - name: daily-backup
    schedule: "0 0 * * *"
    enabled: true
    storageName: local-storage
    keep: 7
    type: logical
```

**Test**: Trigger a manual backup, delete the backup pod, verify backup files persist on PVC.

#### 1.2 MySQL — Add backup configuration
**File**: `percona/MySQLComp.yaml`

Add Percona XtraBackup configuration:
```yaml
backup:
  enabled: true
  image: percona/percona-xtradb-cluster-operator:1.19.0-pxc8.0-backup
  storages:
    local-storage:
      type: filesystem
      volume:
        persistentVolumeClaim:
          accessModes: ["ReadWriteOnce"]
          resources:
            requests:
              storage: 2Gi
          storageClassName: local-path
  schedule:
  - name: daily-backup
    schedule: "0 0 * * *"
    keep: 7
    storageName: local-storage
```

**Note**: Need to verify exact image tag and YAML schema from Percona PXC operator docs. The backup image tag naming may follow the same chart-vs-image pattern we hit with PSMDB.

#### 1.3 PostgreSQL — Verify existing backups work
pgBackRest is already configured with a local PVC. Verify:
- `pgbackrest info` shows valid stanza
- Can list backups
- Retention policy is reasonable (add explicit retention if not set)

#### 1.4 Update XRD parameters
Add optional backup parameters to all three Percona XRDs:
- `backupStorageSize` (default: 2Gi)
- `backupSchedule` (default: "0 0 * * *")
- `backupRetention` (default: 7)

#### 1.5 kuttl backup tests
For each of PG, MongoDB, MySQL:
1. Provision instance (existing test step)
2. Write test data (existing connection test, extend to INSERT)
3. Trigger manual backup
4. Verify backup completed successfully
5. (Later in Phase 2: verify backup exists in S3)

---

### Phase 2: Offsite Backup Storage

**Goal**: All backups replicated to S3-compatible storage outside the cluster.

#### 2.1 Decision: Garage vs external S3

| Option | Pros | Cons |
|--------|------|------|
| **Garage (in-cluster)** | Already deployed in Nordri, free, fast | Dies with cluster — not true offsite |
| **GCP Cloud Storage** | True offsite, battle-tested | Costs money, needs credentials management |
| **DigitalOcean Spaces** | True offsite, S3-compatible, cheap | Another vendor, needs credentials |
| **Garage + GCP** | Local fast backup + offsite DR copy | More complexity, but best of both worlds |

**Recommendation**: Garage as primary (fast, local), GCP bucket as secondary (offsite DR). Start with Garage only, add GCP in a later phase.

#### 2.2 Initialize Garage
In the Nordri cluster:
1. Check Garage pod status and node IDs
2. Run `garage layout assign` for each node
3. Run `garage layout apply --version 1`
4. Create buckets: `mimir-pg-backups`, `mimir-mongo-backups`, `mimir-mysql-backups`, `velero-backups`
5. Create API keys with appropriate permissions

**Automation**: Add a kuttl test or post-bootstrap Job in Nordri that handles layout initialization.

#### 2.3 Configure Percona S3 backups

**PostgreSQL** — Add S3 repo to pgBackRest:
```yaml
backups:
  pgbackrest:
    repos:
    - name: repo1         # existing local PVC
      volume: ...
    - name: repo2         # NEW: S3 offsite
      s3:
        bucket: mimir-pg-backups
        endpoint: garage.garage.svc.cluster.local:3900
        region: garage
```

**MongoDB** — Add S3 storage:
```yaml
backup:
  storages:
    local-storage: ...    # existing
    s3-storage:           # NEW
      type: s3
      s3:
        bucket: mimir-mongo-backups
        region: garage
        endpointUrl: http://garage.garage.svc.cluster.local:3900
        credentialsSecret: garage-s3-credentials
```

**MySQL** — Add S3 storage:
```yaml
backup:
  storages:
    local-storage: ...    # existing
    s3-storage:
      type: s3
      s3:
        bucket: mimir-mysql-backups
        region: garage
        endpointUrl: http://garage.garage.svc.cluster.local:3900
        credentialsSecret: garage-s3-credentials
```

#### 2.4 Credentials management
Create a Kubernetes Secret with Garage API keys that Percona operators can reference. Options:
- Manual secret creation (simplest for now)
- Crossplane Object to create the secret declaratively
- External Secrets Operator (overkill at this stage)

#### 2.5 kuttl test: verify S3 upload
Extend backup tests to verify objects appear in Garage bucket after backup completes.

---

### Phase 3: Velero for PV-level Backups

**Goal**: Infrastructure-level backup of all PVs and cluster state via Velero + Longhorn snapshots.

#### 3.1 Prerequisites (from Phase 2)
- Garage layout initialized
- `velero-backups` bucket created
- API credentials available

#### 3.2 Configure Velero BSL
Update `platform/fundamentals/apps/velero.yaml` in Nordri:
- Add actual S3 credentials (or reference a Secret)
- Verify BSL becomes Available

#### 3.3 Install Velero Longhorn plugin
For CSI snapshot support:
```yaml
initContainers:
- name: velero-plugin-for-csi
  image: velero/velero-plugin-for-csi:v0.7.0
```

#### 3.4 Create Velero schedules
```yaml
schedules:
  daily-full:
    schedule: "0 2 * * *"
    template:
      includedNamespaces: ["*"]
      excludedNamespaces: ["kube-system", "crossplane-system"]
      snapshotVolumes: true
      ttl: 168h0m0s  # 7 days
```

#### 3.5 Test backup/restore cycle
1. Create a Velero backup
2. Verify backup shows as Completed
3. Delete a test namespace
4. Restore from backup
5. Verify data is back

#### 3.6 Nordri kuttl tests for Velero
- `velero-backup/`: Create backup, verify Completed status
- `velero-restore/`: Backup → delete → restore → verify

---

### Phase 4: Test Automation & Documentation

**Goal**: Automated verification that backups work end-to-end.

#### 4.1 Mimir kuttl backup/restore tests
New test suites in `tests/e2e/`:

| Test | Steps |
|------|-------|
| `pg-backup-restore/` | Provision PG → INSERT data → trigger backup → DELETE data → restore → verify data |
| `mongo-backup-restore/` | Provision Mongo → insert doc → backup → drop collection → restore → verify |
| `mysql-backup-restore/` | Provision MySQL → INSERT → backup → DROP → restore → verify |

#### 4.2 BDD scenarios
Add to `features/infrastructure.feature`:
```gherkin
Scenario: PostgreSQL Backup and Restore
  Given the PostgreSQLInstance Claim "pg-backup-test" is applied
  And test data is written to the database
  When a backup is triggered
  Then the backup should complete successfully
  When the test data is deleted
  And a restore is performed
  Then the test data should be recovered

# Similar for MongoDB, MySQL
```

#### 4.3 Update documentation
- `docs/cluster-setup.md`: Add backup configuration section
- `docs/backup-restore.md`: New runbook for manual backup/restore procedures
- `docs/plans/data-resiliency-plan.md`: Update status as phases complete

#### 4.4 Skills
Search for or create skills in `yggdrasil/.agent/skills/`:

| Skill | Scope |
|-------|-------|
| `velero-backup-restore/` | Velero patterns: schedules, BSL config, restore procedures, S3 setup |
| `percona-backup-config/` | Percona-specific backup patterns across PG/MongoDB/MySQL operators |
| `garage-s3-setup/` | Garage layout init, bucket creation, API key management |

---

## Execution Order & Dependencies

```
Phase 0.1 (Cleanup)
  └── Phase 0.2 (True-up Crossplane versions)
        └── Phase 0.3 (Nordri + Mimir integration test)
              ├── Phase 0.4 (Nordri kuttl tests)
              └── Phase 1 (Fix backup gaps)
                    └── Phase 2 (S3 offsite)
                          └── Phase 3 (Velero)
                                └── Phase 4 (Test automation)
```

Phases 0.4 and 1 can run in parallel once 0.3 is verified.

## Session Continuity Notes

This plan lives at `docs/plans/data-resiliency-plan.md` in the Mimir repo. Each phase should be committed when complete. Key files to read when resuming:

- This plan (current status)
- `MEMORY.md` in Claude's project memory
- `docs/cluster-setup.md` (provisioning runbook)
- `setup.sh` (automated setup with flags)
- `docs/testing.md` (test strategy)

### Validated Pinned Versions (as of 2026-02-10)

| Component | Version | Notes |
|-----------|---------|-------|
| Crossplane | 2.1.4 | Target aligned version |
| provider-kubernetes | v1.2.0 | Nordri's version (upgrading Mimir) |
| provider-helm | v1.0.0 | Nordri's version (upgrading Mimir) |
| function-go-templating | v0.4.0 | Adding to Nordri |
| function-auto-ready | v0.2.1 | Adding to Nordri |
| Strimzi | chart 0.50.0 | |
| redis-operator | chart 0.23.0 | |
| pg-operator | chart 2.8.2 | Image: 2.7.0-ppg15-postgres |
| psmdb-operator | chart 1.21.3 | Image: 1.21.2, crVersion: 1.21.2 |
| pxc-operator | chart 1.19.0 | crVersion: 1.19.0 |
| Velero | chart 11.3.1 (v1.17.1) | |
| Longhorn | 1.10.1 | Homelab only |
| Garage | v2.1.0 (chart 0.9.1) | Custom chart in Nordri |

### Risk Register

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Provider version upgrade breaks compositions | Medium | High | Test all 5 services after upgrade |
| Garage S3 API incompatible with Percona | Low | Medium | Test with `aws s3` CLI first |
| Velero + Longhorn CSI plugin version mismatch | Medium | Medium | Pin versions, test on k3d first |
| PXC backup image tag naming mismatch (like PSMDB) | High | Low | Check Docker Hub tags before configuring |
| Percona S3 endpoint config differs between operators | Medium | Low | Test each operator individually |
