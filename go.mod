module github.com/hanzoai/pubsub

go 1.26.4

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0
	github.com/google/go-tpm v0.9.8
	github.com/hanzoai/pubsub-go v1.0.1-0.20260513042624-1b25bdfe16a6
	github.com/klauspost/compress v1.18.5
	github.com/luxfi/age v1.4.0
	github.com/luxfi/consensus v1.22.84
	github.com/luxfi/zap v0.8.1
	github.com/luxfi/zapdb v1.0.0
	github.com/nats-io/jwt/v2 v2.8.1
	github.com/nats-io/nats-server/v2 v2.12.3
	github.com/nats-io/nats.go v1.50.0
	github.com/nats-io/nkeys v0.4.15
	github.com/nats-io/nuid v1.0.1
	github.com/zap-proto/go v0.2.0
	golang.org/x/crypto v0.52.0
	golang.org/x/sys v0.45.0
	golang.org/x/time v0.15.0
)

require (
	filippo.io/hpke v0.4.0 // indirect
	github.com/ALTree/bigfloat v0.2.0 // indirect
	github.com/ProjectZKM/Ziren/crates/go-runtime/zkvm_runtime v0.0.0-20260311194731-d5b7577c683d // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/consensys/gnark-crypto v0.20.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1 // indirect
	github.com/dgraph-io/ristretto/v2 v2.4.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/flatbuffers v25.12.19+incompatible // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/luxfi/accel v1.1.9 // indirect
	github.com/luxfi/cache v1.2.1 // indirect
	github.com/luxfi/crypto v1.19.17 // indirect
	github.com/luxfi/crypto/ipa v1.2.4 // indirect
	github.com/luxfi/ids v1.2.9 // indirect
	github.com/luxfi/lattice/v7 v7.0.0 // indirect
	github.com/luxfi/mdns v0.1.1 // indirect
	github.com/luxfi/mock v0.1.1 // indirect
	github.com/luxfi/ringtail v0.2.0 // indirect
	github.com/luxfi/utils v1.1.4 // indirect
	github.com/miekg/dns v1.1.72 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/minio/minio-go/v7 v7.0.100 // indirect
	github.com/montanaflynn/stats v0.8.2 // indirect
	github.com/mr-tron/base58 v1.2.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/tinylib/msgp v1.6.1 // indirect
	github.com/zeebo/blake3 v0.2.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.40.0 // indirect
	go.opentelemetry.io/otel/metric v1.40.0 // indirect
	go.opentelemetry.io/otel/trace v1.40.0 // indirect
	go.uber.org/mock v0.6.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/exp v0.0.0-20260312153236-7ab1446f8b90 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/tools v0.45.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// We don't usually pin non-tagged commits but so far no release has
// been made that includes https://github.com/minio/highwayhash/pull/29.
// This will be updated if a new tag covers this in the future.
require github.com/minio/highwayhash v1.0.4
