# Strimzi Operator Setup Guide

This guide explains how to install and configure the Strimzi Operator for Kafka management.

## Prerequisites

- Kubernetes cluster (k3s, GKE, EKS, etc.)
- Helm 3.x installed
- `kubectl` configured to access your cluster
- Crossplane installed with:
  - `provider-kubernetes`
  - `function-go-templating`
  - `function-auto-ready` (for readiness propagation)

## Installation

### 0. Install Crossplane Function (if needed)

If `function-auto-ready` is not already installed, add it:

```bash
kubectl apply -f - <<EOF
apiVersion: pkg.crossplane.io/v1beta1
kind: Function
metadata:
  name: function-auto-ready
spec:
  package: xpkg.upbound.io/crossplane-contrib/function-auto-ready:v0.2.1
EOF
```

Verify:

```bash
kubectl get functions.pkg.crossplane.io
```

### 1. Add the Strimzi Helm Repository

```bash
helm repo add strimzi https://strimzi.io/charts/
helm repo update
```

### 2. Create the Namespace

```bash
kubectl create namespace kafka
```

### 3. Install the Operator

```bash
helm install strimzi-kafka-operator strimzi/strimzi-kafka-operator \
  --namespace kafka \
  --set watchAnyNamespace=true
```

The `watchAnyNamespace=true` setting allows the operator to manage Kafka resources in any namespace.

### 4. Verify Installation

```bash
kubectl wait --for=condition=available deployment/strimzi-cluster-operator \
  -n kafka --timeout=120s
```

Check the operator logs:

```bash
kubectl logs -n kafka deployment/strimzi-cluster-operator -f
```

### 5. Verify CRDs

```bash
kubectl get crds | grep kafka.strimzi.io
```

Expected output:

```
kafkabridges.kafka.strimzi.io
kafkaconnectors.kafka.strimzi.io
kafkaconnects.kafka.strimzi.io
kafkamirrormaker2s.kafka.strimzi.io
kafkamirrormakers.kafka.strimzi.io
kafkanodepools.kafka.strimzi.io
kafkarebalances.kafka.strimzi.io
kafkas.kafka.strimzi.io
kafkatopics.kafka.strimzi.io
kafkausers.kafka.strimzi.io
```

## Apply Crossplane Resources

After Strimzi is installed, apply the Crossplane XRD and Composition:

```bash
kubectl apply -f kafka/xrd.yaml
kubectl apply -f kafka/composition.yaml
```

Verify:

```bash
kubectl get xrd | grep kafka
kubectl get compositions | grep kafka
```

## Uninstallation

To remove the Strimzi Operator:

```bash
# Delete all Kafka resources first
kubectl delete kafka --all -n kafka
kubectl delete kafkanodepool --all -n kafka

# Uninstall the operator
helm uninstall strimzi-kafka-operator -n kafka

# Delete the namespace
kubectl delete namespace kafka
```

## Configuration Options

### Helm Values

| Value | Default | Description |
|-------|---------|-------------|
| `watchAnyNamespace` | `false` | Watch Kafka resources in all namespaces. We set `true` but since Crossplane always creates Kafka CRs in `kafka`, this is optional. |
| `replicas` | `1` | Number of operator replicas |
| `resources.requests.memory` | `256Mi` | Memory request for operator |
| `resources.requests.cpu` | `100m` | CPU request for operator |

### Custom Installation

For production, you may want to customize the installation:

```bash
helm install strimzi-kafka-operator strimzi/strimzi-kafka-operator \
  --namespace kafka \
  --set watchAnyNamespace=true \
  --set replicas=2 \
  --set resources.requests.memory=512Mi \
  --set resources.requests.cpu=250m \
  --set resources.limits.memory=1Gi \
  --set resources.limits.cpu=500m
```

## Troubleshooting

### Operator Not Starting

Check events:

```bash
kubectl get events -n kafka --sort-by='.lastTimestamp'
```

### Kafka Cluster Not Ready

Check Strimzi operator logs:

```bash
kubectl logs -n kafka deployment/strimzi-cluster-operator
```

Check Kafka resource status:

```bash
kubectl describe kafka <cluster-name> -n kafka
```

### PVC Issues

Ensure your storage class is available:

```bash
kubectl get storageclass
```

Check PVC status:

```bash
kubectl get pvc -n kafka
```

## Version Compatibility

| Strimzi Version | Kafka Versions |
|-----------------|----------------|
| 0.49.x | 4.0.0, 3.9.x |
| 0.48.x | 3.9.x, 3.8.x |
| 0.47.x | 3.8.x, 3.7.x |

See [Strimzi documentation](https://strimzi.io/docs/operators/latest/overview.html#supported-versions) for full compatibility matrix.

## References

- [Strimzi Documentation](https://strimzi.io/documentation/)
- [Strimzi GitHub](https://github.com/strimzi/strimzi-kafka-operator)
- [Apache Kafka Documentation](https://kafka.apache.org/documentation/)
- [KRaft Documentation](https://kafka.apache.org/documentation/#kraft)

