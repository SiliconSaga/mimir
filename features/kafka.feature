Feature: Kafka Data Service
  As a Platform Engineer
  I want to provision Kafka clusters via Mimir
  So that applications can produce and consume events reliably

  @component:mimir-kafka @phase:0
  Scenario: Kafka Operator Health Check
    Given the "strimzi-cluster-operator" deployment is running in "kafka"
    Then the "kafkas.kafka.strimzi.io" CRD should be established

  @component:mimir-kafka @phase:1
  Scenario: Kafka Provisioning
    Given the KafkaCluster Claim "kafka-test" is applied
    Then the "Kafka" cluster should be ready in "kafka"
    And the Crossplane claim "kafka-test" should be "Ready"

  @component:mimir-kafka @phase:1 @wip
  Scenario: Kafka Functional Validation
    Given the KafkaCluster Claim "kafka-test" is applied
    Then I should be able to list topics on the Kafka cluster
    And I should be able to create a test topic

  @component:mimir-kafka @phase:2 @wip
  Scenario: Kafka Backup Enabled
    Given a "Kafka" cluster "kafka-test" exists
    Then the "MirrorMaker2" resource should be configured for "kafka-test"
    And "KafkaRebalance" should be active

  @component:mimir-kafka @phase:3 @wip
  Scenario: Kafka Alerting Rules
    When I query Prometheus for "kafka_under_replicated_partitions"
    Then I should receive a value of 0
    And the AlertManager rule "KafkaUnderReplicated" should be "Active"
