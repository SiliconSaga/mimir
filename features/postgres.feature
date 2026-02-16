Feature: PostgreSQL Data Service
  As a Platform Engineer
  I want to provision PostgreSQL instances via Mimir
  So that applications can store relational data reliably

  Scenario: PostgreSQL Provisioning
    Given the PostgreSQLInstance Claim "my-pg-db" is applied
    Then the "perconapgcluster" should be ready in "default"
    And the Crossplane claim "my-pg-db" should be "Ready"

  Scenario: PostgreSQL Connection
    Given a provisioned PostgreSQLInstance "my-pg-db"
    Then I should be able to connect and run "SELECT 1"
