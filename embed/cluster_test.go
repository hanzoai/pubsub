// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package embed

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	nats "github.com/hanzoai/pubsub-go"
	"github.com/hanzoai/pubsub-go/jetstream"

	"github.com/hanzoai/pubsub/server"
)

const (
	user     = "route"
	password = "s3cret/with:odd@chars"
)

// ring is three members of one cluster, each on ports that stay its own across
// restarts, so the others find it where they left it.
type ring struct {
	t      *testing.T
	name   [3]string
	client [3]int
	route  [3]int
	srv    [3]*Server
	// account, when set, runs the bus as that account with a host
	// authenticator placing clients in it, as the host that embeds this does.
	account string
}

func newRing(t *testing.T) *ring {
	t.Helper()
	r := &ring{t: t, name: [3]string{"a", "b", "c"}}
	var held []net.Listener
	for i := 0; i < 6; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, l)
		port := l.Addr().(*net.TCPAddr).Port
		if i < 3 {
			r.client[i] = port
		} else {
			r.route[i-3] = port
		}
	}
	for _, l := range held {
		l.Close()
	}
	t.Cleanup(func() {
		var wg sync.WaitGroup
		for i := range r.srv {
			if r.srv[i] != nil {
				wg.Add(1)
				go func(s *Server) { defer wg.Done(); s.Shutdown() }(r.srv[i])
			}
		}
		wg.Wait()
	})
	return r
}

func (r *ring) routes() []string {
	var out []string
	for _, p := range r.route {
		out = append(out, fmt.Sprintf("127.0.0.1:%d", p))
	}
	return out
}

func (r *ring) urls() string {
	var out []string
	for _, p := range r.client {
		out = append(out, fmt.Sprintf("nats://127.0.0.1:%d", p))
	}
	return strings.Join(out, ",")
}

// open starts member i over dir. Every member lists every route, itself among
// them, as a StatefulSet's pods do.
func (r *ring) open(i int, dir string) {
	r.t.Helper()
	o := Options{
		Host:       "127.0.0.1",
		Port:       r.client[i],
		ServerName: r.name[i],
		StoreDir:   dir,
		Cluster:    Cluster{Name: "ring", Port: r.route[i], Routes: r.routes(), User: user, Password: password},
	}
	var auth *planeAuth
	if r.account != "" {
		auth = &planeAuth{}
		o.Auth = auth
		o.Accounts = []string{r.account}
	}
	s, err := Open(o)
	if err != nil {
		r.t.Fatalf("open %s: %v", r.name[i], err)
	}
	if auth != nil {
		auth.srv.Store(s)
	}
	r.srv[i] = s
}

// down shuts member i down as a host does.
func (r *ring) down(i int) {
	r.srv[i].Shutdown()
	r.srv[i] = nil
}

// ready waits for every listed member to be ready at once.
func (r *ring) ready(within time.Duration, members ...int) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	errs := make(chan error, len(members))
	for _, i := range members {
		go func() { errs <- r.srv[i].Ready(ctx) }()
	}
	for range members {
		if err := <-errs; err != nil {
			r.t.Fatal(err)
		}
	}
}

// eventually polls f until it returns nil or the time is up.
func eventually(t *testing.T, within time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still %v after %s", what, err, within)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (r *ring) dial() (*nats.Conn, jetstream.JetStream) {
	r.t.Helper()
	opts := []nats.Option{nats.MaxReconnects(-1), nats.ReconnectWait(100 * time.Millisecond), nats.RetryOnFailedConnect(true)}
	if r.account != "" {
		opts = append(opts, nats.UserInfo(r.account, ""))
	}
	nc, err := nats.Connect(r.urls(), opts...)
	if err != nil {
		r.t.Fatalf("connect: %v", err)
	}
	r.t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		r.t.Fatal(err)
	}
	return nc, js
}

