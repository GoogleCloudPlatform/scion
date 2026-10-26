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

//go:build windows

package privatefs

// Every negative test has its positive control in the same test, so a
// refusal cannot pass because the operation was broken for an unrelated
// reason.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

// treeAt opens a Tree on an existing directory.
func treeAt(t *testing.T, dir string) *Tree {
	t.Helper()
	tr, err := OpenTree(dir, TreeOptions{})
	if err != nil {
		t.Fatalf("OpenTree(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func mustCreate(t *testing.T, tr *Tree, rel string, p Protection, data string) string {
	t.Helper()
	f, err := tr.Create(rel, true, p)
	if err != nil {
		t.Fatalf("Create(%s): %v", rel, err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(tr.Anchor(), filepath.FromSlash(rel))
}

// checkPath runs CheckPrivate on a fresh handle of path.
func checkPath(t *testing.T, path string) error {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	return CheckPrivate(f)
}

func privateDACL(user string) string {
	return fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;%s)", user, wintest.SIDSystem)
}

func assertPrivateSD(t *testing.T, path string) {
	t.Helper()
	user := wintest.UserSID(t)
	sd := wintest.ReadSD(t, path)
	if sd.Owner != user {
		t.Fatalf("%s: owner %s, want the user %s", path, sd.Owner, user)
	}
	if !sd.Protected {
		t.Fatalf("%s: DACL not protected", path)
	}
	if len(sd.ACEs) != 2 {
		t.Fatalf("%s: ACEs %+v, want exactly {user FA, SYSTEM FA}", path, sd.ACEs)
	}
	seen := map[string]bool{}
	for _, a := range sd.ACEs {
		if a.Type != windows.ACCESS_ALLOWED_ACE_TYPE || a.Mask != wintest.FileAllAccess {
			t.Fatalf("%s: ACE %+v, want an allow ACE with FILE_ALL_ACCESS", path, a)
		}
		seen[a.SID] = true
	}
	if !seen[user] || !seen[wintest.SIDSystem] {
		t.Fatalf("%s: ACEs %+v, want the user and SYSTEM", path, sd.ACEs)
	}
}

// TestCreate_PrivateSD: a Private file's DACL is exactly {user FA, SYSTEM
// FA}, protected, owner = user, although the parent grants Users read
// inheritably. Control: an Inherit file in the same directory does carry
// the Users ACE, so the seeded ACE really is inheritable.
func TestCreate_PrivateSD(t *testing.T) {
	dir := t.TempDir()
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	tr := treeAt(t, dir)

	priv := mustCreate(t, tr, "private", Private, "x")
	assertPrivateSD(t, priv)
	if err := checkPath(t, priv); err != nil {
		t.Fatalf("CheckPrivate of a new Private file: %v", err)
	}

	if err := tr.MkdirAll("pdir", Private); err != nil {
		t.Fatal(err)
	}
	if sd := wintest.ReadSD(t, filepath.Join(dir, "pdir")); !sd.Protected || sd.HasAllowACE(wintest.SIDUsers) || len(sd.ACEs) != 2 {
		t.Fatalf("Private directory SD = %+v; want protected {user, SYSTEM}", sd)
	}
	// A file created with Inherit inside the private directory inherits
	// only the private ACEs.
	inner := mustCreate(t, tr, "pdir/inner", Inherit, "x")
	if err := checkPath(t, inner); err != nil {
		t.Fatalf("CheckPrivate of a file inheriting from a Private directory: %v", err)
	}

	control := mustCreate(t, tr, "inherit", Inherit, "x")
	if sd := wintest.ReadSD(t, control); sd.Protected || !sd.HasAllowACE(wintest.SIDUsers) {
		t.Fatalf("control: Inherit file SD = %+v; want the inherited Users ACE", sd)
	}
	if err := checkPath(t, control); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("control: CheckPrivate of the Inherit file: %v, want ErrNotPrivate", err)
	}
	if Describe(Private) != "owner-only ACL" {
		t.Errorf("Describe(Private) = %q", Describe(Private))
	}
}

// TestCreate_OwnerExplicitWhenElevated: under an elevated administrator
// token the default owner of new objects is usually Administrators; a
// Private file's owner must still be the user.
//
// The hosted runner's own token defaults to the user as owner (it runs as
// the built-in RID-500 account), which would make the check vacuous. So
// the test sets up the elevated condition itself: it creates the files on a
// thread impersonating a copy of the process token whose default owner is
// Administrators, and first asserts that the default owner really is
// Administrators there, so it cannot pass vacuously.
func TestCreate_OwnerExplicitWhenElevated(t *testing.T) {
	dir := t.TempDir()
	tr := treeAt(t, dir)
	user := wintest.UserSID(t)
	plain := filepath.Join(dir, "plain")
	var priv string
	wintest.WithDefaultOwner(t, wintest.SIDAdministrators, func() {
		if got := wintest.TokenOwnerSID(t); got != wintest.SIDAdministrators {
			t.Fatalf("the impersonation token's default owner is %s, not Administrators (%s): the test would be vacuous", got, wintest.SIDAdministrators)
		}
		priv = mustCreate(t, tr, "private", Private, "x")
		// Control: a file created without an explicit owner gets the
		// token's default owner, Administrators.
		if err := os.WriteFile(plain, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if owner := wintest.ReadSD(t, priv).Owner; owner != user {
		t.Fatalf("Private file owner = %s, want the user %s", owner, user)
	}
	if owner := wintest.ReadSD(t, plain).Owner; owner != wintest.SIDAdministrators {
		t.Fatalf("control: plain file owner = %s, want Administrators", owner)
	}
	// The Private directory descriptor sets the owner explicitly too.
	wintest.WithDefaultOwner(t, wintest.SIDAdministrators, func() {
		if err := tr.MkdirAll("pdir", Private); err != nil {
			t.Fatal(err)
		}
	})
	if owner := wintest.ReadSD(t, filepath.Join(dir, "pdir")).Owner; owner != user {
		t.Fatalf("Private directory owner = %s, want the user %s", owner, user)
	}
}

// refusedThenAccepted sets bad on path and expects CheckPrivate to refuse
// it naming want; then sets the private DACL (the control) and expects it
// to pass.
func refusedThenAccepted(t *testing.T, path, bad, want string) {
	t.Helper()
	wintest.SetDACL(t, path, bad)
	err := checkPath(t, path)
	if !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckPrivate with %q: %v; want ErrNotPrivate naming %q", bad, err, want)
	}
	if !strings.Contains(err.Error(), filepath.Base(path)) {
		t.Fatalf("refusal %q does not name the path", err)
	}
	wintest.SetDACL(t, path, privateDACL(wintest.UserSID(t)))
	if err := checkPath(t, path); err != nil {
		t.Fatalf("control: CheckPrivate with the private DACL: %v", err)
	}
}

func TestCheckPrivate_RefusesUsersACE(t *testing.T) {
	tr := treeAt(t, t.TempDir())
	p := mustCreate(t, tr, "x", Private, "x")
	u := wintest.UserSID(t)
	refusedThenAccepted(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDUsers), wintest.SIDUsers)
}

func TestCheckPrivate_RefusesEveryoneACE(t *testing.T) {
	tr := treeAt(t, t.TempDir())
	p := mustCreate(t, tr, "x", Private, "x")
	u := wintest.UserSID(t)
	refusedThenAccepted(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDEveryone), wintest.SIDEveryone)
}

func TestCheckPrivate_RefusesCallbackACE(t *testing.T) {
	tr := treeAt(t, t.TempDir())
	p := mustCreate(t, tr, "x", Private, "x")
	u := wintest.UserSID(t)
	// A conditional (callback) allow ACE, even for the user's own SID, is
	// not a plain allow ACE and fails closed.
	refusedThenAccepted(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(XA;;FA;;;%s;(Member_of {SID(BA)}))", u, u), "type")
}

func TestCheckPrivate_RefusesInheritOnlyForeignACE(t *testing.T) {
	tr := treeAt(t, t.TempDir())
	if err := tr.MkdirAll("d", Private); err != nil {
		t.Fatal(err)
	}
	u := wintest.UserSID(t)
	path := filepath.Join(tr.Anchor(), "d")
	check := func() error {
		d, err := tr.OpenDir("d")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = d.Close() }()
		return CheckPrivate(d)
	}
	wintest.SetDACL(t, path, fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICIIO;FR;;;%s)", u, wintest.SIDUsers))
	if err := check(); !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), wintest.SIDUsers) {
		t.Fatalf("directory with an inherit-only Users ACE: %v, want ErrNotPrivate", err)
	}
	wintest.SetDACL(t, path, fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;%s)", u, wintest.SIDSystem))
	if err := check(); err != nil {
		t.Fatalf("control: private directory: %v", err)
	}
}

