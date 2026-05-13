module github.com/hanzoai/pubsub

go 1.26.3

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0
	github.com/google/go-tpm v0.9.8
	github.com/hanzoai/pubsub-go v1.48.0
	github.com/klauspost/compress v1.18.5
	github.com/luxfi/age v1.4.0
	github.com/luxfi/consensus v1.22.0
	github.com/zap-proto/go v0.2.0
	github.com/luxfi/zapdb v1.0.0
	github.com/nats-io/jwt/v2 v2.8.1
	github.com/nats-io/nats.go v1.48.0
	github.com/nats-io/nkeys v0.4.15
	github.com/nats-io/nuid v1.0.1
	golang.org/x/crypto v0.49.0
	golang.org/x/sys v0.42.0
	golang.org/x/time v0.15.0
)

require (
	filippo.io/hpke v0.4.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.0 // indirect
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.1 // indirect
	github.com/consensys/gnark-crypto v0.19.0 // indirect
	github.com/dgraph-io/ristretto/v2 v2.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/flatbuffers v25.2.10+incompatible // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/luxfi/crypto v1.17.4 // indirect
	github.com/luxfi/ids v1.1.2 // indirect
	github.com/luxfi/mdns v0.1.0 // indirect
	github.com/miekg/dns v1.1.62 // indirect
	github.com/mr-tron/base58 v1.2.0 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel v1.37.0 // indirect
	go.opentelemetry.io/otel/metric v1.37.0 // indirect
	go.opentelemetry.io/otel/trace v1.37.0 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/tools v0.42.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// We don't usually pin non-tagged commits but so far no release has
// been made that includes https://github.com/minio/highwayhash/pull/29.
// This will be updated if a new tag covers this in the future.
require github.com/minio/highwayhash v1.0.4

// Use local fork of the Go client (hanzoai/pubsub-go).
// server/ still imports nats-io/nats.go (upstream fork code — left as-is).
replace github.com/hanzoai/pubsub-go => ../pubsub-go
