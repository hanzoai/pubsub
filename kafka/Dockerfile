FROM docker.redpanda.com/redpandadata/redpanda:v25.1.9

LABEL org.opencontainers.image.title="Hanzo Kafka"
LABEL org.opencontainers.image.description="Kafka-compatible streaming via Redpanda — Hanzo PubSub"
LABEL org.opencontainers.image.vendor="Hanzo AI"
LABEL org.opencontainers.image.source="https://github.com/hanzoai/pubsub"

# Redpanda speaks Kafka protocol natively on port 9092
# Also exposes: 8081 (schema registry), 8082 (HTTP proxy), 9644 (admin)
EXPOSE 9092 8081 8082 9644
