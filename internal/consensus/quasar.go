// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

// Package consensus provides a Quasar consensus adapter for pubsub.
//
// Quasar uses two independent cryptographic layers (BLS + PQ), each verifiable
// in parallel. All inter-node communication uses ZAP (Zero-copy Application
// Protocol) — no dependency on github.com/luxfi/p2p.
package consensus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/luxfi/consensus/protocol/quasar"
	"github.com/luxfi/zap"

	"github.com/hanzoai/pubsub/internal/store"
)

// Type aliases to the upstream consensus quasar primitives.
type (
	QuasarConsensus = quasar.Quasar
	QuasarSignature = quasar.QuasarSig
)

// maxPendingOps caps the pending map size to prevent OOM from proposal spam.
const maxPendingOps = 65536

// ZAP opcodes for consensus P2P messages (0x20 range).
const (
	OpPropose   uint16 = 0x20 // broadcast pending op for signing
	OpVote      uint16 = 0x21 // QuasarSignature back to proposer
	OpFinalized uint16 = 0x22 // announce PQ finality to all peers
	OpSyncReq   uint16 = 0x23 // request missed finalized ops
	OpSyncResp  uint16 = 0x24 // return finalized ops
)

// ZAP field offsets for consensus messages.
const (
	fldHash        = 0  // text: quantum hash
	fldOpJSON      = 8  // bytes: JSON-encoded Op
	fldValidatorID = 16 // text: validator ID
	fldSigBLS      = 24 // bytes: BLS signature
	fldSigCorona = 32 // bytes: Corona/ML-DSA signature
	fldHeight      = 40 // uint64: finalized height
	fldSinceHeight = 48 // uint64: sync from height

	proposalSize = 24
	voteSize     = 48
	finalizedSz  = 56
	syncReqSz    = 16
	syncRespSz   = 16
)

// Store key prefixes for consensus state persistence.
var (
	storeKeyHeight    = []byte("quasar:height")
	storeKeyPrefix    = []byte("quasar:finalized:")
)

// Op represents a stream operation submitted for PQ consensus.
type Op struct {
	Type      OpType `json:"type"`
	Stream    string `json:"stream"`
	Subject   string `json:"subject,omitempty"`
	Payload   []byte `json:"payload,omitempty"`
	Timestamp int64  `json:"ts"` // set automatically on submit
}

// OpType identifies the kind of stream operation.
type OpType uint8

const (
	OpStreamCreate  OpType = iota + 1
	OpStreamDelete
	OpMessagePublish
)

// FinalizedOp is an operation that achieved PQ finality.
type FinalizedOp struct {
	Hash        string `json:"hash"`
	Height      uint64 `json:"height"`
	FinalizedAt int64  `json:"finalized_at"`
}

// Status reports the current state of the Quasar consensus layer.
type Status struct {
	Enabled    bool     `json:"enabled"`
	Height     uint64   `json:"height"`
	Pending    int      `json:"pending"`
	Finalized  int      `json:"finalized"`
	Validators int      `json:"validators"`
	Threshold  int      `json:"threshold"`
	Peers      []string `json:"peers"`
	NodeID     string   `json:"node_id"`
}

// pendingOp tracks an operation awaiting threshold signatures.
type pendingOp struct {
	Op        Op
	Hash      string
	Height    uint64
	Sigs      map[string]*QuasarSignature
	CreatedAt time.Time
}

// Quasar wraps the Lux consensus signer for pubsub stream operations.
// All P2P communication uses ZAP.
type Quasar struct {
	mu     sync.RWMutex
	signer *QuasarConsensus
	ctx    context.Context
	cancel context.CancelFunc

	// ZAP transport for inter-node consensus
	node        *zap.Node
	validatorID string
	logger      *slog.Logger

	// peerValidators maps ZAP peer node ID → validator ID (identity binding).
	peerValidators map[string]string

	// store persists consensus state across restarts. nil = in-memory only.
	store *store.Store

	pending   map[string]*pendingOp
	finalized map[string]*FinalizedOp
	height    uint64
	maxKeep   int
}

// Config configures the Quasar consensus adapter.
type Config struct {
	Threshold    int
	ValidatorIDs []string
	ValidatorID  string
	MaxFinalized int

	// ZAP transport
	ZAPPort     int
	ZAPPeers    []string
	ZAPDiscover bool

	// Store enables consensus state persistence.
	Store *store.Store

	Logger *slog.Logger
}

