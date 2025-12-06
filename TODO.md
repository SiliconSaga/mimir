# Percona Workspace TODOs

## PMM & Observability
- **Loki Integration**: Loki integration for log management is currently disabled. 
    - Investigating the correct configuration for Loki 6.x
    - Setting up proper storage backend
    - Configuring log retention policies
    - Integrating with PMM's Grafana instance
- **PMM MongoDB Integration**: The sidecar container is not appearing in MongoDB pods despite proper secret configuration. Needs investigation.

## MongoDB
- **XMongoDB**: Create a Crossplane CompositeResourceDefinition (XRD) and Composition for MongoDB, similar to `XPostgreSQL`. 

## MySQL
- **XMySQL**: Create a Crossplane CompositeResourceDefinition (XRD) and Composition for MySQL, similar to `XPostgreSQL`. 

## Cleanup
- **KubeDB**: Verify if all KubeDB related manifests (`kubedb-*.yaml`) can be safely removed if we are fully committed to Percona + Crossplane.
