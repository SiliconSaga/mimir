Feature: Valkey Data Service
  As a Platform Engineer
  I want to provision Valkey clusters via Mimir
  So that applications can use in-memory caching reliably

  Scenario: Valkey Provisioning
    Given the ValkeyCluster Claim "valkey-test" is applied
    Then the "rediscluster" should be ready in "valkey"

  Scenario: Valkey Functional Validation
    Given the ValkeyCluster Claim "valkey-test" is applied
    Then I should be able to connect to "valkey-test-valkey-leader.valkey.svc:6379"
    And I should receive a "PONG" response
