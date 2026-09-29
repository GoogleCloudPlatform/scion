/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenParentNoFollow_NormalWrite(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	leafPath := filepath.Join(sub, "file")

	fd, leaf, err := OpenParentNoFollow(leafPath)
	if err != nil {
		t.Fatalf("OpenParentNoFollow: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if leaf != "file" {
		t.Errorf("leaf = %q, want %q", leaf, "file")
	}

	f, err := CreateExclAt(fd, leaf, 0o600)
	if err != nil {
		t.Fatalf("CreateExclAt: %v", err)
	}
	if _, err := f.WriteString("hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q, want %q", data, "hello")
	}
}

// TestOpenParentNoFollow_RefusesIntermediateSymlink proves the walk checks
// every path component, not just the final one: replacing an intermediate
// directory (the kind of thing a process that owns $HOME can always do to
// $HOME/.scion) with a symlink must make the walk fail instead of
// transparently following it into the symlink's target.
func TestOpenParentNoFollow_RefusesIntermediateSymlink(t *testing.T) {
	dir := t.TempDir()
	attacker := filepath.Join(dir, "attacker-dir")
	if err := os.Mkdir(attacker, 0o700); err != nil {
		t.Fatalf("mkdir attacker: %v", err)
	}
	victim := filepath.Join(attacker, "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	// ".scion" is normally a real directory under $HOME; here it's a
	// symlink to an attacker-controlled directory instead.
	scionDir := filepath.Join(dir, ".scion")
	if err := os.Symlink(attacker, scionDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	leafPath := filepath.Join(scionDir, "scion-token")
	if _, _, err := OpenParentNoFollow(leafPath); err == nil {
		t.Fatal("expected an error walking through a symlinked intermediate directory, got nil")
	}

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(data) != "do-not-touch" {
		t.Errorf("victim was modified: %q", data)
	}
}

func TestOpenParentNoFollow_MissingParentErrors(t *testing.T) {
	dir := t.TempDir()
	leafPath := filepath.Join(dir, "does-not-exist", "file")
	if _, _, err := OpenParentNoFollow(leafPath); err == nil {
		t.Fatal("expected an error for a missing parent directory, got nil")
	}
}

func TestRefuseSymlinkOrNonRegularAt(t *testing.T) {
	dir := t.TempDir()
	fd, err := syscall.Open(dir, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()

	t.Run("missing is fine", func(t *testing.T) {
		if err := RefuseSymlinkOrNonRegularAt(fd, "missing"); err != nil {
			t.Errorf("expected nil for a missing entry, got %v", err)
		}
	})

	t.Run("regular file is fine", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "regular"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := RefuseSymlinkOrNonRegularAt(fd, "regular"); err != nil {
			t.Errorf("expected nil for a regular file, got %v", err)
		}
	})

	t.Run("symlink is refused", func(t *testing.T) {
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := RefuseSymlinkOrNonRegularAt(fd, "link"); err == nil {
			t.Error("expected an error for a symlink, got nil")
		}
	})

	t.Run("directory is refused", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := RefuseSymlinkOrNonRegularAt(fd, "subdir"); err == nil {
			t.Error("expected an error for a directory, got nil")
		}
	})

	t.Run("fifo is refused without blocking", func(t *testing.T) {
		fifoPath := filepath.Join(dir, "fifo")
		if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- RefuseSymlinkOrNonRegularAt(fd, "fifo") }()
		select {
		case err := <-done:
			if err == nil {
				t.Error("expected an error for a fifo, got nil")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("RefuseSymlinkOrNonRegularAt blocked on a FIFO with no writer")
		}
	})
}

// isCloexec reports whether fd has FD_CLOEXEC set, via fcntl(F_GETFD).
func isCloexec(t *testing.T, fd int) bool {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatalf("fcntl(F_GETFD): %v", errno)
	}
	return flags&syscall.FD_CLOEXEC != 0
}