func TestCheckPrivate_RefusesNullDACL(t *testing.T) {
	tr := treeAt(t, t.TempDir())
	p := mustCreate(t, tr, "x", Private, "x")
	wintest.SetNullDACL(t, p)
	if err := checkPath(t, p); !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), "NULL DACL") {
		t.Fatalf("NULL DACL: %v, want ErrNotPrivate", err)
	}
	wintest.SetDACL(t, p, privateDACL(wintest.UserSID(t)))
	if err := checkPath(t, p); err != nil {
		t.Fatalf("control: %v", err)
	}
}

func TestCheckPrivate_RefusesForeignOwner(t *testing.T) {
	tr := treeAt(t, t.TempDir())
	p := mustCreate(t, tr, "x", Private, "x")
	control := mustCreate(t, tr, "y", Private, "x")
	wintest.SetOwner(t, p, wintest.SIDSystem)
	if err := checkPath(t, p); !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), "owned by "+wintest.SIDSystem) {
		t.Fatalf("SYSTEM-owned file: %v, want ErrNotPrivate naming the owner", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := OwnedByCurrentUser(f)
	_ = f.Close()
	if err != nil || owned {
		t.Fatalf("OwnedByCurrentUser of a SYSTEM-owned file = %v, %v", owned, err)
	}
	if err := checkPath(t, control); err != nil {
		t.Fatalf("control: the user's own file: %v", err)
	}
}

// Administrators are accepted on verify (never granted on create), and a
// deny ACE for anyone is fine.
func TestCheckPrivate_AcceptsUserSystemAdmins(t *testing.T) {
	tr := treeAt(t, t.TempDir())
	p := mustCreate(t, tr, "x", Private, "x")
	u := wintest.UserSID(t)
	wintest.SetDACL(t, p, fmt.Sprintf("D:P(D;;FA;;;%s)(A;;FA;;;%s)(A;;FA;;;%s)(A;;FA;;;%s)", wintest.SIDEveryone, u, wintest.SIDSystem, wintest.SIDAdministrators))
	if err := checkPath(t, p); err != nil {
		t.Fatalf("user + SYSTEM + Administrators: %v", err)
	}
	// Negative in the same test: one more allow ACE for Users fails.
	wintest.SetDACL(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;%s)(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDSystem, wintest.SIDAdministrators, wintest.SIDUsers))
	if err := checkPath(t, p); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("with a Users ACE: %v, want ErrNotPrivate", err)
	}
}

