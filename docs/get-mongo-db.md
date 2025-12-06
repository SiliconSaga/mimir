# Getting a MongoDB Database

To provision a new MongoDB database cluster, create a `XMongoDB` custom resource.
(See `MongoXRD.yaml` and `MongoComp.yaml` for definition)

## Example

`MongoSampleDB.yaml`

Apply it to the cluster:

```bash
kubectl apply -f MongoSampleDB.yaml
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
kubectl get secret my-mongo-db-secrets -n my-mongo-ns -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d
```

### Setup Notes

*   **Replicas**: Default is 1. For production, set to 3.
*   **Backup**: Logical backups are enabled by default (local storage).


## Checking Status

```bash
# Check resources in your target namespace (e.g., default)
kubectl get perconaservermongodb -n my-mongo-ns
kubectl get pods -n my-mongo-ns
```

## Connecting to MongoDB

### Get Admin Password

```bash
# Get the database admin password from the secret in YOUR namespace
# The secret name is usually <claim-name>-secrets
kubectl get secret -n my-mongo-ns my-mongo-db-secrets -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d
```

### Connect using mongosh (non-SSL for testing)

```bash
# Replace <password> with the password from above (without the % if present)
# Host format: <cluster-name>-rs0.<namespace>.svc.cluster.local
mongosh "mongodb://databaseAdmin:<password>@my-mongo-db-rs0.my-mongo-ns.svc.cluster.local:27017/admin"
```

## Testing Connectivity
 
1.  **Retrieve Password:**

    ```bash
    kubectl get secret -n my-mongo-ns my-mongo-db-secrets -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d
    ```

2.  **Start a Client Shell:**

    Start a temporary pod with MongoDB tools installed:

    ```bash
    kubectl run mongo-test-client --rm -it --image=percona/percona-server-mongodb:6.0.24-19 --restart=Never --namespace my-mongo-ns -- bash
    ```

3.  **Connect via Mongosh:**

    Inside the pod, run the connection command (replace `<password>`):
    
    ```bash
    # Connect to the primary replica
    mongosh "mongodb://databaseAdmin:<password>@my-mongo-db-rs0.my-mongo-ns.svc.cluster.local:27017/admin?ssl=false"
    ```

4.  **Run Commands:**

    ```javascript
    db.adminCommand('ping')
    show dbs
    ```

## Troubleshooting

### Deletion Hangs

If deleting the MongoDB cluster hangs due to finalizers:

```bash
# Remove the finalizer (replace <namespace> and <cluster-name>)
kubectl patch perconaservermongodb -n <namespace> <cluster-name> -p '{"metadata":{"finalizers":[]}}' --type=merge

# Then delete the CR
kubectl delete perconaservermongodb -n <namespace> <cluster-name>
```
