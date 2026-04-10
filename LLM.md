# LLM.md - Hanzo Pubsub

## Overview
NATS server fork with ZAP control plane and HTTP management API.
Go module: github.com/nats-io/nats-server/v2

## Architecture
- **NATS protocol** (port 4222): pub/sub messaging -- unchanged
- **ZAP transport** (port 9222, env PUBSUB_ZAP_PORT): binary RPC control plane via github.com/luxfi/zap
- **HTTP management** (port 9280, env PUBSUB_HTTP_PORT): /v1/pubsub/* routes

## ZAP Opcodes
- 0x01: CreateStream (name + subjects)
- 0x02: DeleteStream (name)
- 0x03: ListStreams
- 0x04: Publish (subject + payload)
- 0x05: GetStreamInfo (name)
- 0x06: HealthCheck

## HTTP Routes
- GET /v1/pubsub/health -- health check with JetStream status
- GET /v1/pubsub/varz -- server variables (wraps NATS /varz)
- GET /v1/pubsub/connz -- connection info (wraps NATS /connz)
- GET /v1/pubsub/streams -- JetStream stream list with state

## Build & Run
```bash
go build ./...
go test ./internal/mgmt/ -v
```

## Key Packages
- `internal/mgmt/` -- ZAP transport + HTTP management server
- `server/` -- NATS server core (upstream fork)

## Key Files
- `main.go` -- Entry point, starts NATS + management server
- `internal/mgmt/mgmt.go` -- Management server implementation
- `internal/mgmt/mgmt_test.go` -- Tests (7 tests)
- `go.mod` -- Go module definition
- `Dockerfile` -- Container build