// A local group named like Administrators is just another SID: the
// verifier compares SIDs, never names. A group literally named
// "Administrators" cannot be created (the builtin alias owns that name), so
// the test uses a new local group with a new, non-well-known SID.
func TestCheckPrivate_ForeignLocalGroupRefusedBySID(t *testing.T) {
	const group = "scion-test-administrators"
	_ = exec.Command("net", "localgroup", group, "/delete").Run()
	if out, err := exec.Command("net", "localgroup", group, "/add").CombinedOutput(); err != nil {
		t.Fatalf("create local group %s (the Windows job runs as an administrator): %v: %s", group, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("net", "localgroup", group, "/delete").Run() })
	sid, _, _, err := windows.LookupSID("", group)
	if err != nil {
		t.Fatalf("look up the test group's SID: %v", err)
	}
	if sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		t.Fatal("the test group resolved to the builtin Administrators SID")
	}
	tr := treeAt(t, t.TempDir())
	p := mustCreate(t, tr, "x", Private, "x")
	u := wintest.UserSID(t)
	wintest.SetDACL(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;%s)", u, sid.String()))
	if err := checkPath(t, p); !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), sid.String()) {
		t.Fatalf("ACE for local group %s (%s): %v, want ErrNotPrivate naming its SID", group, sid, err)
	}
	// Control: the same ACE re-pointed at the builtin Administrators SID.
	wintest.SetDACL(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;%s)", u, wintest.SIDAdministrators))
	if err := checkPath(t, p); err != nil {
		t.Fatalf("control: ACE for S-1-5-32-544: %v", err)
	}
}

