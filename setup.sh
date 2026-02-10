#!/bin/bash
# Mimir Data Services Setup
# Installs all operators, Crossplane resources, and data service definitions.
# Assumes: Crossplane is already installed (from infra repo), kubectl context is set.
# Run: ./setup.sh [--skip-crossplane]
set -euo pipefail

SKIP_CROSSPLANE=false
for arg in "$@"; do
  case $arg in
    --skip-crossplane) SKIP_CROSSPLANE=true ;;
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
    --namespace crossplane-system --create-namespace --wait

  echo "=== Crossplane Providers & Functions ==="
  kubectl apply -f platform.yaml
  kubectl apply -f - <<'EOF'
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

  # CRITICAL: ProviderConfigs need CRDs to exist first
  wait_healthy "providers.pkg.crossplane.io"
  wait_healthy "functions.pkg.crossplane.io"

  echo "=== Provider Configs ==="
  kubectl apply -f provider-configs.yaml
fi

# ---------- Namespace RBAC ----------
echo "=== Crossplane RBAC ==="
# Apply if the file exists (may live in infra repo instead)
if [ -f ../refr-k8s/crossplane-rbac.yaml ]; then
  kubectl apply -f ../refr-k8s/crossplane-rbac.yaml
else
  echo "  Skipping crossplane-rbac.yaml (not found at ../refr-k8s/)"
fi

# ---------- Operators ----------
echo "=== Kafka (Strimzi) ==="
kubectl create ns kafka-system --dry-run=client -o yaml | kubectl apply -f -
helm repo add strimzi https://strimzi.io/charts/ 2>/dev/null
helm repo update >/dev/null
helm upgrade --install strimzi-kafka-operator strimzi/strimzi-kafka-operator \
  --namespace kafka-system \
  --set watchAnyNamespace=true
wait_rollout strimzi-cluster-operator kafka-system

echo "=== Valkey (OT-Container-Kit) ==="
kubectl create ns valkey-system --dry-run=client -o yaml | kubectl apply -f -
helm repo add ot-helm https://ot-container-kit.github.io/helm-charts/ 2>/dev/null
helm repo update >/dev/null
helm upgrade --install redis-operator ot-helm/redis-operator \
  --namespace valkey-system
wait_rollout redis-operator valkey-system

echo "=== Percona Operators (PostgreSQL, MongoDB, MySQL) ==="
kubectl create ns percona-system --dry-run=client -o yaml | kubectl apply -f -
helm repo add percona https://percona.github.io/percona-helm-charts/ 2>/dev/null
helm repo update >/dev/null

# CRITICAL: Use --set watchAllNamespaces=true (NOT --set watchNamespace="")
# Helm --set silently ignores empty strings, causing the operator to only watch its own namespace.
helm upgrade --install percona-postgresql-operator percona/pg-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true

helm upgrade --install psmdb-operator percona/psmdb-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true

helm upgrade --install pxc-operator percona/pxc-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true

echo "  Waiting for all Percona operators..."
wait_rollout percona-postgresql-operator-pg-operator percona-system
wait_rollout psmdb-operator percona-system
wait_rollout pxc-operator percona-system

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
