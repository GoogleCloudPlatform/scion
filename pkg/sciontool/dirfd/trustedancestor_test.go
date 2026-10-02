/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestEnsureDirTrustedAncestorFollow_FollowsTrustedSymlink proves the
// positive case: a symlinked ancestor owned by trustedAncestorOwnerUID,
// sitting in a directory also owned by trustedAncestorOwnerUID with no
// group/other write bit, is followed rather than refused, and a missing
// component past it is still created (mkdirat), matching a system alias
// like "/var/run" -> "/run".
func TestEnsureDirTrustedAncestorFollow_FollowsTrustedSymlink(t *testing.T) {
	restore := SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restore()

	parent := t.TempDir()
	real := filepath.Join(parent, "run")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "varrun")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	// The component past the trusted link ("secrets") does not exist yet:
	// this must be created by the walk, exactly like os.MkdirAll would.
	target := filepath.Join(link, "secrets")

	dirFd, err := EnsureDirTrustedAncestorFollow(target)
	if err != nil {
		t.Fatalf("EnsureDirTrustedAncestorFollow(%s) = %v, want nil (trusted symlink ancestor)", target, err)
	}
	defer func() { _ = syscall.Close(dirFd) }()

	info, err := os.Stat(filepath.Join(real, "secrets"))
	if err != nil {
		t.Fatalf("expected %s/secrets to exist after the walk: %v", real, err)
	}
	if !info.IsDir() {
		t.Errorf("real/secrets is not a directory: %v", info.Mode())
	}
}

// TestEnsureDirTrustedAncestorFollow_RefusesUntrustedOwner proves the first
// negative case: a symlink NOT owned by trustedAncestorOwnerUID is refused
// outright, and nothing is created past it — not even the ordinary file a
// caller would have gone on to write at the resolved destination.
func TestEnsureDirTrustedAncestorFollow_RefusesUntrustedOwner(t *testing.T) {
	// trustedAncestorOwnerUID stays at its default (0). The test's own
	// fixtures are owned by the test process's real, non-root uid in this
	// sandbox, so the symlink below is never trusted.
	parent := t.TempDir()
	real := filepath.Join(parent, "run")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "varrun")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(link, "secrets")

	_, err := EnsureDirTrustedAncestorFollow(target)
	if err == nil {
		t.Fatal("EnsureDirTrustedAncestorFollow() = nil error, want a refusal for an untrusted-owner symlink ancestor")
	}

	if _, statErr := os.Stat(filepath.Join(real, "secrets")); !os.IsNotExist(statErr) {
		t.Errorf("real/secrets exists after a refused untrusted ancestor (stat err=%v); nothing must be created past the refusal", statErr)
	}
}

// TestEnsureDirTrustedAncestorFollow_RefusesGroupOtherWritableParent proves
// the second negative case: even a symlink OWNED by trustedAncestorOwnerUID
// is refused if its own containing directory carries the group- or
// other-write bit — a workload with write access to that directory could
// have replanted the symlink itself, so ownership of the link alone is not
// enough. Nothing is created past the refusal.
func TestEnsureDirTrustedAncestorFollow_RefusesGroupOtherWritableParent(t *testing.T) {
	restore := SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restore()

	parent := t.TempDir()
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(parent, "run")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "varrun")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(link, "secrets")

	_, err := EnsureDirTrustedAncestorFollow(target)
	if err == nil {
		t.Fatal("EnsureDirTrustedAncestorFollow() = nil error, want a refusal for a symlink in a group/other-writable directory")
	}

	if _, statErr := os.Stat(filepath.Join(real, "secrets")); !os.IsNotExist(statErr) {
		t.Errorf("real/secrets exists after a refused world-writable-parent ancestor (stat err=%v); nothing must be created past the refusal", statErr)
	}
}

// TestEnsureDirTrustedAncestorFollow_CreatesMissingComponents proves the
// os.MkdirAll-replacement half of the contract on its own, with no symlink
// involved at all: every missing component along the chain is created.
func TestEnsureDirTrustedAncestorFollow_CreatesMissingComponents(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "a", "b", "c")

	dirFd, err := EnsureDirTrustedAncestorFollow(target)
	if err != nil {
		t.Fatalf("EnsureDirTrustedAncestorFollow(%s) = %v, want nil", target, err)
	}
	_ = syscall.Close(dirFd)

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", target, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory: %v", target, info.Mode())
	}
}

