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

package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

func TestCreate_PrivateModes(t *testing.T) {
	tr, dir := openTestTree(t)
	if err := tr.MkdirAll("d", Private); err != nil {
		t.Fatal(err)
	}
	f, err := tr.Create("d/x", true, Private)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for p, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "d"): 0o700, filepath.Join(dir, "d", "x"): 0o600} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v; want %04o", p, fi.Mode().Perm(), err, want)
		}
	}
	if Describe(Private) != "mode 0600" {
		t.Errorf("Describe(Private) = %q", Describe(Private))
	}
}

func TestReadPrivate_RefusesGroupReadable(t *testing.T) {
	tr, dir := openTestTree(t)
	if err := tr.WriteFile("x", []byte("secret"), Private); err != nil {
		t.Fatal(err)
	}
	// Control: the private file reads.
	if _, err := tr.ReadPrivate("x", 0); err != nil {
		t.Fatalf("ReadPrivate of a 0600 file: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ReadPrivate("x", 0); !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), "group- or world-accessible") {
		t.Fatalf("ReadPrivate of a 0640 file: %v, want ErrNotPrivate", err)
	}
}

func TestTree_RefusesSymlinkLeafUnix(t *testing.T) {
	tr, dir := openTestTree(t)
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.MustSymlink(t, outside, filepath.Join(dir, "link"))
	if _, err := tr.ReadPrivate("link", 0); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("ReadPrivate of a symlink: %v, want ErrReparsePoint", err)
	}
	if _, err := tr.Create("link", false, Private); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("Create through a symlink: %v, want ErrReparsePoint", err)
	}
	// Control: a regular file next to it reads.
	if err := tr.WriteFile("real", []byte("x"), Private); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ReadPrivate("real", 0); err != nil {
		t.Fatalf("ReadPrivate of a regular file: %v", err)
	}
}

func TestOpenPrivate_RepairOnWriteRefuseOnRead(t *testing.T) {
	tr, dir := openTestTree(t)
	path := filepath.Join(dir, "x")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.OpenPrivate("x", os.O_RDONLY); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("read-only OpenPrivate of a 0644 file: %v, want ErrNotPrivate", err)
	}
	f, err := tr.OpenPrivate("x", os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenPrivate for writing of an owned 0644 file: %v", err)
	}
	_ = f.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after repair = %04o, want 0600", fi.Mode().Perm())
	}
}

// fakeFileInfo is a stat result with a chosen mode and owner.
type fakeFileInfo struct {
	mode os.FileMode
	uid  uint32
}

func (f fakeFileInfo) Name() string       { return "x" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

// Moved from pkg/tunnelclient (TestCheckPrivateRefusesOtherOwner) with the
// predicate it tested.
func TestCheckPrivate_RefusesOtherOwnerUnix(t *testing.T) {
	me := uint32(os.Getuid())
	if err := checkInfo("f", fakeFileInfo{mode: 0o600, uid: me}); err != nil {
		t.Fatalf("own 0600 file refused: %v", err)
	}
	err := checkInfo("f", fakeFileInfo{mode: 0o600, uid: me + 1})
	if err == nil || !strings.Contains(err.Error(), "owned by another user") {
		t.Fatalf("file owned by another user: %v, want a refusal", err)
	}
	if err := checkInfo("f", fakeFileInfo{mode: 0o604, uid: me}); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("world-readable file: %v, want ErrNotPrivate", err)
	}
}

// A FIFO planted in a tree is refused, never opened in a blocking way
// (a refusal must not become a hang).
func TestOpenPrivate_RefusesFIFO(t *testing.T) {
	tr, dir := openTestTree(t)
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := tr.ReadPrivate("fifo", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("ReadPrivate of a FIFO: %v, want a refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReadPrivate of a FIFO blocked")
	}
	if _, err := tr.OpenPrivate("fifo", os.O_RDWR); err == nil {
		t.Fatal("OpenPrivate(O_RDWR) of a FIFO succeeded")
	}
	// Control: a regular private file next to it reads.
	if err := tr.WriteFile("real", []byte("x"), Private); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ReadPrivate("real", 0); err != nil {
		t.Fatalf("control: %v", err)
	}
}

func TestRenameOpen_RefusesNonPrivateHandleUnix(t *testing.T) {
	tr, dir := openTestTree(t)
	f, err := tr.Create("open.tmp", true, Inherit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o644); err != nil {
		t.Fatal(err)
	}
	if err := tr.RenameOpen(f, "target"); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("RenameOpen of a 0644 file: %v, want ErrNotPrivate", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); !os.IsNotExist(err) {
		t.Fatalf("the refused file was renamed: %v", err)
	}
	// Control: a Private temporary file renames.
	p, _, err := tr.CreateTemp("", ".t.*.tmp", Private)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if err := tr.RenameOpen(p, "target"); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// CheckPrivate, OwnedByCurrentUser and CheckNoForeignWriters compare the
// owner with one uid source (currentUID, the real uid), so they agree on
// every file: with that uid all three accept the user's own 0600 file, and
// with another uid all three refuse it, and a write open of an owned 0644
// file refuses instead of repairing.
func TestOwnerChecks_OneUIDSource(t *testing.T) {
	if got, want := currentUID(), os.Getuid(); got != want {
		t.Fatalf("currentUID() = %d, want the real uid %d", got, want)
	}
	tr, dir := openTestTree(t)
	own := filepath.Join(dir, "own")
	if err := os.WriteFile(own, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(own)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if err := CheckPrivate(f); err != nil {
		t.Errorf("CheckPrivate of the user's own 0600 file: %v", err)
	}
	if owned, err := OwnedByCurrentUser(f); err != nil || !owned {
		t.Errorf("OwnedByCurrentUser of the user's own file = %v, %v; want true", owned, err)
	}
	if err := CheckNoForeignWriters(f); err != nil {
		t.Errorf("CheckNoForeignWriters of the user's own 0600 file: %v", err)
	}

	saved := currentUID
	t.Cleanup(func() { currentUID = saved })
	currentUID = func() int { return os.Getuid() + 1 }

	if err := CheckPrivate(f); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("CheckPrivate with another uid: %v, want ErrNotPrivate", err)
	}
	if owned, err := OwnedByCurrentUser(f); err != nil || owned {
		t.Errorf("OwnedByCurrentUser with another uid = %v, %v; want false", owned, err)
	}
	if err := CheckNoForeignWriters(f); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("CheckNoForeignWriters with another uid: %v, want ErrNotPrivate", err)
	}
	if w, err := tr.OpenPrivate("open", os.O_WRONLY); err == nil {
		_ = w.Close()
		t.Errorf("OpenPrivate for writing of a 0644 file owned by another uid succeeded; want a refusal")
	} else if !errors.Is(err, ErrNotPrivate) {
		t.Errorf("OpenPrivate for writing with another uid: %v, want ErrNotPrivate", err)
	}
	fi, err := os.Stat(open)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode after the refused write open = %04o, want 0644 (not repaired)", fi.Mode().Perm())
	}
}
