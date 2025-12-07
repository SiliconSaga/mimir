# Percona Workspace TODOs

## PMM & Observability
- **Loki Integration**: Loki integration for log management is currently disabled. 
    - Investigating the correct configuration for Loki 6.x
    - Setting up proper storage backend
    - Configuring log retention policies
    - Integrating with PMM's Grafana instance
- **PMM MongoDB Integration**: The sidecar container is not appearing in MongoDB pods despite proper secret configuration. Needs investigation.
