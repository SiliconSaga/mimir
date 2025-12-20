# Testing Strategy for Mimir

This document describes the testing approach for Mimir and how it fits within the broader Yggdrasil ecosystem.

## Philosophy: Right Tool for the Job

Different projects in the ecosystem use testing frameworks appropriate to their technology:

| Project Type | Recommended Framework | Why |
|--------------|----------------------|-----|
| **Infrastructure (Mimir)** | kuttl | Kubernetes-native, declarative YAML tests |
| **Python Services** | pytest + Behave | Native ecosystem, BDD support |
| **Node.js Services** | Jest / Vitest | Fast, modern JS testing |
| **Java Services** | JUnit + Cucumber-JVM | Industry standard, BDD support |

The common thread is **BDD-style scenarios** defined in Gherkin (`.feature` files), which serve as living documentation regardless of the underlying test runner.

## Mimir Testing with kuttl

[kuttl](https://kuttl.dev/) (Kubernetes Test TooL) is ideal for infrastructure testing because:

- **Declarative**: Tests are YAML, matching our Crossplane/K8s paradigm
- **No code required**: Assert on resource states directly
- **Built for operators**: Designed to test Kubernetes controllers and CRDs
- **CI-friendly**: Easy to run in pipelines

### Installation

#### macOS (local development)

```bash
brew install kuttl
```

#### Linux / CI Agents (cross-platform)

Download the binary directly - works in any CI system:

```bash
# Linux (amd64)
KUTTL_VERSION=0.15.0
curl -Lo kuttl "https://github.com/kudobuilder/kuttl/releases/download/v${KUTTL_VERSION}/kubectl-kuttl_${KUTTL_VERSION}_linux_x86_64"
chmod +x kuttl
sudo mv kuttl /usr/local/bin/

# Verify
kuttl version
```

#### Windows

```powershell
# Download Windows binary
$KUTTL_VERSION = "0.15.0"
Invoke-WebRequest -Uri "https://github.com/kudobuilder/kuttl/releases/download/v$KUTTL_VERSION/kubectl-kuttl_${KUTTL_VERSION}_windows_x86_64.exe" -OutFile kuttl.exe

# Add to PATH or move to a directory in PATH
Move-Item kuttl.exe C:\tools\kuttl.exe
```

#### Container-based (Jenkins/CI)

Use a container with kuttl pre-installed:

```yaml
# Jenkinsfile pod template
containers:
- name: kuttl
  image: ghcr.io/kudobuilder/kuttl:v0.15.0
  command: ['sleep']
  args: ['infinity']
```

### Test Structure

```
mimir/
├── features/
│   └── infrastructure.feature    # BDD scenarios (documentation)
├── tests/
│   └── e2e/
│       ├── kuttl-test.yaml       # Test suite configuration
│       ├── kafka-provisioning/
│       │   ├── 00-apply.yaml     # Apply claim
│       │   └── 01-assert.yaml    # Assert ready state
│       ├── valkey-provisioning/
│       │   ├── 00-apply.yaml
│       │   └── 01-assert.yaml
│       └── ...
└── test/
    └── verify_infrastructure.sh  # Quick manual verification
```

### Running Tests

```bash
# Run all e2e tests
kubectl kuttl test tests/e2e/

# Run specific test
kubectl kuttl test tests/e2e/ --test kafka-provisioning

# With verbose output
kubectl kuttl test tests/e2e/ --v 3
```

## BDD Feature Files

The `features/infrastructure.feature` file serves as **living documentation** describing expected behavior:

```gherkin
Feature: Mimir Infrastructure Layer
  As a Platform Engineer
  I want to provide Data Management services via Mimir
  So that applications can consume Kafka and Valkey reliably

  Scenario: Kafka Provisioning
    Given the KafkaCluster Claim "kafka-test" is applied
    Then the "Kafka" cluster should be ready in "kafka-system"
    And the Crossplane claim "kafka-test" should be "Ready"
```

These scenarios are implemented by kuttl test cases (or the shell script for quick checks).

## Quick Verification Script

For rapid manual testing without the full kuttl setup:

```bash
./test/verify_infrastructure.sh
```

This script:
- Checks all Crossplane claims are Ready
- Validates functional connectivity (Kafka topics, Valkey PING)
- Reports pass/fail status with colored output

## Alternatives Considered

### Behave (Python)

```python
# steps/kafka_steps.py
from behave import given, then
from kubernetes import client, config

@given('the KafkaCluster Claim "{name}" is applied')
def step_apply_claim(context, name):
    # Apply claim via kubernetes client
    pass

@then('the Crossplane claim "{name}" should be "Ready"')
def step_check_ready(context, name):
    # Check claim status
    pass
```

**Pros**: Full BDD with step definitions, reusable across Python projects
**Cons**: Requires Python environment, more setup for pure K8s testing

### Cucumber + kubectl

```javascript
// features/step_definitions/kafka.js
const { Given, Then } = require('@cucumber/cucumber');
const { execSync } = require('child_process');

Given('the KafkaCluster Claim {string} is applied', function (name) {
  execSync(`kubectl apply -f kafka/claim.yaml`);
});
```

**Pros**: True Gherkin execution, familiar to many teams
**Cons**: Requires Node.js, shells out to kubectl

### chainsaw (kuttl successor)

[chainsaw](https://kyverno.github.io/chainsaw/) is the next-generation Kubernetes testing tool from the Kyverno project:

```yaml
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: kafka-provisioning
spec:
  steps:
  - try:
    - apply:
        file: claim.yaml
    - assert:
        file: assert-ready.yaml
```

**Pros**: More features than kuttl, actively maintained
**Cons**: Newer, smaller community

## Test Isolation Strategies

Infrastructure testing requires careful isolation to avoid disturbing live environments.

### Recommended: Ephemeral Clusters for CI

Create a fresh cluster per test run, destroy after:

```
┌─────────────────────────────────────────────────────────┐
│                    CI Pipeline                          │
│  ┌─────────┐    ┌──────────────┐    ┌───────────────┐   │
│  │ Create  │───►│ Install      │───►│ Run Tests     │   │
│  │ k3d/kind│    │ Crossplane + │    │ (kuttl)       │   │
│  │ cluster │    │ Operators    │    │               │   │
│  └─────────┘    └──────────────┘    └───────────────┘   │
│                                            │            │
│                                     ┌──────▼──────┐     │
│                                     │ Destroy     │     │
│                                     │ cluster     │     │
│                                     └─────────────┘     │
└─────────────────────────────────────────────────────────┘
```

**Pros**: Complete isolation, no cleanup needed, reproducible
**Cons**: Slower startup, requires cluster creation capability

### Alternative: Namespace Isolation

For shared dev clusters where ephemeral clusters aren't practical:

| Component | Scope | Isolation Strategy |
|-----------|-------|-------------------|
| Crossplane | Cluster (singleton) | Shared - install once |
| XRDs/Compositions | Cluster | Shared - same API for all |
| Claims | Namespace | **Test namespace** (e.g., `test-<run-id>`) |
| Operator resources | Operator namespace | Prefixed names (e.g., `test-kafka-123`) |

```yaml
# Example: Namespaced test claim
apiVersion: mimir.siliconsaga.org/v1alpha1
kind: KafkaCluster
metadata:
  name: kafka-test-${BUILD_ID}  # Unique per run
  namespace: test-${BUILD_ID}   # Isolated namespace
spec:
  parameters:
    replicas: 1  # Minimal for tests
```

### Cleanup Script

```bash
#!/bin/bash
# cleanup-test-resources.sh
BUILD_ID=${1:-"manual"}

# Delete test namespace (cascades to claims)
kubectl delete namespace test-${BUILD_ID} --ignore-not-found

# Clean up operator resources (Kafka, Valkey in their namespaces)
kubectl delete kafka -l test-run=${BUILD_ID} -n kafka-system --ignore-not-found
kubectl delete rediscluster -l test-run=${BUILD_ID} -n valkey-system --ignore-not-found
```

### Resource Labeling for Tests

Add labels to test resources for easy identification and cleanup:

```yaml
metadata:
  labels:
    mimir.siliconsaga.org/test: "true"
    mimir.siliconsaga.org/test-run: "${BUILD_ID}"
```

### Environment Tiers

| Environment | Cluster Strategy | Test Scope |
|-------------|------------------|------------|
| **CI** | Ephemeral k3d/kind | Full e2e |
| **Dev** | Shared, namespace isolation | Targeted tests |
| **Staging** | Dedicated cluster | Integration tests |
| **Production** | Never run tests | Monitoring only |

## CI Integration

### Jenkins Pipeline Example

```groovy
pipeline {
    agent {
        kubernetes {
            yaml '''
apiVersion: v1
kind: Pod
spec:
  containers:
  - name: kubectl
    image: bitnami/kubectl:latest
    command: ['sleep']
    args: ['infinity']
  - name: k3d
    image: rancher/k3d:5.6.0-dind
    securityContext:
      privileged: true
'''
        }
    }
    
    environment {
        KUTTL_VERSION = '0.15.0'
    }
    
    stages {
        stage('Setup Cluster') {
            steps {
                container('k3d') {
                    sh '''
                        k3d cluster create test-${BUILD_ID} --wait
                        k3d kubeconfig get test-${BUILD_ID} > kubeconfig
                    '''
                }
            }
        }
        
        stage('Install kuttl') {
            steps {
                container('kubectl') {
                    sh '''
                        curl -Lo kuttl https://github.com/kudobuilder/kuttl/releases/download/v${KUTTL_VERSION}/kubectl-kuttl_${KUTTL_VERSION}_linux_x86_64
                        chmod +x kuttl && mv kuttl /usr/local/bin/
                    '''
                }
            }
        }
        
        stage('Install Infrastructure') {
            steps {
                container('kubectl') {
                    sh '''
                        export KUBECONFIG=kubeconfig
                        # Install Crossplane, operators, etc.
                        ./scripts/setup-cluster.sh
                    '''
                }
            }
        }
        
        stage('Run Tests') {
            steps {
                container('kubectl') {
                    sh '''
                        export KUBECONFIG=kubeconfig
                        kuttl test tests/e2e/ --config tests/e2e/kuttl-test.yaml
                    '''
                }
            }
        }
    }
    
    post {
        always {
            container('k3d') {
                sh 'k3d cluster delete test-${BUILD_ID} || true'
            }
        }
    }
}
```

### GitHub Actions Example

```yaml
jobs:
  e2e-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      
      - name: Create k3d cluster
        uses: AbsaOSS/k3d-action@v2
        with:
          cluster-name: test-cluster
      
      - name: Install kuttl
        run: |
          curl -Lo kuttl https://github.com/kudobuilder/kuttl/releases/download/v0.15.0/kuttl_0.15.0_linux_x86_64
          chmod +x kuttl && sudo mv kuttl /usr/local/bin/
      
      - name: Setup Crossplane & Operators
        run: |
          # Install Crossplane, Strimzi, etc.
          ./scripts/setup-cluster.sh
      
      - name: Run e2e tests
        run: kubectl kuttl test tests/e2e/
```

## Summary

| Method | Use Case | Speed |
|--------|----------|-------|
| `verify_infrastructure.sh` | Quick manual check | Fast |
| `kubectl kuttl test` | Full e2e validation | Medium |
| CI pipeline | Automated regression | Slow (includes cluster setup) |

The feature file remains the source of truth for expected behavior, while the test implementation varies based on context.

