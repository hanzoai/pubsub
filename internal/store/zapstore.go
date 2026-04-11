// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

// Package store provides a zapdb-backed key-value store for pubsub metadata,
// consensus state, and PQ-safe encrypted backup/restore.
//
// Storage stack:
//
//	PubSub streams → zapdb (AES-256 at rest) → Backup → age(ML-KEM-768+X25519) → S3/disk
//
// All data is encrypted at rest via AES-256 (zapdb EncryptionKey).
// Backups use luxfi/age HybridRecipient (ML-KEM-768 + X25519) for PQ-safe encryption.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/luxfi/age"
	badger "github.com/luxfi/zapdb"
)

// Store wraps zapdb for pubsub metadata, consensus state, and PQ-safe backup.
type Store struct {
	db   *badger.DB
	path string

	// PQ encryption for backups (nil = plaintext backups).
	ageRecipient age.Recipient // public key: encrypt backups
	ageIdentity  age.Identity  // private key: decrypt restores
}

// Config configures the zapdb store.
type Config struct {
	// Dir is the directory for zapdb data files.
	Dir string

	// EncryptionKey enables AES encryption at rest (must be 16, 24, or 32 bytes).
	// Required for non-InMemory stores unless RequireEncryption is false.
	EncryptionKey []byte

	// RequireEncryption fails Open if EncryptionKey is empty on a disk store.
	// Default true — set false only for dev/test.
	RequireEncryption *bool

	// SyncWrites ensures durability on each write (slower but safer).
	SyncWrites bool

	// InMemory runs zapdb without disk persistence (for testing).
	InMemory bool

	// PQ-safe backup encryption using luxfi/age ML-KEM-768 + X25519 (X-Wing).
	// Set AgeRecipient for encrypting backups.
	// Set AgeIdentity for decrypting restores.
	// Both nil = plaintext backups.
	AgeRecipient age.Recipient
	AgeIdentity  age.Identity
}

// Open creates or opens a zapdb store.
func Open(cfg Config) (*Store, error) {
	if cfg.Dir == "" && !cfg.InMemory {
		return nil, fmt.Errorf("zapstore: dir required when not in-memory")
	}

	requireEnc := cfg.RequireEncryption == nil || *cfg.RequireEncryption
	if !cfg.InMemory && requireEnc && len(cfg.EncryptionKey) == 0 {
		return nil, fmt.Errorf("zapstore: encryption key required for disk store (set RequireEncryption=false to disable)")
	}
	if len(cfg.EncryptionKey) > 0 {
		switch len(cfg.EncryptionKey) {
		case 16, 24, 32:
		default:
			return nil, fmt.Errorf("zapstore: encryption key must be 16, 24, or 32 bytes, got %d", len(cfg.EncryptionKey))
		}
	}

	if !cfg.InMemory {
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("zapstore: mkdir %s: %w", cfg.Dir, err)
		}
	}

	opts := badger.DefaultOptions(cfg.Dir)
	opts.SyncWrites = cfg.SyncWrites
	opts.NumVersionsToKeep = 1
	opts.Logger = nil

	if cfg.InMemory {
		opts.InMemory = true
		opts.Dir = ""
		opts.ValueDir = ""
	}

	if len(cfg.EncryptionKey) > 0 {
		opts.EncryptionKey = cfg.EncryptionKey
		opts.EncryptionKeyRotationDuration = 24 * time.Hour
	}

	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("zapstore: open: %w", err)
	}

	return &Store{
		db:           db,
		path:         cfg.Dir,
		ageRecipient: cfg.AgeRecipient,
		ageIdentity:  cfg.AgeIdentity,
	}, nil
}

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// Path returns the store directory.
func (s *Store) Path() string {
	return s.path
}

// DB returns the underlying zapdb instance for advanced operations.
func (s *Store) DB() *badger.DB {
	return s.db
}

// --- Key-Value Operations ---

func (s *Store) Get(key []byte) ([]byte, error) {
	var val []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		val, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return val, nil
}

func (s *Store) Set(key, value []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, value)
	})
}

func (s *Store) SetWithTTL(key, value []byte, ttl time.Duration) error {
	return s.db.Update(func(txn *badger.Txn) error {
		e := badger.NewEntry(key, value).WithTTL(ttl)
		return txn.SetEntry(e)
	})
}

func (s *Store) Delete(key []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
}

// --- JSON Convenience ---

func (s *Store) GetJSON(key []byte, v any) error {
	data, err := s.Get(key)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (s *Store) SetJSON(key []byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Set(key, data)
}

// --- Prefix Scan ---

func (s *Store) Scan(prefix []byte, fn func(key, value []byte) bool) error {
	return s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			k := item.KeyCopy(nil)
			err := item.Value(func(v []byte) error {
				if !fn(k, v) {
					return fmt.Errorf("stop")
				}
				return nil
			})
			if err != nil {
				if err.Error() == "stop" {
					return nil
				}
				return err
			}
		}
		return nil
	})
}

// --- Batch Operations ---

func (s *Store) WriteBatch() *badger.WriteBatch {
	return s.db.NewWriteBatch()
}

// --- PQ-Safe Backup & Restore ---
//
// Backups are encrypted with luxfi/age ML-KEM-768+X25519 (X-Wing hybrid)
// when AgeRecipient is configured. This provides post-quantum safe encryption
// for backup data at rest (S3, disk, etc).

// Backup writes an incremental backup to the given directory.
// If AgeRecipient is set, the backup is PQ-encrypted with age.
// Pass 0 for since to get a full backup.
func (s *Store) Backup(dir string, since uint64) (uint64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("zapstore: backup mkdir: %w", err)
	}

	ext := ".zap"
	if s.ageRecipient != nil {
		ext = ".zap.age"
	}
	name := fmt.Sprintf("pubsub-%d%s", time.Now().Unix(), ext)
	path := filepath.Join(dir, name)

	// Backup to buffer first
	var buf bytes.Buffer
	version, err := s.db.Backup(&buf, since)
	if err != nil {
		return 0, fmt.Errorf("zapstore: backup: %w", err)
	}
	if buf.Len() == 0 {
		return version, nil
	}

	// Encrypt with age if recipient is configured (PQ-safe: ML-KEM-768+X25519)
	var body io.Reader = &buf
	if s.ageRecipient != nil {
		encrypted, err := ageEncrypt(&buf, s.ageRecipient)
		if err != nil {
			return 0, fmt.Errorf("zapstore: age encrypt: %w", err)
		}
		body = encrypted
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("zapstore: backup create: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, body); err != nil {
		return 0, fmt.Errorf("zapstore: backup write: %w", err)
	}

	return version, nil
}

// Restore loads a backup from a file.
// If AgeIdentity is set, the backup is decrypted with age first.
func (s *Store) Restore(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("zapstore: restore open: %w", err)
	}
	defer f.Close()

	var reader io.Reader = f
	if s.ageIdentity != nil && filepath.Ext(path) == ".age" {
		dec, err := age.Decrypt(f, s.ageIdentity)
		if err != nil {
			return fmt.Errorf("zapstore: age decrypt: %w", err)
		}
		reader = dec
	}

	return s.db.Load(reader, 256)
}

func ageEncrypt(plaintext io.Reader, recipient age.Recipient) (*bytes.Buffer, error) {
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipient)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(w, plaintext); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Maintenance ---

func (s *Store) RunGC(discardRatio float64) error {
	return s.db.RunValueLogGC(discardRatio)
}