func TestCheckNoForeignWriters(t *testing.T) {
	tr := treeAt(t, t.TempDir())
	p := mustCreate(t, tr, "x", Private, "x")
	u := wintest.UserSID(t)
	check := func() error {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		return CheckNoForeignWriters(f)
	}
	// Users may read: integrity holds (CheckPrivate would refuse).
	wintest.SetDACL(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDUsers))
	if err := check(); err != nil {
		t.Fatalf("Users read only: %v", err)
	}
	for _, right := range []string{"FW", "WD", "WO", "SD", "GA", "GW"} {
		wintest.SetDACL(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;%s;;;%s)", u, right, wintest.SIDUsers))
		if err := check(); !errors.Is(err, ErrNotPrivate) {
			t.Fatalf("Users granted %s: %v, want ErrNotPrivate", right, err)
		}
	}
}

func TestTree_RefusesJunctionComponent(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	tr := treeAt(t, dir)
	wintest.MustJunction(t, outside, filepath.Join(dir, "j"))
	if _, err := tr.Create("j/x", true, Private); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("Create through a junction: %v, want ErrReparsePoint", err)
	}
	if err := tr.MkdirAll("j/sub", Private); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("MkdirAll through a junction: %v, want ErrReparsePoint", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "y"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ReadPrivate("j/y", 0); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("ReadPrivate through a junction: %v, want ErrReparsePoint", err)
	}
	for _, name := range []string{"x", "sub"} {
		if _, err := os.Lstat(filepath.Join(outside, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s was created behind the junction: %v", name, err)
		}
	}
	// Control: the same walk on a real directory succeeds.
	if err := tr.MkdirAll("real", Private); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, tr, "real/x", Private, "x")
}

func TestTree_RefusesSymlinkComponent(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	tr := treeAt(t, dir)
	wintest.MustSymlink(t, outside, filepath.Join(dir, "s"))
	if _, err := tr.Create("s/x", true, Private); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("Create through a directory symlink: %v, want ErrReparsePoint", err)
	}
	if _, err := tr.OpenDir("s"); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("OpenDir of a directory symlink: %v, want ErrReparsePoint", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("x was created behind the symlink: %v", err)
	}
	if err := tr.MkdirAll("real", Private); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, tr, "real/x", Private, "x")
}

