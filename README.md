# Percona and KubeDB Testing with k3d

This workspace demonstrates how to use Percona operators for database cluster management and KubeDB for lightweight database and user management in a Kubernetes environment. The goal is to show how to create and manage databases using Kubernetes Custom Resources (CRs) while maintaining enterprise-grade database operations.

## Prerequisites

- [k3d](https://k3d.io/) installed
- [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/) installed
- [Helm](https://helm.sh/docs/intro/install/) installed

## Project Structure

### PostgreSQL Files
- `percona-postgres-cluster.yaml`: Percona PostgreSQL cluster configuration
- `kubedb-postgres-database.yaml`: KubeDB PostgreSQL database CR
- `kubedb-postgres-user.yaml`: KubeDB PostgreSQL user CR

### MongoDB Files
- `percona-mongo-cluster.yaml`: Percona MongoDB cluster configuration
- `kubedb-mongo-database.yaml`: KubeDB MongoDB database CR
- `kubedb-mongo-user.yaml`: KubeDB MongoDB user CR

### Monitoring Files
- `pmm-values.yaml`: PMM Helm chart values
- `loki-values.yaml`: Loki Helm chart values

### Other Files
- `sample-app.yaml`: Sample application deployment
- `requirements.txt`: Python dependencies for the sample application

## Setup

1. **Create a k3d Cluster**

   ```bash
   k3d cluster create percona-kubedb-test
   ```

1. **Install Percona PostgreSQL Operator CRDs (one-time setup)**

   ```bash
   kubectl apply --server-side -f https://raw.githubusercontent.com/percona/percona-postgresql-operator/v2.6.0/deploy/crd.yaml
   # (Optional but recommended) Install RBAC for the operator
   kubectl apply -f https://raw.githubusercontent.com/percona/percona-postgresql-operator/v2.6.0/deploy/rbac.yaml -n pgo
   ```

1. **Install Percona PostgreSQL Operator**

   ```bash
   helm repo add percona https://percona.github.io/percona-helm-charts/
   helm repo update
   helm install pgo-operator \
     --namespace pgo \
     --create-namespace \
     percona/pg-operator
   ```

1. **Install Percona MongoDB Operator**

   ```bash
   helm repo add percona https://percona.github.io/percona-helm-charts/
   helm repo update
   helm install psmdb-operator \
     --namespace psmdb \
     --create-namespace \
     percona/psmdb-operator
   ```

1. **Install Percona Monitoring and Management (PMM)**

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

   > **TODO**: Loki integration for log management is currently disabled. The initial attempt encountered configuration issues with the latest version (6.30.1). Future work will include:
   > - Investigating the correct configuration for Loki 6.x
   > - Setting up proper storage backend
   > - Configuring log retention policies
   > - Integrating with PMM's Grafana instance

1. **Install KubeDB Community Edition**

   ```bash
   helm repo add kubedb https://kubedb.github.io/helm-charts
   helm repo update
   helm install kubedb \
     --namespace kubedb \
     --create-namespace \
     kubedb/kubedb
   ```

1. **Create PostgreSQL Cluster with Percona**

   ```bash
   kubectl apply -f percona-postgres-cluster.yaml
   ```

1. **Create MongoDB Cluster with Percona**

   ```bash
   kubectl apply -f percona-mongo-cluster.yaml
   ```

1. **Create PostgreSQL Database with KubeDB**

   ```bash
   kubectl apply -f kubedb-postgres-database.yaml
   ```

1. **Create PostgreSQL User with KubeDB**

   ```bash
   kubectl apply -f kubedb-postgres-user.yaml
   ```

1. **Create MongoDB Database with KubeDB**

   ```bash
   kubectl apply -f kubedb-mongo-database.yaml
   ```

1. **Create MongoDB User with KubeDB**

   ```bash
   kubectl apply -f kubedb-mongo-user.yaml
   ```

## Monitoring Setup

### Accessing PMM

1. **Get PMM Admin Password**:
   ```bash
   kubectl get secret -n pmm pmm-secret -o jsonpath='{.data.PMM_ADMIN_PASSWORD}' | base64 -d
   ```

2. **Access PMM UI**:
   ```bash
   kubectl port-forward -n pmm svc/monitoring-service 8080:80
   ```
   Then open http://localhost:8080 in your browser

### Default Dashboards

PMM comes with pre-built dashboards for:
- PostgreSQL Overview
- PostgreSQL Details
- MongoDB Overview
- MongoDB Details
- System Metrics
- Query Analytics

### Adding Databases to PMM

1. **For PostgreSQL**:
   - PMM automatically discovers PostgreSQL instances managed by Percona operator
   - No additional configuration needed

2. **For MongoDB**:
   - PMM automatically discovers MongoDB instances managed by Percona operator
   - No additional configuration needed

## Testing

- **Check Percona PostgreSQL Cluster Status**:
  ```bash
  kubectl get perconapgclusters.postgresql.percona.com
  ```

- **Check Percona MongoDB Cluster Status**:
  ```bash
  kubectl get perconaservermongodbs.psmdb.percona.com
  ```

- **Check KubeDB PostgreSQL Status**:
  ```bash
  kubectl get postgres.kubedb.com
  kubectl get postgresusers.kubedb.com
  ```

- **Check KubeDB MongoDB Status**:
  ```bash
  kubectl get mongodb.kubedb.com
  kubectl get mongodbusers.kubedb.com
  ```

### PostgreSQL Connection

To connect to the PostgreSQL cluster:
```bash
# Get the password
kubectl get secret -n pgo my-postgres-cluster-pguser-my-postgres-cluster -o jsonpath='{.data.password}' | base64 -d

# Connect using psql
psql "host=my-postgres-cluster-ha.pgo.svc port=5432 dbname=my-postgres-cluster user=my-postgres-cluster sslmode=require sslrootcert=/etc/ssl/certs/ca-certificates.crt"
```

### MongoDB Connection

To connect to the MongoDB cluster:
```bash
# Get the database admin password (note: the % at the end is a terminal artifact, don't include it)
kubectl get secret -n psmdb internal-my-mongo-cluster-users -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d

# Connect using mongosh (non-SSL for testing)
# Replace <password> with the password from above (without the % if present)
mongosh "mongodb://databaseAdmin:<password>@my-mongo-cluster-rs0-0.my-mongo-cluster-rs0.psmdb.svc.cluster.local:27017/admin"
```

### MongoDB Setup Notes

- The MongoDB image version should be specified as `percona/percona-server-mongodb:6.0.24-19` (or latest available version)
- The backup configuration requires a storage section, even for local testing
- For development/testing with a single-node replica set, set `allowUnsafeConfigurations: true` in the CR
- For production, use at least 3 nodes and set `allowUnsafeConfigurations: false`

#### MongoDB Status and Cleanup

Check MongoDB cluster status:
```bash
kubectl get perconaservermongodb -n psmdb
kubectl get pods -n psmdb
```

Full cleanup (if needed):
```bash
# Delete CR, StatefulSet, pods, and PVCs
kubectl delete perconaservermongodb -n psmdb my-mongo-cluster
kubectl delete statefulset -n psmdb my-mongo-cluster-rs0
kubectl delete pod -n psmdb -l app.kubernetes.io/instance=my-mongo-cluster
kubectl delete pvc -n psmdb -l app.kubernetes.io/instance=my-mongo-cluster
```

### Troubleshooting

#### MongoDB CR Deletion Issues
If deleting the MongoDB cluster hangs due to finalizers:
```bash
# Remove the finalizer
kubectl patch perconaservermongodb -n psmdb my-mongo-cluster -p '{"metadata":{"finalizers":[]}}' --type=merge
# Then delete the CR
kubectl delete perconaservermongodb -n psmdb my-mongo-cluster
```

## Sample Database Tests

### PostgreSQL Test

1. Create a test pod:
```bash
kubectl run postgres-test --rm -it --image=postgres:15 -- bash
```

2. Copy the test script to the pod:
```bash
kubectl cp postgres-test.sql postgres-test:/tmp/
```

3. Run the test script:
```bash
psql "host=my-postgres-cluster-ha.pgo.svc port=5432 dbname=my-postgres-cluster user=my-postgres-cluster sslmode=require sslrootcert=/etc/ssl/certs/ca-certificates.crt" -f /tmp/postgres-test.sql
```

### MongoDB Test

1. Create a test pod and copy the test script:
```bash
# Create the pod
kubectl run mongo-test --rm -it --image=mongo:6.0 -- bash

# In a separate terminal, copy the test script
kubectl cp mongo-test.js mongo-test:/tmp/
```

2. Connect and run the test (from within the pod):
```bash
# Get the database admin password (note: the % at the end is a terminal artifact, don't include it)
kubectl get secret -n psmdb internal-my-mongo-cluster-users -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d

# Connect and run the test (using non-SSL connection)
# Replace <password> with the password from above (without the % if present)
mongosh "mongodb://databaseAdmin:<password>@my-mongo-cluster-rs0-0.my-mongo-cluster-rs0.psmdb.svc.cluster.local:27017/admin" --file /tmp/mongo-test.js
```

Note: While SSL is recommended for production, we're using non-SSL for testing purposes. For production environments, you should configure and use SSL connections.

Both test scripts create a sample database with:
- A users collection/table
- Sample user data with roles
- Indexes on email fields
- Example queries and aggregations

## Notes

- Percona operators provide enterprise-grade database cluster management
- KubeDB provides lightweight database and user management
- PMM provides comprehensive monitoring and management
- This combination allows for clear separation of concerns:
  - Infrastructure team manages clusters via Percona
  - Application teams manage databases and users via KubeDB
  - Monitoring team manages observability via PMM
- Use underscores consistently for database and user names

## Cleanup

To clean up the k3d cluster and all resources:

```bash
k3d cluster delete percona-kubedb-test
```

## Troubleshooting

1. Check Percona PostgreSQL operator logs:
   ```bash
   kubectl logs -n pgo -l app.kubernetes.io/name=postgresql-operator
   ```

2. Check Percona MongoDB operator logs:
   ```bash
   kubectl logs -n psmdb -l app.kubernetes.io/name=psmdb-operator
   ```

3. Check KubeDB operator logs:
   ```bash
   kubectl logs -n kubedb -l app.kubernetes.io/name=kubedb
   ```

4. Check PMM logs:
   ```bash
   kubectl logs -n pmm -l app.kubernetes.io/name=pmm
   ```

5. Check database status:
   ```bash
   kubectl describe postgres my_database
   kubectl describe postgresuser my_user
   kubectl describe mongodb my_database
   kubectl describe mongodbuser my_user
   ```

6. Verify network connectivity:
   ```bash
   kubectl get services
   ```