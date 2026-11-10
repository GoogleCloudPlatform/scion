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

package filelock

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// mustAcquireRegular is the positive control of the refusal tests: Acquire
// on a regular path in the same directory succeeds.
func mustAcquireRegular(t *testing.T, dir string) {
	t.Helper()
	l, err := Acquire(filepath.Join(dir, "regular.lock"), 0600, 0)
	if err != nil {
		t.Fatalf("control: Acquire of a regular lock file: %v", err)
	}
	l.Unlock()
}

func TestAcquire_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.lock")
	wintest.MustSymlink(t, target, link)
	if _, err := Acquire(link, 0600, 0); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("symlink: err = %v, want ErrNotRegular", err)
	}
	// A dangling symlink is refused too, and nothing is created behind it.
	dangling := filepath.Join(dir, "dangling.lock")
	wintest.MustSymlink(t, filepath.Join(dir, "absent"), dangling)
	if _, err := Acquire(dangling, 0600, 0); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("dangling symlink: err = %v, want ErrNotRegular", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "absent")); !os.IsNotExist(err) {
		t.Fatalf("Acquire created the dangling symlink's target: %v", err)
	}
	mustAcquireRegular(t, dir)
}

func TestAcquire_RefusesJunction(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	j := filepath.Join(dir, "junction.lock")
	wintest.MustJunction(t, target, j)
	if _, err := Acquire(j, 0600, 0); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("junction: err = %v, want ErrNotRegular", err)
	}
	mustAcquireRegular(t, dir)
}

func TestAcquire_RefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub.lock")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(sub, 0600, 0); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("directory: err = %v, want ErrNotRegular", err)
	}
	mustAcquireRegular(t, dir)
}

// Acquire's handle does not share delete access,
// so a held lock file cannot be deleted (and re-created and locked by a
// third process).
func TestAcquire_HeldFileCannotBeDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("a held lock file was deleted")
	}
	if err := os.Rename(path, path+".moved"); err == nil {
		t.Fatal("a held lock file was renamed")
	}
	l.Unlock()
	// Control: the delete itself works once the handle is closed.
	if err := os.Remove(path); err != nil {
		t.Fatalf("delete after Unlock: %v", err)
	}
}

func TestAcquire_PrivateSDForMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	user := wintest.UserSID(t)
	sd := wintest.ReadSD(t, path)
	if sd.Owner != user || !sd.Protected || len(sd.ACEs) != 2 || !sd.HasAllowACE(user) || !sd.HasAllowACE(wintest.SIDSystem) {
		t.Fatalf("lock file SD = %+v; want owner %s, protected, allow {user, SYSTEM} only", sd, user)
	}
}

