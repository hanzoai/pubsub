module github.com/hanzoai/pubsub

go 1.26.5

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0
	github.com/google/go-tpm v0.9.8
	github.com/hanzoai/pubsub-go v1.0.1-0.20260513042624-1b25bdfe16a6
	github.com/klauspost/compress v1.18.6
	github.com/luxfi/age v1.5.0
	github.com/luxfi/consensus v1.25.17
	github.com/luxfi/zap v0.8.1
	github.com/luxfi/zapdb v1.10.0
	github.com/nats-io/jwt/v2 v2.8.1
	github.com/nats-io/nats-server/v2 v2.12.3
	github.com/nats-io/nats.go v1.50.0
	github.com/nats-io/nkeys v0.4.15
	github.com/nats-io/nuid v1.0.1
	golang.org/x/crypto v0.52.0
	golang.org/x/sys v0.45.0
	golang.org/x/time v0.15.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	filippo.io/hpke v0.4.0 // indirect
	github.com/ALTree/bigfloat v0.2.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/consensys/gnark-crypto v0.20.1 // indirect
	github.com/cronokirby/saferith v0.33.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1 // indirect
	github.com/dgraph-io/ristretto/v2 v2.4.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fxamacker/cbor/v2 v2.9.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/flatbuffers v25.12.19+incompatible // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20260302011040-a15ffb7f9dcc // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/rpc v1.2.1 // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/gtank/merlin v0.1.1 // indirect
	github.com/gtank/ristretto255 v0.2.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/luxfi/accel v1.2.2 // indirect
	github.com/luxfi/corona v0.7.6 // indirect
	github.com/luxfi/crypto v1.19.17 // indirect
	github.com/luxfi/crypto/ipa v1.2.4 // indirect
	github.com/luxfi/ids v1.2.15 // indirect
	github.com/luxfi/lattice/v7 v7.1.4 // indirect
	github.com/luxfi/lens v0.1.4 // indirect
	github.com/luxfi/log v1.4.3 // indirect
	github.com/luxfi/magnetar v1.2.0 // indirect
	github.com/luxfi/math v1.4.1 // indirect
	github.com/luxfi/mdns v0.1.1 // indirect
	github.com/luxfi/metric v1.5.8 // indirect
	github.com/luxfi/mock v0.1.1 // indirect
	github.com/luxfi/pulsar v1.1.1 // indirect
	github.com/luxfi/threshold v1.9.4 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/miekg/dns v1.1.72 // indirect
	github.com/mimoo/StrobeGo v0.0.0-20220103164710-9a04d6ca976b // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/minio/minio-go/v7 v7.0.100 // indirect
	github.com/montanaflynn/stats v0.9.0 // indirect
	github.com/mr-tron/base58 v1.3.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/supranational/blst v0.3.16 // indirect
	github.com/tinylib/msgp v1.6.1 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/zeebo/blake3 v0.2.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.uber.org/mock v0.6.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/exp v0.0.0-20260529124908-c761662dc8c9 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/tools v0.45.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// We don't usually pin non-tagged commits but so far no release has
// been made that includes https://github.com/minio/highwayhash/pull/29.
// This will be updated if a new tag covers this in the future.
require github.com/minio/highwayhash v1.0.4
