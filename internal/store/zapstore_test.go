// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package store

import (
	"path/filepath"
	"testing"

	"github.com/luxfi/age"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Config{InMemory: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenClose(t *testing.T) {
	s := openTestStore(t)
	_ = s.Path()
}

func TestSetGet(t *testing.T) {
	s := openTestStore(t)

	if err := s.Set([]byte("key1"), []byte("value1")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, err := s.Get([]byte("key1"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(val) != "value1" {
		t.Errorf("got %q, want %q", val, "value1")
	}
}

func TestGetMissing(t *testing.T) {
	s := openTestStore(t)

	_, err := s.Get([]byte("missing"))
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestDelete(t *testing.T) {
	s := openTestStore(t)

	s.Set([]byte("k"), []byte("v"))
	if err := s.Delete([]byte("k")); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := s.Get([]byte("k"))
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestJSON(t *testing.T) {
	s := openTestStore(t)

	type data struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	input := data{Name: "test", Count: 42}
	if err := s.SetJSON([]byte("json-key"), input); err != nil {
		t.Fatalf("SetJSON: %v", err)
	}

	var output data
	if err := s.GetJSON([]byte("json-key"), &output); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}

	if output.Name != "test" || output.Count != 42 {
		t.Errorf("got %+v, want %+v", output, input)
	}
}

func TestScan(t *testing.T) {
	s := openTestStore(t)

	s.Set([]byte("stream:a"), []byte("1"))
	s.Set([]byte("stream:b"), []byte("2"))
	s.Set([]byte("other:c"), []byte("3"))

	var keys []string
	err := s.Scan([]byte("stream:"), func(key, value []byte) bool {
		keys = append(keys, string(key))
		return true
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(keys) != 2 {
		t.Errorf("expected 2 keys with prefix stream:, got %d: %v", len(keys), keys)
	}
}

func TestOpenDiskStoreRequiresEncryption(t *testing.T) {
	dir := t.TempDir()
	_, err := Open(Config{Dir: dir})
	if err == nil {
		t.Fatal("expected error for disk store without encryption key")
	}
}

func TestOpenDiskStore(t *testing.T) {
	dir := t.TempDir()
	noEnc := false
	s, err := Open(Config{Dir: dir, SyncWrites: false, RequireEncryption: &noEnc})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	s.Set([]byte("persist"), []byte("data"))
	s.Close()

	// Reopen and verify
	s2, err := Open(Config{Dir: dir, RequireEncryption: &noEnc})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer s2.Close()

	val, err := s2.Get([]byte("persist"))
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(val) != "data" {
		t.Errorf("got %q, want %q", val, "data")
	}
}

func TestPQEncryptedBackupRestore(t *testing.T) {
	// Generate PQ identity (ML-KEM-768 + X25519 hybrid)
	identity, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatalf("GenerateHybridIdentity: %v", err)
	}
	recipient := identity.Recipient()

	// Create source store with PQ backup encryption
	srcDir := t.TempDir()
	noEnc := false
	src, err := Open(Config{
		Dir:               srcDir,
		RequireEncryption: &noEnc,
		AgeRecipient:      recipient,
	})
	if err != nil {
		t.Fatalf("Open src: %v", err)
	}

	// Write test data
	src.Set([]byte("pq-key-1"), []byte("quantum-safe-value"))
	src.Set([]byte("pq-key-2"), []byte("post-quantum"))

	// Backup (PQ-encrypted with age ML-KEM-768+X25519)
	backupDir := t.TempDir()
	_, err = src.Backup(backupDir, 0)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	src.Close()

	// Find the backup file (should be .zap.age)
	matches, _ := filepath.Glob(filepath.Join(backupDir, "*.zap.age"))
	if len(matches) == 0 {
		t.Fatal("expected .zap.age backup file")
	}

	// Create destination store with PQ decryption identity
	dstDir := t.TempDir()
	dst, err := Open(Config{
		Dir:               dstDir,
		RequireEncryption: &noEnc,
		AgeIdentity:       identity,
	})
	if err != nil {
		t.Fatalf("Open dst: %v", err)
	}
	defer dst.Close()

	// Restore from PQ-encrypted backup
	if err := dst.Restore(matches[0]); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Verify data survived PQ encrypt/decrypt roundtrip
	val, err := dst.Get([]byte("pq-key-1"))
	if err != nil {
		t.Fatalf("Get after restore: %v", err)
	}
	if string(val) != "quantum-safe-value" {
		t.Errorf("got %q, want %q", val, "quantum-safe-value")
	}

	val2, err := dst.Get([]byte("pq-key-2"))
	if err != nil {
		t.Fatalf("Get pq-key-2: %v", err)
	}
	if string(val2) != "post-quantum" {
		t.Errorf("got %q, want %q", val2, "post-quantum")
	}
}