// New creates a new Quasar consensus adapter with ZAP transport.
func New(cfg Config) (*Quasar, error) {
	if cfg.Threshold < 1 {
		return nil, fmt.Errorf("quasar: threshold must be >= 1")
	}
	if cfg.MaxFinalized <= 0 {
		cfg.MaxFinalized = 4096
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ZAPPort < 0 {
		cfg.ZAPPort = 0
	}

	// Production BFT consensus requires threshold >= 2 (NewQuasar). A threshold
	// of 1 is a single-node/dev topology, which upstream exposes via NewTestQuasar.
	newSigner := quasar.NewQuasar
	if cfg.Threshold < 2 {
		newSigner = quasar.NewTestQuasar
	}
	signer, err := newSigner(cfg.Threshold)
	if err != nil {
		return nil, fmt.Errorf("quasar: init signer: %w", err)
	}

	for _, id := range cfg.ValidatorIDs {
		if _, err := signer.AddValidator(id, 1); err != nil {
			return nil, fmt.Errorf("quasar: add validator %s: %w", id, err)
		}
	}

	validatorID := cfg.ValidatorID
	if validatorID == "" {
		validatorID = fmt.Sprintf("validator-%d", time.Now().UnixNano()%100000)
	}

	node := zap.NewNode(zap.NodeConfig{
		NodeID:      "quasar-" + validatorID,
		ServiceType: "_pubsub-quasar._tcp",
		Port:        cfg.ZAPPort,
		Logger:      cfg.Logger,
		NoDiscovery: !cfg.ZAPDiscover,
	})

	q := &Quasar{
		signer:         signer,
		node:           node,
		validatorID:    validatorID,
		logger:         cfg.Logger,
		store:          cfg.Store,
		peerValidators: make(map[string]string),
		pending:        make(map[string]*pendingOp),
		finalized:      make(map[string]*FinalizedOp),
		maxKeep:        cfg.MaxFinalized,
	}

	node.Handle(OpPropose, q.handleProposal)
	node.Handle(OpVote, q.handleVote)
	node.Handle(OpFinalized, q.handleFinalized)
	node.Handle(OpSyncReq, q.handleSyncReq)

	// Restore persisted state if store is available
	q.restoreState()

	return q, nil
}

// Start begins the ZAP consensus transport and background routines.
func (q *Quasar) Start(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.ctx, q.cancel = context.WithCancel(ctx)

	if err := q.node.Start(); err != nil {
		return fmt.Errorf("quasar: zap start: %w", err)
	}
	q.logger.Info("quasar ZAP consensus started",
		"node_id", q.node.NodeID(), "validator", q.validatorID)

	go q.evictionLoop(q.ctx)
	return nil
}

// ConnectPeer adds a static peer by address (host:port).
func (q *Quasar) ConnectPeer(addr string) error {
	return q.node.ConnectDirect(addr)
}

// Stop shuts down the ZAP transport and persists state.
func (q *Quasar) Stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.persistState()
	if q.cancel != nil {
		q.cancel()
	}
	q.node.Stop()
}

// Submit submits a stream operation for PQ consensus.
func (q *Quasar) Submit(ctx context.Context, op Op) (string, error) {
	op.Timestamp = time.Now().UnixNano()
	data, err := json.Marshal(op)
	if err != nil {
		return "", fmt.Errorf("quasar: marshal op: %w", err)
	}

	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])

	q.mu.Lock()
	if _, ok := q.finalized[hashHex]; ok {
		q.mu.Unlock()
		return hashHex, nil
	}
	if _, ok := q.pending[hashHex]; ok {
		q.mu.Unlock()
		return hashHex, nil
	}
	if len(q.pending) >= maxPendingOps {
		q.mu.Unlock()
		return "", fmt.Errorf("quasar: pending queue full (%d)", maxPendingOps)
	}

	q.pending[hashHex] = &pendingOp{
		Op:        op,
		Hash:      hashHex,
		Height:    q.height + 1,
		Sigs:      make(map[string]*QuasarSignature),
		CreatedAt: time.Now(),
	}

	if q.signer.GetActiveValidatorCount() > 0 {
		sig, err := q.signer.SignMessage(q.validatorID, hash[:])
		if err == nil {
			q.addVoteLocked(hashHex, q.validatorID, sig)
		}
	}
	q.mu.Unlock()

	q.broadcastProposal(ctx, hashHex, data)
	return hashHex, nil
}

// ReceiveVote records a validator's signature for a pending operation.
func (q *Quasar) ReceiveVote(hash string, validatorID string, sig *QuasarSignature) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.addVoteLocked(hash, validatorID, sig)
}

