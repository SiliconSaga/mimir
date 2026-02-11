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

## Quick Start

For a fresh system with k3d already installed:

```bash
k3d cluster create mimir-test --port "9080:80@loadbalancer" --port "9443:443@loadbalancer" --agents 2
./setup.sh
kubectl kuttl test tests/e2e/
```

The script is idempotent (`helm upgrade --install`, `--dry-run=client`). Use `--skip-crossplane` if Crossplane is managed by your infra repo.

## Usage

To interact with Mimir services, ensure you have the appropriate `Claim` definitions in your namespace.

## Testing

Mimir uses [kuttl](https://kuttl.dev/) for Kubernetes-native e2e testing, with BDD scenarios documented in `features/infrastructure.feature`.

```bash
# Run all e2e tests (creates isolated test resources, cleans up after)
kubectl kuttl test tests/e2e/

# Run specific test
kubectl kuttl test tests/e2e/ --test kafka-provisioning
```

### Test Coverage

| Component | Test | Provisioning Time |
|-----------|------|-------------------|
| Kafka (Strimzi) | `kafka-provisioning` | ~70-112s |
| Valkey (OT-Container-Kit) | `valkey-provisioning` | ~40-52s |
| PostgreSQL (Percona) | `postgres-provisioning` | ~150-210s |
| MySQL (Percona) | `mysql-provisioning` | ~257-330s |
| MongoDB (Percona) | `mongodb-provisioning` | ~126-140s |

> Tests run sequentially (`parallel: 1`). Full suite takes ~12 minutes on a 3-node k3d cluster. Timings measured on a fresh cluster with images pre-pulled for most components.

See [**Testing Strategy**](./docs/testing.md) for details on the testing approach and how it fits within the broader ecosystem.