func TestTree_RefusesSymlinkLeaf(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	tr := treeAt(t, dir)
	target := filepath.Join(outside, "target")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.MustSymlink(t, target, filepath.Join(dir, "leaf"))
	if _, err := tr.ReadPrivate("leaf", 0); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("ReadPrivate of a symlink: %v, want ErrReparsePoint", err)
	}
	if _, err := tr.OpenPrivate("leaf", os.O_RDWR); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("OpenPrivate of a symlink: %v, want ErrReparsePoint", err)
	}
	if _, err := tr.Create("leaf", false, Private); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("non-exclusive Create of a symlink: %v, want ErrReparsePoint", err)
	}
	if err := tr.Remove("leaf"); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("Remove of a symlink: %v, want ErrReparsePoint", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "outside" {
		t.Fatalf("target changed: %q, %v", b, err)
	}
	mustCreate(t, tr, "real", Private, "inside")
	if b, err := tr.ReadPrivate("real", 0); err != nil || string(b) != "inside" {
		t.Fatalf("control: ReadPrivate of a regular file: %q, %v", b, err)
	}
}

func TestTree_RefusesJunctionAtLeafDir(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	tr := treeAt(t, dir)
	if err := tr.MkdirAll("a", Private); err != nil {
		t.Fatal(err)
	}
	wintest.MustJunction(t, outside, filepath.Join(dir, "a", "j"))
	if err := tr.MkdirAll("a/j", Private); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("MkdirAll of a junction leaf: %v, want ErrReparsePoint", err)
	}
	if _, err := tr.OpenDir("a/j"); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("OpenDir of a junction leaf: %v, want ErrReparsePoint", err)
	}
	if err := tr.MkdirAll("a/real", Private); err != nil {
		t.Fatalf("control: MkdirAll of a real leaf: %v", err)
	}
	d, err := tr.OpenDir("a/real")
	if err != nil {
		t.Fatalf("control: OpenDir of a real leaf: %v", err)
	}
	_ = d.Close()
}

// Atomic replace is handle-based end to end, and the result is private
// whatever the old file's DACL was.
func TestTree_HandleRenameReplacesTarget(t *testing.T) {
	dir := t.TempDir()
	tr := treeAt(t, dir)
	old := mustCreate(t, tr, "t", Inherit, "old")
	wintest.Icacls(t, old, "/grant", "*"+wintest.SIDUsers+":R")
	before, err := os.Stat(old)
	if err != nil {
		t.Fatal(err)
	}
	// os.Stat records the file ID lazily on Windows (os.SameFile reads it
	// by path on first use): read it now, before the rename.
	_ = os.SameFile(before, before)
	tmp, name, err := tr.CreateTemp("", ".t.*.tmp", Private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString("new"); err != nil {
		t.Fatal(err)
	}
	// Rename while the temporary file is still open: its handle shares
	// delete access.
	if err := tr.Rename(name, "t"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	_ = tmp.Close()
	after, err := os.Stat(old)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("target rewritten in place, want the temporary file renamed over it")
	}
	if b, err := tr.ReadPrivate("t", 0); err != nil || string(b) != "new" {
		t.Fatalf("after Rename: %q, %v", b, err)
	}
	assertPrivateSD(t, old)
	if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temporary file left: %v", err)
	}
	// WriteFile does the same in one call.
	wintest.Icacls(t, old, "/grant", "*"+wintest.SIDUsers+":R")
	if err := tr.WriteFile("t", []byte("newer"), Private); err != nil {
		t.Fatal(err)
	}
	assertPrivateSD(t, old)
}

func TestTree_RenameIntoJunctionedDirRefused(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	tr := treeAt(t, dir)
	wintest.MustJunction(t, outside, filepath.Join(dir, "j"))
	mustCreate(t, tr, "tmp", Private, "x")
	if err := tr.Rename("tmp", "j/final"); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("Rename into a junctioned directory: %v, want ErrReparsePoint", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "final")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file renamed behind the junction: %v", err)
	}
	if err := tr.MkdirAll("real", Private); err != nil {
		t.Fatal(err)
	}
	if err := tr.Rename("tmp", "real/final"); err != nil {
		t.Fatalf("control: Rename into a real directory: %v", err)
	}
}

