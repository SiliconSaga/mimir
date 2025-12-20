# Mimir BDD & Roadmap Strategy

## Goal

Transform Mimir's testing into a granular, roadmap-aware suite that feeds into the **Vörðu Matrix**. This allows us to visualize the maturity of Data Services across four phases.

## The Phases (Columns)

Never mind the names for now, came from Demicracy, adjust later.

| Phase | Name | Focus | Key Requirements |
| :--- | :--- | :--- | :--- |
| **0** | **Foundation** | Operators & CRDs | Is the Operator running? Do CRDs exist? |
| **1** | **Utility** | Provisioning | Can we create a Cluster? Is it reachable? |
| **2** | **Federation** | Day 2 Ops | Are backups working? Are metrics exposed? |
| **3** | **Sovereignty** | Reliability | Can it scale? Are alerts firing correctly? |

## Granularity Configuration

Mimir is configured to display as a **Single Aggregated Row** ("System" level in Backstage speak)in Vörðu to reduce noise.

*   **File**: `catalog-info.yaml`
*   **Annotation**: `vordu.io/granularity: "system"`
*   **Behavior**:
    *   Vörðu ingests all components (`mimir-kafka`, `mimir-valkey`, etc.).
    *   It **Sums** the scenarios from all components.
    *   It displays one "Mimir" row.

## Feature Organization

We avoid enforcing a rigid directory structure (which rots). Instead, follow these conventions:

1.  **Tagging is Primary**: The `@component` tag determines where the test results go, not the filename.
2.  **Grouping**:
    *   **Components**: Keep at the root of `features/` (e.g., `kafka.feature`).
    *   **Subcomponents**: Group related subcomponents in a folder named after the parent (e.g., `features/percona/postgres.feature`).

### Component Mapping Examples

| Feature Context | Component Tag | Type |
| :--- | :--- | :--- |
| **Kafka** | `@component:mimir-kafka` | Top-level Component |
| **Valkey** | `@component:mimir-valkey` | Top-level Component |
| **Percona** | `@component:mimir-percona` | Top-level Component |
| **Percona: Postgres** | `@component:mimir-percona-postgres` | Subcomponent of `mimir-percona` |

## Tagging Contract

Every scenario must be tagged to light up the matrix.

Syntax: `@component:<name> @phase:<0-3>`

```gherkin
@component:mimir-kafka @phase:1
Scenario: Kafka Provisioning
  Given a Kafka Cluster "test-cluster" is requested
  Then I can produce a message to "test-topic"
```

## Local Testing Guide

To validate Vörðu ingestion locally without running the full Jenkins pipeline:

### Prerequisites

1.  **Python 3** installed.
2.  **Vörðu Repo** checked out (`d:/Dev/GitWS/vordu`).
3.  **Dependencies**:
    ```powershell
    cd d:/Dev/GitWS/vordu
    # Create/Activate venv
    python -m venv api/.venv
    .\api\.venv\Scripts\activate
    
    # Install Script Dependencies
    pip install PyYAML requests
    ```

### Running a Dry Run

You can simulate ingestion using the `vordu_ingest.py` script. By default, it uses **Mock Data** if no `cucumber.json` is provided.

```powershell
# FROM: d:/Dev/GitWS/vordu
python resources/scripts/vordu_ingest.py "d:/Dev/GitWS/mimir/catalog-info.yaml"
```

**Expected Output**:

*   JSON describing the parsed **Config** (System + Components).
*   JSON describing the **Ingest Status** (Aggregated into 1 System Row).
*   Check `completion` percentage and `status` fields.

### Testing with Real Reports

If you have a real `cucumber.json` from Mimir's test suite:

```powershell
python resources/scripts/vordu_ingest.py "d:/Dev/GitWS/mimir/catalog-info.yaml" --report "path/to/mimir/cucumber.json"
```

## Jenkins Integration

The `Jenkinsfile` uses the Shared Library `ingestVordu` step which wraps this script automatically.
