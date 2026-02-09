# Percona & Crossplane Database Workspace

This directory contains configuration for running enterprise-grade databases using **Percona Operators** and **Crossplane** on Kubernetes.

## Architecture

*   **Crossplane**: Manages the abstraction and lifecycle of the database.
*   **Percona PostgreSQL Operator**: Orchestrates the PostgreSQL clusters, handling high availability, backups, and updates.
*   **Namespace Isolation**: Each database instance runs in its own namespace for security and resource isolation.

## Documentation

### 🚀 Getting Started
*   [**Setup Guide**](docs/setup-percona-operator.md): How to install Percona Operators (PostgreSQL & MongoDB) and PMM.

### 🐘 PostgreSQL
*   [**Provision PostgreSQL**](docs/get-postgres-db.md): How to create a Postgres database using Crossplane (`PostgreSQLInstance`).
*   [**Manual Cluster Config**](percona-postgres-cluster.yaml): Reference manifest for manually creating a PerconaPGCluster (bypassing Crossplane).

### 🍃 MongoDB
*   [**Provision MongoDB**](docs/get-mongo-db.md): How to create a MongoDB database using Crossplane (`MongoDBInstance`).

### 🐬 MySQL (PXC)
*   [**Provision MySQL**](docs/get-mysql-db.md): How to create a MySQL database using Crossplane (`MySQLInstance`).

### 📊 Observability
*   [**PMM Setup**](docs/observability-pmm.md): Monitoring your databases with Percona Monitoring and Management.

### ⚠️ Legacy / Archive
*   [**Archive**](archive/): Contains deprecated documentation regarding `db-operator`, `YAPGO`, and older experiments.

## Key Files

*   `PostgresComp.yaml`: Crossplane Composition for PostgreSQL.
*   `PostgresXRD.yaml`: Crossplane CompositeResourceDefinition for PostgreSQL.
*   `MongoComp.yaml`: Crossplane Composition for MongoDB.
*   `MongoXRD.yaml`: Crossplane CompositeResourceDefinition for MongoDB.
*   `MySQLComp.yaml`: Crossplane Composition for MySQL (PXC).
*   `MySQLXRD.yaml`: Crossplane CompositeResourceDefinition for MySQL.
*   `percona-postgres-cluster.yaml`: Reference CR for PostgreSQL.
*   `percona-mongo-cluster.yaml`: Reference CR for MongoDB.

## Future Work
## Future Work & Known Issues
*   **Loki Integration**: Currently disabled. Needs correct configuration for Loki 6.x and storage backends.
*   **PMM MongoDB Integration**: Sidecar container for PMM is not injecting correctly; needs investigation.
*   **XMySQL Support**: Planned support for `XMySQL` composite resoures.