// Verify-or-repair: an owned, too-open file opened for writing is repaired on the
// same handle before any byte is written; opened read-only it is refused.
func TestRepair_InPlaceOwned(t *testing.T) {
	dir := t.TempDir()
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	tr := treeAt(t, dir)
	p := mustCreate(t, tr, "x", Inherit, "old")
	if err := checkPath(t, p); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("precondition: the file must start too open: %v", err)
	}
	if _, err := tr.OpenPrivate("x", os.O_RDONLY); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("read-only OpenPrivate of a too-open file: %v, want ErrNotPrivate", err)
	}
	f, err := tr.OpenPrivate("x", os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenPrivate for writing of an owned, too-open file: %v", err)
	}
	if err := CheckPrivate(f); err != nil {
		t.Fatalf("handle not private before the write: %v", err)
	}
	if _, err := f.WriteString("secret"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	assertPrivateSD(t, p)
	if b, err := tr.ReadPrivate("x", 0); err != nil || string(b) != "secret" {
		t.Fatalf("content after repair: %q, %v", b, err)
	}
}

// Verify-or-repair: a too-open file owned by someone else is refused, not repaired.
func TestRefuse_InPlaceForeignOwned(t *testing.T) {
	dir := t.TempDir()
	tr := treeAt(t, dir)
	u := wintest.UserSID(t)
	open := fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDUsers)
	p := mustCreate(t, tr, "x", Private, "old")
	wintest.SetDACL(t, p, open)
	wintest.SetOwner(t, p, wintest.SIDSystem)
	if _, err := tr.OpenPrivate("x", os.O_RDWR|os.O_TRUNC); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("OpenPrivate of a SYSTEM-owned too-open file: %v, want ErrNotPrivate", err)
	}
	if _, err := tr.Create("x", false, Private); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("non-exclusive Create of a SYSTEM-owned too-open file: %v, want ErrNotPrivate", err)
	}
	sd := wintest.ReadSD(t, p)
	if sd.Owner != wintest.SIDSystem || !sd.HasAllowACE(wintest.SIDUsers) {
		t.Fatalf("the foreign file was modified: %+v", sd)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "old" {
		t.Fatalf("the foreign file's content changed: %q, %v", b, err)
	}
	// Control: the same DACL on a file the user owns is repaired.
	c := mustCreate(t, tr, "y", Private, "old")
	wintest.SetDACL(t, c, open)
	f, err := tr.OpenPrivate("y", os.O_RDWR|os.O_TRUNC)
	if err != nil {
		t.Fatalf("control: OpenPrivate of an owned too-open file: %v", err)
	}
	_ = f.Close()
	assertPrivateSD(t, c)
}

