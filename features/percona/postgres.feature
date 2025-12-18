Feature: Percona PostgreSQL Services
  As a Platform Engineer
  I want to provide managed PostgreSQL clusters via Percona
  So that applications have relational storage

  @component:mimir-percona-postgres @phase:0
  Scenario: Percona PG Operator Check
    Given the "percona-postgresql-operator" deployment is running
    Then the "perconapgclusters.pg.percona.com" CRD should be established

  @component:mimir-percona-postgres @phase:1
  Scenario: PostgreSQL Provisioning
    Given the PostgreSQLInstance Claim "my-pg-db" is applied
    Then the "perconapgcluster" should be ready in "default"
    And the Crossplane claim "my-pg-db" should be "Ready"

  @component:mimir-percona-postgres @phase:2 @wip
  Scenario: PostgreSQL Backup & Restore
    Given the DB "my-pg-db" contains data
    When I trigger an on-demand backup "backup-01"
    And I delete the DB "my-pg-db"
    And I restore from "backup-01"
    Then the data should be present

  @component:mimir-percona-postgres @phase:3 @wip
  Scenario: PostgreSQL Monitoring
    Then the PMM agent should be registered for "my-pg-db"
    And I should see query metrics in PMM