// log creates the replicated stream every test writes to. A member is ready
// when it has a meta leader, which can be before that leader has heard from
// every peer, so a placement that finds one offline is tried again.
func (r *ring) log() (jetstream.JetStream, jetstream.Stream) {
	r.t.Helper()
	_, js := r.dial()
	var st jetstream.Stream
	eventually(r.t, 60*time.Second, "stream created", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var err error
		st, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "LOG", Subjects: []string{"log.>"}, Replicas: 3})
		return err
	})
	return js, st
}

// writer appends numbered records and remembers each one the cluster
// acknowledged. A record that fails is tried again under its own id, which the
// stream deduplicates, so a lost acknowledgement never counts twice.
type writer struct {
	js    jetstream.JetStream
	stop  chan struct{}
	done  chan struct{}
	mu    sync.Mutex
	acked map[string]uint64
}

func (r *ring) write(js jetstream.JetStream) *writer {
	w := &writer{js: js, stop: make(chan struct{}), done: make(chan struct{}), acked: map[string]uint64{}}
	go w.run()
	r.t.Cleanup(func() { w.finish() })
	return w
}

func (w *writer) run() {
	defer close(w.done)
	for i := 0; ; {
		select {
		case <-w.stop:
			return
		default:
		}
		id := fmt.Sprintf("record-%06d", i)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ack, err := w.js.Publish(ctx, "log.event", []byte(id), jetstream.WithMsgID(id))
		cancel()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		w.mu.Lock()
		w.acked[id] = ack.Sequence
		w.mu.Unlock()
		i++
		time.Sleep(10 * time.Millisecond)
	}
}

func (w *writer) finish() map[string]uint64 {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]uint64, len(w.acked))
	for k, v := range w.acked {
		out[k] = v
	}
	return out
}

func (w *writer) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.acked)
}

// kept fails the test for any acknowledged record the stream does not hold
// under the sequence it was acknowledged at.
func kept(t *testing.T, st jetstream.Stream, acked map[string]uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	lost := 0
	for id, seq := range acked {
		msg, err := st.GetMsg(ctx, seq)
		if err != nil || string(msg.Data) != id {
			lost++
			if lost <= 5 {
				t.Errorf("acknowledged %s at %d is gone: %v", id, seq, err)
			}
		}
	}
	if lost > 0 {
		t.Fatalf("%d of %d acknowledged records lost", lost, len(acked))
	}
}

// replica reads what member s itself holds of the stream, not what the
// cluster says it holds.
func replica(s *Server) (server.StreamState, error) {
	info, err := s.NATS().Jsz(&server.JSzOptions{Accounts: true, Streams: true})
	if err != nil {
		return server.StreamState{}, err
	}
	for _, a := range info.AccountDetails {
		for _, d := range a.Streams {
			if d.Name == "LOG" {
				return d.State, nil
			}
		}
	}
	return server.StreamState{}, errors.New("holds no replica")
}

// converged waits until every live member holds the same stream, as long as the
// leader's, and returns its length.
func (r *ring) converged(within time.Duration, st jetstream.Stream) uint64 {
	r.t.Helper()
	var last uint64
	eventually(r.t, within, "replicas converge", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		info, err := st.Info(ctx)
		if err != nil {
			return err
		}
		last = info.State.LastSeq
		for i, s := range r.srv {
			if s == nil {
				continue
			}
			got, err := replica(s)
			if err != nil {
				return fmt.Errorf("%s %v", r.name[i], err)
			}
			if got.LastSeq != last || got.Msgs != info.State.Msgs {
				return fmt.Errorf("%s holds %d of %d records", r.name[i], got.Msgs, info.State.Msgs)
			}
		}
		return nil
	})
	return last
}

