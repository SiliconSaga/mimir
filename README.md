# Mimir (Data Management Layer)

**Mimir** is the keeper of wisdom and memory. This workspace provides the Data Management layer for the platform, offering standard interfaces for Databases, Caches, and Event Buses.

This infrastructure is built on **Crossplane**, allowing other workspaces (like Heimdall or Demicracy Apps) to request data services via standard Kubernetes Claims (`Kind: <Type>Cluster`, `Group: mimir.siliconsaga.org`).

## Components

### 🧠 [Kafka](./kafka/)
Distributed Event Streaming capabilities powered by **Strimzi**.
- **Claim Kind**: `KafkaCluster`
- **Use Case**: High-throughput event buses, streaming data pipelines.

### ⚡ [Valkey](./valkey/)
High-performance key-value store powered by **OT-Container-Kit** (using Valkey image).
- **Claim Kind**: `ValkeyCluster`
- **Use Case**: Caching, session store, real-time analytics.

### 🐘 [Percona](./percona/)
Enterprise-grade SQL and NoSQL databases powered by **Percona Operators**.
- **Supported**: PostgreSQL, MySQL (XtraDB), MongoDB.
- **Use Case**: Primary relational or document storage.

## Usage

To interact with Mimir services, ensure you have the appropriate `Claim` definitions in your namespace.

## Testing

Mimir uses [kuttl](https://kuttl.dev/) for Kubernetes-native e2e testing, with BDD scenarios documented in `features/infrastructure.feature`.

| Method | Command | Use Case |
|--------|---------|----------|
| Quick check | `./test/verify_infrastructure.sh` | Manual verification |
| Full e2e | `kubectl kuttl test tests/e2e/` | Comprehensive testing |

See [**Testing Strategy**](./docs/testing.md) for details on the testing approach and how it fits within the broader ecosystem.
