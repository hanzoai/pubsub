// Copyright 2026 Hanzo AI Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package embed

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// generation is the file in StoreDir that names the incarnation the store
// belongs to.
const generation = "generation"

// incarnation returns the generation of the store in dir: the id written there
// at first boot, or a random one it writes now.
//
// The generation is the other half of a raft identity. A peer's identity is
// its server name, and a peer that comes back under a name it held before,
// without the log it held under it, answers for votes and acknowledgements it
// no longer remembers. The log and the name must therefore live and die
// together: both are in the store, so a store that survives keeps its name and
// a store that is gone yields a new one.
//
// want, when set, is a generation the host chose. A store that already has a
// generation must be that one: the store's log belongs to it, and renaming a
// log is the one thing this file exists to prevent.
func incarnation(dir, want string) (string, error) {
	if want != "" && !token(want) {
		return "", fmt.Errorf("embed: generation %q is not a plain name", want)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("embed: store dir: %w", err)
	}
	path := filepath.Join(dir, generation)
	raw, err := os.ReadFile(path)
	if err == nil {
		have := strings.TrimSpace(string(raw))
		if !token(have) {
			return "", fmt.Errorf("embed: %s holds %q, which is not a generation", path, have)
		}
		if want != "" && want != have {
			return "", fmt.Errorf("embed: store dir belongs to generation %q, not %q", have, want)
		}
		return have, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("embed: generation: %w", err)
	}

	id := want
	if id == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("embed: generation: %w", err)
		}
		id = hex.EncodeToString(b[:])
	}
	if err := write(dir, path, []byte(id+"\n")); err != nil {
		return "", fmt.Errorf("embed: generation: %w", err)
	}
	return id, nil
}

// write puts data at path so that a crash leaves the old file or the new one,
// never part of either.
func write(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, generation+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
