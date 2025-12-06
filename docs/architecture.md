## Architecture

*   **Crossplane**: Manages the abstraction and lifecycle of the database.
*   **Percona PostgreSQL Operator**: Orchestrates the PostgreSQL clusters, handling high availability, backups, and updates.
*   **Namespace Isolation**: Each database instance runs in its own namespace for security and resource isolation.

