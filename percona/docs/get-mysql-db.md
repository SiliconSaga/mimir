# Getting a MySQL Database

To provision a new MySQL (Percona XtraDB Cluster) database, create a `XMySQL` custom resource.
(See `MySQLXRD.yaml` and `MySQLComp.yaml` for definition)

## Example

Apply to cluster:

```bash
kubectl create ns my-mysql-ns
kubectl apply -f MySQLSampleDB.yaml
```

## Accessing the Database

### Connection Details

*   **Namespace**: Your application namespace (e.g., `my-mysql-ns`)
*   **Host**: `my-mysql-db.my-mysql-ns.svc.cluster.local` (Use the HAProxy service)
*   **Port**: `3306`
*   **User**: `root` or `app_user` (depending on setup, PXC creates `root`, `xtrabackup`, etc.)
*   **Password**: Retrieved from the generated secret.

### Retrieving Credentials

The PXC operator generates a secret named `<cluster-name>-secrets`.

```bash
# Get root password
kubectl get secret -n my-mysql-ns my-mysql-db-secrets -o jsonpath='{.data.root}' | base64 -d
```

## Testing Connectivity

1.  **Start a Client Pod:**

    ```bash
    kubectl run mysql-client --rm -it --image=percona/percona-xtradb-cluster:8.0 --restart=Never --namespace my-mysql-ns -- bash
    ```

2.  **Connect via MySQL Client:**

    Inside the pod:

    ```bash
    # Retrieve password (or paste it if you got it earlier)
    export MYSQL_PWD=$(cat /etc/secret/root-password) # If mounted, otherwise paste it

    # Connect
    mysql -h my-mysql-db -u root -p

    # Show databases that exist to validate
    SHOW DATABASES;
    ```