// On a volume without persistent ACLs (FAT32) a Private create
// fails, naming the volume as the cause; there is no fallback.
func TestNoPersistentACLs_Refused(t *testing.T) {
	vol := os.Getenv("SCION_TEST_FAT_VOLUME")
	if vol == "" {
		t.Fatal("SCION_TEST_FAT_VOLUME is not set: the Windows job must create and mount a FAT32 volume and export its root (see .github/workflows/windows.yml); this is a job configuration failure, not a skip")
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	dir := filepath.Join(vol, "scion-test-"+hex.EncodeToString(b[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create a directory on the FAT volume %s: %v", vol, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	tr := treeAt(t, dir)
	const want = "does not support access control lists, so the file cannot be protected"
	_, err := tr.Create("x", true, Private)
	if !errors.Is(err, ErrNoACLs) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "FAT") {
		t.Fatalf("Private create on FAT: %v; want ErrNoACLs naming the FAT volume", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file was left on the FAT volume: %v", err)
	}
	if err := tr.MkdirAll("d", Private); !errors.Is(err, ErrNoACLs) {
		t.Fatalf("Private MkdirAll on FAT: %v, want ErrNoACLs", err)
	}
	if err := tr.WriteFile("w", []byte("x"), Private); !errors.Is(err, ErrNoACLs) {
		t.Fatalf("Private WriteFile on FAT: %v, want ErrNoACLs", err)
	}
	if _, err := OpenTree(filepath.Join(dir, "new", "anchor"), TreeOptions{CreateAnchor: true}); !errors.Is(err, ErrNoACLs) {
		t.Fatalf("OpenTree creating a private anchor on FAT: %v, want ErrNoACLs", err)
	}
	// The volume itself is writable: an Inherit file works there.
	fat := mustCreate(t, tr, "inherit", Inherit, "x")
	// A file on a volume without ACLs is never private: CheckPrivate
	// refuses it (it must not crash on a missing owner or DACL).
	ff, err := os.Open(fat)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckPrivate(ff)
	_ = ff.Close()
	if !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("CheckPrivate of a FAT file: %v, want ErrNotPrivate", err)
	}
	if _, err := tr.ReadPrivate("inherit", 0); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("ReadPrivate of a FAT file: %v, want ErrNotPrivate", err)
	}

	// Control: the same Private create on NTFS succeeds.
	ntfs := treeAt(t, t.TempDir())
	mustCreate(t, ntfs, "x", Private, "x")
}

// The anchor's volume serial and file index are recorded for diagnostics
// only: they appear in refusal text, and the walk never moves the anchor.
func TestAnchorDiagnosticIdentity(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	tr := treeAt(t, dir)
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bhfi windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &bhfi)
	_ = windows.CloseHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	wantIndex := uint64(bhfi.FileIndexHigh)<<32 | uint64(bhfi.FileIndexLow)
	if tr.volSerial != bhfi.VolumeSerialNumber || tr.fileIndex != wantIndex {
		t.Fatalf("anchor identity %08x/%016x, want %08x/%016x", tr.volSerial, tr.fileIndex, bhfi.VolumeSerialNumber, wantIndex)
	}
	if err := tr.MkdirAll("a/b/c", Private); err != nil {
		t.Fatal(err)
	}
	if tr.volSerial != bhfi.VolumeSerialNumber || tr.fileIndex != wantIndex {
		t.Fatal("the walk changed the recorded anchor identity")
	}
	wintest.MustJunction(t, outside, filepath.Join(dir, "j"))
	_, err = tr.Create("j/x", true, Private)
	if !errors.Is(err, ErrReparsePoint) || !strings.Contains(err.Error(), fmt.Sprintf("%016x", wantIndex)) {
		t.Fatalf("refusal %v does not carry the anchor identity", err)
	}
}

// TestOpenTree_CreateAnchorPrivate: missing anchor components are created
// with the private descriptor; an existing one is left as it is.
func TestOpenTree_CreateAnchorPrivate(t *testing.T) {
	base := t.TempDir()
	wintest.Icacls(t, base, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	anchor := filepath.Join(base, "a", "b")
	tr, err := OpenTree(anchor, TreeOptions{CreateAnchor: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Close()
	for _, d := range []string{filepath.Join(base, "a"), anchor} {
		if sd := wintest.ReadSD(t, d); !sd.Protected || sd.HasAllowACE(wintest.SIDUsers) {
			t.Fatalf("%s: SD %+v; want a protected private DACL", d, sd)
		}
	}
	// Control: the pre-existing base keeps its inherited Users grant.
	if sd := wintest.ReadSD(t, base); !sd.HasAllowACE(wintest.SIDUsers) {
		t.Fatalf("control: the existing base was re-secured: %+v", sd)
	}
}

// openNoFollowHandle opens path itself (FILE_FLAG_OPEN_REPARSE_POINT).
func openNoFollowHandle(t *testing.T, path string) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
	return h
}

// The FAT32 fallback (attributes from GetFileInformationByHandle) still
// sees a reparse point: exercised here on NTFS, where junctions and
// symlinks exist, on handles opened without following.
func TestAttrsByHandleInfo_SeesReparsePoints(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	j, s, f := filepath.Join(dir, "j"), filepath.Join(dir, "s"), filepath.Join(dir, "f")
	wintest.MustJunction(t, outside, j)
	wintest.MustSymlink(t, outside, s)
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{j, s} {
		attrs, err := attrsByHandleInfo(openNoFollowHandle(t, p))
		if err != nil || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
			t.Fatalf("attrsByHandleInfo(%s) = %#x, %v; want the reparse bit", p, attrs, err)
		}
	}
	// Control: a regular file has no reparse bit.
	attrs, err := attrsByHandleInfo(openNoFollowHandle(t, f))
	if err != nil || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		t.Fatalf("attrsByHandleInfo(regular file) = %#x, %v", attrs, err)
	}
}

// RenameOpen renames the file the handle is open on, not whatever holds
// the temporary name at rename time: a file swapped in under that name
// (by anyone who may delete and create entries in the directory) never
// lands at the target.
func TestRenameOpen_SwappedTempNeverLands(t *testing.T) {
	dir := t.TempDir()
	tr := treeAt(t, dir)
	mustCreate(t, tr, "target", Private, "old")
	f, name, err := tr.CreateTemp("", ".target.*.tmp", Private)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("mine"); err != nil {
		t.Fatal(err)
	}
	// Swap: move our temporary file away and put an impostor in its place.
	tmpPath := filepath.Join(dir, name)
	if err := os.Rename(tmpPath, filepath.Join(dir, "away")); err != nil {
		t.Fatalf("swap the temporary file away: %v", err)
	}
	if err := os.WriteFile(tmpPath, []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tr.RenameOpen(f, "target"); err != nil {
		t.Fatalf("RenameOpen: %v", err)
	}
	// Close our handle (it holds DELETE access, which os.ReadFile's open
	// does not share) before reading the result.
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "target")); err != nil || string(b) != "mine" {
		t.Fatalf("target = %q, %v; want the file written through the handle", b, err)
	}
	assertPrivateSD(t, filepath.Join(dir, "target"))
	if b, err := os.ReadFile(tmpPath); err != nil || string(b) != "evil" {
		t.Fatalf("the impostor moved: %q, %v", b, err)
	}
	// Control: a rename by name moves whatever holds the name now, the
	// impostor, which shows the swap was real.
	if err := tr.Rename(name, "byname"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "byname")); string(b) != "evil" {
		t.Fatalf("control: rename by name moved %q, want the impostor", b)
	}
}

