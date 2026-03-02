# Mimir Cluster Setup

Complete runbook for provisioning a fresh k3d cluster with all Mimir data services. Validated on 2026-02-09. Note that prerequisites (and Crossplane) would be covered if going through the separate infrastructure foundation repo.

TODO: The Crossplane instructions here may need to be compared to the ones in the infra repo then pulled out from here in favor of a dependency on the infra repo.

## Prerequisites

- [k3d](https://k3d.io/) installed
- [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/) installed
- [Helm](https://helm.sh/docs/intro/install/) installed
- [kuttl](https://kuttl.dev/) installed (for testing)

## 1. Create k3d Cluster

```bash
k3d cluster create mimir-test \
  --port "9080:80@loadbalancer" \
  --port "9443:443@loadbalancer" \
  --agents 2
```

This creates a 3-node cluster (1 server + 2 agents) with port-forwarding for ingress.

## 2. Install Crossplane

```bash
helm repo add crossplane-stable https://charts.crossplane.io/stable
helm repo update
helm install crossplane crossplane-stable/crossplane \
  --namespace crossplane --create-namespace
kubectl wait --for=condition=available deployment/crossplane \
  -n crossplane --timeout=120s
```

## 3. Install Crossplane Providers & Functions

Apply provider-kubernetes (includes ServiceAccount, RuntimeConfig, ClusterRoleBinding):

```bash
kubectl apply -f platform.yaml
```

Install provider-helm and composition functions:

```bash
kubectl apply -f - <<EOF
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-helm
spec:
  package: xpkg.upbound.io/crossplane-contrib/provider-helm:v1.0.0
---
apiVersion: pkg.crossplane.io/v1beta1
kind: Function
metadata:
  name: function-auto-ready
spec:
  package: xpkg.upbound.io/crossplane-contrib/function-auto-ready:v0.2.1
---
apiVersion: pkg.crossplane.io/v1beta1
kind: Function
metadata:
  name: function-go-templating
spec:
  package: xpkg.upbound.io/crossplane-contrib/function-go-templating:v0.4.0
EOF
```

**Wait for all providers and functions to become healthy before proceeding:**

```bash
kubectl wait --for=condition=Healthy providers.pkg.crossplane.io --all --timeout=120s
kubectl wait --for=condition=Healthy functions.pkg.crossplane.io --all --timeout=120s
```

> **Important**: The next step (provider-configs) will fail if applied before provider CRDs are registered. Always wait for Healthy status first.

## 4. Apply Provider Configs & RBAC

```bash
kubectl apply -f provider-configs.yaml
```

> **Note**: Namespace-management RBAC for the Crossplane SA is handled by `setup.sh` (standalone mode) or by Nordri's `crossplane-configs.yaml` (when running atop Nordri with `--skip-crossplane`).

## 5. Install Operators

### Kafka (Strimzi)

```bash
helm repo add strimzi https://strimzi.io/charts/
helm install strimzi-kafka-operator strimzi/strimzi-kafka-operator \
  --namespace kafka --create-namespace \
  --set watchAnyNamespace=true
```

### Valkey (OT-Container-Kit)

```bash
helm repo add ot-helm https://ot-container-kit.github.io/helm-charts/
helm install redis-operator ot-helm/redis-operator \
  --namespace valkey --create-namespace
```

### Percona (PostgreSQL, MongoDB, MySQL)

```bash
kubectl create namespace percona
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update

helm install percona-postgresql-operator percona/pg-operator \
  --namespace percona \
  --set watchAllNamespaces=true

helm install psmdb-operator percona/psmdb-operator \
  --namespace percona \
  --set watchAllNamespaces=true

helm install pxc-operator percona/pxc-operator \
  --namespace percona \
  --set watchAllNamespaces=true
```

> **Warning**: For the PG operator, do NOT use `--set watchNamespace=""`. Passing an empty string via Helm `--set` is silently ignored, causing the operator to only watch its own namespace. Always use `--set watchAllNamespaces=true`.

Apply Percona RBAC for Crossplane:

```bash
kubectl apply -f percona/rbac.yaml
```

### Verify All Operators

```bash
kubectl get pods -n kafka
kubectl get pods -n valkey
kubectl get pods -n percona
```

All operator pods should be `Running` and `Ready`.

## 6. Apply XRDs and Compositions

```bash
# Kafka
kubectl apply -f kafka/xrd.yaml
kubectl apply -f kafka/composition.yaml

# Valkey
kubectl apply -f valkey/xrd.yaml
kubectl apply -f valkey/composition.yaml

# PostgreSQL, MySQL, MongoDB
kubectl apply -f percona/PostgresXRD.yaml
kubectl apply -f percona/PostgresComp.yaml
kubectl apply -f percona/MySQLXRD.yaml
kubectl apply -f percona/MySQLComp.yaml
kubectl apply -f percona/MongoXRD.yaml
kubectl apply -f percona/MongoComp.yaml
```

> **Note**: You may see a deprecation warning about `CompositeResourceDefinition v1`. This is cosmetic — XRDs still work on v1, but should be migrated to v2 in a future release.

Verify:

```bash
kubectl get xrd
kubectl get compositions
```

## 7. Run Tests

```bash
kubectl kuttl test tests/e2e/
```

Expected result: all 5 tests pass (Kafka, Valkey, PostgreSQL, MySQL, MongoDB). Full sequential run takes ~12 minutes.

## Quick Start (New System)

For a fresh system with k3d already installed:

```bash
k3d cluster create mimir-test --port "9080:80@loadbalancer" --port "9443:443@loadbalancer" --agents 2
./setup.sh
kubectl kuttl test tests/e2e/
```

When running atop Nordri (which installs its own Traefik), disable the k3s built-in Traefik:

```bash
k3d cluster create refr-k8s \
  --port "8080:80@loadbalancer" --port "8443:443@loadbalancer" \
  --agents 2 --k3s-arg "--disable=traefik@server:*"
# Then: bootstrap Nordri, then ./setup.sh --skip-crossplane
```

The script is idempotent (`helm upgrade --install`, `--dry-run=client`). Flags:
- `--skip-crossplane` — if Crossplane is managed by your infra repo
- `--patch-security-context` — needed on Rancher Desktop (patches PG operator `runAsNonRoot`; not needed on k3d)

## Teardown

```bash
k3d cluster delete mimir-test
```

## ArgoCD Deployment (via Nidavellir)

In production, Mimir is deployed via ArgoCD rather than `setup.sh`. The `argocd/` directory contains everything ArgoCD needs:

- **`argocd/kustomization.yaml`** — top-level entry point referencing operator Applications, RBAC, XRDs, and Compositions
- **`argocd/apps/`** — individual ArgoCD Application manifests for each operator

Nidavellir's `mimir-app.yaml` points ArgoCD at `argocd/` in the mimir repo. Nordri's `bootstrap.sh` hydrates all three repos (nordri, nidavellir, mimir) into the in-cluster Gitea.

### Operator Applications

| Application | Chart | Namespace | Notes |
|-------------|-------|-----------|-------|
| `mimir-strimzi` | `strimzi-kafka-operator` 0.50.0 | `kafka` | `watchAnyNamespace: true` |
| `mimir-valkey-operator` | `redis-operator` 0.23.0 | `valkey` | Chart name is "redis-operator" but deploys Valkey |
| `mimir-percona-pg-operator` | `pg-operator` 2.8.2 | `percona` | Kustomize-rendered (see below) |
| `mimir-percona-psmdb-operator` | `psmdb-operator` 1.21.3 | `percona` | `watchAllNamespaces: true` |
| `mimir-percona-pxc-operator` | `pxc-operator` 1.19.0 | `percona` | `watchAllNamespaces: true` |

### ServerSideApply (required for all operators)

All operator Applications use `ServerSideApply=true` in their syncOptions. This is **required**, not optional.

**Background**: When Kubernetes applies a resource using client-side apply (the default), it stores the entire "last-applied-configuration" as an annotation. CRDs for database operators are often very large (hundreds of KB of OpenAPI schema). When the annotation exceeds 262,144 bytes, the apply fails with:

```
metadata.annotations: Too long: must have at most 262144 bytes
```

Server-side apply uses server-managed field ownership instead of the annotation, avoiding the size limit. It's the standard fix for large CRDs and has no drawbacks for normal resources. If you add a new operator Application, always include `ServerSideApply=true`.

### Percona PG operator: runAsNonRoot patch

The pg-operator Helm chart (2.8.2) hardcodes `runAsNonRoot: true` in the operator Deployment with no Helm values override. This causes `CreateContainerConfigError` on Rancher Desktop and potentially GKE.

The fix uses **Kustomize helmCharts rendering** in `argocd/apps/percona-pg-operator/kustomization.yaml`:

1. Kustomize renders the Helm chart locally (requires `--enable-helm` in ArgoCD's kustomize.buildOptions — set in Nordri's `bootstrap.sh`)
2. A JSON patch sets `runAsNonRoot: false` on the rendered Deployment
3. `includeCRDs: true` is required — Kustomize does NOT include Helm CRDs by default (unlike `helm install`)

The ArgoCD Application for PG operator points to this directory (Kustomize source) instead of using a Helm source directly.

Note: This is the **operator deployment** fix. The workload-level fix (PG cluster pods) is handled separately in `PostgresComp.yaml` via `initContainer.containerSecurityContext.runAsNonRoot: false`.

## Troubleshooting

### PostgreSQL claim stays "Synced but not Ready"

Most common cause: PG operator is not watching the claim's namespace. Check:

```bash
kubectl get deployment -n percona percona-postgresql-operator-pg-operator \
  -o jsonpath='{.spec.template.spec.containers[0].env}' | python3 -m json.tool
```

If `WATCH_NAMESPACE` is set to `percona` (not empty), the operator needs to be upgraded:

```bash
helm upgrade percona-postgresql-operator percona/pg-operator \
  --namespace percona \
  --set watchAllNamespaces=true
```

### CEL readiness error: "no such key: status"

This happens when a Crossplane Object's CEL readiness query (e.g., `object.status.state == 'ready'`) runs before the operator has written any status. It resolves itself once the operator starts reconciling. If it persists, the operator likely isn't watching the namespace (see above).

### Percona PG operator: `CreateContainerConfigError` / `runAsNonRoot`

The operator pod fails to start with `container has runAsNonRoot and image will run as root`. This happens on Rancher Desktop and potentially GKE with strict Pod Security Standards. Not observed on k3d.

Two separate fixes exist:

1. **Operator deployment** (the operator pod itself): In standalone mode, use `./setup.sh --patch-security-context`. In ArgoCD mode, this is handled automatically by the Kustomize helmCharts patch — see "ArgoCD Deployment" section above.
2. **Workload init containers** (PG cluster pods): Handled in `PostgresComp.yaml` via `initContainer.containerSecurityContext.runAsNonRoot: false`.

### provider-configs.yaml fails on apply

The Helm ProviderConfig CRD doesn't exist until provider-helm is installed and healthy. Wait for providers first:

```bash
kubectl wait --for=condition=Healthy providers.pkg.crossplane.io --all --timeout=120s
```