// whole waits until the stream has three current replicas and none of them is
// a member named in gone.
func (r *ring) whole(within time.Duration, st jetstream.Stream, gone ...string) {
	r.t.Helper()
	live := map[string]bool{}
	for _, s := range r.srv {
		if s != nil {
			live[s.NATS().Name()] = true
		}
	}
	eventually(r.t, within, "R3 restored", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		info, err := st.Info(ctx)
		if err != nil {
			return err
		}
		c := info.Cluster
		if c == nil || c.Leader == "" || len(c.Replicas) != 2 {
			return fmt.Errorf("cluster %+v", c)
		}
		for _, name := range append([]string{c.Leader}, replicaNames(c)...) {
			if !live[name] {
				return fmt.Errorf("%s is a replica but no live member", name)
			}
			for _, g := range gone {
				if name == g {
					return fmt.Errorf("%s is still a replica", name)
				}
			}
		}
		for _, p := range c.Replicas {
			if !p.Current || p.Offline {
				return fmt.Errorf("%s is not current", p.Name)
			}
		}
		return nil
	})
}

func replicaNames(c *jetstream.ClusterInfo) []string {
	var out []string
	for _, p := range c.Replicas {
		out = append(out, p.Name)
	}
	return out
}

// members lists the names the meta leader holds as members of the cluster.
func (r *ring) members() ([]string, error) {
	for _, s := range r.srv {
		if s == nil || !s.NATS().JetStreamIsLeader() {
			continue
		}
		info, err := s.NATS().Jsz(nil)
		if err != nil || info.Meta == nil {
			return nil, fmt.Errorf("meta: %v", err)
		}
		out := []string{info.Meta.Leader}
		for _, p := range info.Meta.Replicas {
			out = append(out, p.Name)
		}
		return out, nil
	}
	return nil, errors.New("no meta leader")
}

// forgotten waits until the meta group no longer holds the named member.
func (r *ring) forgotten(within time.Duration, name string) {
	r.t.Helper()
	eventually(r.t, within, name+" removed", func() error {
		names, err := r.members()
		if err != nil {
			return err
		}
		for _, n := range names {
			if n == name {
				return fmt.Errorf("meta group still holds it among %v", names)
			}
		}
		if len(names) != 3 {
			return fmt.Errorf("meta group holds %v", names)
		}
		return nil
	})
}

// absent fails the test when the meta group holds the named member now. Unlike
// forgotten it does not wait for the member to leave: it asks what Ready
// promised.
func (r *ring) absent(name string) {
	r.t.Helper()
	var names []string
	eventually(r.t, 10*time.Second, "meta group", func() (err error) {
		names, err = r.members()
		return err
	})
	for _, n := range names {
		if n == name {
			r.t.Fatalf("a member is ready while the meta group still holds %s among %v", name, names)
		}
	}
}

// One server restarted with an empty store catches up under a new name, the old
// one is removed, R3 is whole again, and nothing the cluster acknowledged on the
// way is lost.
func TestRestartedEmptyCatchesUp(t *testing.T) { restartEmpty(t, "") }

// The same when the bus runs as a separate account behind the host's
// authenticator, as it does in the host that embeds it: the account's JetStream
// is the one that must be repaired.
func TestRestartedEmptyInAnAccount(t *testing.T) { restartEmpty(t, "event") }

func restartEmpty(t *testing.T, account string) {
	r := newRing(t)
	r.account = account
	for i := range r.srv {
		r.open(i, t.TempDir())
	}
	r.ready(90*time.Second, 0, 1, 2)
	js, st := r.log()
	w := r.write(js)

	eventually(t, 30*time.Second, "records", func() error {
		if w.count() < 100 {
			return fmt.Errorf("%d acknowledged", w.count())
		}
		return nil
	})
	r.converged(30*time.Second, st)

	old := r.srv[1].NATS().Name()
	r.down(1)
	empty := t.TempDir()
	r.open(1, empty)
	renamed := r.srv[1].NATS().Name()
	if renamed == old || !strings.HasPrefix(renamed, "b.") {
		t.Fatalf("restarted as %q after %q", renamed, old)
	}
	r.ready(120*time.Second, 0, 1, 2)
	r.absent(old)
	r.whole(90*time.Second, st, old)

	acked := w.finish()
	r.converged(60*time.Second, st)
	kept(t, st, acked)
	if len(acked) <= 100 {
		t.Fatalf("only %d records acknowledged", len(acked))
	}
}

