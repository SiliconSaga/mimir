Feature: Percona MongoDB Services
  As a Platform Engineer
  I want to provide managed MongoDB clusters via Percona
  So that applications have NoSQL document storage

  @component:mimir-percona-mongo @phase:0
  Scenario: Percona Mongo Operator Check
    Given the "percona-server-mongodb-operator" deployment is running
    Then the "perconaservermongodbs.psmdb.percona.com" CRD should be established

  @component:mimir-percona-mongo @phase:1
  Scenario: MongoDB Provisioning
    Given the MongoDBInstance Claim "my-mongo-db" is applied
    Then the "perconaservermongodb" should be ready in "default"
    And the Crossplane claim "my-mongo-db" should be "Ready"

  @component:mimir-percona-mongo @phase:2 @wip
  Scenario: MongoDB Backup & Restore
    Given the DB "my-mongo-db" contains data
    When I trigger an on-demand backup "backup-mongo-01"
    Then the backup should complete successfully

  @component:mimir-percona-mongo @phase:3 @wip
  Scenario: MongoDB Monitoring
    Then the PMM agent should be registered for "my-mongo-db"
