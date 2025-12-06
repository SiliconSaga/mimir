# Percona Operator Setup

This guide covers the installation of Percona Operators for PostgreSQL and MongoDB.

## Prerequisites

- [k3d](https://k3d.io/) installed
- [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/) installed
- [Helm](https://helm.sh/docs/intro/install/) installed

## 1. Install Percona PostgreSQL Operator

### Install CRDs (One-time setup)

```bash
kubectl apply --server-side -f https://raw.githubusercontent.com/percona/percona-postgresql-operator/v2.6.0/deploy/crd.yaml
# (Optional but recommended) Install RBAC for the operator
kubectl apply -f https://raw.githubusercontent.com/percona/percona-postgresql-operator/v2.6.0/deploy/rbac.yaml -n pgo
```

### Install Operator via Helm

```bash
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update
helm install pgo-operator \
  --namespace pgo \
  --create-namespace \
  percona/pg-operator
```

## 2. Install Percona MongoDB Operator

```bash
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update
helm install psmdb-operator \
  --namespace psmdb \
  --create-namespace \
  percona/psmdb-operator
```

## Verification

Check that operators are running:

```bash
# PostgreSQL Operator
kubectl logs -n pgo -l app.kubernetes.io/name=postgresql-operator

# MongoDB Operator
kubectl logs -n psmdb -l app.kubernetes.io/name=psmdb-operator
```
