// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

// Package embed runs an in-process Hanzo PubSub server (core NATS + JetStream)
// for folding into a host binary such as hanzoai/cloud.
//
// It is the ONE public embedding surface and imports ONLY the self-contained
// server package and the Go client the cluster dials itself with — the same
// core data plane the standalone `pubsub` deployment runs in production
// (`--jetstream --store_dir=…`, no consensus flags). A host imports this
// package and nothing else, and drives the lifecycle through Open, Ready and
// Server.Shutdown.
//
// Unlike package main, Open owns NO process lifecycle: it installs no signal
// handler (server.Options.NoSigs) and never calls os.Exit.
//
// Coordination model. Without Options.Cluster a single embedded node runs
// JetStream over its local file store and nothing replicates. With it the node
// routes to its peers and JetStream replicates through NATS's own raft: one
// group of every member that orders streams and consumers, and one group per
// stream that orders its messages. R3 is that raft, with three members; there
// is no ZooKeeper or etcd in the path. The optional Quasar PQ consensus (Lux)
// + zapdb control plane that package main can enable is intentionally NOT part
// of this surface: it is unused in the production deployment and its adapter
// (internal/consensus) is pinned to the legacy luxfi/consensus v1.22 API,
// incompatible with the v1.25.x a host like cloud resolves. Embedding it is a
// follow-up gated on that adapter migration.
//
// Identity. A raft member is its server name, and a member that returns under
// a name it held, without the log it held under it, answers for votes and
// acknowledgements it no longer remembers. A host that keeps the store on a
// volume that dies with the process therefore cannot keep the name either. The
// server's name is <ServerName>.<generation>, the generation a random id written
// in StoreDir at first boot: a store that survives restarts under the name it
// had, and an empty one starts under a new name, as a new member. Once it is
// current that member removes the older generations of its name from the
// cluster, which are offline and would otherwise count toward every majority
// for good.
//
// Lifecycle. Open returns once the server accepts connections, not once the
// cluster has formed; Ready says when it has. Shutdown is lame duck.
package embed

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hanzoai/pubsub/server"
)

// readyTimeout bounds how long Open waits for the NATS accept loop before
// failing closed.
const readyTimeout = 10 * time.Second

// DefaultMaxPayload is the bus's message-body ceiling when Options.MaxPayload is
// unset: 8 MiB, eight times NATS's own 1 MiB default.
//
// 1 MiB is too small for the traffic this bus actually carries. The Kafka-wire
// adaptor rides it, and its clients size themselves in MiB — insights' ingestion
// consumer alone runs a 10 MiB fetch ceiling — so a 1 MiB bus silently caps every
// one of them, and the failure lands on the PRODUCER as "Message size too large".
// Consumer-side tuning cannot fix that, which is what makes it so easy to chase
// in the wrong direction.
const DefaultMaxPayload int32 = 8 << 20

