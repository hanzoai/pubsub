# Hanzo PubSub

High-performance event streaming and message queue for modern distributed systems.

## Overview

Hanzo PubSub is a lightweight, high-performance messaging system designed for cloud-native applications. It provides reliable pub/sub messaging, persistent streams, and exactly-once delivery semantics.

## Features

- **Pub/Sub Messaging** - Flexible publish-subscribe patterns with subject-based routing
- **Persistent Streams** - Durable message storage with configurable retention
- **Consumer Groups** - Scalable message consumption with automatic load balancing
- **Exactly-Once Delivery** - Guaranteed message delivery with deduplication
- **Key-Value Store** - Built-in distributed key-value storage
- **Object Store** - Store and retrieve large objects efficiently
- **Clustering** - Horizontal scaling with automatic failover

## Quick Start

### Docker

```bash
docker run -d --name hanzo-pubsub \
  -p 4222:4222 \
  -p 8222:8222 \
  hanzoai/pubsub:latest
```

### Docker Compose

```yaml
version: '3.8'
services:
  pubsub:
    image: hanzoai/pubsub:latest
    ports:
      - "4222:4222"  # Client connections
      - "8222:8222"  # Management/monitoring
    volumes:
      - pubsub-data:/data
    command: ["--jetstream", "--store_dir=/data"]

volumes:
  pubsub-data:
```

## SDK Support

- Python: `pip install hanzo-pubsub`
- Go: `go get github.com/hanzoai/pubsub-go`
- TypeScript: `npm install @hanzo/pubsub`
- Rust: `cargo add hanzo-pubsub`

## Example Usage

### Basic Pub/Sub

```python
from hanzo.pubsub import connect

async def main():
    # Connect to PubSub
    ps = await connect("nats://localhost:4222")
    
    # Subscribe to a subject
    async def handler(msg):
        print(f"Received: {msg.data}")
    
    await ps.subscribe("events.>", handler)
    
    # Publish messages
    await ps.publish("events.user.created", {"user_id": "123"})
```

### Persistent Streams

```python
from hanzo.pubsub import connect, StreamConfig

async def main():
    ps = await connect("nats://localhost:4222")
    js = ps.jetstream()
    
    # Create a stream
    await js.add_stream(StreamConfig(
        name="ORDERS",
        subjects=["orders.*"],
        retention="limits",
        max_msgs=1_000_000,
    ))
    
    # Publish to stream
    await js.publish("orders.new", {"order_id": "abc123"})
    
    # Create durable consumer
    consumer = await js.pull_subscribe("orders.*", durable="processor")
    
    # Process messages
    async for msg in consumer.fetch(batch=10):
        await process_order(msg.data)
        await msg.ack()
```

## Architecture

```
┌──────────────────────────────────────────────────┐
│                    Cluster                        │
│  ┌─────────┐    ┌─────────┐    ┌─────────┐      │
│  │ Node 1  │◀──▶│ Node 2  │◀──▶│ Node 3  │      │
│  └─────────┘    └─────────┘    └─────────┘      │
│       │              │              │            │
│       └──────────────┼──────────────┘            │
│                      │                           │
│              ┌───────┴───────┐                   │
│              │  JetStream    │                   │
│              │  (Streams)    │                   │
│              └───────────────┘                   │
└──────────────────────────────────────────────────┘
```

## Performance

- **Messages/sec**: 10M+ messages per second
- **Latency**: Sub-millisecond publish latency
- **Connections**: 100K+ concurrent connections per node

## Documentation

- [Getting Started](https://docs.hanzo.ai/pubsub/getting-started)
- [Streams & Consumers](https://docs.hanzo.ai/pubsub/streams)
- [Clustering](https://docs.hanzo.ai/pubsub/clustering)
- [Security](https://docs.hanzo.ai/pubsub/security)

## License

MIT License - see [LICENSE](LICENSE) for details.
