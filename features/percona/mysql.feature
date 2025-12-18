Feature: Percona MySQL Services
  As a Platform Engineer
  I want to provide managed MySQL clusters via Percona
  So that applications have relational storage

  @component:mimir-percona-mysql @phase:0
  Scenario: Percona XtraDB Operator Check
    Given the "percona-xtradb-cluster-operator" deployment is running
    Then the "perconaxtradbclusters.pxc.percona.com" CRD should be established

  @component:mimir-percona-mysql @phase:1
  Scenario: MySQL Provisioning
    Given the MySQLInstance Claim "my-mysql-db" is applied
    Then the "perconaxtradbcluster" should be ready in "default"
    And the Crossplane claim "my-mysql-db" should be "Ready"

  @component:mimir-percona-mysql @phase:2 @wip
  Scenario: MySQL Backup & Restore
    Given the DB "my-mysql-db" contains data
    When I trigger an on-demand backup "backup-mysql-01"
    Then the backup should complete successfully

  @component:mimir-percona-mysql @phase:3 @wip
  Scenario: MySQL Monitoring
    Then the PMM agent should be registered for "my-mysql-db"
