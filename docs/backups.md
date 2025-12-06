## Backups and Restore

The Percona Operator automatically handles backups using `pgBackRest`.

### Verifying Backups

You can verify that backups are being created by checking the `pgbackrest` repository pod and the backup jobs.

```bash
# Check for backup jobs
kubectl get jobs -n my-ns

# Check backup status in the PerconaPGCluster custom resource
kubectl get perconapgcluster my-db -n my-ns -o yaml | grep -A 10 status:
```

### Restoring a Database

To restore a database, you typically create a new `PostgreSQLInstance` and reference the backup repository of the old instance. 
*(Note: Specific restore procedures involving Crossplane Composition parameters are currently under development. For manual restore using the operator directly, refer to the [Percona Documentation](https://docs.percona.com/percona-operator-for-postgresql/2.0/backups.html).)*
