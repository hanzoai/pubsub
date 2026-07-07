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
