# Kafka Architecture

This document describes the technical architecture of the Kafka service in Mimir.

## Overview

The Kafka service uses a layered architecture that separates infrastructure management from service consumption:

```
┌─────────────────────────────────────────────────────────────┐
│                     User Namespace                          │
│  ┌───────────────────────────────────────────────────────┐  │
│  │ KafkaCluster Claim (mimir.siliconsaga.org/v1alpha1)   │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                     Crossplane                              │
│  ┌───────────────────────────────────────────────────────┐  │
│  │ XRD: xkafkaclusters.mimir.siliconsaga.org             │  │
│  └───────────────────────────────────────────────────────┘  │
│                             │                               │
│                             ▼                               │
│  ┌───────────────────────────────────────────────────────┐  │
│  │ Composition: xkafkacluster-strimzi                    │  │
│  │ (function-go-templating pipeline)                     │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                   kafka-system Namespace                    │
│  ┌───────────────────────────────────────────────────────┐  │
│  │ Strimzi Operator                                      │  │
│  └───────────────────────────────────────────────────────┘  │
│                             │                               │
│                             ▼                               │
│  ┌───────────────────────────────────────────────────────┐  │
│  │ Kafka CR (KRaft Mode)    │    KafkaNodePool CR        │  │
│  └───────────────────────────────────────────────────────┘  │
│                             │                               │
│                             ▼                               │
│  ┌───────────────────────────────────────────────────────┐  │
│  │ Kafka Pods + Services + PVCs                          │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

## Components

### 1. Crossplane XRD (CompositeResourceDefinition)

The XRD defines the user-facing API (`KafkaCluster`) with a simple, opinionated schema:

- **API Group**: `mimir.siliconsaga.org`
- **Kind**: `KafkaCluster` (claim) / `XKafkaCluster` (composite)
- **Version**: `v1alpha1`

The XRD abstracts away Strimzi complexity, exposing only essential parameters.

### 2. Crossplane Composition

The Composition uses `function-go-templating` to translate the claim into Strimzi resources:

1. **Kafka CR**: The main Strimzi Kafka resource in KRaft mode
2. **KafkaNodePool CR**: Defines the broker/controller nodes

Both resources are wrapped in `kubernetes.crossplane.io/v1alpha2 Object` to allow Crossplane to manage arbitrary CRDs.

### 3. Strimzi Operator

Strimzi manages the Kafka lifecycle:

- Deploys Kafka broker pods
- Manages configuration and rolling updates
- Handles TLS certificates (for TLS listener)
- Deploys Entity Operator (topic/user management)

## KRaft Mode

This implementation uses **KRaft mode** (Kafka Raft), which eliminates ZooKeeper:

### Benefits

- **Simpler architecture**: No ZooKeeper cluster to manage
- **Faster startup**: Metadata stored in Kafka itself
- **Fewer resources**: Reduced pod count and memory usage
- **Better scalability**: Improved partition limits

### Combined Node Pools

In this configuration, each Kafka node acts as both:

- **Controller**: Manages cluster metadata (replaces ZooKeeper)
- **Broker**: Handles client connections and data

For production, you may want separate controller and broker node pools.

## Network Architecture

### Internal Listeners

| Listener | Port | TLS | Purpose |
|----------|------|-----|---------|
| `plain` | 9092 | No | Internal unencrypted traffic |
| `tls` | 9093 | Yes | Internal encrypted traffic |

### Service Discovery

Strimzi creates a bootstrap service for client connections:

```
<cluster-name>-kafka-bootstrap.kafka-system.svc:9092
```

Individual broker services are also available:

```
<cluster-name>-kafka-<broker-id>.kafka-system.svc:9092
```

## Storage

Each broker uses a Persistent Volume Claim (PVC) with:

- **Storage Class**: `local-path` (configurable)
- **Access Mode**: `ReadWriteOnce`
- **Delete Claim**: `false` (data preserved on deletion)

## Resource Limits

Default resource configuration per broker:

| Resource | Request | Limit |
|----------|---------|-------|
| CPU | 250m | 1 |
| Memory | 512Mi | 2Gi |

Adjust these based on your workload requirements.

## Replication Settings

The composition automatically sets replication factors based on replica count:

| Setting | Value |
|---------|-------|
| `offsets.topic.replication.factor` | min(3, replicas) |
| `transaction.state.log.replication.factor` | min(3, replicas) |
| `transaction.state.log.min.isr` | min(2, replicas) |
| `default.replication.factor` | min(3, replicas) |
| `min.insync.replicas` | min(2, replicas) |

## Entity Operator

The composition includes the Strimzi Entity Operator, which provides:

- **Topic Operator**: Manages `KafkaTopic` CRDs
- **User Operator**: Manages `KafkaUser` CRDs (for ACLs and authentication)

This allows declarative topic and user management via Kubernetes resources.

## Health and Readiness

### Object-Level Readiness

The Crossplane composition uses CEL queries to detect when individual resources are ready:

```yaml
celQuery: "object.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')"
```

This checks the Strimzi `Ready` condition on the Kafka CR.

### Composite Readiness (function-auto-ready)

In Pipeline mode, Crossplane requires `function-auto-ready` to propagate readiness from composed resources to the composite resource (XR) and claim:

```yaml
- step: auto-ready
  functionRef:
    name: function-auto-ready
```

This function checks all composed resources and marks the XR as Ready when all are ready. Without it, the claim would remain in "Creating" state indefinitely.
