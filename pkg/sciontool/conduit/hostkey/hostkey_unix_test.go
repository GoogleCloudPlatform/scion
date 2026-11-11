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

package hostkey

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/sshd"
)

// TestNewKeyPerLaunch: a new launch id gets a new key; the same launch id
// gets the stored key back.
func TestNewKeyPerLaunch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conduit")
	a, err := LoadOrCreate(dir, "launch-1")
	if err != nil || !a.Persisted {
		t.Fatalf("launch-1: %+v, %v", a, err)
	}
	again, err := LoadOrCreate(dir, "launch-1")
	if err != nil || !again.Persisted {
		t.Fatalf("launch-1 again: %+v, %v", again, err)
	}
	if sshd.AuthorizedKey(a.Signer) != sshd.AuthorizedKey(again.Signer) {
		t.Fatal("the same launch id got a different key")
	}
	b, err := LoadOrCreate(dir, "launch-2")
	if err != nil || !b.Persisted {
		t.Fatalf("launch-2: %+v, %v", b, err)
	}
	if sshd.AuthorizedKey(a.Signer) == sshd.AuthorizedKey(b.Signer) {
		t.Fatal("a new launch id kept the old key")
	}
	// Back to launch-1: the stored key is launch-2's, so a new one.
	c, err := LoadOrCreate(dir, "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	if sshd.AuthorizedKey(c.Signer) == sshd.AuthorizedKey(b.Signer) {
		t.Fatal("launch-1 got launch-2's key")
	}
}

// TestFileModesAndOwner: the key file is 0600 and the directory 0700,
// both owned by this process's user (root in an agent container).
func TestFileModesAndOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conduit")
	if _, err := LoadOrCreate(dir, "launch-1"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, keyFile): 0o600, filepath.Join(dir, launchFile): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode %o, want %o", filepath.Base(path), got, want)
		}
		if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != os.Geteuid() {
			t.Errorf("%s owner %d, want %d", filepath.Base(path), st.Uid, os.Geteuid())
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Errorf("directory holds %d entries, want the key and the launch file only", len(ents))
	}
}

// TestInMemoryFallback: no launch id, or a directory that cannot be
// written, gives a usable key held in memory.
func TestInMemoryFallback(t *testing.T) {
	r, err := LoadOrCreate(filepath.Join(t.TempDir(), "conduit"), "")
	if err != nil || r.Persisted || r.Signer == nil || r.Reason == "" {
		t.Fatalf("no launch id: %+v, %v", r, err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = LoadOrCreate(filepath.Join(blocker, "conduit"), "launch-1")
	if err != nil || r.Persisted || r.Signer == nil || r.Reason == "" {
		t.Fatalf("unwritable dir: %+v, %v", r, err)
	}
	if strings.Contains(r.Reason, blocker) {
		t.Fatalf("reason %q names the path", r.Reason)
	}
}

// TestAuthorizedKeyIsPublicOnly: the reported line is a single public key
// line and carries no private key material.
func TestAuthorizedKeyIsPublicOnly(t *testing.T) {
	r, err := LoadOrCreate(filepath.Join(t.TempDir(), "conduit"), "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	line := sshd.AuthorizedKey(r.Signer)
	if !strings.HasPrefix(line, "ssh-ed25519 ") || strings.ContainsAny(line, "\n") || strings.Contains(line, "PRIVATE") {
		t.Fatalf("AuthorizedKey = %q, want one public ssh-ed25519 line", line)
	}
}

// TestReuseRequiresPrivateKeyFile: a stored key is reused only from a
// regular file of this user with no group or other permission bits, and
// reuse sets the directory back to 0700. Otherwise a new key is
// generated and stored.
func TestReuseRequiresPrivateKeyFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conduit")
	keyPath := filepath.Join(dir, keyFile)
	first, err := LoadOrCreate(dir, "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	orig := sshd.AuthorizedKey(first.Signer)

	// Directory loosened: the key is reused and the directory is fixed.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreate(dir, "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	if sshd.AuthorizedKey(again.Signer) != orig {
		t.Fatal("key not reused")
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o after reuse, want 700", fi.Mode().Perm())
	}

	// Key file readable by group: a new key replaces it.
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	loose, err := LoadOrCreate(dir, "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	if sshd.AuthorizedKey(loose.Signer) == orig || !loose.Persisted {
		t.Fatal("a group-readable key file was reused")
	}
	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("new key file mode %o, want 600", fi.Mode().Perm())
	}
	current := sshd.AuthorizedKey(loose.Signer)

	// Key file replaced by a link to a private file: not reused.
	target := filepath.Join(t.TempDir(), "elsewhere")
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, keyPath); err != nil {
		t.Fatal(err)
	}
	linked, err := LoadOrCreate(dir, "launch-1")
	if err != nil {
		t.Fatal(err)
	}
	if sshd.AuthorizedKey(linked.Signer) == current {
		t.Fatal("a key file behind a link was reused")
	}
	if fi, err := os.Lstat(keyPath); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("key file after regeneration: %v, %v", fi, err)
	}
}
