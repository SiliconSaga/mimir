#!/bin/bash
# Mimir Data Services Setup
# Installs all operators, Crossplane resources, and data service definitions.
# Assumes: Crossplane is already installed (from infra repo), kubectl context is set.
# Run: ./setup.sh [--skip-crossplane] [--patch-security-context]
set -euo pipefail

# Ensure we run from the repo root regardless of where the script is invoked
cd "$(dirname "$0")"

SKIP_CROSSPLANE=false
PATCH_SECURITY_CONTEXT=false
for arg in "$@"; do
  case $arg in
    --skip-crossplane) SKIP_CROSSPLANE=true ;;
    --patch-security-context) PATCH_SECURITY_CONTEXT=true ;;
  esac
done

wait_healthy() {
  local resource=$1 timeout=${2:-120}
  echo "  Waiting for $resource to be healthy..."
  kubectl wait --for=condition=Healthy "$resource" --all --timeout="${timeout}s"
}

wait_rollout() {
  local deploy=$1 ns=$2 timeout=${3:-120}
  kubectl rollout status "deployment/$deploy" -n "$ns" --timeout="${timeout}s"
}

# ---------- Crossplane (skip if managed by infra repo) ----------
if [ "$SKIP_CROSSPLANE" = false ]; then
  echo "=== Crossplane Core ==="
  helm repo add crossplane-stable https://charts.crossplane.io/stable 2>/dev/null
  helm repo update >/dev/null
  helm upgrade --install crossplane crossplane-stable/crossplane \
    --namespace crossplane-system --create-namespace --wait --version 2.1.4

  echo "=== Crossplane Providers & Functions ==="
  kubectl apply -f platform.yaml
  kubectl apply -f - <<'EOF'
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

  # CRITICAL: ProviderConfigs need CRDs to exist first
  wait_healthy "providers.pkg.crossplane.io"
  wait_healthy "functions.pkg.crossplane.io"

  echo "=== Provider Configs ==="
  kubectl apply -f provider-configs.yaml

  echo "=== Crossplane Namespace RBAC ==="
  # Allow Crossplane core SA to manage namespaces (for XR-created namespaces).
  # When running atop Nordri (--skip-crossplane), this is provided by Nordri's crossplane-configs.yaml.
  kubectl apply -f - <<'RBAC'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: crossplane-namespace-manager
rules:
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: crossplane-namespace-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: crossplane-namespace-manager
subjects:
- kind: ServiceAccount
  name: crossplane
  namespace: crossplane-system
RBAC
fi

# ---------- Operators ----------
echo "=== Kafka (Strimzi) ==="
kubectl create ns kafka --dry-run=client -o yaml | kubectl apply -f -
helm repo add strimzi https://strimzi.io/charts/ 2>/dev/null
helm repo update >/dev/null
helm upgrade --install strimzi-kafka-operator strimzi/strimzi-kafka-operator \
  --namespace kafka \
  --set watchAnyNamespace=true \
  --version 0.50.0
wait_rollout strimzi-cluster-operator kafka

echo "=== Valkey (OT-Container-Kit) ==="
kubectl create ns valkey --dry-run=client -o yaml | kubectl apply -f -
helm repo add ot-helm https://ot-container-kit.github.io/helm-charts/ 2>/dev/null
helm repo update >/dev/null
helm upgrade --install redis-operator ot-helm/redis-operator \
  --namespace valkey \
  --version 0.23.0
wait_rollout redis-operator valkey

echo "=== Percona Operators (PostgreSQL, MongoDB, MySQL) ==="
kubectl create ns percona --dry-run=client -o yaml | kubectl apply -f -
helm repo add percona https://percona.github.io/percona-helm-charts/ 2>/dev/null
helm repo update >/dev/null

# CRITICAL: Use --set watchAllNamespaces=true (NOT --set watchNamespace="")
# Helm --set silently ignores empty strings, causing the operator to only watch its own namespace.
helm upgrade --install percona-postgresql-operator percona/pg-operator \
  --namespace percona \
  --set watchAllNamespaces=true \
  --version 2.8.2

# The pg-operator chart hardcodes runAsNonRoot: true with no values.yaml override.
# This causes CreateContainerConfigError on Rancher Desktop (and possibly GKE with strict
# Pod Security Standards). Not needed on k3d. The chart would need a Kustomize post-renderer
# or upstream fix to work declaratively with Argo CD. See docs/cluster-setup.md for options.
if [ "$PATCH_SECURITY_CONTEXT" = true ]; then
  echo "  Patching percona-postgresql-operator for runAsNonRoot issue..."
  kubectl patch deployment percona-postgresql-operator-pg-operator \
    -n percona \
    --type strategic \
    --patch '{"spec": {"template": {"spec": {"containers": [{"name":"operator","securityContext":{"runAsNonRoot":false}}]}}}}'
fi

helm upgrade --install psmdb-operator percona/psmdb-operator \
  --namespace percona \
  --set watchAllNamespaces=true \
  --version 1.21.3

helm upgrade --install pxc-operator percona/pxc-operator \
  --namespace percona \
  --set watchAllNamespaces=true \
  --version 1.19.0

echo "  Waiting for all Percona operators..."
wait_rollout percona-postgresql-operator-pg-operator percona
wait_rollout psmdb-operator percona
wait_rollout pxc-operator percona

echo "=== Percona RBAC ==="
kubectl apply -f percona/rbac.yaml

# ---------- XRDs & Compositions ----------
echo "=== Data Service Definitions ==="
kubectl apply -f kafka/xrd.yaml -f kafka/composition.yaml
kubectl apply -f valkey/xrd.yaml -f valkey/composition.yaml
kubectl apply -f percona/PostgresXRD.yaml -f percona/PostgresComp.yaml
kubectl apply -f percona/MySQLXRD.yaml -f percona/MySQLComp.yaml
kubectl apply -f percona/MongoXRD.yaml -f percona/MongoComp.yaml

echo "  Waiting for XRDs to be established..."
kubectl wait --for=condition=Established xrd --all --timeout=60s

echo ""
echo "=== Setup complete ==="
echo "Run tests:  kubectl kuttl test tests/e2e/"
echo "Or if on Windows: ./test.ps1 (uses a Docker wrapper for kuttl)"
