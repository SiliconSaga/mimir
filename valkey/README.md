# Valkey Service (Redis)

This component provides a managed Valkey (Redis API compatible) cluster using the **OT-Container-Kit Operator**, abstracted via Crossplane.

## 🛠 Usage

To provision a Valkey cluster, create a `ValkeyCluster` claim in your application namespace.

See `claim.yaml` for an example.

### Parameters
- `replicas`: Number of nodes (default: 3).
- `storageSize`: PVC size for storage per node (default: "1Gi").

## ✅ Validation

To verify the service works:

1.  **Apply a Test Claim**:
    ```bash
    kubectl apply -f claim.yaml
    ```
2.  **Check Status**:
    Wait for the claim to be `Ready`.
    ```bash
    kubectl get valkeyclusters -n mimir
    ```
3.  **Connection Details**:
    The service name is **stable and derived from the claim name** — the
    composition names the `RedisCluster` after the claim (`crossplane.io/claim-name`),
    so a claim named `<claim>` always yields the leader Service `<claim>-leader.valkey.svc`.
    Consumers can hardcode this host; no need to scrape the generated composite name.

    ```bash
    # For the example claim `valkey-test`:
    VALKEY_HOST="valkey-test-leader.valkey.svc"
    ```
    - Port: `6379`

### Validation with Client
To verify connectivity and functionality, run a temporary Pod with `valkey-cli`:

```bash
kubectl run valkey-client --rm -i --restart=Never --image valkey/valkey:8.0 -- \
  valkey-cli -h $VALKEY_HOST -p 6379 ping
```
*Expected Output: `PONG`*

## 📦 Consumption from Other Projects

External projects (like `Heimdall` or `AppProject`) should treat this as a dependency.

1.  **Define Dependency**: In your project's Helm chart or wiring, reference the expected Host URL pattern.
2.  **Network Policies**: Ensure your namespace allows egress to `valkey`.
