// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

// Package embed runs an in-process Hanzo PubSub server (core NATS + JetStream)
// for folding into a host binary such as hanzoai/cloud.
//
// It is the ONE public embedding surface and imports ONLY the self-contained
// server package — the same core data plane the standalone `pubsub` deployment
// runs in production (`--jetstream --store_dir=…`, no consensus flags). A host
// imports this package and nothing else, and drives the lifecycle through Open +
// Server.Shutdown.
//
// Unlike package main, Open owns NO process lifecycle: it installs no signal
// handler (server.Options.NoSigs) and never calls os.Exit.
//
// Coordination model. A single embedded node runs JetStream over its local file
// store — there is NO ZooKeeper, raft, or etcd in the path (raft engages only
// for a multi-route JetStream cluster, which this does not form). The optional
// Quasar PQ consensus (Lux) + zapdb control plane that package main can enable
// is intentionally NOT part of this surface: it is unused in the production
// deployment and its adapter (internal/consensus) is pinned to the legacy
// luxfi/consensus v1.22 API, incompatible with the v1.25.x a host like cloud
// resolves. Embedding it is a follow-up gated on that adapter migration.
package embed

import (
	"fmt"
	"time"

	"github.com/hanzoai/pubsub/server"
)

// readyTimeout bounds how long Open waits for the NATS accept loop before
// failing closed.
const readyTimeout = 10 * time.Second

// Options configures an embedded PubSub server. StoreDir is required; every
// other field has a working default.
type Options struct {
	// Host is the NATS client bind address (default "0.0.0.0").
	Host string
	// Port is the NATS client port (default 4222). Use -1 for a random free
	// port (tests).
	Port int
	// ServerName identifies this node (default "pubsub-embedded").
	ServerName string
	// StoreDir is the JetStream file-storage root. Required — JetStream is
	// always on in an embedded server.
	StoreDir string
	// Debug and Trace raise the core server's log verbosity.
	Debug bool
	Trace bool
}

// Server is a running embedded PubSub instance.
type Server struct {
	ns *server.Server
}

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

// Open constructs and starts the core NATS + JetStream server. It blocks only
// until the accept loop is ready (bounded by readyTimeout) and returns; a
// failure tears down partial state and returns an error, so the caller fails
// closed.
func Open(o Options) (*Server, error) {
	if o.StoreDir == "" {
		return nil, fmt.Errorf("embed: StoreDir required")
	}

	opts := &server.Options{
		Host:       strOr(o.Host, "0.0.0.0"),
		Port:       intOr(o.Port, 4222),
		ServerName: strOr(o.ServerName, "pubsub-embedded"),
		JetStream:  true,
		StoreDir:   o.StoreDir,
		NoSigs:     true, // the host owns process signals
		Debug:      o.Debug,
		Trace:      o.Trace,
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("embed: new server: %w", err)
	}
	ns.ConfigureLogger()

	// Start is non-blocking — it launches the accept loops in goroutines.
	ns.Start()
	if !ns.ReadyForConnections(readyTimeout) {
		ns.Shutdown()
		return nil, fmt.Errorf("embed: nats not ready within %s", readyTimeout)
	}

	return &Server{ns: ns}, nil
}

// ClientURL is the NATS URL clients dial (e.g. nats://0.0.0.0:4222). In-process
// clients pass the underlying server (NATS) to nats.InProcessServer for a
// zero-socket connection.
func (s *Server) ClientURL() string { return s.ns.ClientURL() }

// NATS exposes the underlying server for in-process clients.
func (s *Server) NATS() *server.Server { return s.ns }

// Shutdown stops the NATS server and waits for the accept loops to drain. Safe
// to call once on host shutdown.
func (s *Server) Shutdown() {
	if s.ns != nil {
		s.ns.Shutdown()
		s.ns.WaitForShutdown()
	}
}
