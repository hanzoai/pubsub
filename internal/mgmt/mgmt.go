// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

// Package mgmt provides ZAP transport and HTTP management routes for pubsub.
// ZAP is the control plane protocol; NATS stays for pub/sub messaging.
package mgmt

import (
	"context"
	"crypto/hmac"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/luxfi/zap"
	"github.com/hanzoai/pubsub/internal/consensus"
	"github.com/hanzoai/pubsub/internal/store"
	"github.com/hanzoai/pubsub/server"
	"github.com/hanzoai/pubsub-go"
	"github.com/hanzoai/pubsub-go/jetstream"
)

// ZAP opcodes for control plane operations.
const (
	OpCreateStream  uint16 = 0x01
	OpDeleteStream  uint16 = 0x02
	OpListStreams   uint16 = 0x03
	OpPublish       uint16 = 0x04
	OpGetStreamInfo uint16 = 0x05
	OpHealthCheck   uint16 = 0x06

	// Quasar PQ consensus opcodes (0x10 range)
	OpQuasarSubmit  uint16 = 0x10
	OpQuasarStatus  uint16 = 0x11
	OpQuasarVerify  uint16 = 0x12
	OpQuasarRotate  uint16 = 0x13
)

// ZAP object field offsets for requests.
const (
	// CreateStream / DeleteStream / GetStreamInfo: name at offset 0 (text = offset+len = 8 bytes)
	FieldName = 0

	// CreateStream: subjects at offset 8 (text, comma-separated)
	FieldSubjects = 8

	// Publish: subject at offset 0 (text), payload at offset 8 (bytes)
	FieldSubject = 0
	FieldPayload = 8

	// Response: status at offset 0 (uint8, 0=ok, 1=error), message at offset 8 (text)
	FieldStatus  = 0
	FieldMessage = 8
	FieldData    = 16

	// Quasar fields: op_type at offset 16 (uint8), hash at offset 24 (text)
	FieldOpType    = 16
	FieldHash      = 24

	// Object sizes
	RequestSize  = 24
	ResponseSize = 64
	QuasarReqSize = 40
)

// Config configures the management server.
type Config struct {
	NATSServer *server.Server
	ZAPPort    int
	HTTPPort   int
	Logger     *slog.Logger

	// Quasar enables PQ consensus on stream operations.
	Quasar *consensus.Config

	// Store configures the zapdb backing store for metadata/consensus state.
	Store *store.Config

	// HTTPToken is the bearer token for HTTP management endpoints.
	// Empty = no HTTP auth (dev mode only).
	HTTPToken string

	// ZAPSecret is the shared secret for ZAP management HMAC authentication.
	// Each ZAP mgmt message must include HMAC-SHA256(secret, message_bytes) in the
	// signature field. Empty = no ZAP auth (dev mode only).
	// Consensus ZAP (port 9223) uses cryptographic signature verification instead.
	ZAPSecret []byte
}

// Server is the management server providing ZAP transport and HTTP routes.
type Server struct {
	ns         *server.Server
	nc         *nats.Conn
	js         jetstream.JetStream
	zapNode    *zap.Node
	zapPort    int
	httpPort   int
	httpServer *http.Server
	httpLn     net.Listener
	logger     *slog.Logger

	quasar    *consensus.Quasar // nil when PQ consensus disabled
	quasarCfg *consensus.Config
	store     *store.Store     // nil when zapdb store disabled
	storeCfg  *store.Config
	httpToken string // bearer token for HTTP auth
	zapSecret []byte // shared secret for ZAP HMAC auth
}

// New creates a new management server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ZAPPort == 0 {
		cfg.ZAPPort = envInt("PUBSUB_ZAP_PORT", 9222)
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = envInt("PUBSUB_HTTP_PORT", 9280)
	}

	return &Server{
		ns:         cfg.NATSServer,
		zapPort:    cfg.ZAPPort,
		httpPort:   cfg.HTTPPort,
		logger:     cfg.Logger,
		quasarCfg:  cfg.Quasar,
		storeCfg:   cfg.Store,
		httpToken:  cfg.HTTPToken,
		zapSecret:  cfg.ZAPSecret,
	}
}