// RenameOpen is a private-tree primitive: it verifies the source handle
// with CheckPrivate and refuses a file that is not private.
func TestRenameOpen_RefusesNonPrivateHandle(t *testing.T) {
	dir := t.TempDir()
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	tr := treeAt(t, dir)
	f, err := tr.Create("open.tmp", true, Inherit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := tr.RenameOpen(f, "target"); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("RenameOpen of a Users-readable file: %v, want ErrNotPrivate", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused file was renamed: %v", err)
	}
	// Control: a Private temporary file renames.
	p, _, err := tr.CreateTemp("", ".t.*.tmp", Private)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if err := tr.RenameOpen(p, "target"); err != nil {
		t.Fatalf("control: RenameOpen of a Private file: %v", err)
	}
}

// RemoveOpen deletes the file the handle is open on, not whatever holds
// its name: a file swapped in under the temporary name survives.
func TestRemoveOpen_DeletesHandleNotName(t *testing.T) {
	dir := t.TempDir()
	tr := treeAt(t, dir)
	f, name, err := tr.CreateTemp("", ".t.*.tmp", Private)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, name)
	away := filepath.Join(dir, "away")
	if err := os.Rename(tmp, away); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("impostor"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tr.RemoveOpen(f); err != nil {
		t.Fatalf("RemoveOpen: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(away); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the file the handle was open on survived: %v", err)
	}
	// Control: the impostor under the temporary name is untouched.
	if b, err := os.ReadFile(tmp); err != nil || string(b) != "impostor" {
		t.Fatalf("control: the impostor = %q, %v", b, err)
	}
}
