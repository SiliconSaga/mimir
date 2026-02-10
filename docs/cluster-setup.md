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
  --namespace crossplane-system --create-namespace
kubectl wait --for=condition=available deployment/crossplane \
  -n crossplane-system --timeout=120s
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
  package: xpkg.upbound.io/crossplane-contrib/provider-helm:v0.19.0
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
  package: xpkg.upbound.io/crossplane-contrib/function-go-templating:v0.7.0
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
kubectl apply -f ../refr-k8s/crossplane-rbac.yaml   # Namespace management permissions
```

## 5. Install Operators

### Kafka (Strimzi)

```bash
helm repo add strimzi https://strimzi.io/charts/
helm install strimzi-kafka-operator strimzi/strimzi-kafka-operator \
  --namespace kafka-system --create-namespace \
  --set watchAnyNamespace=true
```

### Valkey (OT-Container-Kit)

```bash
helm repo add ot-helm https://ot-container-kit.github.io/helm-charts/
helm install redis-operator ot-helm/redis-operator \
  --namespace valkey-system --create-namespace
```

### Percona (PostgreSQL, MongoDB, MySQL)

```bash
kubectl create namespace percona-system
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update

helm install percona-postgresql-operator percona/pg-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true

helm install psmdb-operator percona/psmdb-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true

helm install pxc-operator percona/pxc-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true
```

> **Warning**: For the PG operator, do NOT use `--set watchNamespace=""`. Passing an empty string via Helm `--set` is silently ignored, causing the operator to only watch its own namespace. Always use `--set watchAllNamespaces=true`.

Apply Percona RBAC for Crossplane:

```bash
kubectl apply -f percona/rbac.yaml
```

### Verify All Operators

```bash
kubectl get pods -n kafka-system
kubectl get pods -n valkey-system
kubectl get pods -n percona-system
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

The script is idempotent (`helm upgrade --install`, `--dry-run=client`). Use `--skip-crossplane` if Crossplane is managed by your infra repo.

## Teardown

```bash
k3d cluster delete mimir-test
```

## Argo CD Integration

The setup has a natural ordering that maps to Argo CD sync waves:

| Wave | Resources | Why |
|------|-----------|-----|
| 0 | Crossplane core (Helm) | Foundation |
| 1 | `platform.yaml` (Providers, Functions) | Need Crossplane CRDs |
| 2 | `provider-configs.yaml`, RBAC | Need provider CRDs registered |
| 3 | Operator Helm releases (Strimzi, OT, Percona) | Need working Crossplane |
| 4 | `percona/rbac.yaml` | Needs Percona CRDs |
| 5 | XRDs and Compositions | Needs operators + Crossplane ready |

### Recommended restructuring for Argo

1. **App-of-Apps pattern**: One root Application pointing to a directory of Application manifests.
2. **Each wave = one Application** with `argocd.argoproj.io/sync-wave` annotations.
3. **Operators via Helm**: Argo natively supports `kind: Application` with `source.helm` — no need for a setup script.
4. **Health checks**: Argo already understands Crossplane health for Providers/Functions. For Percona CRDs, you may need custom health checks or just rely on sync-wave ordering.
5. **Move `crossplane-rbac.yaml` into this repo** rather than referencing `../refr-k8s/` (Argo needs self-contained repos).

Example Application for Percona operators:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: mimir-percona-operators
  annotations:
    argocd.argoproj.io/sync-wave: "3"
spec:
  source:
    repoURL: https://percona.github.io/percona-helm-charts/
    chart: pg-operator
    targetRevision: "*"
    helm:
      values: |
        watchAllNamespaces: true
  destination:
    namespace: percona-system
```

### Key gotcha for Argo

The `provider-configs.yaml` will fail if applied before provider CRDs exist. In Argo, this means wave 2 must not sync until wave 1 providers report Healthy. Use `argocd.argoproj.io/sync-wave` plus a `SyncPolicy` with `retry` to handle the ordering, or split into separate Applications with explicit dependencies.

## Troubleshooting

### PostgreSQL claim stays "Synced but not Ready"

Most common cause: PG operator is not watching the claim's namespace. Check:

```bash
kubectl get deployment -n percona-system percona-postgresql-operator-pg-operator \
  -o jsonpath='{.spec.template.spec.containers[0].env}' | python3 -m json.tool
```

If `WATCH_NAMESPACE` is set to `percona-system` (not empty), the operator needs to be upgraded:

```bash
helm upgrade percona-postgresql-operator percona/pg-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true
```

### CEL readiness error: "no such key: status"

This happens when a Crossplane Object's CEL readiness query (e.g., `object.status.state == 'ready'`) runs before the operator has written any status. It resolves itself once the operator starts reconciling. If it persists, the operator likely isn't watching the namespace (see above).

### provider-configs.yaml fails on apply

The Helm ProviderConfig CRD doesn't exist until provider-helm is installed and healthy. Wait for providers first:

```bash
kubectl wait --for=condition=Healthy providers.pkg.crossplane.io --all --timeout=120s
```