// Start starts the ZAP listener and HTTP management server.
// Must be called after the NATS server is ready to accept connections.
func (s *Server) Start() error {
	// Connect to the NATS server as an internal client
	nc, err := nats.Connect(s.ns.ClientURL(), nats.InProcessServer(s.ns))
	if err != nil {
		return fmt.Errorf("mgmt: nats connect: %w", err)
	}
	s.nc = nc

	if s.ns.JetStreamEnabled() {
		js, err := jetstream.New(nc)
		if err != nil {
			nc.Close()
			return fmt.Errorf("mgmt: jetstream init: %w", err)
		}
		s.js = js
	}

	// Initialize zapdb store if configured
	if s.storeCfg != nil {
		st, err := store.Open(*s.storeCfg)
		if err != nil {
			s.nc.Close()
			return fmt.Errorf("mgmt: zapdb store: %w", err)
		}
		s.store = st
		s.logger.Info("zapdb store opened", "path", st.Path())
	}

	// Initialize Quasar PQ consensus if configured
	if s.quasarCfg != nil {
		if s.quasarCfg.Logger == nil {
			s.quasarCfg.Logger = s.logger
		}
		qu, err := consensus.New(*s.quasarCfg)
		if err != nil {
			if s.store != nil {
				s.store.Close()
			}
			s.nc.Close()
			return fmt.Errorf("mgmt: quasar init: %w", err)
		}
		if err := qu.Start(context.Background()); err != nil {
			if s.store != nil {
				s.store.Close()
			}
			s.nc.Close()
			return fmt.Errorf("mgmt: quasar start: %w", err)
		}
		// Connect static peers
		for _, peer := range s.quasarCfg.ZAPPeers {
			if err := qu.ConnectPeer(peer); err != nil {
				s.logger.Warn("quasar: connect peer failed", "peer", peer, "error", err)
			}
		}
		s.quasar = qu
		s.logger.Info("quasar PQ consensus started",
			"threshold", s.quasarCfg.Threshold,
			"zap_port", s.quasarCfg.ZAPPort,
			"peers", len(s.quasarCfg.ZAPPeers))
	}

	// Start ZAP node
	s.zapNode = zap.NewNode(zap.NodeConfig{
		NodeID:      "pubsub-mgmt",
		ServiceType: "_pubsub._tcp",
		Port:        s.zapPort,
		Logger:      s.logger,
		NoDiscovery: true,
	})
	s.registerZAPHandlers()

	if err := s.zapNode.Start(); err != nil {
		nc.Close()
		return fmt.Errorf("mgmt: zap start: %w", err)
	}
	s.logger.Info("management ZAP listener started", "port", s.zapPort)

	// Start HTTP (with auth middleware when token is configured)
	mux := http.NewServeMux()
	// Health is always unauthenticated (K8s probes)
	mux.HandleFunc("/v1/pubsub/health", s.handleHealth)
	// All other routes require auth when token is set
	mux.HandleFunc("/v1/pubsub/varz", s.requireHTTPAuth(s.handleVarz))
	mux.HandleFunc("/v1/pubsub/connz", s.requireHTTPAuth(s.handleConnz))
	mux.HandleFunc("/v1/pubsub/streams", s.requireHTTPAuth(s.handleStreams))
	mux.HandleFunc("/v1/pubsub/quasar", s.requireHTTPAuth(s.handleQuasarStatus))
	mux.HandleFunc("/v1/pubsub/quasar/submit", s.requireHTTPAuth(s.handleQuasarSubmit))
	mux.HandleFunc("/v1/pubsub/quasar/verify", s.requireHTTPAuth(s.handleQuasarVerify))

	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.httpPort))
	if err != nil {
		s.zapNode.Stop()
		nc.Close()
		return fmt.Errorf("mgmt: http listen: %w", err)
	}
	s.httpLn = ln
	s.logger.Info("management HTTP server started", "addr", ln.Addr().String())

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("management HTTP server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully stops the management server.
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if s.httpServer != nil {
		s.httpServer.Shutdown(ctx)
	}
	if s.zapNode != nil {
		s.zapNode.Stop()
	}
	if s.quasar != nil {
		s.quasar.Stop()
	}
	if s.store != nil {
		s.store.Close()
	}
	if s.nc != nil {
		s.nc.Close()
	}
	s.logger.Info("management server stopped")
}

