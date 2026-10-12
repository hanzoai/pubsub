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
	"strconv"
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
//
// Each member needs its own Options.ServerName, which no other member shares: a
// member takes every other generation of its name for an older one of itself,
// and removes it once it is offline.
type Cluster struct {
	// Name is the cluster's name. Every member names the same one.
	Name string
	// Port is the route port (default 6222). Use -1 for a random free port
	// (tests). A port that is already bound is refused by Open; left to the
	// server it stops the host's process.
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
	port := intOr(c.Port, 6222)
	if port > 0 {
		// The server calls Fatalf when it cannot listen on its route port, which
		// ends the host's process: say so here instead.
		l, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
		if err != nil {
			return fmt.Errorf("embed: cluster route port %d: %w", port, err)
		}
		l.Close()
	}
	opts.Cluster.Name = c.Name
	opts.Cluster.Port = port
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
// has yet to remove, or to retry after a failure.
const retirePoll = 2 * time.Second

// settle is how long a new generation lets the other members hear its report
// before it removes the ones it replaced. The server sends a report at most once
// a second.
const settle = 2 * time.Second

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
// this one is current, and then says so by closing s.retired, which is what
// Ready waits for.
//
// A server that comes back with an empty store comes back as a new peer, and
// the peer it was before stays in every raft group as a member that never
// answers: it counts toward every majority and holds a replica nobody can
// read. JetStream drops it only when told to, so this tells it: any peer named
// <name>.<other generation> that is offline is a generation this one
// replaced, since a name is held by one process at a time. A peer of that
// name still online is waited for, and a peer of another name is left alone,
// since it may only be restarting.
//
// It waits on the node being current, not on Ready, since Ready waits on it.
// A failure is logged once and tried again, for as long as ctx lasts: until
// the older generations are gone this node is not ready.
func (s *Server) retire(ctx context.Context) {
	defer close(s.done)
	defer func() {
		if s.conn != nil {
			s.conn.Close()
		}
	}()
	base, _ := split(s.ns.Name())
	var said string
	for {
		wait := readyPoll
		if s.current() == "" {
			removed, left, err := s.sweep(ctx, base)
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				if err.Error() != said {
					said = err.Error()
					s.ns.Warnf("embed: generations: %v", err)
				}
				wait = retirePoll
			case removed > 0:
				said = ""
			case len(left) == 0:
				close(s.retired)
				return
			default:
				if msg := fmt.Sprintf("waiting for %s to go offline", strings.Join(left, ", ")); msg != said {
					said = msg
					s.ns.Noticef("embed: generations: %s", msg)
				}
				wait = retirePoll
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// dial returns the system-account client retire speaks through, connecting it
// again when it has closed.
func (s *Server) dial() (*nats.Conn, error) {
	if s.conn != nil && !s.conn.IsClosed() {
		return s.conn, nil
	}
	nc, err := nats.Connect("", nats.InProcessServer(s.ns), nats.Token(s.secret), nats.NoReconnect())
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	s.conn = nc
	return nc, nil
}

// sweep asks the meta leader for the other generations of base and removes the
// offline ones. It reports how many it removed and the names of those it left,
// which are online.
func (s *Server) sweep(ctx context.Context, base string) (removed int, left []string, err error) {
	nc, err := s.dial()
	if err != nil {
		return 0, nil, err
	}
	gone, err := stale(ctx, nc, s.ns.Name(), base)
	if err != nil {
		return 0, nil, err
	}
	for _, p := range gone {
		if p.Offline {
			if err := hear(ctx, nc); err != nil {
				return 0, nil, err
			}
			break
		}
	}
	for _, p := range gone {
		if !p.Offline {
			left = append(left, p.Name)
			continue
		}
		if err := remove(ctx, nc, p); err != nil {
			return removed, left, fmt.Errorf("removing generation %s: %w", p.Name, err)
		}
		s.ns.Noticef("embed: removed generation %s", p.Name)
		removed++
	}
	return removed, left, nil
}

// hear has every member report to the others, and waits for the reports to land.
//
// The meta leader replaces a removed member with one it has heard a JetStream
// report from since its route last registered, and a route that registers again
// forgets it. The next report on its own can be seconds away, so a removal made
// the moment a member is current can find nothing to replace the old one with:
// its streams then stay a replica short for good, and the next member restarted
// takes the cluster below a majority.
func hear(ctx context.Context, nc *nats.Conn) error {
	if err := nc.Publish("$SYS.REQ.SERVER.PING.STATSZ", nil); err != nil {
		return fmt.Errorf("asking for reports: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settle):
		return nil
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
