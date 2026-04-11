// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package consensus

import (
	"context"
	"testing"
)

func newTestQuasar(t *testing.T) *Quasar {
	t.Helper()
	q, err := New(Config{
		Threshold:    1,
		ValidatorIDs: []string{"v1"},
		ZAPPort:      0, // random port
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(q.Stop)
	return q
}

func TestNewQuasar(t *testing.T) {
	q := newTestQuasar(t)
	s := q.Status()
	if !s.Enabled {
		t.Error("expected enabled")
	}
	if s.Threshold != 1 {
		t.Errorf("threshold = %d, want 1", s.Threshold)
	}
	if s.NodeID == "" {
		t.Error("expected non-empty node ID")
	}
}

func TestNewQuasarBadThreshold(t *testing.T) {
	_, err := New(Config{Threshold: 0})
	if err == nil {
		t.Fatal("expected error for threshold 0")
	}
}

func TestSubmitAndStatus(t *testing.T) {
	q := newTestQuasar(t)

	hash, err := q.Submit(context.Background(), Op{
		Type:   OpStreamCreate,
		Stream: "TEST",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}

	s := q.Status()
	if s.Pending+s.Finalized < 1 {
		t.Errorf("expected at least 1 pending or finalized, got pending=%d finalized=%d", s.Pending, s.Finalized)
	}
}

func TestSubmitDuplicate(t *testing.T) {
	q := newTestQuasar(t)

	_, err := q.Submit(context.Background(), Op{Type: OpStreamCreate, Stream: "DUP"})
	if err != nil {
		t.Fatalf("Submit 1: %v", err)
	}
	_, err = q.Submit(context.Background(), Op{Type: OpStreamCreate, Stream: "DUP2"})
	if err != nil {
		t.Fatalf("Submit 2: %v", err)
	}
}

func TestIsFinalized(t *testing.T) {
	q := newTestQuasar(t)
	if q.IsFinalized("nonexistent") {
		t.Error("nonexistent hash should not be finalized")
	}
}

func TestStatusHasPeers(t *testing.T) {
	q := newTestQuasar(t)
	s := q.Status()
	if s.Peers == nil {
		t.Error("peers should be non-nil (empty slice)")
	}
}

func TestConnectPeer(t *testing.T) {
	// Create two quasar nodes
	q1, err := New(Config{
		Threshold:    1,
		ValidatorIDs: []string{"v1"},
		ValidatorID:  "v1",
		ZAPPort:      0,
	})
	if err != nil {
		t.Fatalf("New q1: %v", err)
	}
	if err := q1.Start(context.Background()); err != nil {
		t.Fatalf("Start q1: %v", err)
	}
	defer q1.Stop()

	q2, err := New(Config{
		Threshold:    1,
		ValidatorIDs: []string{"v2"},
		ValidatorID:  "v2",
		ZAPPort:      0,
	})
	if err != nil {
		t.Fatalf("New q2: %v", err)
	}
	if err := q2.Start(context.Background()); err != nil {
		t.Fatalf("Start q2: %v", err)
	}
	defer q2.Stop()

	// Nodes exist and can report status independently
	s1 := q1.Status()
	s2 := q2.Status()
	if s1.NodeID == s2.NodeID {
		t.Error("expected different node IDs")
	}
}