// registerZAPHandlers wires up opcode handlers on the ZAP node.
// All handlers except HealthCheck require auth when token is configured.
func (s *Server) registerZAPHandlers() {
	// Health is unauthenticated (probes)
	s.zapNode.Handle(OpHealthCheck, s.zapHealthCheck)

	// All management ops require auth
	s.zapNode.Handle(OpCreateStream, s.zapRequireHMAC(s.zapCreateStream))
	s.zapNode.Handle(OpDeleteStream, s.zapRequireHMAC(s.zapDeleteStream))
	s.zapNode.Handle(OpListStreams, s.zapRequireHMAC(s.zapListStreams))
	s.zapNode.Handle(OpPublish, s.zapRequireHMAC(s.zapPublish))
	s.zapNode.Handle(OpGetStreamInfo, s.zapRequireHMAC(s.zapGetStreamInfo))

	// Quasar PQ consensus opcodes require auth
	s.zapNode.Handle(OpQuasarSubmit, s.zapRequireHMAC(s.zapQuasarSubmit))
	s.zapNode.Handle(OpQuasarStatus, s.zapRequireHMAC(s.zapQuasarStatus))
	s.zapNode.Handle(OpQuasarVerify, s.zapRequireHMAC(s.zapQuasarVerify))
	s.zapNode.Handle(OpQuasarRotate, s.zapRequireHMAC(s.zapQuasarRotate))
}

// --- ZAP Handlers ---

func (s *Server) zapCreateStream(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	name := root.Text(FieldName)
	subjectsRaw := root.Text(FieldSubjects)

	if name == "" {
		return zapError("stream name required")
	}
	if s.js == nil {
		return zapError("jetstream not enabled")
	}

	subjects := splitSubjects(subjectsRaw)
	if len(subjects) == 0 {
		subjects = []string{name + ".>"}
	}

	_, err := s.js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     name,
		Subjects: subjects,
	})
	if err != nil {
		return zapError(err.Error())
	}

	return zapOK("stream created: " + name)
}

func (s *Server) zapDeleteStream(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	name := root.Text(FieldName)

	if name == "" {
		return zapError("stream name required")
	}
	if s.js == nil {
		return zapError("jetstream not enabled")
	}

	if err := s.js.DeleteStream(ctx, name); err != nil {
		return zapError(err.Error())
	}

	return zapOK("stream deleted: " + name)
}

func (s *Server) zapListStreams(_ context.Context, _ string, _ *zap.Message) (*zap.Message, error) {
	info, err := s.listStreamsFromServer()
	if err != nil {
		return zapError(err.Error())
	}

	data, err := json.Marshal(info)
	if err != nil {
		return zapError(err.Error())
	}

	return zapOKWithData(data)
}

func (s *Server) zapPublish(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	subject := root.Text(FieldSubject)
	payload := root.Bytes(FieldPayload)

	if subject == "" {
		return zapError("subject required")
	}

	if err := s.nc.Publish(subject, payload); err != nil {
		return zapError(err.Error())
	}

	return zapOK("published to " + subject)
}

func (s *Server) zapGetStreamInfo(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	name := root.Text(FieldName)

	if name == "" {
		return zapError("stream name required")
	}
	if s.js == nil {
		return zapError("jetstream not enabled")
	}

	stream, err := s.js.Stream(ctx, name)
	if err != nil {
		return zapError(err.Error())
	}

	si, err := stream.Info(ctx)
	if err != nil {
		return zapError(err.Error())
	}

	info := streamInfoResponse{
		Name:     si.Config.Name,
		Subjects: si.Config.Subjects,
		Messages: si.State.Msgs,
		Bytes:    si.State.Bytes,
		FirstSeq: si.State.FirstSeq,
		LastSeq:  si.State.LastSeq,
	}

	data, err := json.Marshal(info)
	if err != nil {
		return zapError(err.Error())
	}

	return zapOKWithData(data)
}