func (q *Quasar) addVoteLocked(hash string, validatorID string, sig *QuasarSignature) bool {
	if _, ok := q.finalized[hash]; ok {
		return false
	}
	p, ok := q.pending[hash]
	if !ok {
		return false
	}

	hashBytes, err := hex.DecodeString(hash)
	if err != nil || len(hashBytes) == 0 {
		return false
	}
	if !q.signer.VerifyQuasarSig(hashBytes, sig) {
		return false
	}

	p.Sigs[validatorID] = sig

	if len(p.Sigs) >= q.signer.GetThreshold() {
		delete(q.pending, hash)
		q.height++
		fop := &FinalizedOp{
			Hash:        hash,
			Height:      q.height,
			FinalizedAt: time.Now().UnixNano(),
		}
		q.finalized[hash] = fop
		q.pruneFinalized()
		q.persistStateLocked()
		go q.broadcastFinalized(hash, q.height)
	}
	return true
}

// IsFinalized checks whether an operation has achieved PQ finality.
func (q *Quasar) IsFinalized(hash string) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	_, ok := q.finalized[hash]
	return ok
}

// Status returns the current Quasar consensus status.
func (q *Quasar) Status() Status {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return Status{
		Enabled:    true,
		Height:     q.height,
		Pending:    len(q.pending),
		Finalized:  len(q.finalized),
		Validators: q.signer.GetActiveValidatorCount(),
		Threshold:  q.signer.GetThreshold(),
		Peers:      q.node.Peers(),
		NodeID:     q.node.NodeID(),
	}
}

// --- State persistence ---

// persistState writes height to zapdb. Caller must hold q.mu or be in Stop().
func (q *Quasar) persistState() {
	if q.store == nil {
		return
	}
	q.persistStateLocked()
}

func (q *Quasar) persistStateLocked() {
	if q.store == nil {
		return
	}
	data, _ := json.Marshal(q.height)
	_ = q.store.Set(storeKeyHeight, data)
}

// restoreState loads persisted height from zapdb.
func (q *Quasar) restoreState() {
	if q.store == nil {
		return
	}
	data, err := q.store.Get(storeKeyHeight)
	if err != nil {
		return
	}
	var h uint64
	if json.Unmarshal(data, &h) == nil && h > q.height {
		q.height = h
		q.logger.Info("quasar: restored height from store", "height", h)
	}
}

// --- ZAP P2P transport ---

func (q *Quasar) broadcastProposal(ctx context.Context, hash string, opJSON []byte) {
	b := zap.NewBuilder(256 + len(opJSON))
	obj := b.StartObject(proposalSize)
	obj.SetText(fldHash, hash)
	obj.SetBytes(fldOpJSON, opJSON)
	obj.FinishAsRoot()

	msg, err := zap.Parse(b.Finish())
	if err != nil {
		q.logger.Error("quasar: build proposal msg", "error", err)
		return
	}
	errs := q.node.Broadcast(ctx, msg)
	for peer, err := range errs {
		q.logger.Warn("quasar: proposal broadcast failed", "peer", peer, "error", err)
	}
}

func (q *Quasar) broadcastFinalized(hash string, height uint64) {
	b := zap.NewBuilder(128)
	obj := b.StartObject(finalizedSz)
	obj.SetText(fldHash, hash)
	obj.SetUint64(fldHeight, height)
	obj.FinishAsRoot()

	msg, err := zap.Parse(b.Finish())
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	q.node.Broadcast(ctx, msg)
}

func (q *Quasar) sendVote(ctx context.Context, peerID string, hash string, sig *QuasarSignature) {
	b := zap.NewBuilder(512)
	obj := b.StartObject(voteSize)
	obj.SetText(fldHash, hash)
	obj.SetText(fldValidatorID, q.validatorID)
	obj.SetBytes(fldSigBLS, sig.BLS)
	obj.SetBytes(fldSigCorona, sig.Corona)
	obj.FinishAsRoot()

	msg, err := zap.Parse(b.Finish())
	if err != nil {
		return
	}
	if err := q.node.Send(ctx, peerID, msg); err != nil {
		q.logger.Warn("quasar: send vote failed", "peer", peerID, "error", err)
	}
}

// --- ZAP incoming handlers ---

// handleProposal verifies hash binding, signs, and returns vote.
func (q *Quasar) handleProposal(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	hash := root.Text(fldHash)
	opJSON := root.Bytes(fldOpJSON)

	if hash == "" || len(opJSON) == 0 {
		return zapConsensusError("invalid proposal")
	}

	// Verify hash matches payload (prevents signing wrong ops).
	computed := sha256.Sum256(opJSON)
	if hex.EncodeToString(computed[:]) != hash {
		q.logger.Warn("quasar: proposal hash mismatch", "claimed", hash)
		return zapConsensusError("hash mismatch")
	}

	var op Op
	if err := json.Unmarshal(opJSON, &op); err != nil {
		return zapConsensusError("bad op json")
	}

	q.mu.Lock()
	if _, ok := q.finalized[hash]; ok {
		q.mu.Unlock()
		return zapConsensusOK("already finalized")
	}
	if _, exists := q.pending[hash]; !exists {
		if len(q.pending) >= maxPendingOps {
			q.mu.Unlock()
			return zapConsensusError("pending queue full")
		}
		q.pending[hash] = &pendingOp{
			Op:        op,
			Hash:      hash,
			Height:    q.height + 1,
			Sigs:      make(map[string]*QuasarSignature),
			CreatedAt: time.Now(),
		}
	}
	q.mu.Unlock()

	hashBytes, _ := hex.DecodeString(hash)
	sig, err := q.signer.SignMessage(q.validatorID, hashBytes)
	if err != nil {
		return zapConsensusError("sign failed: " + err.Error())
	}

	q.ReceiveVote(hash, q.validatorID, sig)
	go q.sendVote(ctx, from, hash, sig)

	return zapConsensusOK("signed")
}