// A rolling restart that waits for each member's Ready and for nothing else
// loses nothing, however fast it goes. Every member is emptied in turn, the next
// the moment the one before is ready. A member that were ready while its older
// generation still sat in the cluster would leave that generation counting
// toward every majority, and the next restart would take the cluster below one.
func TestRollingRestartLosesNothing(t *testing.T) {
	r := newRing(t)
	for i := range r.srv {
		r.open(i, t.TempDir())
	}
	r.ready(90*time.Second, 0, 1, 2)
	js, st := r.log()
	w := r.write(js)
	eventually(t, 30*time.Second, "records", func() error {
		if w.count() < 100 {
			return fmt.Errorf("%d acknowledged", w.count())
		}
		return nil
	})
	r.converged(30*time.Second, st)

	var gone []string
	for _, i := range []int{2, 1, 0} {
		old := r.srv[i].NATS().Name()
		gone = append(gone, old)
		r.down(i)
		r.open(i, t.TempDir())
		r.ready(120*time.Second, i)
		r.absent(old)
	}

	r.ready(120*time.Second, 0, 1, 2)
	for _, name := range gone {
		r.forgotten(60*time.Second, name)
	}
	r.whole(120*time.Second, st, gone...)
	acked := w.finish()
	r.converged(60*time.Second, st)
	kept(t, st, acked)
	if len(acked) <= 100 {
		t.Fatalf("only %d records acknowledged", len(acked))
	}
}

