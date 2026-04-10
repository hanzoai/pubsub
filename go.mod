module github.com/nats-io/nats-server/v2

go 1.26.1

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0
	github.com/google/go-tpm v0.9.8
	github.com/klauspost/compress v1.18.5
	github.com/luxfi/zap v0.2.0
	github.com/nats-io/jwt/v2 v2.8.1
	github.com/nats-io/nats.go v1.50.0
	github.com/nats-io/nkeys v0.4.15
	github.com/nats-io/nuid v1.0.1
	golang.org/x/crypto v0.49.0
	golang.org/x/sys v0.42.0
	golang.org/x/time v0.15.0
)

require (
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/luxfi/mdns v0.1.0 // indirect
	github.com/miekg/dns v1.1.62 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/tools v0.42.0 // indirect
)

// We don't usually pin non-tagged commits but so far no release has
// been made that includes https://github.com/minio/highwayhash/pull/29.
// This will be updated if a new tag covers this in the future.
require github.com/minio/highwayhash v1.0.4
