//go:build unix

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

package hub

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// publishForTest publishes a fresh client's state.
func publishForTest(t *testing.T, jti string) {
	t.Helper()
	now := time.Now()
	c := stateTestClient(t, "http://127.0.0.1:1", stateToken(t, "agent-1", jti, now, now.Add(time.Hour)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if !PublishCredentialState(ctx, c) {
		t.Fatal("PublishCredentialState returned false")
	}
}

// writeOnceForTest runs one write of a fresh client's state, without
// the writer goroutine PublishCredentialState starts (a failed write would
// be retried there while the test restores the seam), and reports whether
// the file now holds the state.
func writeOnceForTest(t *testing.T, jti string) bool {
	t.Helper()
	now := time.Now()
	c := stateTestClient(t, "http://127.0.0.1:1", stateToken(t, "agent-1", jti, now, now.Add(time.Hour)))
	return (&credentialStateWriter{client: c}).writeLatest()
}

// assertEmptyDir fails unless dir exists and has no entries.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s has %d entries, want none", dir, len(entries))
	}
}

// assertNotPublished fails if the state reads as OK.
func assertNotPublished(t *testing.T) {
	t.Helper()
	if _, status, _ := ReadCredentialState(); status == CredentialStateOK {
		t.Fatal("state published in an unsafe layout")
	}
}

// A run/scion directory with a mode the checks refuse (and, when the test
// runs as root, owned by another uid), as some runtimes create it, does not
// stop the publish: the state directory is not below it. The state
// directory comes from the production value, read before the seam
// overrides it, so moving it back under run/scion fails this test.
func TestPublishCredentialState_WorldWritableScionDirDoesNotBlock(t *testing.T) {
	prodDir := credentialStateDir
	for _, foreign := range []bool{false, true} {
		name := "own uid"
		if foreign {
			name = "other uid"
		}
		t.Run(name, func(t *testing.T) {
			if foreign && os.Geteuid() != 0 {
				t.Skip("needs root to give the directory to another uid")
			}
			base := t.TempDir()
			scionDir := filepath.Join(base, "run", "scion")
			if err := os.MkdirAll(scionDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(scionDir, 0o777); err != nil {
				t.Fatal(err)
			}
			if foreign {
				if err := os.Chown(scionDir, 65534, 65534); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(SetCredentialStateDirForTest(filepath.Join(base, prodDir), base))

			publishForTest(t, "jti-t1")
			st, status, reason := ReadCredentialState()
			if status != CredentialStateOK {
				t.Fatalf("status=%s reason=%s", status, reason)
			}
			if st.Instance == "" || st.Credential.JTI != "jti-t1" {
				t.Fatalf("published state %+v is not this run's", st)
			}
			assertEmptyDir(t, scionDir)
		})
	}
}

// The state directory init creates is 0755 and owned by the owner uid
// whatever the umask, so the agent user can read the state.
func TestPublishCredentialState_CreatedDirModeIgnoresUmask(t *testing.T) {
	dir := useStateDir(t)
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	publishForTest(t, "jti-t2")
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("state directory mode %v, want a 0755 directory", fi.Mode())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != credentialStateOwner {
		t.Fatalf("state directory owner is not uid %d", credentialStateOwner)
	}
	ffi, err := os.Lstat(filepath.Join(dir, credentialStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ffi.Mode().Perm() != 0o644 {
		t.Fatalf("state file mode %v, want 0644", ffi.Mode())
	}
	if _, status, reason := ReadCredentialState(); status != CredentialStateOK {
		t.Fatalf("status=%s reason=%s", status, reason)
	}
}

// The writer refuses a state directory, or a directory above it, that
// another user could control, and writes nothing there.
func TestPublishCredentialState_RefusesUnsafeStateDir(t *testing.T) {
	t.Run("world-writable state directory", func(t *testing.T) {
		dir := useStateDir(t)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if writeOnceForTest(t, "jti-t3a") {
			t.Fatal("write succeeded in an unsafe layout")
		}
		assertEmptyDir(t, dir)
		assertNotPublished(t)
	})
	t.Run("state directory is a symlink", func(t *testing.T) {
		dir := useStateDir(t)
		target := filepath.Join(filepath.Dir(dir), "elsewhere")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
		if writeOnceForTest(t, "jti-t3b") {
			t.Fatal("write succeeded in an unsafe layout")
		}
		assertEmptyDir(t, target)
		assertNotPublished(t)
	})
	t.Run("group-writable parent", func(t *testing.T) {
		dir := useStateDir(t)
		if err := os.Chmod(filepath.Dir(dir), 0o775); err != nil {
			t.Fatal(err)
		}
		if writeOnceForTest(t, "jti-t3c") {
			t.Fatal("write succeeded in an unsafe layout")
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("state directory created below a group-writable parent: %v", err)
		}
		assertNotPublished(t)
	})
	t.Run("other owner", func(t *testing.T) {
		dir := useStateDir(t)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The directories are the test uid's; requiring another owner
		// makes them foreign. (PublishCredentialState itself would not
		// run at all: it only runs as the owner uid.)
		credentialStateOwner = os.Getuid() + 1
		if writeOnceForTest(t, "jti-t3d") {
			t.Fatal("write succeeded in directories not owned by the owner uid")
		}
		assertEmptyDir(t, dir)
	})
}
