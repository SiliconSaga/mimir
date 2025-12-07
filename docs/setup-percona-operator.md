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

### Install Operator via Helm

```bash
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update
helm install percona-postgresql-operator \
  --namespace percona-system \
  --set watchNamespace="" \
  percona/pg-operator
```

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

Check that operators are running:

```bash
# PostgreSQL Operator
kubectl logs -n pgo -l app.kubernetes.io/name=postgresql-operator

# MongoDB Operator
kubectl logs -n psmdb -l app.kubernetes.io/name=psmdb-operator
```
