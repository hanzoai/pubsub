# Hanzo PubSub

## Overview
NATS server fork with Quasar PQ consensus, ZAP control plane, zapdb persistence,
and PQ-safe encrypted replication via luxfi/age (ML-KEM-768 + X25519).

**Upstream**: [NATS Server](https://github.com/nats-io/nats-server) (Apache-2.0). `server/` imports `nats-io/*` directly; the rest is Hanzo-built.

Go module: `github.com/hanzoai/pubsub`
Binary: `pubsub`
JetStream (persistent streams/consumers/KV) enabled by default.

## Architecture
- **NATS protocol** (port 4222): pub/sub messaging
- **PubSub** (JetStream): persistent streams, consumers, KV store — enabled by default
- **ZAP mgmt** (port 9222): binary RPC control plane with HMAC auth
- **ZAP consensus** (port 9223): Quasar P2P with BLS+PQ signature verification
- **HTTP mgmt** (port 9280): REST API with bearer token auth
- **Quasar consensus** (opt-in): dual PQ consensus (BLS + ML-DSA/Corona)
- **zapdb store** (opt-in): embedded KV with AES-256 at rest + PQ-safe backup via age

## Storage Stack
```
PubSub streams → zapdb (AES-256 at rest) → age (ML-KEM-768+X25519) → S3/disk
```
- At rest: AES-256 via zapdb EncryptionKey
- Backups: PQ-encrypted with luxfi/age HybridRecipient (ML-KEM-768 + X25519, X-Wing)
- Restore: PQ-decrypted with luxfi/age HybridIdentity

## Quasar Consensus
Upstream `luxfi/consensus` HEAD implements triple consensus (BLS + Corona + ML-DSA).
PubSub pins `luxfi/consensus` v1.22.0 which has dual consensus (BLS + ML-DSA/Corona):
1. **BLS12-381** — classical fast-path, 48-byte aggregate proof
2. **PQ proof** (ML-DSA-65 or Corona Ring-LWE) — post-quantum, variable size

Triple mode (`TripleSignRound1`, all 3 in parallel) available when pubsub upgrades to v1.23+.

Types: `QuasarConsensus`, `QuasarSignature` (aliases for upstream v1.22.0 legacy names).

## Security
- HTTP: bearer token auth (`PUBSUB_HTTP_TOKEN`), constant-time comparison
- ZAP mgmt: shared secret auth (`PUBSUB_ZAP_SECRET`), peer-allowlist on first message
- ZAP consensus: BLS+ML-DSA signature verification per vote, peer-validator identity binding
- Finality: requires op in pending set with verified votes (no trust-peer finality)
- Proposal: hash-payload binding verified (SHA-256(opJSON) == claimed hash)
- Pending: capped at 65536 to prevent OOM
- HTTP body: capped at 1MB
- Health: minimal response without auth, full details with auth
- zapdb: AES encryption enforced for disk stores (key length validated: 16/24/32)
- Backups: 0o600 file permissions

## ZAP Opcodes

### Management (0x01-0x06, HMAC auth)
- 0x01: CreateStream
- 0x02: DeleteStream
- 0x03: ListStreams
- 0x04: Publish
- 0x05: GetStreamInfo
- 0x06: HealthCheck (unauthenticated)

### Quasar Control (0x10-0x13, HMAC auth)
- 0x10: QuasarSubmit
- 0x11: QuasarStatus
- 0x12: QuasarVerify
- 0x13: QuasarRotate

### Quasar P2P (0x20-0x24, crypto-verified)
- 0x20: Propose (hash-verified)
- 0x21: Vote (BLS+PQ signature verified, peer-identity bound)
- 0x22: Finalized (requires pending op + verified votes)
- 0x23: SyncReq (capped at 100 responses)
- 0x24: SyncResp

## Environment Variables
```
PUBSUB_ZAP_PORT          ZAP mgmt port (default: 9222)
PUBSUB_HTTP_PORT         HTTP management port (default: 9280)
PUBSUB_HTTP_TOKEN        HTTP bearer token (empty = no auth)
PUBSUB_ZAP_SECRET        ZAP shared secret for HMAC (empty = no auth)
PUBSUB_QUASAR_ENABLED    Enable Quasar consensus (true/1)
PUBSUB_QUASAR_THRESHOLD  Signature threshold (default: 1)
PUBSUB_QUASAR_VALIDATORS Comma-separated validator IDs
PUBSUB_QUASAR_VALIDATOR_ID  This node's validator identity
PUBSUB_QUASAR_ZAP_PORT  Consensus ZAP port (default: 9223)
PUBSUB_QUASAR_PEERS     Comma-separated peer addresses (host:port)
PUBSUB_QUASAR_DISCOVER  Enable mDNS peer discovery (true)
PUBSUB_STORE_ENABLED    Enable zapdb store (true/1)
PUBSUB_STORE_DIR        zapdb data directory
```

## Build & Test
```bash
go build ./...
go test ./internal/... -v
./pubsub                              # JetStream enabled by default
./pubsub -sd /data/pubsub             # with storage directory
PUBSUB_QUASAR_ENABLED=true ./pubsub   # with Quasar consensus
```

## Embedding (`embed/`)
`embed.Open(Options)` runs Hanzo pubsub in process for a host binary (hanzoai/cloud).
- **Cluster**: `Options.Cluster{Name, Port, Routes, User, Password}` joins the node to its peers.
  JetStream replicates through the server's own raft: one meta group of every member, one group per
  stream. R3 is that raft. Routes are refused without credentials, and list every member, this one
  too: the first election's size is the number of routes. A route port that is already bound is
  refused by `Open`; left to the server it would end the host's process.
- **Identity**: the server name is `<ServerName>.<generation>`. The generation is a random id
  persisted in `StoreDir/generation` at first boot (`Options.Generation` fixes it for a store with
  none). A store that survives keeps its raft name and log together; an empty store is a new member,
  never an old name without its log. `ServerName` is required in a cluster and is the member's own,
  a pod name: members that share one take each other for older generations of themselves. `Open`
  validates the options before it writes the generation, so a refused `Open` leaves `StoreDir` as
  it was.
- **Retire**: once current, a node removes offline older generations of its own name from the meta
  group (`$JS.API.SERVER.REMOVE`, over an in-process system-account client, dialed again if it
  closes). It waits on health and `JetStreamIsCurrent`, not on `Ready`, which waits on it. A failure
  is logged once per distinct message and retried. Before it removes, it has every member report
  (`$SYS.REQ.SERVER.PING.STATSZ`, no reply) and waits `settle`: the meta leader replaces a removed
  member only with one whose JetStream report it has heard since the route last registered, and
  without a replacement the stream stays a replica short.
- **Ready(ctx)**: `Healthz` ok, `JetStreamIsCurrent`, and the older generations removed. The last
  makes it safe to pace a rolling restart by `Ready`: a member that came back empty is current long
  before its old generation is gone, and until then that generation counts toward every majority.
  **Shutdown** is lame duck, and takes at least `lameDuckGrace` (1 s) when a client is connected.
- **Limits**: `MaxStore`, `MaxMemory` bound JetStream; zero leaves the server's own.
- Tests: `go test -race ./embed/...` runs three in-process servers (restart empty, also as a
  separate account behind the host's `Auth`; every member emptied in turn gated on `Ready` alone;
  two emptied at once; all lost; route auth and refused options).

## Key Packages
- `internal/consensus/` — Quasar consensus + ZAP P2P (7 tests)
- `internal/store/` — zapdb + PQ-safe age backup/restore (9 tests)
- `internal/mgmt/` — ZAP + HTTP management with auth (9 tests)
- `server/` — NATS/PubSub server core

## Dependencies
- `github.com/hanzoai/pubsub-go` — PubSub Go client (NATS client fork)
- `github.com/luxfi/zap` — Zero-copy binary RPC
- `github.com/luxfi/consensus` — BLS + PQ consensus engine
- `github.com/luxfi/zapdb/v4` — Embedded KV store (BadgerDB fork)
- `github.com/luxfi/age` — PQ-safe encryption (ML-KEM-768 + X25519)
- `server/` imports `nats-io/*` (upstream fork, not our code)
