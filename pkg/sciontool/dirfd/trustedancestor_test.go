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

	"golang.org/x/sys/unix"
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
	// The seam is set to a uid that can never equal the fixtures' real
	// owner (os.Getuid()), rather than left at its default (0): relying on
	// the default would make this test pass for the wrong reason — and
	// silently stop testing anything — if the suite ever ran as root,
	// where os.Getuid() == 0 == the default trusted uid.
	restore := SetTrustedAncestorOwnerUIDForTest(os.Getuid() + 1)
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

// TestSymlinkTrustedByStat is a table test for symlinkTrustedByStat's
// three independent conditions, each pinned on its own: link uid ==
// trusted, containing-dir uid == trusted, and the containing directory
// carries neither the group- nor the other-write bit. Each row below
// isolates exactly ONE of those conditions as the thing that decides it,
// so deleting any single guard (or masking only one of S_IWGRP/S_IWOTH
// instead of both) flips that row's expected result and fails the test —
// unlike the end-to-end dirfd/stagedsecrets tests, whose fixtures happen
// to fail every condition at once and so cannot tell the guards apart.
func TestSymlinkTrustedByStat(t *testing.T) {
	const trusted = 5
	tests := []struct {
		name    string
		linkUID uint32
		dirUID  uint32
		dirMode uint32
		want    bool
	}{
		{
			name:    "all conditions satisfied",
			linkUID: trusted, dirUID: trusted, dirMode: 0755,
			want: true,
		},
		{
			// Only the link's own uid is wrong; the containing dir's uid
			// and mode are both otherwise trustworthy. Deleting the
			// linkUID == trusted check entirely would make this row
			// incorrectly true.
			name:    "link uid wrong, everything else trustworthy",
			linkUID: trusted + 1, dirUID: trusted, dirMode: 0755,
			want: false,
		},
		{
			// Only the containing directory's uid is wrong. Deleting the
			// dirUID == trusted check entirely would make this row
			// incorrectly true.
			name:    "containing-dir uid wrong, everything else trustworthy",
			linkUID: trusted, dirUID: trusted + 1, dirMode: 0755,
			want: false,
		},
		{
			// dirMode carries ONLY the group-write bit (no other-write, no
			// read/exec bits at all). A mask that checked only S_IWOTH
			// (dropping S_IWGRP) would miss this and return true.
			name:    "containing dir group-writable only",
			linkUID: trusted, dirUID: trusted, dirMode: unix.S_IWGRP,
			want: false,
		},
		{
			// dirMode carries ONLY the other-write bit. A mask that
			// checked only S_IWGRP (dropping S_IWOTH) would miss this and
			// return true.
			name:    "containing dir other-writable only",
			linkUID: trusted, dirUID: trusted, dirMode: unix.S_IWOTH,
			want: false,
		},
		{
			// Sticky bit plus full group/other write: still refused, and
			// for the write-bit reason, not the sticky bit — proves the
			// mask isn't accidentally narrower (or broader) because of an
			// unrelated bit sharing the same mode word.
			name:    "sticky bit with group and other write",
			linkUID: trusted, dirUID: trusted, dirMode: 01777,
			want: false,
		},
		{
			// Sticky bit ALONE, no group/other write at all: must still
			// be trusted. Proves the sticky bit itself plays no part in
			// the decision — a mask that (wrongly) folded the sticky bit
			// in would refuse this and return false.
			name:    "sticky bit alone does not defeat trust",
			linkUID: trusted, dirUID: trusted, dirMode: 01755,
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := symlinkTrustedByStat(tt.linkUID, tt.dirUID, tt.dirMode, trusted)
			if got != tt.want {
				t.Errorf("symlinkTrustedByStat(linkUID=%d, dirUID=%d, dirMode=%#o, trusted=%d) = %v, want %v",
					tt.linkUID, tt.dirUID, tt.dirMode, trusted, got, tt.want)
			}
		})
	}
}

// TestEnsureDirTrustedAncestorFollow_RefusesSymlinkWhoseOwnOwnerIsUntrusted
// proves the call site actually uses the symlink's OWN owner, not its
// containing directory's owner, as the link half of the trust decision.
// An unprivileged test process can only ever create a symlink owned by its
// own real uid, the same uid that already owns the directory it creates
// the symlink in — so a real on-disk fixture alone can never exercise the
// case where the link's own owner differs from an otherwise-trustworthy
// containing directory. This overrides fstatatTrustedAncestor to report a
// fabricated, untrusted owner for the link's own stat while leaving the
// containing directory's independently-fetched stat genuinely real and
// genuinely trusted, so the walk reaches a real symlink and a real
// containing directory throughout — only the one stat field central to
// this test's own question is fabricated.
func TestEnsureDirTrustedAncestorFollow_RefusesSymlinkWhoseOwnOwnerIsUntrusted(t *testing.T) {
	restoreUID := SetTrustedAncestorOwnerUIDForTest(os.Getuid())
	defer restoreUID()

	origFstatat := fstatatTrustedAncestor
	fstatatTrustedAncestor = func(dirfd int, path string, stat *unix.Stat_t, flags int) error {
		if err := origFstatat(dirfd, path, stat, flags); err != nil {
			return err
		}
		stat.Uid = uint32(os.Getuid() + 1)
		return nil
	}
	defer func() { fstatatTrustedAncestor = origFstatat }()

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
		t.Fatal("EnsureDirTrustedAncestorFollow() = nil error, want a refusal when the symlink's own reported owner is untrusted, even though its containing directory is genuinely trusted")
	}

	if _, statErr := os.Stat(filepath.Join(real, "secrets")); !os.IsNotExist(statErr) {
		t.Errorf("real/secrets exists after a refused untrusted-link-owner symlink (stat err=%v); nothing must be created past the refusal", statErr)
	}
}

// TestEnsureDirTrustedAncestorFollow_RootReturnsRootFd proves "/" itself
// resolves rather than being refused — the parent directory of a file
// target such as "/token" — and that the returned fd refers to the real
// root directory: its Fstat device and inode match a fresh Stat of "/".
func TestEnsureDirTrustedAncestorFollow_RootReturnsRootFd(t *testing.T) {
	fd, err := EnsureDirTrustedAncestorFollow("/")
	if err != nil {
		t.Fatalf("EnsureDirTrustedAncestorFollow(%q) = %v, want nil", "/", err)
	}
	defer func() { _ = syscall.Close(fd) }()

	var got unix.Stat_t
	if err := unix.Fstat(fd, &got); err != nil {
		t.Fatalf("Fstat(returned fd): %v", err)
	}
	var want unix.Stat_t
	if err := unix.Stat("/", &want); err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if got.Dev != want.Dev || got.Ino != want.Ino {
		t.Fatalf("returned fd has dev/ino %d/%d, want %d/%d (the root directory)", got.Dev, got.Ino, want.Dev, want.Ino)
	}
}
