# Getting a MongoDB Database

To provision a new MongoDB database cluster, create a `XMongoDB` custom resource.
(See `MongoXRD.yaml` and `MongoComp.yaml` for definition)

## Example: `my-mongo.yaml`

```yaml
apiVersion: database.example.org/v1alpha1
kind: MongoDBInstance
metadata:
  name: my-mongo
  namespace: my-app-ns
spec:
  parameters:
    storageSize: 5Gi
    version: "6.0.24-19"
    replicas: 1
```

Apply it to the cluster:

```bash
kubectl apply -f my-mongo.yaml
```

## Accessing the Database

The database is deployed directly into the **same namespace** where you created the `MongoDBInstance` claim.

### Connection Details

*   **Namespace**: Your application namespace (e.g., `my-app-ns`)
*   **Host**: `my-mongo.my-app-ns.svc.cluster.local` (or whatever the Service is named)
*   **Port**: `27017`
*   **User/Password**: 
    *   The Percona Operator automatically generates secrets.
    *   Secret Name: `my-mongo-secrets` (based on claim name)
    *   Keys: `MONGODB_DATABASE_ADMIN_PASSWORD`, `MONGODB_CLUSTER_ADMIN_PASSWORD`, etc.

### Retrieving Credentials

```bash
kubectl get secret my-mongo-secrets -n my-app-ns -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d
```

### Setup Notes

*   **Replicas**: Default is 1. For production, set to 3.
*   **Backup**: Logical backups are enabled by default (local storage).


## Checking Status

```bash
kubectl get perconaservermongodb -n psmdb
kubectl get perconaservermongodbs.psmdb.percona.com
kubectl get pods -n psmdb
```

## Connecting to MongoDB

### Get Admin Password

```bash
# Get the database admin password (note: the % at the end is a terminal artifact, don't include it)
kubectl get secret -n psmdb internal-my-mongo-cluster-users -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d
```

### Connect using mongosh (non-SSL for testing)

```bash
# Replace <password> with the password from above (without the % if present)
mongosh "mongodb://databaseAdmin:<password>@my-mongo-cluster-rs0-0.my-mongo-cluster-rs0.psmdb.svc.cluster.local:27017/admin"
```

## Testing

1. **Create a test pod:**

   ```bash
   kubectl run mongo-test --rm -it --image=mongo:6.0 -- bash
   ```

2. **Copy test script (optional):**

   ```bash
   kubectl cp mongo-test.js mongo-test:/tmp/
   ```

3. **Run Test:**

   Connect as shown above and run commands or execute the script.

## Troubleshooting

### Deletion Hangs

If deleting the MongoDB cluster hangs due to finalizers:

```bash
# Remove the finalizer
kubectl patch perconaservermongodb -n psmdb my-mongo-cluster -p '{"metadata":{"finalizers":[]}}' --type=merge
# Then delete the CR
kubectl delete perconaservermongodb -n psmdb my-mongo-cluster
```
