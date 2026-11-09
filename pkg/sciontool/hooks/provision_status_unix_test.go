//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// memStatusFixture points the in-memory root at a fresh directory and makes
// the writer non-root with the test process's own uid, so the writer owns
// what it creates. The umask is 022 for the test, so what the writer
// creates does not depend on the caller's umask. It returns the root and
// the status path in its outputs directory, which does not exist yet.
func memStatusFixture(t *testing.T) (root, statusPath string) {
	t.Helper()
	uid := os.Geteuid()
	if uid == 0 {
		t.Skip("needs a non-root test process: only a non-root writer creates the directory, and it must own it")
	}
	setTestUmask(t, 0o022)
	root = t.TempDir()
	t.Cleanup(SetHarnessDirsRootForTest(root))
	t.Cleanup(SetProvisionStatusGeteuidForTest(func() int { return uid }))
	return root, filepath.Join(root, "outputs", "status.json")
}

func assertProvisionState(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if got := string(data); got != `{"schema_version":1,"state":"`+want+`"}`+"\n" {
		t.Errorf("status = %q, want state %q", got, want)
	}
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists (err %v), want it absent", path, err)
	}
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s has %d entries, want none", dir, len(entries))
	}
}

// privateTempDir returns a new temporary directory with mode 0700, owned by
// the test process. t.TempDir creates every directory it returns with mode
// 0777 less the umask (0755 under the fixture's 022), so a symlink pointing
// at one would be refused by the mode check (wider than 0700) before the
// no-follow open is ever tested. A symlink target made with this helper
// passes every other check, so only the no-follow open can refuse the link.
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func setTestUmask(t *testing.T, mask int) {
	t.Helper()
	prev := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(prev) })
}

// A first start with no outputs directory: the status is recorded.
func TestWriteHarnessProvisionStatus_CreatesMemOutputsDir(t *testing.T) {
	_, statusPath := memStatusFixture(t)
	if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, ""); err != nil {
		t.Fatalf("WriteHarnessProvisionStatus: %v", err)
	}
	assertProvisionState(t, statusPath, ProvisionStatusRunning)
}

// A created directory is mode 0700 and owned by the writer, whatever the
// umask; the status file is 0600.
func TestWriteHarnessProvisionStatus_CreatedDirModeAndOwner(t *testing.T) {
	for _, mask := range []int{0o022, 0o277} {
		t.Run(fmt.Sprintf("umask %04o", mask), func(t *testing.T) {
			_, statusPath := memStatusFixture(t)
			setTestUmask(t, mask)
			if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, ""); err != nil {
				t.Fatalf("WriteHarnessProvisionStatus (umask %#o): %v", mask, err)
			}
			fi, err := os.Lstat(filepath.Dir(statusPath))
			if err != nil {
				t.Fatal(err)
			}
			if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
				t.Errorf("outputs mode = %v, want a 0700 directory", fi.Mode())
			}
			if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid() {
				t.Errorf("outputs owner = %d:%d, want %d:%d", st.Uid, st.Gid, os.Geteuid(), os.Getegid())
			}
			sfi, err := os.Lstat(statusPath)
			if err != nil {
				t.Fatal(err)
			}
			if sfi.Mode().Perm() != 0o600 {
				t.Errorf("status mode = %v, want 0600", sfi.Mode())
			}
		})
	}
}

// A pre-existing directory owned by the writer with a mode no wider than
// 0700 is used as it is: never chmod-repaired, even when narrower.
func TestWriteHarnessProvisionStatus_ExistingPrivateDirUntouched(t *testing.T) {
	t.Run("0700", func(t *testing.T) {
		_, statusPath := memStatusFixture(t)
		dir := mkdirMode(t, filepath.Dir(statusPath), 0o700)
		if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusOK, ""); err != nil {
			t.Fatalf("WriteHarnessProvisionStatus: %v", err)
		}
		assertProvisionState(t, statusPath, ProvisionStatusOK)
		assertPerm(t, dir, 0o700)
	})
	t.Run("narrower 0500", func(t *testing.T) {
		_, statusPath := memStatusFixture(t)
		dir := mkdirMode(t, filepath.Dir(statusPath), 0o500)
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // so t.TempDir can remove it
		if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusOK, ""); err == nil {
			t.Fatal("expected the write into a read-only directory to fail")
		}
		assertPerm(t, dir, 0o500)
		assertNotExist(t, statusPath)
	})
}

// mkdirMode creates dir with exactly mode (chmod after mkdir, so the umask
// does not apply) and returns it.
func mkdirMode(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Errorf("%s mode = %v, want %v", path, fi.Mode().Perm(), want)
	}
}

