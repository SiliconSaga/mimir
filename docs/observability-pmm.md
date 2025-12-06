# PMM (Percona Monitoring and Management) Setup

PMM provides comprehensive monitoring for your database clusters.

## Installation

```bash
# Add PMM Helm repository
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update

# Create PMM namespace
kubectl create namespace pmm

# Install PMM with custom values
helm install pmm \
  --namespace pmm \
  -f pmm-values.yaml \
  percona/pmm
```

> **TODO**: Loki integration for log management is currently disabled. See [TODO](../TODO.md).

## PMM Integration with Operators

### Creating the PMM Secret

To enable PMM integration with the Percona Server MongoDB Operator (and others), create a secret with PMM credentials:

```bash
kubectl create secret generic pmm-secret -n psmdb \
  --from-literal=PMM_SERVER_USER=admin \
  --from-literal=PMM_SERVER_PASSWORD=admin \
  --from-literal=PMM_SERVER_API_KEY=admin
```

Ensure the secret exists in the namespace where the database cluster runs (`psmdb` for Mongo, `pgo` for Postgres).

## Accessing PMM

1. **Get PMM Admin Password**:
   ```bash
   kubectl get secret -n pmm pmm-secret -o jsonpath='{.data.PMM_ADMIN_PASSWORD}' | base64 -d
   ```

2. **Access PMM UI**:
   ```bash
   kubectl port-forward -n pmm svc/monitoring-service 8080:80
   ```
   Open http://localhost:8080.

## Default Dashboards

PMM includes dashboards for:
- PostgreSQL Overview/Details
- MongoDB Overview/Details
- System Metrics
- Query Analytics

## Adding Databases

*   **PostgreSQL**: Automatically discovered if configured in CR.
*   **MongoDB**: Automatically discovered if configured in CR.