func (s *Server) zapHealthCheck(_ context.Context, _ string, _ *zap.Message) (*zap.Message, error) {
	status := s.ns.Healthz(&server.HealthzOptions{})
	if status.Error != "" {
		return zapError(status.Error)
	}
	return zapOK(status.Status)
}

// --- HTTP Handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hz := s.ns.Healthz(&server.HealthzOptions{})
	// Minimal response for unauthenticated health probes (K8s liveness/readiness).
	// Detailed info (server_id, quasar, zapdb) only included for authenticated requests.
	resp := map[string]any{
		"status": hz.Status,
	}
	if hz.Error != "" {
		resp["error"] = hz.Error
	}

	// Include details only if auth is disabled or token is provided
	auth := r.Header.Get("Authorization")
	showDetail := s.httpToken == "" ||
		(len(auth) >= 8 && auth[:7] == "Bearer " &&
			subtle.ConstantTimeCompare([]byte(auth[7:]), []byte(s.httpToken)) == 1)
	if showDetail {
		resp["jetstream"] = s.ns.JetStreamEnabled()
		resp["server_id"] = s.ns.ID()
		resp["server_name"] = s.ns.Name()
		resp["quasar"] = s.quasar != nil
		resp["zapdb"] = s.store != nil
	}

	writeJSON(w, resp)
}

func (s *Server) handleVarz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	varz, err := s.ns.Varz(&server.VarzOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, varz)
}

func (s *Server) handleConnz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	connz, err := s.ns.Connz(&server.ConnzOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, connz)
}

func (s *Server) handleStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.ns.JetStreamEnabled() {
		writeJSON(w, map[string]any{"streams": []any{}, "count": 0})
		return
	}

	info, err := s.listStreamsFromServer()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"streams": info, "count": len(info)})
}

// --- Helpers ---

type streamInfoResponse struct {
	Name     string   `json:"name"`
	Subjects []string `json:"subjects"`
	Messages uint64   `json:"messages"`
	Bytes    uint64   `json:"bytes"`
	FirstSeq uint64   `json:"first_seq"`
	LastSeq  uint64   `json:"last_seq"`
}

func (s *Server) listStreamsFromServer() ([]streamInfoResponse, error) {
	jsz, err := s.ns.Jsz(&server.JSzOptions{Accounts: true})
	if err != nil {
		return nil, err
	}

	var streams []streamInfoResponse
	for _, ai := range jsz.AccountDetails {
		for _, si := range ai.Streams {
			streams = append(streams, streamInfoResponse{
				Name:     si.Config.Name,
				Subjects: si.Config.Subjects,
				Messages: si.State.Msgs,
				Bytes:    si.State.Bytes,
				FirstSeq: si.State.FirstSeq,
				LastSeq:  si.State.LastSeq,
			})
		}
	}

	if streams == nil {
		streams = []streamInfoResponse{}
	}
	return streams, nil
}

