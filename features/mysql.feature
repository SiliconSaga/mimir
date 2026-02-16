Feature: MySQL Data Service
  As a Platform Engineer
  I want to provision MySQL instances via Mimir
  So that applications can store relational data reliably

  Scenario: MySQL Provisioning
    Given the MySQLInstance Claim "my-mysql-db" is applied
    Then the "perconaxtradbcluster" should be ready in "default"
    And the Crossplane claim "my-mysql-db" should be "Ready"

  Scenario: MySQL Connection
    Given a provisioned MySQLInstance "my-mysql-db"
    Then I should be able to connect and run "SELECT 1"
