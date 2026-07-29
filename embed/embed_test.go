// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package embed

import (
	"context"
	"testing"
	"time"

	nats "github.com/hanzoai/pubsub-go"
	"github.com/hanzoai/pubsub-go/jetstream"
)

// TestOpenServesCoreAndJetStream proves the embedded server accepts a client,
// round-trips a core NATS message, and persists a JetStream publish through the
// file store — the full path the Kafka adaptor rides. Port -1 picks a random
// free port so the test never collides with a standalone pubsub.
func TestOpenServesCoreAndJetStream(t *testing.T) {
	s, err := Open(Options{
		Port:       -1,
		ServerName: "embed-test",
		StoreDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Shutdown()

	if !s.NATS().JetStreamEnabled() {
		t.Fatal("JetStream not enabled on embedded server")
	}

	nc, err := nats.Connect("", nats.InProcessServer(s.NATS()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	// Core NATS round-trip.
	sub, err := nc.SubscribeSync("ping")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Publish("ping", []byte("pong")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("nextmsg: %v", err)
	}
	if string(msg.Data) != "pong" {
		t.Fatalf("core round-trip: got %q, want %q", msg.Data, "pong")
	}

	// JetStream stream + persisted publish.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "EMBED_T",
		Subjects: []string{"t.>"},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	ack, err := js.Publish(ctx, "t.a", []byte("hello"))
	if err != nil {
		t.Fatalf("js publish: %v", err)
	}
	if ack.Sequence != 1 {
		t.Fatalf("js publish seq: got %d, want 1", ack.Sequence)
	}
}

// TestMaxPayloadDefaultsAboveNATSOwn proves the bus does NOT silently inherit
// NATS's 1 MiB default. That default is what wedged insights' ingestion loop:
// the Kafka-wire adaptor rides this server, so a >1 MiB produce failed with
// "Broker: Message size too large", the plugin treated it as an unhandled
// rejection and exited(1), and it crash-looped. The failure is on the PRODUCER,
// so no consumer-side fetch tuning could ever clear it.
func TestMaxPayloadDefaultsAboveNATSOwn(t *testing.T) {
	s, err := Open(Options{Port: -1, ServerName: "embed-maxpayload-default", StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Shutdown()

	const natsOwnDefault = 1 << 20
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	// What the client is TOLD is what bounds it — this is the same value the
	// production server advertised as max_payload=1048576.
	got := nc.MaxPayload()
	if got != int64(DefaultMaxPayload) {
		t.Fatalf("advertised MaxPayload = %d, want DefaultMaxPayload %d", got, DefaultMaxPayload)
	}
	if got <= natsOwnDefault {
		t.Fatalf("MaxPayload %d must exceed NATS's own %d default", got, natsOwnDefault)
	}
}

// TestMaxPayloadHonoursExplicit proves an explicit ceiling reaches the server,
// so an operator can raise it without a code change.
func TestMaxPayloadHonoursExplicit(t *testing.T) {
	const want = 32 << 20
	s, err := Open(Options{Port: -1, ServerName: "embed-maxpayload-explicit", StoreDir: t.TempDir(), MaxPayload: want})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Shutdown()

	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	if got := nc.MaxPayload(); got != int64(want) {
		t.Fatalf("advertised MaxPayload = %d, want %d", got, want)
	}
}
