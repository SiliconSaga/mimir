# Requesting a PostgreSQL Database

To provision a new PostgreSQL database, create a `XPostgreSQL` custom resource.
(See `PostgresXRD.yaml` and `PostgresComp.yaml` for definition)

## Example

Apply to cluster:

```bash
kubectl create ns my-pg-ns
kubectl apply -f PostgresSampleDB.yaml
```

## Accessing the Database

The database is deployed directly into the **same namespace** where you created the `PostgreSQLInstance` claim.

> [!IMPORTANT]
> You must ensure the target namespace exists before creating the `PostgreSQLInstance`.

### Connection Details

*   **Namespace**: Your application namespace (e.g., `my-ns`)
*   **Host**: `my-db.my-ns.svc.cluster.local`
*   **Port**: `5432`
*   **Database**: `mydb` (from `databaseName` parameter)
*   **User**: `mydb` (same as database name)
*   **Password**: Retrieved from the secret in your namespace

### Retrieving Credentials

```bash
# Get the password from the secret in your namespace
kubectl get secret my-pg-db-user-secret -n my-pg-ns -o jsonpath="{.data.password}" | base64 -d
```

### Testing Connectivity

You can verify connectivity and run SQL queries using a temporary client pod.

1.  **Deploy a Client Pod:**

    ```bash
    kubectl run postgres-client --rm -it --image=postgres:15 --restart=Never --namespace my-pg-ns -- bash
    ```

2.  **Connect to the Database:**

    Inside the pod, run:

    ```bash
    # Replace <password> with the actual password retrieved above
    PGPASSWORD='<password>' psql -h my-pg-db -U my-pg-db -d my-pg-db
    ```

3.  **Run SQL Commands:**

    ```sql
    -- Check version
    SELECT version();

    -- Create a table
    CREATE TABLE users (id SERIAL PRIMARY KEY, name TEXT, email TEXT);

    -- Insert data
    INSERT INTO users (name, email) VALUES ('Alice', 'alice@example.com');

    -- Query data
    SELECT * FROM users;
    ```