// TestFdsAreCloseOnExec proves every fd this package hands back is
// close-on-exec, so it never leaks into a child process this one execs —
// on Substrate, that child is the workload itself, running with dropped
// privileges. Go's raw syscall.Open/Openat, unlike os.OpenFile, do not set
// O_CLOEXEC by default, so this has to be forced explicitly.
func TestFdsAreCloseOnExec(t *testing.T) {
	dir := t.TempDir()

	t.Run("OpenParentNoFollow's parent fd", func(t *testing.T) {
		leafPath := filepath.Join(dir, "a", "file")
		if err := os.MkdirAll(filepath.Dir(leafPath), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		fd, _, err := OpenParentNoFollow(leafPath)
		if err != nil {
			t.Fatalf("OpenParentNoFollow: %v", err)
		}
		defer func() { _ = syscall.Close(fd) }()
		if !isCloexec(t, fd) {
			t.Error("parent fd is not close-on-exec")
		}
	})

	t.Run("CreateExclAt", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(dir, "b"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		fd, _, err := OpenParentNoFollow(filepath.Join(dir, "b", "file"))
		if err != nil {
			t.Fatalf("OpenParentNoFollow: %v", err)
		}
		defer func() { _ = syscall.Close(fd) }()

		f, err := CreateExclAt(fd, "created", 0o600)
		if err != nil {
			t.Fatalf("CreateExclAt: %v", err)
		}
		defer func() { _ = f.Close() }()
		if !isCloexec(t, int(f.Fd())) {
			t.Error("CreateExclAt's fd is not close-on-exec")
		}
	})

	t.Run("OpenAt", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(dir, "c"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "c", "existing"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		fd, _, err := OpenParentNoFollow(filepath.Join(dir, "c", "existing"))
		if err != nil {
			t.Fatalf("OpenParentNoFollow: %v", err)
		}
		defer func() { _ = syscall.Close(fd) }()

		// Deliberately don't OR in O_CLOEXEC here: OpenAt must force it in
		// regardless of what the caller passes.
		f, err := OpenAt(fd, "existing", syscall.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("OpenAt: %v", err)
		}
		defer func() { _ = f.Close() }()
		if !isCloexec(t, int(f.Fd())) {
			t.Error("OpenAt's fd is not close-on-exec even though the caller didn't ask for it")
		}
	})

	t.Run("EnsureDirNoFollow", func(t *testing.T) {
		f, err := EnsureDirNoFollow(filepath.Join(dir, "d"), 0o755)
		if err != nil {
			t.Fatalf("EnsureDirNoFollow: %v", err)
		}
		defer func() { _ = f.Close() }()
		if !isCloexec(t, int(f.Fd())) {
			t.Error("EnsureDirNoFollow's fd is not close-on-exec")
		}
	})
}

// TestEnsureDirNoFollowUnderRoot_CreatesNestedChain proves the normal path
// keeps working: a multi-component chain that does not exist yet is created
// entirely, and the function reports it as under root.
func TestEnsureDirNoFollowUnderRoot_CreatesNestedChain(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")

	dirFd, underRoot, err := EnsureDirNoFollowUnderRoot(root, target, 0o755, 0, 0)
	if err != nil {
		t.Fatalf("EnsureDirNoFollowUnderRoot: %v", err)
	}
	defer func() { _ = syscall.Close(dirFd) }()
	if !underRoot {
		t.Fatal("underRoot = false, want true")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat %s: %v", target, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", target)
	}
}

// TestEnsureDirNoFollowUnderRoot_PreExistingChainUntouched proves a fully
// pre-existing chain is accepted without needing to create anything, and
// without needing any chown at all (uid=1/gid=1 here would fail for an
// unprivileged test process if the function ever tried to chown an
// already-existing component).
func TestEnsureDirNoFollowUnderRoot_PreExistingChainUntouched(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	dirFd, underRoot, err := EnsureDirNoFollowUnderRoot(root, target, 0o755, 1, 1)
	if err != nil {
		t.Fatalf("EnsureDirNoFollowUnderRoot on a fully pre-existing chain must not try to chown it: %v", err)
	}
	defer func() { _ = syscall.Close(dirFd) }()
	if !underRoot {
		t.Fatal("underRoot = false, want true")
	}
}

// TestEnsureDirNoFollowUnderRoot_ChownsOnlyTheComponentItCreates proves that
// when part of the chain already exists and part must be created, only the
// newly created component is ever a chown target: passing an unprivileged
// uid/gid that will fail chown, the resulting error names the created
// component ("new"), not the pre-existing one ("existing").
func TestEnsureDirNoFollowUnderRoot_ChownsOnlyTheComponentItCreates(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(existing, "new")

	// uid 1 is never the current test uid; an unprivileged process cannot
	// chown to it, so this only succeeds if "existing" (pre-existing) is
	// never a chown target at all.
	_, _, err := EnsureDirNoFollowUnderRoot(root, target, 0o755, 1, 1)
	if err == nil {
		t.Fatal("expected a chown failure for the newly created component")
	}
	if !strings.Contains(err.Error(), "new") {
		t.Errorf("error = %v, want it to name the newly created component (\"new\")", err)
	}
	if strings.Contains(err.Error(), "chown existing") {
		t.Errorf("error = %v, must not attempt to chown the pre-existing component", err)
	}
	// The directory was still created even though its chown failed.
	if info, statErr := os.Stat(target); statErr != nil || !info.IsDir() {
		t.Errorf("expected %s to have been created despite the chown failure", target)
	}
}

// TestEnsureDirNoFollowUnderRoot_RefusesSymlinkAtLeaf proves a symlink
// planted at the leaf component is refused before any chown, and the
// symlink's target is left untouched.
func TestEnsureDirNoFollowUnderRoot_RefusesSymlinkAtLeaf(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(root, "leaf")
	if err := os.Symlink(victim, leaf); err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := EnsureDirNoFollowUnderRoot(root, leaf, 0o755, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("expected a refusal for a symlinked leaf")
	}
	gotInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode() != wantInfo.Mode() {
		t.Errorf("victim mode changed: got %v, want %v", gotInfo.Mode(), wantInfo.Mode())
	}
}

