# Percona Operator Setup

This guide covers the installation of Percona Operators for PostgreSQL and MongoDB.

## Prerequisites

- [k3d](https://k3d.io/) installed
- [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/) installed
- [Helm](https://helm.sh/docs/intro/install/) installed

## 1. Setup Infrastructure

### Consolidated Namespace

We use a single namespace `percona-system` for all database operators.

```bash
kubectl create ns percona-system
```

### Install RBAC for Crossplane

For Crossplane to provision Percona resources, it needs permission. Apply the RBAC configuration:

```bash
kubectl apply -f rbac.yaml
```

## 2. Install Percona PostgreSQL Operator

### Install CRDs (One-time setup)

```bash
kubectl apply --server-side -f https://raw.githubusercontent.com/percona/percona-postgresql-operator/v2.7.0/deploy/crd.yaml
```

### Install RBAC (Required for Cluster-Wide Mode)

Helm chart might not create ClusterRoles by default when upgrading or if not configured strictly. Apply our custom RBAC:

```bash
kubectl apply -f ../postgres-operator-rbac.yaml
```

### Install Operator via Helm

```bash
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update
helm install percona-postgresql-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true \
  percona/pg-operator
```

> **Warning**: Do NOT use `--set watchNamespace=""` — passing an empty string via `--set` is silently ignored by Helm, causing the operator to only watch its own namespace. Use `--set watchAllNamespaces=true` instead.

## 3. Install Percona MongoDB Operator

```bash
helm install psmdb-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true \
  percona/psmdb-operator
```

## 4. Install Percona MySQL (PXC) Operator

```bash
helm install pxc-operator \
  --namespace percona-system \
  --set watchAllNamespaces=true \
  percona/pxc-operator
```

## Verification

Check that all operators are running in the shared namespace:

```bash
kubectl get pods -n percona-system

# Check individual operator logs
kubectl logs -n percona-system -l app.kubernetes.io/name=pg-operator
kubectl logs -n percona-system -l app.kubernetes.io/name=psmdb-operator
kubectl logs -n percona-system -l app.kubernetes.io/name=pxc-operator
```

Verify operators are watching all namespaces (critical for Crossplane-managed claims):

```bash
# Should show WATCH_NAMESPACE is empty or watchAllNamespaces is true
kubectl get deployment -n percona-system -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .spec.template.spec.containers[*]}{range .env[*]}{.name}={.value}{" "}{end}{end}{"\n"}{end}'
```
