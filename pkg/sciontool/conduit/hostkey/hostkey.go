// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package hostkey keeps the conduit SSH server's host key for one agent
// launch (one incarnation): a new launch id gets a new key, and the same
// launch id (a container restart) keeps its key. The private key lives in
// a root-owned file outside the agent home and is never logged or sent;
// only its public half is reported to the hub.
package hostkey

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// DefaultDir holds the host key files.
const DefaultDir = "/var/lib/scion/conduit"

// File names inside the directory.
const (
	keyFile    = "ssh_host_ed25519_key"
	launchFile = "ssh_host_key.launch"
)

// Result is a loaded or generated host key.
type Result struct {
	Signer ssh.Signer
	// Persisted is false when the key is held in memory only (no launch
	// id, or the files could not be written); Reason says why.
	Persisted bool
	Reason    string
}

// LoadOrCreate returns the host key for launchID from dir. It reuses the
// stored key when the stored launch id equals launchID and the key
// parses; otherwise it generates a new ed25519 key and stores it (key
// file mode 0600, directory 0700, both owned by this process's user).
// When launchID is empty or the files cannot be read or written, it
// returns an in-memory key with Persisted false; only a failure to
// generate a key is an error.
func LoadOrCreate(dir, launchID string) (Result, error) {
	if launchID != "" {
		if s, err := load(dir, launchID); err == nil {
			return Result{Signer: s, Persisted: true}, nil
		}
	}
	priv, s, err := generate()
	if err != nil {
		return Result{}, err
	}
	if launchID == "" {
		return Result{Signer: s, Reason: "no launch id"}, nil
	}
	if err := store(dir, launchID, priv); err != nil {
		reason := "host key file could not be written"
		if errors.Is(err, os.ErrPermission) {
			reason = "no permission to write the host key file"
		}
		return Result{Signer: s, Reason: reason}, nil
	}
	return Result{Signer: s, Persisted: true}, nil
}

// load reads the stored key when it belongs to launchID and the key file
// is a regular file owned by this process's user with no group or other
// permission bits. On reuse the directory is set back to mode 0700.
func load(dir, launchID string) (ssh.Signer, error) {
	got, err := os.ReadFile(filepath.Join(dir, launchFile))
	if err != nil || strings.TrimSpace(string(got)) != launchID {
		return nil, errors.New("no key for this launch")
	}
	path := filepath.Join(dir, keyFile)
	if err := checkKeyFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

// generate creates an ed25519 key.
func generate() (ed25519.PrivateKey, ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("hostkey: generate: %w", err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("hostkey: signer: %w", err)
	}
	return priv, s, nil
}

// store writes the key, then the launch id, each through a temporary file
// renamed into place, so a reader never sees a partial file or a launch
// id without its key.
func store(dir, launchID string, priv ed25519.PrivateKey) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("hostkey: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("hostkey: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return fmt.Errorf("hostkey: %w", err)
	}
	if err := writeFile(dir, keyFile, pem.EncodeToMemory(block)); err != nil {
		return err
	}
	return writeFile(dir, launchFile, []byte(launchID+"\n"))
}

// writeFile writes data to dir/name with mode 0600 via a new temporary
// file and a rename.
func writeFile(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("hostkey: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("hostkey: %w", err)
	}
	if _, err := bytes.NewReader(data).WriteTo(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("hostkey: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("hostkey: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("hostkey: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("hostkey: %w", err)
	}
	return nil
}
