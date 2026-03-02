Feature: Valkey Data Service
  As a Platform Engineer
  I want to provision Valkey clusters via Mimir
  So that applications can use in-memory caching reliably

  @component:mimir-valkey @phase:0
  Scenario: Valkey Operator Health Check
    Given the "redis-operator" deployment is running in "valkey"
    Then the "redisclusters.redis.redis.opstreelabs.in" CRD should be established

  @component:mimir-valkey @phase:1
  Scenario: Valkey Provisioning
    Given the ValkeyCluster Claim "valkey-test" is applied
    Then the "rediscluster" should be ready in "valkey"

  @component:mimir-valkey @phase:1  Scenario: Valkey Functional Validation
    Given the ValkeyCluster Claim "valkey-test" is applied
    Then I should be able to connect to "valkey-test-valkey-leader.valkey.svc:6379"
    And I should receive a "PONG" response

  @component:mimir-valkey @phase:2  Scenario: Valkey Persistence
    Given a ValkeyCluster "valkey-test" with persistence enabled
    When I write key "persistence-check" with value "true"
    And I restart the "valkey-test-0" pod
    Then key "persistence-check" should have value "true"

  @component:mimir-valkey @phase:3  Scenario: Valkey Horizontal Scaling
    Given the ValkeyCluster "valkey-test" has 3 nodes
    When I update the replica count to 5
    Then the cluster should eventually have 5 nodes with status "Ready"