func TestAcquire_InheritForMode0644(t *testing.T) {
	dir := t.TempDir()
	// The parent grants Users read, inheritably: an Inherit lock file
	// carries that ACE, a private one (0600, the control) does not.
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	path := filepath.Join(dir, "x.lock")
	l, err := Acquire(path, 0644, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if sd := wintest.ReadSD(t, path); sd.Protected || !sd.HasAllowACE(wintest.SIDUsers) {
		t.Fatalf("0644 lock file SD = %+v; want the inherited Users ACE, not protected", sd)
	}
	priv := filepath.Join(dir, "p.lock")
	lp, err := Acquire(priv, 0600, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lp.Unlock()
	if sd := wintest.ReadSD(t, priv); !sd.Protected || sd.HasAllowACE(wintest.SIDUsers) {
		t.Fatalf("control: 0600 lock file SD = %+v; want protected, no Users ACE", sd)
	}
}

// The tunnelclient pattern: a file created with delete sharing is locked
// with LockFile and renamed through its own handle (RenameOpen) while held.
// The lock stays held under the new name, and a file swapped in under the
// temporary name before the rename is never what becomes the lock file.
func TestLockFile_ShareDeleteRename(t *testing.T) {
	dir := t.TempDir()
	tr, err := privatefs.OpenTree(dir, privatefs.TreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	f, err := tr.Create(".x.lock.tmp", true, privatefs.Private)
	if err != nil {
		t.Fatal(err)
	}
	l, err := LockFile(f, 0)
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	defer l.Unlock()
	// Swap: move the locked temp away and plant an unlocked impostor.
	tmp := filepath.Join(dir, ".x.lock.tmp")
	if err := os.Rename(tmp, filepath.Join(dir, "away")); err != nil {
		t.Fatalf("swap the lock temp away: %v", err)
	}
	if err := os.WriteFile(tmp, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := tr.RenameOpen(f, "x.lock"); err != nil {
		t.Fatalf("rename while held: %v", err)
	}
	final := filepath.Join(dir, "x.lock")
	// probe locks rel through a second handle that, like the owner's,
	// shares delete access (OpenPrivate opens with every share mode).
	probe := func(rel string, wait time.Duration) (*Lock, error) {
		pf, err := tr.OpenPrivate(rel, os.O_RDWR)
		if err != nil {
			t.Fatalf("open %s for the probe: %v", rel, err)
		}
		pl, err := LockFile(pf, wait)
		if err != nil {
			_ = pf.Close()
		}
		return pl, err
	}
	if _, err := probe("x.lock", 20*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("probe of the renamed held lock: %v, want ErrTimeout (the locked file must be the one renamed)", err)
	}
	// Control: the impostor under the temporary name is not locked.
	li, err := probe(".x.lock.tmp", 0)
	if err != nil {
		t.Fatalf("control: the impostor is lockable: %v", err)
	}
	li.Unlock()
	// The owner can remove its own name while holding the lock.
	if err := tr.Remove("x.lock"); err != nil {
		t.Fatalf("remove while held: %v", err)
	}
	if _, err := os.Lstat(final); !os.IsNotExist(err) {
		t.Fatalf("lock file after Remove: %v", err)
	}
}

// The FAT32 fallback (attributes from GetFileInformationByHandle) still
// sees a reparse point: exercised on NTFS junction and symlink handles
// opened without following, with a regular-file control.
func TestAttrsByHandleInfo_SeesReparsePoints(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	j, s, f := filepath.Join(dir, "j"), filepath.Join(dir, "s"), filepath.Join(dir, "f")
	wintest.MustJunction(t, outside, j)
	wintest.MustSymlink(t, filepath.Join(outside), s)
	if err := os.WriteFile(f, nil, 0600); err != nil {
		t.Fatal(err)
	}
	open := func(path string) windows.Handle {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, noFollowOpen, 0)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		t.Cleanup(func() { _ = windows.CloseHandle(h) })
		return h
	}
	for _, p := range []string{j, s} {
		attrs, err := attrsByHandleInfo(open(p))
		if err != nil || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
			t.Fatalf("attrsByHandleInfo(%s) = %#x, %v; want the reparse bit", p, attrs, err)
		}
	}
	attrs, err := attrsByHandleInfo(open(f))
	if err != nil || attrs&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		t.Fatalf("control: attrsByHandleInfo(regular file) = %#x, %v", attrs, err)
	}
}

// A lock in a directory the user may not write, where the file does not
// exist, reports the denied create, not "not found".
func TestAcquire_DeniedCreateReportsDenial(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0700); err != nil {
		t.Fatal(err)
	}
	u := wintest.UserSID(t)
	// The user may list and traverse, but not add files.
	wintest.SetDACL(t, ro, "D:P(A;OICI;0x1200a9;;;"+u+")(A;OICI;FA;;;"+wintest.SIDSystem+")")
	t.Cleanup(func() { wintest.SetDACL(t, ro, "D:P(A;OICI;FA;;;"+u+")") })
	_, err := Acquire(filepath.Join(ro, "x.lock"), 0600, 0)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Acquire in a read-only directory: %v, want a permission error", err)
	}
	// Control: the same Acquire in a writable directory works.
	mustAcquireRegular(t, dir)
}

// The refusal message names a reparse tag only when one was read: the FAT
// attribute fallback reads none, and its 0 is not a tag.
func TestReparseTagText_NoTagClaimedWithoutOne(t *testing.T) {
	if got := reparseTagText(0, false); strings.Contains(got, "0x") || strings.Contains(got, "tag 0") {
		t.Fatalf("fallback text %q claims a tag", got)
	}
	// Control: a tag that was read is printed.
	if got := reparseTagText(0xa000000c, true); !strings.Contains(got, "0xa000000c") {
		t.Fatalf("known-tag text %q does not name the tag", got)
	}
}
