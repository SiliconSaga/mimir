Feature: Mimir Infrastructure Layer
  As a Platform Engineer
  I want to provide Data Management services via Mimir
  So that applications can consume Kafka and Valkey reliably

  Scenario: Kafka Provisioning
    Given the KafkaCluster Claim "kafka-test" is applied
    Then the "Kafka" cluster should be ready in "kafka-system"
    And the Crossplane claim "kafka-test" should be "Ready"

  Scenario: Kafka Functional Validation
    Given the KafkaCluster Claim "kafka-test" is applied
    Then I should be able to list topics on the Kafka cluster
    And I should be able to create a test topic

  Scenario: Valkey Provisioning
    Given the ValkeyCluster Claim "valkey-test" is applied
    Then the "rediscluster" should be ready in "valkey-system"

  Scenario: Valkey Functional Validation
    Given the ValkeyCluster Claim "valkey-test" is applied
    Then I should be able to connect to "valkey-test-valkey-leader.valkey-system.svc:6379"
    And I should receive a "PONG" response

  Scenario: PostgreSQL Provisioning
    Given the PostgreSQLInstance Claim "my-pg-db" is applied
    Then the "perconapgcluster" should be ready in "default"
    And the Crossplane claim "my-pg-db" should be "Ready"

  Scenario: MySQL Provisioning
    Given the MySQLInstance Claim "my-mysql-db" is applied
    Then the "perconaxtradbcluster" should be ready in "default"
    And the Crossplane claim "my-mysql-db" should be "Ready"

  Scenario: MongoDB Provisioning
    Given the MongoDBInstance Claim "my-mongo-db" is applied
    Then the "perconaservermongodb" should be ready in "default"
    And the Crossplane claim "my-mongo-db" should be "Ready"