// An existing outputs directory that is not a private directory of the
// writer's is never written into and never repaired: each branch of the
// check skips the write with ErrProvisionStatusDirUnsafe.
func TestWriteHarnessProvisionStatus_SkipsUnsafeExistingDir(t *testing.T) {
	t.Run("not a directory", func(t *testing.T) {
		_, statusPath := memStatusFixture(t)
		dir := filepath.Dir(statusPath)
		if err := os.WriteFile(dir, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, "")
		if !errors.Is(err, ErrProvisionStatusDirUnsafe) {
			t.Fatalf("err = %v, want ErrProvisionStatusDirUnsafe", err)
		}
		if data, rerr := os.ReadFile(dir); rerr != nil || string(data) != "file" {
			t.Errorf("outputs file changed: %q, %v", data, rerr)
		}
	})
	t.Run("owned by another uid", func(t *testing.T) {
		root := t.TempDir()
		t.Cleanup(SetHarnessDirsRootForTest(root))
		other := os.Geteuid() + 1
		t.Cleanup(SetProvisionStatusGeteuidForTest(func() int { return other }))
		dir := filepath.Join(root, "outputs")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		err := WriteHarnessProvisionStatus(filepath.Join(dir, "status.json"), ProvisionStatusRunning, "")
		if !errors.Is(err, ErrProvisionStatusDirUnsafe) {
			t.Fatalf("err = %v, want ErrProvisionStatusDirUnsafe", err)
		}
		assertDirEmpty(t, dir)
	})
	for _, mode := range []os.FileMode{0o755, 0o770, 0o701} {
		t.Run("mode "+mode.String(), func(t *testing.T) {
			_, statusPath := memStatusFixture(t)
			dir := filepath.Dir(statusPath)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, "")
			if !errors.Is(err, ErrProvisionStatusDirUnsafe) {
				t.Fatalf("err = %v, want ErrProvisionStatusDirUnsafe", err)
			}
			assertDirEmpty(t, dir)
			fi, err := os.Lstat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != mode {
				t.Errorf("outputs mode = %v, want it left at %v", fi.Mode().Perm(), mode)
			}
		})
	}
}

// A symbolic link at the outputs directory is refused, and nothing is
// written where it points.
func TestWriteHarnessProvisionStatus_RefusesSymlinkAtOutputs(t *testing.T) {
	root, statusPath := memStatusFixture(t)
	elsewhere := privateTempDir(t)
	if err := os.Symlink(elsewhere, filepath.Join(root, "outputs")); err != nil {
		t.Fatal(err)
	}
	err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, "")
	if !errors.Is(err, ErrProvisionStatusDirUnsafe) {
		t.Fatalf("err = %v, want ErrProvisionStatusDirUnsafe", err)
	}
	assertDirEmpty(t, elsewhere)
}

// Whatever already exists at outputs makes mkdirat fail with EEXIST, which
// is never treated as created: the entry is left exactly as it is (no
// chmod, no write through a link, no change to a file), and the status is
// not recorded.
func TestWriteHarnessProvisionStatus_ExistingEntryTakesPreExistingBranch(t *testing.T) {
	t.Run("dir 0500", func(t *testing.T) {
		_, statusPath := memStatusFixture(t)
		dir := mkdirMode(t, filepath.Dir(statusPath), 0o500)
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // so t.TempDir can remove it
		if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, ""); err == nil {
			t.Fatal("expected the write into an existing read-only directory to fail")
		}
		assertPerm(t, dir, 0o500)
		assertNotExist(t, statusPath)
	})
	t.Run("symlink", func(t *testing.T) {
		root, statusPath := memStatusFixture(t)
		elsewhere := privateTempDir(t)
		link := filepath.Join(root, "outputs")
		if err := os.Symlink(elsewhere, link); err != nil {
			t.Fatal(err)
		}
		if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, ""); !errors.Is(err, ErrProvisionStatusDirUnsafe) {
			t.Fatalf("err = %v, want ErrProvisionStatusDirUnsafe", err)
		}
		if target, err := os.Readlink(link); err != nil || target != elsewhere {
			t.Errorf("link = %q, %v; want it unchanged to %q", target, err, elsewhere)
		}
		assertDirEmpty(t, elsewhere)
	})
	t.Run("file", func(t *testing.T) {
		_, statusPath := memStatusFixture(t)
		file := filepath.Dir(statusPath)
		if err := os.WriteFile(file, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, ""); !errors.Is(err, ErrProvisionStatusDirUnsafe) {
			t.Fatalf("err = %v, want ErrProvisionStatusDirUnsafe", err)
		}
		assertPerm(t, file, 0o600)
		if data, err := os.ReadFile(file); err != nil || string(data) != "file" {
			t.Errorf("outputs file = %q, %v; want it unchanged", data, err)
		}
	})
}

