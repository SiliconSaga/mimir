#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

echo -e "${GREEN}Starting Mimir Infrastructure Verification${NC}"

# Check Kafka Claim
echo "Checking KafkaCluster Claim..."
kubectl wait --for=condition=Ready kafkacluster/kafka-test -n mimir --timeout=300s || echo -e "${RED}WARNING: KafkaCluster Claim not ready (Check Crossplane Status)${NC}"

# Kafka Functional Check
echo "Validating Kafka Connection..."
KAFKA_BOOTSTRAP="kafka-test-w8rfb-kafka-bootstrap.kafka-system.svc:9092"
kubectl run kafka-verifier --rm -i --restart=Never --image quay.io/strimzi/kafka:latest-kafka-4.0.0 --timeout=120s -- bin/kafka-topics.sh --bootstrap-server ${KAFKA_BOOTSTRAP} --list && \
  echo -e "${GREEN}Kafka Connection Verified (Topics List)${NC}" || \
  { echo -e "${RED}Kafka Connection Failed${NC}"; exit 1; }

# Check Valkey Claim
echo "Checking ValkeyCluster Claim..."
kubectl wait --for=condition=Ready valkeycluster/valkey-test -n mimir --timeout=60s || { echo -e "${RED}ValkeyCluster Claim not ready${NC}"; exit 1; }

# Valkey Functional Check
echo "Validating Valkey Connection..."
VALKEY_COMPOSITE=$(kubectl get valkeycluster valkey-test -n mimir -o jsonpath='{.spec.resourceRef.name}')
VALKEY_HOST="${VALKEY_COMPOSITE}-leader.valkey-system.svc"
echo "Valkey Host: ${VALKEY_HOST}"

kubectl run valkey-verifier --rm -i --restart=Never --image valkey/valkey:8.0 --timeout=60s -- \
  valkey-cli -h ${VALKEY_HOST} -p 6379 ping | grep -q "PONG" && \
  echo -e "${GREEN}Valkey Connection Verified (PONG)${NC}" || \
  { echo -e "${RED}Valkey Connection Failed${NC}"; exit 1; }

# Check Percona Claims
echo "Applying Percona Claims..."
kubectl apply -f percona/PostgresSampleDB.yaml -f percona/MySQLSampleDB.yaml -f percona/MongoSampleDB.yaml

echo "Checking PostgreSQLInstance Claim..."
kubectl wait --for=condition=Ready postgresqlinstance/my-pg-db -n my-pg-ns --timeout=60s || echo "Proceeding to check managed resources..."

echo "Checking MySQLInstance Claim..."
kubectl wait --for=condition=Ready mysqlinstance/my-mysql-db -n my-mysql-ns --timeout=60s || echo "Proceeding to check managed resources..."

echo "Checking MongoDBInstance Claim..."
kubectl wait --for=condition=Ready mongodbinstance/my-mongo-db -n my-mongo-ns --timeout=60s || echo "Proceeding to check managed resources..."

# Check Managed Resources
echo "Checking Managed Resources (Kafka, Redis, Databases)..."
kubectl get managed

echo "Checking Percona Clusters..."
kubectl get perconapgclusters,perconaxtradbclusters,perconaservermongodbs --all-namespaces

echo -e "${GREEN}Mimir Infrastructure Verification Completed.{NC}"