// Options configures an embedded PubSub server. StoreDir is required; every
// other field has a working default.
type Options struct {
	// Host is the NATS client bind address (default "0.0.0.0").
	Host string
	// Port is the NATS client port (default 4222). Use -1 for a random free
	// port (tests).
	Port int
	// ServerName identifies this node (default "pubsub-embedded"). The server
	// runs as <ServerName>.<generation>.
	ServerName string
	// StoreDir is the JetStream file-storage root. Required — JetStream is
	// always on in an embedded server.
	StoreDir string
	// Generation is the incarnation of StoreDir this node runs as. Left empty
	// it is the one written in StoreDir at first boot, a random id minted
	// then. Set, it is that generation for a store with none, and a store with
	// another is refused, since its log is another member's.
	Generation string
	// MaxStore and MaxMemory bound JetStream's file and memory storage, in
	// bytes. Zero leaves NATS's own limit: three quarters of the memory the
	// host reports and of the disk that is free. A node on a volume smaller
	// than the disk it reads must say how much it may fill.
	MaxStore  int64
	MaxMemory int64
	// Cluster joins this node to its peers; its zero value is a node alone.
	Cluster Cluster
	// MaxPayload is the largest message body the bus will accept, in bytes
	// (default DefaultMaxPayload). It is the hard ceiling on everything that
	// rides this server, including the Kafka-wire adaptor: a producer whose
	// record exceeds it gets "Message size too large" and cannot make progress
	// — and a client that treats that as fatal will crash-loop forever, because
	// no amount of consumer-side tuning can widen a producer-side ceiling.
	//
	// Left unset this inherited NATS's own 1 MiB default, which is far too small
	// for analytics batches and was never a decision anyone made. Declare it.
	MaxPayload int32
	// Debug and Trace raise the core server's log verbosity.
	Debug bool
	Trace bool

	// Auth decides every client connection: whether it is admitted, which
	// account it lands in, and what it may publish and subscribe to — through
	// ClientAuthentication.RegisterUser. Nil admits every client to the global
	// account with no limit, which is the server a host gets when it says
	// nothing.
	Auth server.Authentication

	// Accounts are named accounts the server runs beside the global one, each
	// with JetStream enabled. An account is a separate subject space AND a
	// separate JetStream: nothing a client does in one — a publish, a stream, a
	// stream that republishes or sources — reaches another, which is the one
	// isolation NATS enforces without trusting a permission list. A client
	// reaches an account only by Auth placing it there.
	//
	// The global account keeps JetStream and every stream it already stored.
	// Naming accounts is what takes that away in NATS — a server with accounts
	// configured enables JetStream only where an account asks, and the global
	// account cannot ask in configuration — so Open enables it itself, and no
	// client is admitted until it has.
	Accounts []string
}

// Server is a running embedded PubSub instance.
type Server struct {
	ns     *server.Server
	secret string             // the credential of the system-account client retire dials with
	stop   context.CancelFunc // ends retire
	done   chan struct{}      // closed when retire has returned and its client is closed
}

// lameDuckGrace and lameDuckSpan are how long Shutdown keeps clients connected
// to a server that no longer accepts them, and how long it takes to close them
// once it starts: long enough for a client to find another member, short
// enough for a pod's termination grace.
const (
	lameDuckGrace = time.Second
	lameDuckSpan  = 5 * time.Second
)

func strOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func intOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func int32Or(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

// Open constructs and starts the core NATS + JetStream server. It blocks only
// until the accept loop is ready (bounded by readyTimeout) and returns; a
// failure tears down partial state and returns an error, so the caller fails
// closed.
func Open(o Options) (*Server, error) {
	if o.StoreDir == "" {
		return nil, fmt.Errorf("embed: StoreDir required")
	}
	gen, err := incarnation(o.StoreDir, o.Generation)
	if err != nil {
		return nil, err
	}

	opts := &server.Options{
		Host:       strOr(o.Host, "0.0.0.0"),
		Port:       intOr(o.Port, 4222),
		ServerName: strOr(o.ServerName, "pubsub-embedded") + "." + gen,
		JetStream:  true,
		StoreDir:   o.StoreDir,
		MaxPayload: int32Or(o.MaxPayload, DefaultMaxPayload),
		NoSigs:     true, // the host owns process signals
		Debug:      o.Debug,
		Trace:      o.Trace,

		JetStreamMaxStore:   o.MaxStore,
		JetStreamMaxMemory:  o.MaxMemory,
		LameDuckDuration:    lameDuckSpan,
		LameDuckGracePeriod: lameDuckGrace,

		CustomClientAuthentication: o.Auth,
	}
	var gate *entry
	if o.Cluster.enabled() {
		if err := o.Cluster.apply(opts); err != nil {
			return nil, err
		}
		gate = &entry{inner: o.Auth}
	}
	if len(o.Accounts) > 0 {
		conf, err := accounts(o.Accounts)
		if err != nil {
			return nil, err
		}
		if err := opts.ProcessConfigString(conf); err != nil {
			return nil, fmt.Errorf("embed: accounts: %w", err)
		}
		if gate == nil {
			gate = &entry{inner: o.Auth}
		}
		gate.open = make(chan struct{})
	}
	s := &Server{done: make(chan struct{})}
	if o.Cluster.enabled() {
		if s.secret, err = secret(); err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		gate.secret = s.secret
	}
	if gate != nil {
		opts.CustomClientAuthentication = gate
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("embed: new server: %w", err)
	}
	ns.ConfigureLogger()
	s.ns = ns
	if gate != nil {
		gate.ns.Store(ns)
	}

	// Start is non-blocking — it launches the accept loops in goroutines.
	ns.Start()
	if !ns.ReadyForConnections(readyTimeout) {
		ns.Shutdown()
		return nil, fmt.Errorf("embed: nats not ready within %s", readyTimeout)
	}
	if gate != nil && gate.open != nil {
		if err := ns.GlobalAccount().EnableJetStream(nil, nil); err != nil {
			ns.Shutdown()
			return nil, fmt.Errorf("embed: jetstream on the global account: %w", err)
		}
		close(gate.open)
	}

	ctx, stop := context.WithCancel(context.Background())
	s.stop = stop
	if o.Cluster.enabled() {
		go s.retire(ctx)
	} else {
		close(s.done)
	}
	return s, nil
}

// accounts renders names as the server's own account configuration, each with
// JetStream enabled — the one public way to enable it on an account before the
// server starts.
//
// A name must be a plain token: it is written into that language, and the
// reserved names ($G, $SYS) are not tokens, so no host can take one.
func accounts(names []string) (string, error) {
	var b strings.Builder
	b.WriteString("accounts {\n")
	seen := map[string]bool{}
	for _, name := range names {
		if !token(name) || seen[name] {
			return "", fmt.Errorf("embed: account %q is not a distinct plain name", name)
		}
		seen[name] = true
		b.WriteString("  " + name + ": { jetstream: enabled }\n")
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// token reports whether s is letters, digits, '-' and '_' only.
func token(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// ClientURL is the NATS URL clients dial (e.g. nats://0.0.0.0:4222). In-process
// clients pass the underlying server (NATS) to nats.InProcessServer for a
// zero-socket connection.
func (s *Server) ClientURL() string { return s.ns.ClientURL() }

// NATS exposes the underlying server for in-process clients.
func (s *Server) NATS() *server.Server { return s.ns }

// Ready blocks until the node can serve: its health is ok, which for a
// clustered node means it has a meta leader and every stream it holds a
// replica of is current, and JetStream is current with the meta leader. It
// returns an error naming what is missing when ctx is done first.
//
// A node alone is ready as soon as it has recovered its streams. A node in a
// cluster is not ready while it is a minority: one that lost its log cannot be
// told by the others from one that is late, and none of them acknowledges
// anything until a majority holds the log.
func (s *Server) Ready(ctx context.Context) error {
	tick := time.NewTicker(readyPoll)
	defer tick.Stop()
	for {
		why := s.waiting()
		if why == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("embed: not ready: %s: %w", why, ctx.Err())
		case <-tick.C:
		}
	}
}

// readyPoll is how often Ready looks again.
const readyPoll = 100 * time.Millisecond

// waiting names what Ready is waiting for, or is empty when it is ready.
func (s *Server) waiting() string {
	if h := s.ns.Healthz(&server.HealthzOptions{}); h.Status != "ok" {
		return strOr(h.Error, h.Status)
	}
	if !s.ns.JetStreamIsCurrent() {
		return "jetstream is not current with the meta leader"
	}
	return ""
}

// Shutdown stops the NATS server as a lame duck: it hands off the groups it
// leads, stops accepting clients and closes the ones it has over lameDuckSpan,
// then stops and waits for the accept loops to drain. Safe to call once on
// host shutdown.
func (s *Server) Shutdown() {
	if s.ns == nil {
		return
	}
	s.stop()
	<-s.done
	s.ns.LameDuckShutdown()
	// Lame duck does nothing for a server that is already in it or has no
	// listener; this ends the server either way.
	s.ns.Shutdown()
	s.ns.WaitForShutdown()
}