func zapOK(message string) (*zap.Message, error) {
	b := zap.NewBuilder(256)
	obj := b.StartObject(ResponseSize)
	obj.SetUint8(FieldStatus, 0)
	obj.SetText(FieldMessage, message)
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

func zapOKWithData(data []byte) (*zap.Message, error) {
	b := zap.NewBuilder(256 + len(data))
	obj := b.StartObject(ResponseSize)
	obj.SetUint8(FieldStatus, 0)
	obj.SetBytes(FieldData, data)
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

func zapError(message string) (*zap.Message, error) {
	b := zap.NewBuilder(256)
	obj := b.StartObject(ResponseSize)
	obj.SetUint8(FieldStatus, 1)
	obj.SetText(FieldMessage, message)
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

func splitSubjects(s string) []string {
	if s == "" {
		return nil
	}
	var subjects []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if t := trimSpace(s[start:i]); t != "" {
				subjects = append(subjects, t)
			}
			start = i + 1
		}
	}
	if t := trimSpace(s[start:]); t != "" {
		subjects = append(subjects, t)
	}
	return subjects
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && s[i] == ' ' {
		i++
	}
	for j > i && s[j-1] == ' ' {
		j--
	}
	return s[i:j]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// --- Quasar ZAP Handlers ---

func (s *Server) zapQuasarSubmit(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	if s.quasar == nil {
		return zapError("quasar not enabled")
	}
	root := msg.Root()
	op := consensus.Op{
		Type:    consensus.OpType(root.Uint8(FieldOpType)),
		Stream:  root.Text(FieldName),
		Subject: root.Text(FieldSubject),
		Payload: root.Bytes(FieldPayload),
	}
	hash, err := s.quasar.Submit(ctx, op)
	if err != nil {
		return zapError(err.Error())
	}
	return zapOK(hash)
}

func (s *Server) zapQuasarStatus(_ context.Context, _ string, _ *zap.Message) (*zap.Message, error) {
	if s.quasar == nil {
		return zapError("quasar not enabled")
	}
	data, err := json.Marshal(s.quasar.Status())
	if err != nil {
		return zapError(err.Error())
	}
	return zapOKWithData(data)
}

func (s *Server) zapQuasarVerify(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	if s.quasar == nil {
		return zapError("quasar not enabled")
	}
	root := msg.Root()
	hash := root.Text(FieldHash)
	if hash == "" {
		return zapError("hash required")
	}
	if s.quasar.IsFinalized(hash) {
		return zapOK("finalized")
	}
	return zapOK("pending")
}

func (s *Server) zapQuasarRotate(_ context.Context, _ string, _ *zap.Message) (*zap.Message, error) {
	if s.quasar == nil {
		return zapError("quasar not enabled")
	}
	data, err := json.Marshal(s.quasar.Status())
	if err != nil {
		return zapError(err.Error())
	}
	return zapOKWithData(data)
}

// --- Quasar HTTP Handlers ---

func (s *Server) handleQuasarStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.quasar == nil {
		writeJSON(w, consensus.Status{Enabled: false})
		return
	}
	writeJSON(w, s.quasar.Status())
}

func (s *Server) handleQuasarSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.quasar == nil {
		http.Error(w, "quasar not enabled", http.StatusServiceUnavailable)
		return
	}
	// SECURITY: Limit request body to 1MB to prevent OOM (red finding R-HIGH-3).
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var op consensus.Op
	if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hash, err := s.quasar.Submit(r.Context(), op)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"quantum_hash": hash})
}

func (s *Server) handleQuasarVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.quasar == nil {
		http.Error(w, "quasar not enabled", http.StatusServiceUnavailable)
		return
	}
	hash := r.URL.Query().Get("hash")
	if hash == "" {
		http.Error(w, "hash query param required", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"hash":      hash,
		"finalized": s.quasar.IsFinalized(hash),
	})
}

// --- Auth ---

// requireHTTPAuth wraps an HTTP handler with bearer token authentication.
// When httpToken is empty, auth is disabled (dev mode).
func (s *Server) requireHTTPAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.httpToken != "" {
			auth := r.Header.Get("Authorization")
			if len(auth) < 8 || auth[:7] != "Bearer " ||
				subtle.ConstantTimeCompare([]byte(auth[7:]), []byte(s.httpToken)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// zapRequireHMAC wraps a ZAP handler with HMAC-SHA256 peer authentication.
// The ZAP peer must be in the allowedPeers set when zapSecret is configured.
// Authentication works by verifying the peer sent the correct secret during
// its first message (carried in bytes field at offset 56). Once verified,
// the peer ID is added to the allow set for subsequent messages.
// When zapSecret is empty, all peers are allowed (dev mode).
func (s *Server) zapRequireHMAC(next zap.Handler) zap.Handler {
	const fldAuthSecret = 56 // bytes: shared secret for first-message auth
	verified := make(map[string]bool)
	var vmu sync.Mutex

	return func(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
		if len(s.zapSecret) == 0 {
			return next(ctx, from, msg)
		}

		vmu.Lock()
		ok := verified[from]
		vmu.Unlock()
		if ok {
			return next(ctx, from, msg)
		}

		// First message from this peer — verify shared secret.
		root := msg.Root()
		peerSecret := root.Bytes(fldAuthSecret)
		if !hmac.Equal(peerSecret, s.zapSecret) {
			return zapError("unauthorized")
		}

		vmu.Lock()
		verified[from] = true
		vmu.Unlock()

		return next(ctx, from, msg)
	}
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