// TestEnsureDirTrustedAncestorFollow_AbsoluteTargetRestartsFromRoot proves
// an absolute symlink target is resolved from "/" again, not relative to
// the symlink's own containing directory — matching how the kernel itself
// continues resolving a path after dereferencing an absolute-target
// symlink. This anchors the walk's fixture under a *second* independent
// temp directory, reachable only via the absolute symlink target, so a
// relative-continuation bug (which would instead look for the rest of the
// path under the FIRST temp directory) fails this test.
func TestEnsureDirTrustedAncestorFollow_AbsoluteTargetRestartsFromRoot(t *testing.T) {
	restore := SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restore()

	linkParent := t.TempDir()
	realParent := t.TempDir()
	real := filepath.Join(realParent, "run")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkParent, "varrun")
	// An absolute target: real lives under a wholly different temp
	// directory than link does.
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(link, "secrets")

	dirFd, err := EnsureDirTrustedAncestorFollow(target)
	if err != nil {
		t.Fatalf("EnsureDirTrustedAncestorFollow(%s) = %v, want nil", target, err)
	}
	_ = syscall.Close(dirFd)

	if _, statErr := os.Stat(filepath.Join(real, "secrets")); statErr != nil {
		t.Errorf("expected %s/secrets to exist (absolute target resolved from /): %v", real, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(linkParent, "secrets")); !os.IsNotExist(statErr) {
		t.Errorf("secrets created under %s instead of following the absolute target from /", linkParent)
	}
}

// TestEnsureDirTrustedAncestorFollow_RelativeTargetContinuesFromLinkDir
// proves a relative symlink target continues from the symlink's OWN
// containing directory, not from "/" — the opposite restart rule from the
// absolute-target case above, and the one real system aliases like a
// relative "/var/run -> ../run" would need.
func TestEnsureDirTrustedAncestorFollow_RelativeTargetContinuesFromLinkDir(t *testing.T) {
	restore := SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restore()

	parent := t.TempDir()
	varDir := filepath.Join(parent, "var")
	if err := os.Mkdir(varDir, 0755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(parent, "run")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	// "var/run" -> "../run" (relative): resolving it must continue from
	// "var"'s own containing directory (parent), landing on parent/run —
	// exactly what a real "/var/run -> ../run" alias means on a real root.
	link := filepath.Join(varDir, "run")
	if err := os.Symlink("../run", link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(link, "secrets")

	dirFd, err := EnsureDirTrustedAncestorFollow(target)
	if err != nil {
		t.Fatalf("EnsureDirTrustedAncestorFollow(%s) = %v, want nil", target, err)
	}
	_ = syscall.Close(dirFd)

	if _, statErr := os.Stat(filepath.Join(real, "secrets")); statErr != nil {
		t.Errorf("expected %s/secrets to exist (relative target resolved from the link's own dir): %v", real, statErr)
	}
}

// TestEnsureDirTrustedAncestorFollow_RefusesSymlinkLoop proves the hop
// bound: a cycle of individually-trusted symlinks (a -> b, b -> a) is
// refused via ErrTooManyTrustedAncestorSymlinks instead of spinning
// forever.
func TestEnsureDirTrustedAncestorFollow_RefusesSymlinkLoop(t *testing.T) {
	restore := SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restore()

	parent := t.TempDir()
	a := filepath.Join(parent, "a")
	b := filepath.Join(parent, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}

	_, err := EnsureDirTrustedAncestorFollow(filepath.Join(a, "x"))
	if err == nil {
		t.Fatal("EnsureDirTrustedAncestorFollow() = nil error, want a refusal for a symlink loop")
	}
	if !errors.Is(err, ErrTooManyTrustedAncestorSymlinks) {
		t.Errorf("error = %v, want errors.Is(..., ErrTooManyTrustedAncestorSymlinks)", err)
	}
}
