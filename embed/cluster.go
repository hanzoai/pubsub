// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package embed

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	nats "github.com/hanzoai/pubsub-go"

	"github.com/hanzoai/pubsub/server"
)

// Cluster joins an embedded server to the others that serve the same bus.
//
// Members route to one another over Port. JetStream then replicates through
// the server's own raft: one group of every member that orders the streams and
// consumers, and one group per stream that orders its messages, each electing a
// leader by majority vote and committing by majority acknowledgement.
type Cluster struct {
	// Name is the cluster's name. Every member names the same one.
	Name string
	// Port is the route port (default 6222). Use -1 for a random free port
	// (tests).
	Port int
	// Routes are the members this one dials, as host:port or
	// nats-route://host:port. List every member, this one too: the size of the
	// first election is the number of routes, and a member that counts fewer
	// than there are lets a minority elect itself.
	Routes []string
	// User and Password authenticate routes in both directions. They are
	// required: a route port that admits anyone admits a member that votes.
	User     string
	Password string
}

// enabled reports whether any field asks for a cluster, so that a half-written
// one is refused rather than ignored.
func (c Cluster) enabled() bool {
	return c.Name != "" || c.Port != 0 || len(c.Routes) > 0 || c.User != "" || c.Password != ""
}

// apply writes the cluster into the server's options.
func (c Cluster) apply(opts *server.Options) error {
	if c.Name == "" {
		return fmt.Errorf("embed: cluster name required")
	}
	if c.User == "" || c.Password == "" {
		return fmt.Errorf("embed: cluster routes need credentials: set Cluster.User and Cluster.Password")
	}
	// JetStream's clustering stops the process when it has no route to start
	// from, and Open never stops the host's.
	if len(c.Routes) == 0 {
		return fmt.Errorf("embed: cluster needs routes")
	}
	opts.Cluster.Name = c.Name
	opts.Cluster.Port = intOr(c.Port, 6222)
	opts.Cluster.Username = c.User
	opts.Cluster.Password = c.Password
	for _, r := range c.Routes {
		if !strings.Contains(r, "://") {
			r = "nats-route://" + r
		}
		u, err := url.Parse(r)
		if err != nil {
			return fmt.Errorf("embed: route %q: %w", r, err)
		}
		if _, _, err := net.SplitHostPort(u.Host); err != nil || u.Scheme != "nats-route" || u.User != nil {
			return fmt.Errorf("embed: route %q is not nats-route://host:port without credentials of its own", r)
		}
		u.User = url.UserPassword(c.User, c.Password)
		opts.Routes = append(opts.Routes, u)
	}
	return nil
}

// secret returns a credential that only this process knows.
func secret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// entry decides who connects to the server's client port. It holds every
// client until the global account has JetStream again when accounts are
// named, admits this process's own system-account client by its credential,
// and hands every other decision to the host's authenticator.
type entry struct {
	inner  server.Authentication
	open   chan struct{} // closed once the global account has JetStream; nil when nothing waits
	secret string        // the credential of this process's system-account client; empty for none
	ns     atomic.Pointer[server.Server]
}

func (e *entry) Check(c server.ClientAuthentication) bool {
	if e.secret != "" && c.Kind() == server.CLIENT &&
		subtle.ConstantTimeCompare([]byte(c.GetOpts().Token), []byte(e.secret)) == 1 {
		ns := e.ns.Load()
		if ns == nil || ns.SystemAccount() == nil {
			return false
		}
		c.RegisterUser(&server.User{Username: "pubsub", Account: ns.SystemAccount()})
		return true
	}
	if e.open != nil {
		select {
		case <-e.open:
		case <-time.After(readyTimeout):
			return false
		}
	}
	if e.inner == nil {
		return true
	}
	return e.inner.Check(c)
}

// retirePoll is how often a new generation looks again for the older ones it
// has yet to remove.
const retirePoll = 2 * time.Second

// members is the part of the meta leader's answer to a JSZ request that names
// the members of the cluster.
type members struct {
	Data struct {
		Meta *server.MetaClusterInfo `json:"meta_cluster"`
	} `json:"data"`
}

// reply is the JetStream API's answer to a server-remove request.
type reply struct {
	Success bool `json:"success"`
	Error   *struct {
		Description string `json:"description"`
	} `json:"error"`
}

// retire removes the older generations of this server from the cluster, once
// this one is current.
//
// A server that comes back with an empty store comes back as a new peer, and
// the peer it was before stays in every raft group as a member that never
// answers: it counts toward every majority and holds a replica nobody can
// read. JetStream drops it only when told to, so this tells it: any peer named
// <name>.<other generation> that is offline is a generation this one
// replaced, since a name is held by one process at a time. A peer of that
// name still online is left alone, and so is every peer of another name,
// which may only be restarting.
//
// It returns when no older generation is left, or when ctx is done.
func (s *Server) retire(ctx context.Context) {
	defer close(s.done)
	if err := s.Ready(ctx); err != nil {
		return
	}
	nc, err := nats.Connect("", nats.InProcessServer(s.ns), nats.Token(s.secret), nats.NoReconnect())
	if err != nil {
		s.ns.Warnf("embed: generations: %v", err)
		return
	}
	defer nc.Close()

	base, _ := split(s.ns.Name())
	for {
		gone, err := stale(ctx, nc, s.ns.Name(), base)
		if err != nil {
			s.ns.Debugf("embed: generations: %v", err)
		}
		if err == nil && len(gone) == 0 {
			return
		}
		for _, p := range gone {
			if !p.Offline {
				continue
			}
			if err := remove(ctx, nc, p); err != nil {
				s.ns.Warnf("embed: removing generation %s: %v", p.Name, err)
				continue
			}
			s.ns.Noticef("embed: removed generation %s", p.Name)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retirePoll):
		}
	}
}

// split cuts a server name into the name the host gave and its generation.
func split(name string) (base, gen string) {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return name, ""
	}
	return name[:i], name[i+1:]
}

// stale asks the meta leader for the members of the cluster and returns those
// that are another generation of base.
func stale(ctx context.Context, nc *nats.Conn, self, base string) ([]*server.PeerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	msg, err := nc.RequestWithContext(ctx, "$SYS.REQ.SERVER.PING.JSZ", []byte(`{"leader_only":true}`))
	if err != nil {
		return nil, err
	}
	var got members
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		return nil, err
	}
	if got.Data.Meta == nil {
		return nil, errors.New("the meta leader sent no members")
	}
	var out []*server.PeerInfo
	for _, p := range got.Data.Meta.Replicas {
		if b, _ := split(p.Name); b == base && p.Name != self {
			out = append(out, p)
		}
	}
	return out, nil
}

// remove asks the meta leader to drop p from every group, as a system-account
// client does with `$JS.API.SERVER.REMOVE`. The answer comes once the removal
// has committed.
func remove(ctx context.Context, nc *nats.Conn, p *server.PeerInfo) error {
	req, err := json.Marshal(server.JSApiMetaServerRemoveRequest{Server: p.Name, Peer: p.Peer})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	msg, err := nc.RequestWithContext(ctx, server.JSApiRemoveServer, req)
	if err != nil {
		return err
	}
	var got reply
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		return err
	}
	if got.Error != nil {
		return errors.New(got.Error.Description)
	}
	if !got.Success {
		return errors.New("refused")
	}
	return nil
}
