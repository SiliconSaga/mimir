# Kafka Service (Strimzi)

This component provides a managed Apache Kafka cluster using the **Strimzi Operator** in KRaft mode (no ZooKeeper), abstracted via Crossplane.

## Documentation

- [**Architecture**](docs/architecture.md): Technical architecture and design decisions.
- [**Setup Guide**](docs/setup-strimzi.md): How to install the Strimzi Operator.

## Usage

To provision a Kafka cluster, create a `KafkaCluster` claim in your application namespace.

See `claim.yaml` for an example.

### Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `replicas` | integer | 3 | Number of Kafka broker nodes (combined controller + broker in KRaft mode) |
| `storageSize` | string | "10Gi" | PVC size for storage per broker node |
| `version` | string | "4.0.0" | Kafka version |

### Example Claim

```yaml
apiVersion: mimir.siliconsaga.org/v1alpha1
kind: KafkaCluster
metadata:
  name: my-kafka
  namespace: my-app
spec:
  parameters:
    replicas: 3
    storageSize: "20Gi"
    version: "4.0.0"
```

## Validation

To verify the service works:

1. **Apply a Test Claim**:

   ```bash
   kubectl apply -f claim.yaml
   ```

2. **Check Status**:

   Wait for the claim to be `Ready`.

   ```bash
   kubectl get kafkaclusters -n mimir
   ```

3. **Connection Details**:

   Read the address off the claim — the composition publishes it, so there is nothing to assemble:

   ```bash
   kubectl get kafkacluster kafka-test -n mimir -o jsonpath='{.status.bootstrapServers}'
   ```

   It is also the `BOOTSTRAP` column of `kubectl get kafkaclusters`.

   The address resolves to `<claim-name>-kafka-bootstrap.kafka.svc:9092`, because Strimzi resources are named after the claim. Prefer reading `status.bootstrapServers` anyway: it is the supported contract, whereas the pattern is an implementation detail. Do **not** derive the name from `spec.resourceRef.name` — that is the composite, which Crossplane suffixes randomly, and it is not what the cluster is called.

### Validation with Client

To verify connectivity, exec into one of the Kafka broker pods:

```bash
# The Strimzi cluster is named after the CLAIM, so the label selector is just
# the claim name — no lookup needed (run both lines)
KAFKA_POD=$(kubectl get pods -n kafka -l strimzi.io/cluster=kafka-test,strimzi.io/broker-role=true -o jsonpath='{.items[0].metadata.name}')
echo "Using pod: $KAFKA_POD"

# List topics
kubectl exec -n kafka $KAFKA_POD -- \
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list
```

Create a test topic:

```bash
kubectl exec -n kafka $KAFKA_POD -- \
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --create --topic test-topic2 --partitions 3 --replication-factor 2
```

**Note:** Using `kubectl exec` on existing broker pods is more reliable than `kubectl run` in some cluster configurations (e.g., k3d) where pod attachment may have networking issues.

## Key Files

| File | Purpose |
|------|---------|
| `xrd.yaml` | Crossplane CompositeResourceDefinition for KafkaCluster |
| `composition.yaml` | Crossplane Composition using function-go-templating |
| `claim.yaml` | Example test claim |

## Consumption from Other Projects

External projects (like `Heimdall` or application projects) should treat this as a dependency.

1. **Define Dependency**: Read `status.bootstrapServers` off the claim rather than hardcoding an address. A committed manifest carrying a guessed cluster name is the failure this API exists to prevent — and a `KafkaTopic` whose `strimzi.io/cluster` label names a cluster that does not exist is silently ignored by Strimzi, so the mistake surfaces as missing topics with no error anywhere.
2. **Network Policies**: Ensure your namespace allows egress to `kafka`.
3. **Topics**: Use the Strimzi `KafkaTopic` CRD to create topics (managed by the Entity Operator).

### Creating Topics

```yaml
apiVersion: kafka.strimzi.io/v1beta2
kind: KafkaTopic
metadata:
  name: my-topic
  namespace: kafka
  labels:
    strimzi.io/cluster: <kafka-cluster-name>
spec:
  partitions: 3
  replicas: 2
  config:
    retention.ms: 604800000  # 7 days
```