// TestEnsureDirNoFollowUnderRoot_RefusesSymlinkAtIntermediateComponent
// proves the same for a symlink at an intermediate component, one level
// above the leaf being ensured.
func TestEnsureDirNoFollowUnderRoot_RefusesSymlinkAtIntermediateComponent(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(root, "mid")
	if err := os.Symlink(victim, intermediate); err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(intermediate, "leaf")
	if _, _, err := EnsureDirNoFollowUnderRoot(root, target, 0o755, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("expected a refusal for a symlinked intermediate component")
	}
	gotInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode() != wantInfo.Mode() {
		t.Errorf("victim mode changed: got %v, want %v", gotInfo.Mode(), wantInfo.Mode())
	}
	if _, err := os.Stat(filepath.Join(victim, "leaf")); !os.IsNotExist(err) {
		t.Error("a new component was created through the symlinked intermediate directory")
	}
}

// TestEnsureDirNoFollowUnderRoot_RefusesNonDirectoryComponent proves a
// regular file sitting where a directory component is expected is refused,
// not silently treated as absent or descended into.
func TestEnsureDirNoFollowUnderRoot_RefusesNonDirectoryComponent(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(blocker, "leaf")
	if _, _, err := EnsureDirNoFollowUnderRoot(root, target, 0o755, 0, 0); err == nil {
		t.Fatal("expected a refusal when a path component is a regular file")
	}
}

// TestEnsureDirNoFollowUnderRoot_OutsideRootReportsFalseUntouched proves a
// path that does not resolve under root at all is reported as such, with a
// nil error and without creating anything, so a caller can fall back to its
// own handling for a legitimate outside-root target.
func TestEnsureDirNoFollowUnderRoot_OutsideRootReportsFalseUntouched(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "a", "b")

	dirFd, underRoot, err := EnsureDirNoFollowUnderRoot(root, target, 0o755, 0, 0)
	if err != nil {
		t.Fatalf("EnsureDirNoFollowUnderRoot: %v", err)
	}
	if dirFd != -1 {
		_ = syscall.Close(dirFd)
	}
	if underRoot {
		t.Fatal("underRoot = true, want false for a target outside root")
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Error("EnsureDirNoFollowUnderRoot must not create anything for an outside-root target")
	}
}

// TestEnsureDirNoFollowUnderRoot_PathEqualsRootIsUnderRoot proves that
// path == root itself is reported as underRoot=true, not an escape: this is
// the shape a file secret directly inside the agent home takes (its parent
// dir IS the home directory). Root already exists, so nothing is created,
// but the call still opens root no-follow to confirm it resolves to a real
// directory.
func TestEnsureDirNoFollowUnderRoot_PathEqualsRootIsUnderRoot(t *testing.T) {
	root := t.TempDir()

	dirFd, underRoot, err := EnsureDirNoFollowUnderRoot(root, root, 0o755, 1, 1)
	if err != nil {
		t.Fatalf("EnsureDirNoFollowUnderRoot(root, root): %v (must not try to chown pre-existing root)", err)
	}
	defer func() { _ = syscall.Close(dirFd) }()
	if !underRoot {
		t.Fatal("underRoot = false, want true for path == root")
	}
}

// TestEnsureDirNoFollowUnderRoot_PathEqualsRootButSymlinkRefused proves the
// path==root case still refuses a symlinked root rather than silently
// reporting it usable.
func TestEnsureDirNoFollowUnderRoot_PathEqualsRootButSymlinkRefused(t *testing.T) {
	parent := t.TempDir()
	victim := t.TempDir()
	root := filepath.Join(parent, "home")
	if err := os.Symlink(victim, root); err != nil {
		t.Fatal(err)
	}

	if _, _, err := EnsureDirNoFollowUnderRoot(root, root, 0o755, 0, 0); err == nil {
		t.Fatal("expected a refusal for a symlinked root even when path == root")
	}
}

// TestEnsureDirNoFollowUnderRoot_RootVsSiblingContainment is the
// containment table: path == root counts as under-root, but a sibling
// path that merely shares root's own string prefix (not a real descendant)
// must NOT — treating root itself as under-root must not loosen
// containment for anything that isn't genuinely root or beneath it.
func TestEnsureDirNoFollowUnderRoot_RootVsSiblingContainment(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "home")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// "home-other" shares the string prefix "home" with root but is a
	// sibling, not a descendant.
	sibling := filepath.Join(parent, "home-other")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"root itself is under root", root, true},
		{"sibling sharing a string prefix is not under root", sibling, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirFd, underRoot, err := EnsureDirNoFollowUnderRoot(root, tt.path, 0o755, 0, 0)
			if err != nil {
				t.Fatalf("EnsureDirNoFollowUnderRoot: %v", err)
			}
			if dirFd != -1 {
				defer func() { _ = syscall.Close(dirFd) }()
			}
			if underRoot != tt.want {
				t.Errorf("underRoot = %v, want %v", underRoot, tt.want)
			}
		})
	}
}
