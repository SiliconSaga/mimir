Feature: Kafka Data Service
  As a Platform Engineer
  I want to provision Kafka clusters via Mimir
  So that applications can produce and consume events reliably

  Scenario: Kafka Provisioning
    Given the KafkaCluster Claim "kafka-test" is applied
    Then the "Kafka" cluster should be ready in "kafka"
    And the Crossplane claim "kafka-test" should be "Ready"

  Scenario: Kafka Functional Validation
    Given the KafkaCluster Claim "kafka-test" is applied
    Then I should be able to list topics on the Kafka cluster
    And I should be able to create a test topic
