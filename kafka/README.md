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

   The bootstrap server address follows the pattern: `<composite-name>-kafka-bootstrap.kafka-system.svc:9092`

   Get the bootstrap server:

   ```bash
   COMPOSITE_NAME=$(kubectl get kafkacluster kafka-test -n mimir -o jsonpath='{.spec.resourceRef.name}')
   KAFKA_BOOTSTRAP="${COMPOSITE_NAME}-kafka-bootstrap.kafka-system.svc:9092"
   echo $KAFKA_BOOTSTRAP
   ```

### Validation with Client

To verify connectivity, exec into one of the Kafka broker pods:

```bash
# Get composite name and broker pod (run all 3 lines)
COMPOSITE_NAME=$(kubectl get kafkacluster kafka-test -n mimir -o jsonpath='{.spec.resourceRef.name}')
KAFKA_POD=$(kubectl get pods -n kafka-system -l strimzi.io/cluster=$COMPOSITE_NAME,strimzi.io/broker-role=true -o jsonpath='{.items[0].metadata.name}')
echo "Using pod: $KAFKA_POD"

# List topics
kubectl exec -n kafka-system $KAFKA_POD -- \
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list
```

Create a test topic:

```bash
kubectl exec -n kafka-system $KAFKA_POD -- \
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

1. **Define Dependency**: In your project's Helm chart or configuration, reference the expected bootstrap URL pattern.
2. **Network Policies**: Ensure your namespace allows egress to `kafka-system`.
3. **Topics**: Use the Strimzi `KafkaTopic` CRD to create topics (managed by the Entity Operator).

### Creating Topics

```yaml
apiVersion: kafka.strimzi.io/v1beta2
kind: KafkaTopic
metadata:
  name: my-topic
  namespace: kafka-system
  labels:
    strimzi.io/cluster: <kafka-cluster-name>
spec:
  partitions: 3
  replicas: 2
  config:
    retention.ms: 604800000  # 7 days
```