// An outputs directory that already exists below a root the writer cannot
// write (here mode 0500) is still used: mkdirat reports EEXIST before any
// permission error, so the pre-existing branch records the status. This is
// the shape of a root init preparing the directory for the workload.
func TestWriteHarnessProvisionStatus_ExistingDirBelowUnwritableRoot(t *testing.T) {
	root, statusPath := memStatusFixture(t)
	mkdirMode(t, filepath.Dir(statusPath), 0o700)
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) // so t.TempDir can remove it
	if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, ""); err != nil {
		t.Fatalf("WriteHarnessProvisionStatus below a 0500 root: %v", err)
	}
	assertProvisionState(t, statusPath, ProvisionStatusRunning)
	assertPerm(t, root, 0o500)
}

// A symbolic link at the in-memory root itself is refused, even one that a
// trusted-ancestor walk (dirfd.EnsureDirTrustedAncestorFollow) would
// follow, and nothing is created where it points.
func TestWriteHarnessProvisionStatus_RefusesSymlinkedRoot(t *testing.T) {
	uid := os.Geteuid()
	if uid == 0 {
		t.Skip("needs a non-root test process: only a non-root writer creates the directory")
	}
	realRoot := t.TempDir()
	// The link's directory is 0700 whatever the umask and the link's owner
	// is trusted (dirfd.SetTrustedAncestorOwnerUIDForTest below), so a
	// trusted-ancestor walk would follow it: only the strict root walk
	// refuses it.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "mem")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetHarnessDirsRootForTest(link))
	t.Cleanup(SetProvisionStatusGeteuidForTest(func() int { return uid }))
	t.Cleanup(dirfd.SetTrustedAncestorOwnerUIDForTest(uid))
	if err := WriteHarnessProvisionStatus(filepath.Join(link, "outputs", "status.json"), ProvisionStatusRunning, ""); err == nil {
		t.Fatal("expected a symlinked in-memory root to be refused")
	}
	assertDirEmpty(t, realRoot)
}

// Only the outputs directory directly below the in-memory root is ever
// created: never a nested or differently named directory, and never a
// missing root.
func TestWriteHarnessProvisionStatus_CreatesOnlyTheFixedPath(t *testing.T) {
	root, _ := memStatusFixture(t)
	for _, p := range []string{
		filepath.Join(root, "a", "outputs", "status.json"),
		filepath.Join(root, "outputs-other", "status.json"),
		filepath.Join(root, "status.json", "status.json"),
	} {
		if err := WriteHarnessProvisionStatus(p, ProvisionStatusRunning, ""); err == nil {
			t.Errorf("write to %s succeeded, want it refused", p)
		}
	}
	assertDirEmpty(t, root)

	missing := filepath.Join(t.TempDir(), "mem")
	t.Cleanup(SetHarnessDirsRootForTest(missing))
	if err := WriteHarnessProvisionStatus(filepath.Join(missing, "outputs", "status.json"), ProvisionStatusRunning, ""); err == nil {
		t.Error("write below a missing in-memory root succeeded, want it refused")
	}
	assertNotExist(t, missing)
}

// The home bundle's outputs directory is never created, even by a non-root
// writer.
func TestWriteHarnessProvisionStatus_NeverCreatesHomeOutputsDir(t *testing.T) {
	memStatusFixture(t)
	home := t.TempDir()
	outputs := filepath.Join(home, ".scion", "harness", "outputs")
	if err := os.MkdirAll(filepath.Dir(outputs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteHarnessProvisionStatus(filepath.Join(outputs, "status.json"), ProvisionStatusRunning, ""); err == nil {
		t.Fatal("expected the write to fail with no home outputs directory")
	}
	assertNotExist(t, outputs)
}

// A root writer never creates the in-memory outputs directory, and writes
// into an existing one as before, without the non-root ownership and mode
// check (a root init created it for the workload).
func TestWriteHarnessProvisionStatus_RootWriterDoesNotCreate(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(SetHarnessDirsRootForTest(root))
	t.Cleanup(SetProvisionStatusGeteuidForTest(func() int { return 0 }))
	statusPath := filepath.Join(root, "outputs", "status.json")
	if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusRunning, ""); err == nil {
		t.Fatal("expected a root writer to fail with no outputs directory")
	}
	assertNotExist(t, filepath.Dir(statusPath))

	if err := os.Mkdir(filepath.Dir(statusPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(statusPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteHarnessProvisionStatus(statusPath, ProvisionStatusOK, ""); err != nil {
		t.Fatalf("root writer into an existing directory: %v", err)
	}
	assertProvisionState(t, statusPath, ProvisionStatusOK)
}
