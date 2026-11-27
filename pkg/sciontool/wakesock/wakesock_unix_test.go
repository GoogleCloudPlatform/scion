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

//go:build unix

package wakesock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListenAndNotify_DeliversBytesAndEnforcesPermissions(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("/proc/self/fd unavailable on this host")
	}
	dir, err := os.MkdirTemp("", "ws-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, ".scion", "wake.sock")
	got := make(chan byte, 4)
	closer, err := Listen(sockPath, Options{}, func(b byte) {
		got <- b
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// Verify socket exists with 0600 permissions.
	fi, err := os.Lstat(sockPath)
	if err != nil {
		_ = closer.Close()
		t.Fatalf("lstat socket: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		_ = closer.Close()
		t.Fatalf("socket mode = %#o, want 0600", fi.Mode().Perm())
	}

	// Deliver distinct 1-byte triggers.
	for _, want := range []byte{'s', 'm'} {
		Notify(sockPath, want)
		select {
		case b := <-got:
			if b != want {
				_ = closer.Close()
				t.Fatalf("got byte %q, want %q", b, want)
			}
		case <-time.After(2 * time.Second):
			_ = closer.Close()
			t.Fatalf("timed out waiting for byte %q", want)
		}
	}

	// Close must unlink the socket, and Notify on a closed socket must be a safe no-op.
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("expected socket to be unlinked after Close, got err=%v", err)
	}
	Notify(sockPath, 's')
}

func TestListen_RefusesSymlinkParentDir(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("/proc/self/fd unavailable on this host")
	}
	dir, err := os.MkdirTemp("", "wsl-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	target := t.TempDir()
	symlinkParent := filepath.Join(dir, ".scion")
	if err := os.Symlink(target, symlinkParent); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	closer, err := Listen(filepath.Join(symlinkParent, "wake.sock"), Options{}, nil)
	if closer != nil {
		_ = closer.Close()
	}
	if err == nil {
		t.Fatalf("expected Listen to refuse symlink parent directory")
	}
}
