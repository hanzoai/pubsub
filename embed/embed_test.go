// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package embed

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/hanzoai/pubsub-go"
	"github.com/hanzoai/pubsub-go/jetstream"

	"github.com/hanzoai/pubsub/server"
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

// planeAuth places a client whose CONNECT user names an account into it and
// every other client into the global account — the shape a host's authenticator
// takes, without the credential check, which is the host's to write.
type planeAuth struct{ srv atomic.Pointer[Server] }

func (a *planeAuth) Check(c server.ClientAuthentication) bool {
	name := c.GetOpts().Username
	if name == "" {
		return true
	}
	acc, err := a.srv.Load().NATS().LookupAccount(name)
	if err != nil {
		return false
	}
	c.RegisterUser(&server.User{Username: name, Account: acc})
	return true
}

func openPlane(t *testing.T, dir string) (*Server, *planeAuth) {
	t.Helper()
	auth := &planeAuth{}
	s, err := Open(Options{Port: -1, ServerName: "embed-accounts", StoreDir: dir, Auth: auth, Accounts: []string{"event"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	auth.srv.Store(s)
	t.Cleanup(s.Shutdown)
	return s, auth
}

func dial(t *testing.T, s *Server, opts ...nats.Option) jetstream.JetStream {
	t.Helper()
	nc, err := nats.Connect(s.ClientURL(), opts...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// An account is its own subject space and its own JetStream: a global client
// publishing onto the plane's subjects reaches nothing of the plane's, cannot see
// its stream, and even a global stream bound to the same subjects is a different
// stream — while the global account keeps JetStream of its own.
func TestAccountsIsolateThePlane(t *testing.T) {
	s, _ := openPlane(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	plane := dial(t, s, nats.UserInfo("event", ""))
	stream, err := plane.CreateStream(ctx, jetstream.StreamConfig{Name: "EVENT", Subjects: []string{"event.>"}})
	if err != nil {
		t.Fatalf("plane stream: %v", err)
	}

	global := dial(t, s)
	if _, err := global.Stream(ctx, "EVENT"); err == nil {
		t.Fatal("the global account sees the plane's stream")
	}
	if _, err := global.CreateStream(ctx, jetstream.StreamConfig{Name: "G", Subjects: []string{"event.>"}}); err != nil {
		t.Fatalf("the global account lost JetStream, or shares the plane's subjects: %v", err)
	}
	if _, err := global.Publish(ctx, "event.economic", []byte("forged")); err != nil {
		t.Fatalf("global publish: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("a global publish landed %d message(s) on the plane", info.State.Msgs)
	}
}

// Naming accounts does not cost the global account the streams it already
// stored: a server restarted with an account beside it recovers them.
func TestAccountsKeepTheGlobalStreams(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	before, err := Open(Options{Port: -1, ServerName: "embed-before", StoreDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	js := dial(t, before)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "KEPT", Subjects: []string{"kept.>"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, "kept.a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	before.Shutdown()

	after, _ := openPlane(t, dir)
	stream, err := dial(t, after).Stream(ctx, "KEPT")
	if err != nil {
		t.Fatalf("the global stream did not survive naming an account: %v", err)
	}
	if info, err := stream.Info(ctx); err != nil || info.State.Msgs != 1 {
		t.Fatalf("recovered stream: %+v %v", info, err)
	}
}

// A name is written into the server's configuration language, so only a plain,
// distinct token is one; the reserved names are not tokens.
func TestAccountsRefuseAnythingButAPlainName(t *testing.T) {
	for _, names := range [][]string{{"$G"}, {"$SYS"}, {"a b"}, {"x\n}"}, {""}, {"e", "e"}} {
		if s, err := Open(Options{Port: -1, StoreDir: t.TempDir(), Accounts: names}); err == nil {
			s.Shutdown()
			t.Errorf("accounts %q were accepted", names)
		}
	}
}
