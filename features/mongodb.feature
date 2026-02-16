Feature: MongoDB Data Service
  As a Platform Engineer
  I want to provision MongoDB instances via Mimir
  So that applications can store document data reliably

  Scenario: MongoDB Provisioning
    Given the MongoDBInstance Claim "my-mongo-db" is applied
    Then the "perconaservermongodb" should be ready in "default"
    And the Crossplane claim "my-mongo-db" should be "Ready"

  Scenario: MongoDB Connection
    Given a provisioned MongoDBInstance "my-mongo-db"
    Then I should be able to connect and run a ping command
