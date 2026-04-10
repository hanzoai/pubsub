// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package mgmt

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

func startTestServer(t *testing.T) (*server.Server, *Server) {
	t.Helper()

	dir := t.TempDir()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1, // Random port
		HTTPPort:  -1, // Random port for monitoring
		JetStream: true,
		StoreDir:  filepath.Join(dir, "jetstream"),
		NoLog:     true,
		NoSigs:    true,
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("creating nats server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}

	// Use a random port for mgmt HTTP to avoid conflicts
	os.Setenv("PUBSUB_ZAP_PORT", "0")
	os.Setenv("PUBSUB_HTTP_PORT", "0")
	defer os.Unsetenv("PUBSUB_ZAP_PORT")
	defer os.Unsetenv("PUBSUB_HTTP_PORT")

	mgmt := New(Config{
		NATSServer: ns,
		ZAPPort:    0, // envInt will see "0" env
		HTTPPort:   0, // envInt will see "0" env
	})

	// Override ports to 0 for random assignment
	mgmt.zapPort = 0
	mgmt.httpPort = 0

	if err := mgmt.Start(); err != nil {
		ns.Shutdown()
		t.Fatalf("starting mgmt server: %v", err)
	}

	t.Cleanup(func() {
		mgmt.Stop()
		ns.Shutdown()
	})

	return ns, mgmt
}

func mgmtURL(s *Server, path string) string {
	return "http://" + s.httpLn.Addr().String() + path
}

func TestHealthEndpoint(t *testing.T) {
	_, mgmt := startTestServer(t)

	resp, err := http.Get(mgmtURL(mgmt, "/v1/pubsub/health"))
	if err != nil {
		t.Fatalf("GET /v1/pubsub/health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %v", body["status"])
	}
	if body["jetstream"] != true {
		t.Errorf("expected jetstream true, got %v", body["jetstream"])
	}
	if body["server_id"] == nil || body["server_id"] == "" {
		t.Error("expected server_id to be set")
	}
}

func TestVarzEndpoint(t *testing.T) {
	_, mgmt := startTestServer(t)

	resp, err := http.Get(mgmtURL(mgmt, "/v1/pubsub/varz"))
	if err != nil {
		t.Fatalf("GET /v1/pubsub/varz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body["server_id"] == nil {
		t.Error("expected server_id in varz")
	}
	if body["version"] == nil {
		t.Error("expected version in varz")
	}
}

func TestConnzEndpoint(t *testing.T) {
	_, mgmt := startTestServer(t)

	resp, err := http.Get(mgmtURL(mgmt, "/v1/pubsub/connz"))
	if err != nil {
		t.Fatalf("GET /v1/pubsub/connz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body["server_id"] == nil {
		t.Error("expected server_id in connz")
	}
}

func TestStreamsEndpoint(t *testing.T) {
	_, mgmt := startTestServer(t)

	// Initially no streams
	resp, err := http.Get(mgmtURL(mgmt, "/v1/pubsub/streams"))
	if err != nil {
		t.Fatalf("GET /v1/pubsub/streams: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	count, ok := body["count"].(float64)
	if !ok {
		t.Fatalf("expected count to be a number, got %T", body["count"])
	}
	if count != 0 {
		t.Errorf("expected 0 streams, got %v", count)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	_, mgmt := startTestServer(t)

	endpoints := []string{
		"/v1/pubsub/health",
		"/v1/pubsub/varz",
		"/v1/pubsub/connz",
		"/v1/pubsub/streams",
	}

	for _, ep := range endpoints {
		resp, err := http.Post(mgmtURL(mgmt, ep), "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", ep, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: expected 405, got %d", ep, resp.StatusCode)
		}
	}
}

func TestSplitSubjects(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"foo", []string{"foo"}},
		{"foo,bar", []string{"foo", "bar"}},
		{"foo, bar, baz", []string{"foo", "bar", "baz"}},
		{" foo , bar ", []string{"foo", "bar"}},
		{",", nil},
		{",,", nil},
	}

	for _, tt := range tests {
		got := splitSubjects(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitSubjects(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitSubjects(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestZAPResponseBuilders(t *testing.T) {
	// Test zapOK
	msg, err := zapOK("test message")
	if err != nil {
		t.Fatalf("zapOK: %v", err)
	}
	root := msg.Root()
	if root.Uint8(FieldStatus) != 0 {
		t.Errorf("expected status 0, got %d", root.Uint8(FieldStatus))
	}
	if root.Text(FieldMessage) != "test message" {
		t.Errorf("expected 'test message', got %q", root.Text(FieldMessage))
	}

	// Test zapError
	msg, err = zapError("bad thing")
	if err != nil {
		t.Fatalf("zapError: %v", err)
	}
	root = msg.Root()
	if root.Uint8(FieldStatus) != 1 {
		t.Errorf("expected status 1, got %d", root.Uint8(FieldStatus))
	}
	if root.Text(FieldMessage) != "bad thing" {
		t.Errorf("expected 'bad thing', got %q", root.Text(FieldMessage))
	}

	// Test zapOKWithData
	data := []byte(`{"hello":"world"}`)
	msg, err = zapOKWithData(data)
	if err != nil {
		t.Fatalf("zapOKWithData: %v", err)
	}
	root = msg.Root()
	if root.Uint8(FieldStatus) != 0 {
		t.Errorf("expected status 0, got %d", root.Uint8(FieldStatus))
	}
	gotData := root.Bytes(FieldData)
	if string(gotData) != string(data) {
		t.Errorf("expected %q, got %q", data, gotData)
	}
}