// handleVote enforces peer-to-validator identity binding.
func (q *Quasar) handleVote(_ context.Context, from string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	hash := root.Text(fldHash)
	validatorID := root.Text(fldValidatorID)
	bls := root.Bytes(fldSigBLS)
	corona := root.Bytes(fldSigCorona)

	if hash == "" || validatorID == "" {
		return zapConsensusError("invalid vote")
	}

	// Enforce peer identity binding — one peer = one validator.
	q.mu.Lock()
	if bound, ok := q.peerValidators[from]; ok {
		if bound != validatorID {
			q.mu.Unlock()
			q.logger.Warn("quasar: vote identity mismatch",
				"peer", from, "claimed", validatorID, "bound", bound)
			return zapConsensusError("identity mismatch")
		}
	} else {
		q.peerValidators[from] = validatorID
	}
	q.mu.Unlock()

	sig := &QuasarSignature{
		BLS:         bls,
		Corona:    corona,
		ValidatorID: validatorID,
	}

	q.ReceiveVote(hash, validatorID, sig)
	return zapConsensusOK("vote accepted")
}

// handleFinalized requires op in pending set with verified votes.
func (q *Quasar) handleFinalized(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	hash := root.Text(fldHash)
	height := root.Uint64(fldHeight)

	if hash == "" {
		return zapConsensusError("invalid finalized")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if _, ok := q.finalized[hash]; ok {
		return zapConsensusOK("ack")
	}

	p, known := q.pending[hash]
	if !known {
		q.logger.Warn("quasar: rejected finality for unknown op", "hash", hash)
		return zapConsensusError("unknown operation")
	}
	if len(p.Sigs) == 0 {
		q.logger.Warn("quasar: rejected finality with no local votes", "hash", hash)
		return zapConsensusError("no verified votes")
	}

	delete(q.pending, hash)
	if height > q.height {
		q.height = height
	}
	q.finalized[hash] = &FinalizedOp{
		Hash:        hash,
		Height:      height,
		FinalizedAt: time.Now().UnixNano(),
	}
	q.pruneFinalized()
	q.persistStateLocked()

	return zapConsensusOK("ack")
}

// handleSyncReq returns finalized ops capped at 100.
func (q *Quasar) handleSyncReq(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	since := root.Uint64(fldSinceHeight)

	const maxSyncResponse = 100

	q.mu.RLock()
	var ops []FinalizedOp
	for _, f := range q.finalized {
		if f.Height > since {
			ops = append(ops, *f)
			if len(ops) >= maxSyncResponse {
				break
			}
		}
	}
	q.mu.RUnlock()

	data, err := json.Marshal(ops)
	if err != nil {
		return zapConsensusError(err.Error())
	}

	b := zap.NewBuilder(64 + len(data))
	obj := b.StartObject(syncRespSz)
	obj.SetBytes(fldOpJSON, data)
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

// --- ZAP message helpers ---

func zapConsensusOK(message string) (*zap.Message, error) {
	b := zap.NewBuilder(64)
	obj := b.StartObject(16)
	obj.SetUint8(0, 0)
	obj.SetText(8, message)
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

func zapConsensusError(message string) (*zap.Message, error) {
	b := zap.NewBuilder(64)
	obj := b.StartObject(16)
	obj.SetUint8(0, 1)
	obj.SetText(8, message)
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

// --- Background routines ---

func (q *Quasar) evictionLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.mu.Lock()
			now := time.Now()
			for hash, p := range q.pending {
				if now.Sub(p.CreatedAt) > time.Minute {
					delete(q.pending, hash)
				}
			}
			q.mu.Unlock()
		}
	}
}

func (q *Quasar) pruneFinalized() {
	if len(q.finalized) <= q.maxKeep {
		return
	}
	var minHash string
	var minHeight uint64 = ^uint64(0)
	for h, f := range q.finalized {
		if f.Height < minHeight {
			minHeight = f.Height
			minHash = h
		}
	}
	if minHash != "" {
		delete(q.finalized, minHash)
	}
}
