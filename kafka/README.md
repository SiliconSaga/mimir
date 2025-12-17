# Kafka Service

This component provides a managed Kafka cluster using the **Strimzi Operator**, abstracted via Crossplane.

## 🛠 Usage

To provision a Kafka cluster, create a `KafkaCluster` claim in your application namespace.

See `claim.yaml` for an example.

### Parameters
- `replicas`: Number of Kafka brokers (default: 1).
- `retentionHours`: Log retention period in hours (default: 72).
- `storageSize`: PVC size for storage (default: "10Gi").

## ✅ Validation

To verify the service works:

1.  **Apply a Test Claim**:
    ```bash
    kubectl apply -f claim.yaml
    ```
2.  **Check Status**:
    Wait for the claim to be `Ready`.
    ```bash
    kubectl get kafkaclusters -n mimir
    ```
3.  **Connection Details**:
    The service provides a connection secret (if configured) or predictable DNS names (update `kafka-test`):
    - Bootstrap: `kafka-test-kafka-kafka-bootstrap.kafka-system.svc:9092` (Plain)
    - Bootstrap: `kafka-test-kafka-kafka-bootstrap.kafka-system.svc:9093` (TLS)

## 📦 Consumption from Other Projects

External projects (like `Heimdall` or `AppProject`) should treat this as a dependency.

1.  **Define Dependency**: In your project's Helm chart or wiring, reference the expected bootstrap URL pattern.
2.  **Network Policies**: Ensure your namespace allows egress to `kafka-system`.