// Two members emptied at once leave the third as the only one that holds the
// log. Without it the two elect nobody and acknowledge nothing. With it they
// still acknowledge nothing: admitting a new member takes a majority of the
// members it already has, and two of those are gone. What the survivor
// acknowledged before it still holds, whole.
func TestTwoEmptiedNeedTheSurvivor(t *testing.T) {
	r := newRing(t)
	survivor := t.TempDir()
	r.open(0, survivor)
	r.open(1, t.TempDir())
	r.open(2, t.TempDir())
	r.ready(90*time.Second, 0, 1, 2)
	js, st := r.log()
	w := r.write(js)
	eventually(t, 30*time.Second, "records", func() error {
		if w.count() < 100 {
			return fmt.Errorf("%d acknowledged", w.count())
		}
		return nil
	})
	acked := w.finish()
	r.converged(30*time.Second, st)
	if len(acked) < 100 {
		t.Fatalf("only %d records acknowledged", len(acked))
	}

	r.down(0)
	r.down(1)
	r.down(2)
	r.open(1, t.TempDir())
	r.open(2, t.TempDir())

	// Nobody forged an acknowledgement through any member.
	_, alone := r.dial()
	forge := func(when string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if ack, err := alone.Publish(ctx, "log.event", []byte("forged")); err == nil {
			t.Fatalf("%s: acknowledged at %d", when, ack.Sequence)
		}
	}

	// Two members that remember nothing are not a majority of anything: past
	// every election timeout they have no leader, are not ready, and cannot
	// acknowledge a write.
	time.Sleep(15 * time.Second)
	for _, i := range []int{1, 2} {
		if r.srv[i].NATS().JetStreamIsLeader() {
			t.Fatalf("%s leads the meta group without the survivor", r.name[i])
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := r.srv[1].Ready(ctx); err == nil {
		t.Fatal("a member with no meta leader is ready")
	}
	cancel()
	forge("without the survivor")

	// The survivor returns. It is the only member that can lead, and it leads
	// no one it can commit with.
	r.open(0, survivor)
	eventually(t, 60*time.Second, "the survivor's records", func() error {
		got, err := replica(r.srv[0])
		if err != nil {
			return err
		}
		if got.Msgs != uint64(len(acked)) || got.LastSeq != uint64(len(acked)) {
			return fmt.Errorf("holds %d records ending at %d, acknowledged %d", got.Msgs, got.LastSeq, len(acked))
		}
		return nil
	})
	time.Sleep(15 * time.Second)
	forge("with the survivor")
	got, err := replica(r.srv[0])
	if err != nil || got.Msgs != uint64(len(acked)) || got.LastSeq != uint64(len(acked)) {
		t.Fatalf("the survivor holds %+v after the stall: %v", got, err)
	}
	for _, i := range []int{1, 2} {
		if _, err := replica(r.srv[i]); err == nil {
			t.Fatalf("%s holds a replica it was never admitted to", r.name[i])
		}
	}
}

// With nobody left to remember, a cluster forms from nothing: every member is a
// new generation, and the first election needs the whole cluster rather than a
// majority.
func TestAllLostFormsAFreshCluster(t *testing.T) {
	r := newRing(t)
	for i := range r.srv {
		r.open(i, t.TempDir())
	}
	r.ready(90*time.Second, 0, 1, 2)
	was := [3]string{}
	for i, s := range r.srv {
		was[i] = s.NATS().Name()
	}
	r.log()
	for i := range r.srv {
		r.down(i)
	}

	for i := range r.srv {
		r.open(i, t.TempDir())
		if r.srv[i].NATS().Name() == was[i] {
			t.Fatalf("%s came back under %s", r.name[i], was[i])
		}
	}
	r.ready(180*time.Second, 0, 1, 2)

	js, st := r.log()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < 20; i++ {
		if _, err := js.Publish(ctx, "log.event", []byte(fmt.Sprint(i))); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	r.converged(60*time.Second, st)
	r.whole(60*time.Second, st, was[:]...)
	names, err := r.members()
	if err != nil || len(names) != 3 {
		t.Fatalf("members %v %v", names, err)
	}
}

// The generation file is the store's identity, so a refused Open must not have
// written one, or a store would be pinned to a generation by a host that never
// started.
func untouched(t *testing.T, what, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, generation)); err == nil {
		t.Errorf("%s: a refused Open left a generation in the store", what)
	}
}

// A cluster port that does not ask who is dialling admits a member that votes,
// so a cluster with no credentials is refused, and so is a route that arrives
// without them.
func TestRoutesWithoutCredentialsAreRefused(t *testing.T) {
	for name, c := range map[string]Cluster{
		"no user":     {Name: "ring", Port: -1, Routes: []string{"127.0.0.1:6222"}, Password: password},
		"no password": {Name: "ring", Port: -1, Routes: []string{"127.0.0.1:6222"}, User: user},
		"none":        {Name: "ring", Port: -1, Routes: []string{"127.0.0.1:6222"}},
		"no routes":   {Name: "ring", Port: -1, User: user, Password: password},
		"no name":     {Port: -1, User: user, Password: password},
		"own":         {Name: "ring", Port: -1, User: user, Password: password, Routes: []string{"nats-route://x:y@127.0.0.1:6222"}},
		"no port":     {Name: "ring", Port: -1, User: user, Password: password, Routes: []string{"127.0.0.1"}},
		"scheme":      {Name: "ring", Port: -1, User: user, Password: password, Routes: []string{"http://127.0.0.1:6222"}},
	} {
		dir := t.TempDir()
		s, err := Open(Options{Port: -1, ServerName: "member", StoreDir: dir, Generation: "pinned", Cluster: c})
		if err == nil {
			s.Shutdown()
			t.Errorf("%s: opened", name)
		}
		untouched(t, name, dir)
	}

	// The route it dials is never there; the one that matters is the one it
	// listens on.
	s, err := Open(Options{Host: "127.0.0.1", Port: -1, ServerName: "member", StoreDir: t.TempDir(), Cluster: Cluster{Name: "ring", Port: -1, Routes: []string{"127.0.0.1:1"}, User: user, Password: password}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	dial := func(connect string) string {
		conn, err := net.Dial("tcp", s.NATS().ClusterAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		rd := bufio.NewReader(conn)
		if _, err := rd.ReadString('\n'); err != nil { // INFO
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "CONNECT %s\r\nPING\r\n", connect)
		line, _ := rd.ReadString('\n')
		return line
	}
	for _, connect := range []string{
		`{"verbose":false,"pedantic":false,"cluster":"ring"}`,
		`{"verbose":false,"pedantic":false,"cluster":"ring","user":"route","pass":"wrong"}`,
	} {
		if line := dial(connect); !strings.HasPrefix(line, "-ERR") || !strings.Contains(strings.ToLower(line), "authorization") {
			t.Errorf("route with %s answered %q, want an authorization error", connect, line)
		}
	}
	right := fmt.Sprintf(`{"verbose":false,"pedantic":false,"cluster":"ring","user":%q,"pass":%q}`, user, password)
	if line := dial(right); strings.HasPrefix(line, "-ERR") {
		t.Errorf("route with credentials answered %q", line)
	}
}

// A member is its name: with none given a cluster would run every member as
// the same default, each taking the others for older generations of itself.
func TestAClusterNeedsAServerName(t *testing.T) {
	dir := t.TempDir()
	c := Cluster{Name: "ring", Port: -1, Routes: []string{"127.0.0.1:6222"}, User: user, Password: password}
	if s, err := Open(Options{Port: -1, StoreDir: dir, Generation: "pinned", Cluster: c}); err == nil {
		s.Shutdown()
		t.Fatal("a clustered server opened without a name")
	} else if !strings.Contains(err.Error(), "ServerName") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	untouched(t, "no name", dir)

	// A node alone keeps its default.
	s, err := Open(Options{Port: -1, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	if got := s.NATS().Name(); !strings.HasPrefix(got, "pubsub-embedded.") {
		t.Fatalf("a node alone is named %q", got)
	}
}

// The client retire speaks through is dialed again when it has closed, so one
// failed connection does not leave the older generations in the cluster for the
// life of the process.
func TestRetireDialsAgain(t *testing.T) {
	// Two members that are never there: this one cannot form a majority alone,
	// so it is never current and retire leaves the client to the test.
	c := Cluster{Name: "ring", Port: -1, Routes: []string{"127.0.0.1:1", "127.0.0.1:2"}, User: user, Password: password}
	s, err := Open(Options{Host: "127.0.0.1", Port: -1, ServerName: "member", StoreDir: t.TempDir(), Cluster: c})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	first, err := s.dial()
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.dial(); err != nil || again != first {
		t.Fatalf("an open client was replaced: %v", err)
	}
	first.Close()
	second, err := s.dial()
	if err != nil {
		t.Fatal(err)
	}
	if second == first || second.IsClosed() {
		t.Fatal("a closed client was handed out again")
	}
}

// A route port that is taken is an error from Open: the server's own answer is
// to stop the process, which would be the host's.
func TestARouteThatCannotBeHeldIsRefused(t *testing.T) {
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	c := Cluster{Name: "ring", Port: port, Routes: []string{fmt.Sprintf("127.0.0.1:%d", port)}, User: user, Password: password}
	if s, err := Open(Options{Host: "127.0.0.1", Port: -1, ServerName: "member", StoreDir: dir, Generation: "pinned", Cluster: c}); err == nil {
		s.Shutdown()
		t.Fatal("opened on a route port that is taken")
	} else if !strings.Contains(err.Error(), fmt.Sprint(port)) {
		t.Fatalf("the error does not name the port: %v", err)
	}
	untouched(t, "busy route port", dir)
}

// A store keeps the generation it was first booted with, and a new store gets a
// new one: the name carries the log's identity.
func TestGenerationFollowsTheStore(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(Options{Port: -1, ServerName: "gen", StoreDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	name := first.NATS().Name()
	first.Shutdown()
	raw, err := os.ReadFile(filepath.Join(dir, generation))
	if err != nil {
		t.Fatal(err)
	}
	if want := "gen." + strings.TrimSpace(string(raw)); name != want || len(strings.TrimSpace(string(raw))) != 16 {
		t.Fatalf("server name %q, generation file %q", name, raw)
	}

	again, err := Open(Options{Port: -1, ServerName: "gen", StoreDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.NATS().Name(); got != name {
		t.Fatalf("a store that survived renamed %q to %q", name, got)
	}
	again.Shutdown()

	fresh, err := Open(Options{Port: -1, ServerName: "gen", StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.NATS().Name(); got == name {
		t.Fatalf("an empty store came back as %q", got)
	}
	fresh.Shutdown()

	// A host may choose the generation of a store that has none, but not
	// rename a store that has one.
	chosen, err := Open(Options{Port: -1, ServerName: "gen", StoreDir: t.TempDir(), Generation: "pinned"})
	if err != nil {
		t.Fatal(err)
	}
	if got := chosen.NATS().Name(); got != "gen.pinned" {
		t.Fatalf("chosen generation became %q", got)
	}
	chosen.Shutdown()
	if s, err := Open(Options{Port: -1, ServerName: "gen", StoreDir: dir, Generation: "other"}); err == nil {
		s.Shutdown()
		t.Fatal("a store was renamed")
	}
	for _, bad := range []string{"has.dot", "has space", "$G"} {
		if s, err := Open(Options{Port: -1, StoreDir: t.TempDir(), Generation: bad}); err == nil {
			s.Shutdown()
			t.Errorf("generation %q was accepted", bad)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, generation), []byte("not a name!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(Options{Port: -1, ServerName: "gen", StoreDir: dir}); err == nil {
		s.Shutdown()
		t.Fatal("a damaged generation was replaced")
	}
}

// Shutdown is lame duck: a client is told the server is going before it is
// disconnected.
func TestShutdownIsLameDuck(t *testing.T) {
	s, err := Open(Options{Port: -1, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	told := make(chan struct{})
	nc, err := nats.Connect(s.ClientURL(), nats.LameDuckModeHandler(func(*nats.Conn) { close(told) }), nats.NoReconnect())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	done := make(chan struct{})
	go func() { s.Shutdown(); close(done) }()
	select {
	case <-told:
	case <-time.After(10 * time.Second):
		t.Fatal("the client was never told the server is going")
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

// Ready is the server's health, and for a node alone that is recovered at once.
func TestReadyWaitsForTheCluster(t *testing.T) {
	alone, err := Open(Options{Port: -1, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer alone.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := alone.Ready(ctx); err != nil {
		t.Fatalf("a node alone: %v", err)
	}

	// One of three has nobody to elect with.
	r := newRing(t)
	r.open(0, t.TempDir())
	short, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.srv[0].Ready(short); err == nil {
		t.Fatal("a member of a cluster that has not formed is ready")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("not ready for the wrong reason: %v", err)
	}
}

// The storage limits reach JetStream, so a node on a small volume is bounded by
// what it was given rather than by the disk it can see.
func TestStorageLimitsReachJetStream(t *testing.T) {
	s, err := Open(Options{Port: -1, StoreDir: t.TempDir(), MaxStore: 3 << 30, MaxMemory: 1 << 28})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	cfg := s.NATS().JetStreamConfig()
	if cfg.MaxStore != 3<<30 || cfg.MaxMemory != 1<<28 {
		t.Fatalf("limits are %d store and %d memory", cfg.MaxStore, cfg.MaxMemory)
	}
}
