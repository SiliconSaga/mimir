# Getting a MongoDB Database

> [!NOTE]
> Currently, MongoDB provisioning is manual. A Crossplane composition (XMongoDB) is planned for the future.

## Prerequisites

- Percona MongoDB Operator installed (see [Setup](setup-percona-operator.md))

## Create a MongoDB Cluster

Use the `percona-mongo-cluster.yaml` manifest:

```bash
kubectl apply -f percona-mongo-cluster.yaml
```

### Setup Notes

- The MongoDB image version should be specified as `percona/percona-server-mongodb:6.0.24-19` (or latest available version)
- The backup configuration requires a storage section, even for local testing
- For development/testing with a single-node replica set, set `allowUnsafeConfigurations: true` in the CR
- For production, use at least 3 nodes and set `allowUnsafeConfigurations: false`

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
