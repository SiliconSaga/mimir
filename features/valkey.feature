Feature: Valkey Data Services
  As a Platform Engineer
  I want to provide managed Valkey (Redis) clusters
  So that applications can cache data with high availability

  @component:mimir-valkey @phase:0
  Scenario: Valkey Operator Health Check
    Given the "valkey-operator" deployment is running in "valkey-system"
    Then the "valkeyclusters.valkey.io" CRD should be established

  @component:mimir-valkey @phase:1
  Scenario: Valkey Provisioning
    Given the ValkeyCluster Claim "valkey-test" is applied
    Then the "rediscluster" should be ready in "valkey-system"

  @component:mimir-valkey @phase:1
  Scenario: Valkey Functional Validation
    Given the ValkeyCluster Claim "valkey-test" is applied
    Then I should be able to connect to "valkey-test-valkey-leader.valkey-system.svc:6379"
    And I should receive a "PONG" response

  @component:mimir-valkey @phase:2 @wip
  Scenario: Valkey Persistence
    Given a ValkeyCluster "valkey-test" with persistence enabled
    When I write key "persistence-check" with value "true"
    And I restart the "valkey-test-0" pod
    Then key "persistence-check" should have value "true"

  @component:mimir-valkey @phase:3 @wip
  Scenario: Valkey Horizontal Scaling
    Given the ValkeyCluster "valkey-test" has 3 nodes
    When I update the replica count to 5
    Then the cluster should eventually have 5 nodes with status "Ready"
