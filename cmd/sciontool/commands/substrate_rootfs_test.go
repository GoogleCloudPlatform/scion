/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

// trustedTestRoot creates a fresh directory to stand in for a fixup's
// filesystem root, anchored under realHomeDir rather than t.TempDir()
// (which resolves under the real, world-writable "/tmp").
// stripSudoSetuidBits now verifies the whole ancestor chain of each
// candidate it strips (dirfd.VerifyRootOwnedExecutable, added for the
// symlinked-sudo fix), so a fixture rooted under "/tmp" would fail that
// check before ever reaching the fixture's own planted binary. realHomeDir
// is this test binary's real, original $HOME (captured before $HOME is
// redirected to a throwaway sandbox for the rest of the suite — see its own
// doc comment in testmain_test.go), so it is genuinely trusted without
// needing real root to construct.
func trustedTestRoot(t *testing.T) string {
	t.Helper()
	if realHomeDir == "" {
		t.Skip("cannot resolve a real home directory to anchor a trusted fixture root under")
	}
	dir, err := os.MkdirTemp(realHomeDir, "sudo-fixup-test-*")
	if err != nil {
		t.Skipf("cannot create a trusted fixture root under %s: %v", realHomeDir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestCanSearchDir_OwnerFirst pins the kernel's own permission-check order
// that canSearchDir's doc comment claims: once a directory's owning uid
// matches the target uid, only the owner bit decides traversability —
// never falling through to the group or other bits, no matter how
// permissive they are. Every other canSearchDir-exercising test in this
// package uses directories the target uid doesn't own, so none of them
// distinguish checking the owner bit from checking the "other" bit; this is
// the one that pins the owner-first rule directly.
func TestCanSearchDir_OwnerFirst(t *testing.T) {
	const uid, gid = 1000, 1000
	cases := []struct {
		name string
		mode fs.FileMode
		want bool
	}{
		// Owner has no bits at all, but group (which also matches gid) and
		// other both have full rwx. The owner branch must still win.
		{"owner empty, group+other rwx (0o070)", fs.ModeDir | 0o070, false},
		{"owner empty, group+other rwx (0o077)", fs.ModeDir | 0o077, false},
		// Owner has read+write but not execute; other has execute. The
		// owner branch must still win (no execute bit there means false),
		// not fall through to other's execute bit.
		{"owner rw only, other x (0o601)", fs.ModeDir | 0o601, false},
		// Control: owner does have the execute bit.
		{"owner has x (0o100)", fs.ModeDir | 0o100, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := fakeFileInfo{mode: c.mode, uid: uid, gid: gid}
			if got := canSearchDir(info, uid, gid); got != c.want {
				t.Errorf("canSearchDir(mode=%v, owned by the target uid/gid) = %v, want %v", c.mode, got, c.want)
			}
		})
	}
}

// TestFixupRootfsForScion_WidensRestrictiveRoot proves condition (i) from
// fixupRootfsForScion's doc comment: a rootfs whose '/' comes up too
// restrictive to traverse (e.g. 0700, from bundle_linux.go's MkdirAll) gets
// widened to 0755.
func TestFixupRootfsForScion_WidensRestrictiveRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	fixupRootfsForScion(root, home, 1000, 1000)

	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("root mode = %o, want 0755", got)
	}
}

// TestFixupRootfsForScion_LeavesAlreadyWideRootAlone proves the "only ever
// widen" half of the chmod's own doc comment: with the perm&0o755 != 0o755
// guard in place, a pre-existing 0777 root is left unchanged rather than
// narrowed to 0755.
func TestFixupRootfsForScion_LeavesAlreadyWideRootAlone(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	fixupRootfsForScion(root, home, 1000, 1000)

	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o777 {
		t.Errorf("root mode = %o, want unchanged 0777", got)
	}
}

// TestFixupRootfsForScion_ChownsRootOwnedHomeEntriesThenIsIdempotent proves
// condition (ii): root-owned entries under home get chowned to the target
// uid/gid via chownTreeRootOwned, and a second pass — once a real chown(2)
// has actually taken effect — makes no further changes.
//
// This drives chownTreeRootOwned's own injectable chownTreeRootOwnedFilter
// (init.go) rather than real root-owned files, since this test process has
// neither — but the actual chown(2) chownTreeRootOwned (via
// dirfd.ChownTreeNoFollow) issues runs for real, to uid/gid = the test's own
// uid/gid (always permitted, and still bumps ctime — see
// TestWriteEnvFile_DirChownSurvivesSwapAfterWrite for the same technique).
// The filter fakes "every entry looks root-owned" until simulateFixed flips,
// standing in for what a real chown(2) would leave behind on disk once the
// first pass has actually run.
//
// This also proves chownTreeRootOwned's own filter-driven skip: on the
// second pass, once the filter reports every entry as already fixed, no
// further chown(2) call runs, so ctime does not change.
func TestFixupRootfsForScion_ChownsRootOwnedHomeEntriesThenIsIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sub", "b"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		// Already correct: isolates this test to the home/chown behaviour,
		// independent of the root-chmod tests above.
		t.Fatal(err)
	}

	origFilter := chownTreeRootOwnedFilter
	t.Cleanup(func() { chownTreeRootOwnedFilter = origFilter })

	var simulateFixed bool
	chownTreeRootOwnedFilter = func(uint32) bool { return !simulateFixed }

	uid, gid := os.Getuid(), os.Getgid()
	aCtimeBefore := ctimeOfFile(t, filepath.Join(home, "a"))
	// A brief settle so the ctime bump below is measurably distinct even on
	// filesystems/clock sources with coarse timestamp resolution.
	time.Sleep(15 * time.Millisecond)

	fixupRootfsForScion(root, home, uid, gid)
	aCtimeAfterFirst := ctimeOfFile(t, filepath.Join(home, "a"))
	if aCtimeAfterFirst == aCtimeBefore {
		t.Fatal("first call: expected root-owned home entries to be chowned, ctime did not advance")
	}

	simulateFixed = true
	time.Sleep(15 * time.Millisecond)
	fixupRootfsForScion(root, home, uid, gid)
	aCtimeAfterSecond := ctimeOfFile(t, filepath.Join(home, "a"))
	if aCtimeAfterSecond != aCtimeAfterFirst {
		t.Error("second call: ctime advanced again — a real chown(2) would already have fixed ownership by now")
	}
}

// ctimeOfFile returns path's change time — chown(2) always bumps it, even
// when it sets the same uid/gid a file already has, so "ctime advanced" is a
// reliable proxy for "chown actually ran against this path" without needing
// real root.
func ctimeOfFile(t *testing.T, path string) syscall.Timespec {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no *syscall.Stat_t for %s", path)
	}
	return statCtime(st)
}

// TestFixupWorldWritableTmpDirSticky_SetsStickyOnWorldWritable proves
// condition (iii) from fixupRootfsForScion's doc comment: a world-writable
// directory missing the sticky bit (the observed Substrate /tmp state) gets
// set to 01777.
func TestFixupWorldWritableTmpDirSticky_SetsStickyOnWorldWritable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	if changed := fixupWorldWritableTmpDirSticky(dir); !changed {
		t.Fatal("expected fixupWorldWritableTmpDirSticky to report a change")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSticky == 0 {
		t.Errorf("mode = %v, want the sticky bit set", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o777 {
		t.Errorf("perm = %o, want 0777", got)
	}
}

// TestFixupWorldWritableTmpDirSticky_OnlyAddsStickyBit proves the fixup
// adds just the sticky bit rather than forcing the mode to exactly 01777:
// a world-writable directory that's more restrictive than 0777 elsewhere
// (e.g. 0773, no group-other split relevant here) must keep those other
// bits as they were.
func TestFixupWorldWritableTmpDirSticky_OnlyAddsStickyBit(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o773); err != nil {
		t.Fatal(err)
	}

	if changed := fixupWorldWritableTmpDirSticky(dir); !changed {
		t.Fatal("expected fixupWorldWritableTmpDirSticky to report a change")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSticky == 0 {
		t.Errorf("mode = %v, want the sticky bit set", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o773 {
		t.Errorf("perm = %o, want unchanged 0773 (sticky bit added, not widened to 0777)", got)
	}
}

// TestFixupWorldWritableTmpDirSticky_LeavesAlreadyStickyAlone is the "only
// ever fix the missing-sticky case" idempotency check: a directory that
// already has the sticky bit (e.g. a real 1777 /tmp) is left completely
// alone, and fixupWorldWritableTmpDirSticky reports no change.
func TestFixupWorldWritableTmpDirSticky_LeavesAlreadyStickyAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}

	if changed := fixupWorldWritableTmpDirSticky(dir); changed {
		t.Error("expected no change for an already-sticky directory")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSticky == 0 {
		t.Error("sticky bit should still be set")
	}
	if got := info.Mode().Perm(); got != 0o777 {
		t.Errorf("perm = %o, want unchanged 0777", got)
	}
}

// TestFixupWorldWritableTmpDirSticky_LeavesNonWorldWritableAlone proves the
// fixup never widens or narrows a directory that isn't already
// world-writable — this is a defence-in-depth fixup for the observed
// 0777-no-sticky case specifically, not a general "make /tmp public" step.
func TestFixupWorldWritableTmpDirSticky_LeavesNonWorldWritableAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if changed := fixupWorldWritableTmpDirSticky(dir); changed {
		t.Error("expected no change for a non-world-writable directory")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("perm = %o, want unchanged 0755", got)
	}
}

// TestFixupWorldWritableTmpDirSticky_MissingDirIsNoop proves a rootfs
// without the directory at all (every current test double, and any real
// rootfs image that lacks a /var/tmp) is handled as "nothing to fix", not
// an error.
func TestFixupWorldWritableTmpDirSticky_MissingDirIsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	if changed := fixupWorldWritableTmpDirSticky(dir); changed {
		t.Error("expected no change for a missing directory")
	}
}

// TestFixupWorldWritableTmpDirSticky_RefusesSymlink proves that a symlink
// planted at the tmp-dir path (instead of a real directory) is refused via
// O_NOFOLLOW rather than followed to whatever it points at.
func TestFixupWorldWritableTmpDirSticky_RefusesSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target-dir")
	if err := os.Mkdir(target, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "tmp")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if changed := fixupWorldWritableTmpDirSticky(link); changed {
		t.Error("expected no change when the tmp-dir path is a symlink")
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSticky != 0 {
		t.Error("symlink target must be untouched, but the sticky bit was set")
	}
}

// TestFixupRootfsForScion_FixesTmpAndVarTmpStickyBit proves
// fixupRootfsForScion itself drives the /tmp and /var/tmp fixup (not just
// the standalone helper), deriving both paths from its root parameter, and
// that the combined info log line's gate includes them (the "only when
// something changed" idempotency guard from the two-condition version of
// this test above).
func TestFixupRootfsForScion_FixesTmpAndVarTmpStickyBit(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	// Mkdir's mode argument is subject to umask, so chmod explicitly
	// afterwards to get the exact 0777-no-sticky starting condition this
	// test needs, the same way the root-chmod tests above do.
	tmpDir := filepath.Join(root, "tmp")
	if err := os.Mkdir(tmpDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpDir, 0o777); err != nil {
		t.Fatal(err)
	}
	varTmpDir := filepath.Join(root, "var", "tmp")
	if err := os.MkdirAll(varTmpDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(varTmpDir, 0o777); err != nil {
		t.Fatal(err)
	}

	fixupRootfsForScion(root, home, 1000, 1000)

	for _, dir := range []string{tmpDir, varTmpDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSticky == 0 {
			t.Errorf("%s: mode = %v, want the sticky bit set", dir, info.Mode())
		}
		if got := info.Mode().Perm(); got != 0o777 {
			t.Errorf("%s: perm = %o, want 0777", dir, got)
		}
	}

	// Idempotent: a second call makes no further changes and doesn't error.
	fixupRootfsForScion(root, home, 1000, 1000)
	for _, dir := range []string{tmpDir, varTmpDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o777 {
			t.Errorf("%s: perm = %o, want unchanged 0777 after the second call", dir, got)
		}
	}
}

// TestFixupRootfsForScion_NoopWhenAlreadyCorrect proves the doc comment's
// "idempotent: when neither condition needs fixing, this makes no changes"
// claim end to end, using the real (non-faked) chownTreeRootOwnedFilter
// (isRootOwned) and dirfd.ChownTreeNoFollow: a t.TempDir()'s entries are
// owned by the test's own uid, not root, so nothing here should be touched
// at all. Combined with
// TestFixupRootfsForScion_ChownsRootOwnedHomeEntriesThenIsIdempotent above
// (which proves the info-on-change log line stays silent by inspection:
// fixupRootfsForScion's only log.Info call is gated on
// rootChanged || homeChanged > 0, and that test already proves both are
// false/0 on a no-op pass — the unconditional log.Debug line logs on every
// call, on purpose, and isn't part of this claim), this covers the case
// where they start out false/0 rather than becoming so after a first fix.
func TestFixupRootfsForScion_NoopWhenAlreadyCorrect(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	beforeUID, ok := ownerUIDOf(statFile(t, filepath.Join(home, "a")))
	if !ok {
		t.Fatal("could not read the test file's owning uid")
	}

	fixupRootfsForScion(root, home, 1000, 1000)

	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := rootInfo.Mode().Perm(); got != 0o755 {
		t.Errorf("root mode = %o, want unchanged 0755", got)
	}
	afterUID, ok := ownerUIDOf(statFile(t, filepath.Join(home, "a")))
	if !ok {
		t.Fatal("could not read the test file's owning uid")
	}
	if afterUID != beforeUID {
		t.Errorf("home entry's owning uid changed from %d to %d, want left alone (it wasn't root-owned)", beforeUID, afterUID)
	}
}

func statFile(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// ownerUIDOf reads a file's owning uid from its already-stat'd fs.FileInfo,
// for fixtures in this file only: chownTreeRootOwned's real implementation
// reads ownership via its own fd-relative fstatat, not this path.
func ownerUIDOf(info fs.FileInfo) (uid uint32, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

// plantSetuidSudo creates a regular file at root/dir/sudo with the setuid,
// setgid, and sticky bits all set, standing in for an image that shipped a
// setuid-root sudo binary. os.Chmod's own special-bit handling is used
// directly (unlike a plain executable's permission bits, Go's os.Chmod does
// apply os.ModeSetuid/ModeSetgid/ModeSticky correctly), so no umask
// workaround is needed the way the plain-permission tests above need one.
func plantSetuidSudo(t *testing.T, root, dir string) string {
	t.Helper()
	fullDir := filepath.Join(root, dir)
	if err := os.MkdirAll(fullDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fullDir, "sudo")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, os.ModeSetuid|os.ModeSetgid|os.ModeSticky|0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSudoCheckDirs_MatchesRootexecSudoCheckDirs pins sudoCheckDirs to its
// single source of truth: every other test in this file that loops over
// sudoCheckDirs to prove the fixup or the precondition reaches "every
// candidate directory" would still pass even if sudoCheckDirs were
// hardcoded to a narrower list (they only ever check that the loop
// variable itself is covered, not what that list should actually be) — so
// narrowing sudoCheckDirs to, say,
// {"usr/sbin","usr/bin","sbin","bin"} would silently stop checking
// "usr/local/sbin"/"usr/local/bin" without any of them failing.
func TestSudoCheckDirs_MatchesRootexecSudoCheckDirs(t *testing.T) {
	want := rootexec.SudoCheckDirs()
	if !reflect.DeepEqual(sudoCheckDirs, want) {
		t.Errorf("sudoCheckDirs = %v, want %v (rootexec.SudoCheckDirs())", sudoCheckDirs, want)
	}
}

// TestStripSudoSetuidBits_StripsSetuidSetgidStickyFromEveryCandidateDir
// proves stripSudoSetuidBits reaches every one of sudoCheckDirs, strips all
// three special bits (not just setuid), and leaves the file's other
// permission bits and content untouched.
func TestStripSudoSetuidBits_StripsSetuidSetgidStickyFromEveryCandidateDir(t *testing.T) {
	root := trustedTestRoot(t)
	var paths []string
	for _, dir := range sudoCheckDirs {
		paths = append(paths, plantSetuidSudo(t, root, dir))
	}

	if !stripSudoSetuidBits(root) {
		t.Fatal("stripSudoSetuidBits() = false, want true (planted setuid binaries)")
	}

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s: mode = %v, want setuid/setgid/sticky all cleared", path, info.Mode())
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s: perm = %o, want unchanged 0755", path, info.Mode().Perm())
		}
	}

	// Idempotent: a second call finds nothing left to strip.
	if stripSudoSetuidBits(root) {
		t.Error("stripSudoSetuidBits() second call = true, want false (nothing left to strip)")
	}
}

// TestStripSudoSetuidBits_RefusesSymlinkThroughUntrustedDir proves a
// symlinked "sudo" whose real destination sits behind a workload-writable
// directory is left untouched: dirfd.VerifyRootOwnedExecutable's ancestor
// chain check refuses the whole candidate before stripSetuidBitsNoFollow
// ever opens anything, so a workload that can plant a writable directory
// somewhere in the chain can't get its own escalation target quietly
// "fixed" (and thereby have findSetuidRootSudo start trusting it) by this
// pass.
func TestStripSudoSetuidBits_RefusesSymlinkThroughUntrustedDir(t *testing.T) {
	root := trustedTestRoot(t)
	dir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	untrustedDir := filepath.Join(root, "untrusted")
	if err := os.MkdirAll(untrustedDir, 0o777); err != nil {
		t.Fatal(err)
	}
	// MkdirAll's mode is subject to umask, so explicitly chmod to guarantee
	// this directory really is other-writable regardless of the process's
	// umask.
	if err := os.Chmod(untrustedDir, 0o777); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(untrustedDir, "victim-sudo")
	if err := os.WriteFile(victim, []byte("victim"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "sudo")); err != nil {
		t.Fatal(err)
	}

	stripSudoSetuidBits(root)

	info, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Error("victim's setuid bit was cleared through a symlink whose real destination sits behind a workload-writable directory; the whole chain must be verified trusted first")
	}
}

// TestStripSudoSetuidBits_NeutralizesFullyTrustedSymlinkedSudo proves the
// positive-path counterpart above: when every hop of a symlinked "sudo"
// candidate (e.g. an alternatives-managed sudo implementation) is
// root/self-owned and free of group/other write, stripSudoSetuidBits
// follows it and strips the special bits from the REAL destination — not a
// no-op on the link itself, which would otherwise leave a still-setuid
// binary that findSetuidRootSudo's own symlink-following check would then
// refuse to boot past (see TestFindSetuidRootSudo_PassesAfterFixupNeutralizesSymlinkedSudo
// below for that half of the property).
func TestStripSudoSetuidBits_NeutralizesFullyTrustedSymlinkedSudo(t *testing.T) {
	root := trustedTestRoot(t)
	dir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(root, "libexec")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(realDir, "sudo-real")
	if err := os.WriteFile(victim, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, os.ModeSetuid|os.ModeSetgid|os.ModeSticky|0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "sudo")); err != nil {
		t.Fatal(err)
	}

	if !stripSudoSetuidBits(root) {
		t.Fatal("stripSudoSetuidBits() = false, want true (a fully-trusted symlinked sudo should be neutralized)")
	}

	info, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("victim's special bits = %v after stripSudoSetuidBits, want all three cleared", info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
	}
}

// findSetuidRootSudoStatPath builds a statPath for findSetuidRootSudo that
// reads a candidate's REAL, current mode from the fixture rooted at root
// (so it reflects whatever stripSudoSetuidBits actually left behind) while
// reporting a synthetic root ownership (uid 0) for that candidate —
// findSetuidRootSudo only treats a setuid binary as dangerous when it's
// root-owned, a property this self-owned test fixture can't reproduce for
// real without running the whole suite as root.
func findSetuidRootSudoStatPath(root string) func(string) (fs.FileInfo, error) {
	return func(candidate string) (fs.FileInfo, error) {
		info, err := os.Stat(filepath.Join(root, candidate))
		if err != nil {
			return nil, err
		}
		return fakeFileInfo{mode: info.Mode(), uid: 0, gid: 0}, nil
	}
}

// TestFindSetuidRootSudo_PassesAfterFixupNeutralizesSymlinkedSudo is the
// required end-to-end pairing: a symlinked sudo whose whole chain is
// trusted is (a) neutralized by stripSudoSetuidBits and (b) no longer
// reported by findSetuidRootSudo afterward — the precondition that gated
// bootstrap on this fixup having worked must actually pass once it has, or
// a symlinked sudo binary would permanently refuse every future bootstrap.
func TestFindSetuidRootSudo_PassesAfterFixupNeutralizesSymlinkedSudo(t *testing.T) {
	root := trustedTestRoot(t)
	dir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(root, "libexec")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(realDir, "sudo-real")
	if err := os.WriteFile(victim, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "sudo")); err != nil {
		t.Fatal(err)
	}

	statPath := findSetuidRootSudoStatPath(root)
	if found := findSetuidRootSudo(statPath); found == "" {
		t.Fatal("findSetuidRootSudo found nothing before the fixup ran; test fixture is not set up correctly")
	}

	stripSudoSetuidBits(root)

	if found := findSetuidRootSudo(statPath); found != "" {
		t.Errorf("findSetuidRootSudo() = %q after stripSudoSetuidBits ran through a fully-trusted symlink chain, want \"\" (the fixup should have neutralized the real destination)", found)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it. log.write always writes to whatever os.Stderr
// currently is (read fresh on each call, never cached), so this needs no
// change to the log package itself. Not safe to run with t.Parallel(); none
// of this file's tests use it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	// Some other test elsewhere in this package's suite exercises a real
	// cobra command invocation that calls log.SetQuiet(true) without ever
	// resetting it (it's the production PersistentPreRun behavior for hook
	// subcommands, not a test-only setting), which would otherwise silently
	// suppress every stderr write regardless of test order. Force it off
	// for the duration of this capture so the result reflects this test's
	// own behavior, not accumulated global state from elsewhere in the
	// suite.
	log.SetQuiet(false)
	t.Cleanup(func() { log.SetQuiet(false) })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })
	fn()
	os.Stderr = orig
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// TestAncestorSymlinkIsRootOwned_TrueForSelfOwnedSymlinkedDir proves the
// positive path of the exact predicate that decides stripSetuidBitsNoFollow's
// log level: a candidate whose immediate parent directory entry is itself a
// symlink owned by this process (the merged-/usr compatibility shape, e.g.
// "/bin" -> "usr/bin", is root-owned on a real substrate actor; a
// non-root test process reproduces the same shape with a self-owned
// symlink, since dirfd's own chainIsTrusted treats uid-or-self identically
// everywhere else in this trust chain).
func TestAncestorSymlinkIsRootOwned_TrueForSelfOwnedSymlinkedDir(t *testing.T) {
	root := trustedTestRoot(t)
	real := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if !ancestorSymlinkIsRootOwned(filepath.Join(link, "sudo")) {
		t.Error("ancestorSymlinkIsRootOwned() = false, want true for a self-owned symlinked parent directory")
	}
}

// TestAncestorSymlinkIsRootOwned_FalseForPlainDirectory proves the negative
// path: an ordinary (non-symlink) parent directory is never classified as
// the merged-/usr compatibility case, regardless of its ownership — this is
// what keeps a genuinely untrusted chain (e.g. a workload-writable
// directory, which VerifyRootOwnedExecutable already refuses on its own
// merits) logged at ERROR rather than downgraded.
func TestAncestorSymlinkIsRootOwned_FalseForPlainDirectory(t *testing.T) {
	root := trustedTestRoot(t)
	dir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if ancestorSymlinkIsRootOwned(filepath.Join(dir, "sudo")) {
		t.Error("ancestorSymlinkIsRootOwned() = true, want false for a plain (non-symlink) parent directory")
	}
}

// TestAncestorSymlinkIsRootOwned_FalseWhenParentMissing proves the
// fail-closed default: a candidate whose parent cannot even be statted
// (e.g. it doesn't exist) is never classified as the compatibility case.
func TestAncestorSymlinkIsRootOwned_FalseWhenParentMissing(t *testing.T) {
	root := trustedTestRoot(t)
	if ancestorSymlinkIsRootOwned(filepath.Join(root, "does-not-exist", "sudo")) {
		t.Error("ancestorSymlinkIsRootOwned() = true, want false when the parent directory does not exist")
	}
}

// TestStripSetuidBitsNoFollow_MergedUsrCompatSymlinkLogsDebugNotError is the
// literal, observable version of the classification fix: with a merged-/usr
// shaped fixture (a self-owned symlinked "bin" standing in for the real
// image's root-owned one), the candidate reached only through that
// compatibility symlink is skipped WITHOUT an ERROR line — and, since
// log.Debug only emits with debug logging enabled, this also proves nothing
// escalates to ERROR once debug is off (the default in production).
func TestStripSetuidBitsNoFollow_MergedUsrCompatSymlinkLogsDebugNotError(t *testing.T) {
	root := trustedTestRoot(t)
	realDir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "bin")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(linkDir, "sudo")

	log.SetDebug(true)
	t.Cleanup(func() { log.SetDebug(false) })

	output := captureStderr(t, func() {
		if stripSetuidBitsNoFollow(candidate) {
			t.Error("stripSetuidBitsNoFollow() = true for a candidate that was never actually written")
		}
	})

	if strings.Contains(output, "ERROR") {
		t.Errorf("stderr = %q, want no ERROR line for a candidate reached only through a self/root-owned compatibility symlink", output)
	}
	if !strings.Contains(output, "DEBUG") {
		t.Errorf("stderr = %q, want a DEBUG line explaining the skip", output)
	}
}

// TestStripSetuidBitsNoFollow_UntrustedAncestorStillLogsError proves the
// paired negative: a candidate that fails VerifyRootOwnedExecutable for a
// reason OTHER than a compatibility symlink (here, a plain, non-symlink
// parent directory that VerifyRootOwnedExecutable still won't produce a
// leaf for — a missing grandparent making the whole lookup fail with an
// error that is not ENOENT-shaped at the leaf) still logs at ERROR, and the
// candidate is still refused. Real-world equivalent: a workload-writable
// directory somewhere in the chain, or any other unexpected failure —
// ancestorSymlinkIsRootOwned only ever downgrades the one narrow shape it
// exists for.
func TestStripSetuidBitsNoFollow_UntrustedAncestorStillLogsError(t *testing.T) {
	root := trustedTestRoot(t)
	dir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	untrustedDir := filepath.Join(root, "untrusted")
	if err := os.MkdirAll(untrustedDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(untrustedDir, 0o777); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(untrustedDir, "victim-sudo")
	if err := os.WriteFile(victim, []byte("victim"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "sudo")
	if err := os.Symlink(victim, candidate); err != nil {
		t.Fatal(err)
	}

	log.SetDebug(true)
	t.Cleanup(func() { log.SetDebug(false) })

	output := captureStderr(t, func() {
		if stripSetuidBitsNoFollow(candidate) {
			t.Error("stripSetuidBitsNoFollow() = true for a symlink through a workload-writable directory")
		}
	})

	if !strings.Contains(output, "ERROR") {
		t.Errorf("stderr = %q, want an ERROR line for a symlink reached through a workload-writable directory", output)
	}

	info, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Error("victim's setuid bit was cleared despite sitting behind a workload-writable directory")
	}
}

// TestStripSetuidBitsNoFollow_MissingCandidateLogsNothing proves a candidate
// directory that simply doesn't exist at all (the common case for every
// SearchPath entry that doesn't ship "sudo") produces no log line at any
// level. VerifyRootOwnedExecutable's error here is %w-wrapped, so this
// checks it with errors.Is(err, fs.ErrNotExist), not os.IsNotExist, to
// actually recognize it.
func TestStripSetuidBitsNoFollow_MissingCandidateLogsNothing(t *testing.T) {
	root := trustedTestRoot(t)
	candidate := filepath.Join(root, "usr", "local", "sbin", "sudo")

	log.SetDebug(true)
	t.Cleanup(func() { log.SetDebug(false) })

	output := captureStderr(t, func() {
		if stripSetuidBitsNoFollow(candidate) {
			t.Error("stripSetuidBitsNoFollow() = true for a candidate whose directory doesn't exist")
		}
	})

	if output != "" {
		t.Errorf("stderr = %q, want no log line at all for a simply-missing candidate", output)
	}
}

// TestStripSudoSetuidBits_MergedUsrCompatSymlinkStillStripsRealSudo proves
// the log-level change above never affects the actual outcome: even though
// the "bin" entry is skipped (reached only through a compatibility
// symlink), stripSudoSetuidBits still finds and neutralizes the real sudo
// binary through its own, more direct "usr/bin" candidate — the trust
// decision is unchanged, only the noise around one redundant alias is.
func TestStripSudoSetuidBits_MergedUsrCompatSymlinkStillStripsRealSudo(t *testing.T) {
	root := trustedTestRoot(t)
	realDir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "bin")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	sudoPath := plantSetuidSudo(t, root, "usr/bin")

	if !stripSudoSetuidBits(root) {
		t.Fatal("stripSudoSetuidBits() = false, want true (the real sudo binary should still be found and stripped)")
	}
	info, err := os.Lstat(sudoPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("sudo's special bits = %v after stripSudoSetuidBits, want all three cleared", info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
	}
}

// TestRemoveSudoersGrants_RemovesBothNamesLeavesOthersAlone proves both
// grant files are removed, an unrelated file in the same directory survives,
// and a missing directory or missing individual grant is a benign no-op.
func TestRemoveSudoersGrants_RemovesBothNamesLeavesOthersAlone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "etc", "sudoers.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range sudoersGrantNames {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("scion ALL=(ALL) NOPASSWD:ALL\n"), 0o440); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "README")
	if err := os.WriteFile(unrelated, []byte("unrelated"), 0o440); err != nil {
		t.Fatal(err)
	}

	if !removeSudoersGrants(root) {
		t.Fatal("removeSudoersGrants() = false, want true (planted grants)")
	}
	for _, name := range sudoersGrantNames {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists after removeSudoersGrants, err=%v", name, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file %s was removed or is inaccessible: %v", unrelated, err)
	}

	// Second call: nothing left to remove, no error, benign false.
	if removeSudoersGrants(root) {
		t.Error("removeSudoersGrants() second call = true, want false (nothing left)")
	}
	// A root with no /etc/sudoers.d at all is equally benign.
	if removeSudoersGrants(t.TempDir()) {
		t.Error("removeSudoersGrants() on a root with no sudoers.d = true, want false")
	}
}

// TestFixupRootfsForScion_StripsSudoSetuidAndRemovesSudoersGrantsOnEveryCall
// proves the sudo hardening runs as part of fixupRootfsForScion itself —
// the one function both of substrate-serve's rootfs-fixup call sites
// invoke (startup and the /bootstrap fallback) — and that it re-applies on
// every call rather than a one-shot, golden-boot-only action: the setuid
// bit and the sudoers grant are re-planted between two calls, standing in
// for a persisted rootfs that reverted them between bootstraps, and both
// calls strip/remove them again.
func TestFixupRootfsForScion_StripsSudoSetuidAndRemovesSudoersGrantsOnEveryCall(t *testing.T) {
	root := trustedTestRoot(t)
	home := t.TempDir()
	sudoersDir := filepath.Join(root, "etc", "sudoers.d")

	for i := 0; i < 2; i++ {
		sudoPath := plantSetuidSudo(t, root, "usr/bin")
		if err := os.MkdirAll(sudoersDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sudoersDir, "scion"), []byte("scion ALL=(ALL) NOPASSWD:ALL\n"), 0o440); err != nil {
			t.Fatal(err)
		}

		fixupRootfsForScion(root, home, 1000, 1000)

		info, err := os.Stat(sudoPath)
		if err != nil {
			t.Fatalf("call %d: stat sudo binary: %v", i, err)
		}
		if info.Mode()&os.ModeSetuid != 0 {
			t.Errorf("call %d: sudo binary still setuid after fixupRootfsForScion", i)
		}
		if _, err := os.Stat(filepath.Join(sudoersDir, "scion")); !os.IsNotExist(err) {
			t.Errorf("call %d: sudoers grant still present after fixupRootfsForScion, err=%v", i, err)
		}
	}
}

// TestFixupRootfsForScionUser_RunsSudoFixup proves the sudo hardening is
// reachable through fixupRootfsForScionUser — the function both real
// call sites (substrate_serve.go's startupRootfsFixup and
// bootstrapRootfsFixup package vars) actually invoke — not just through a
// direct call to fixupRootfsForScion that a real bootstrap would never
// make.
func TestFixupRootfsForScionUser_RunsSudoFixup(t *testing.T) {
	origLookup := scionUserLookup
	t.Cleanup(func() { scionUserLookup = origLookup })
	root := trustedTestRoot(t)
	home := filepath.Join(root, "home", "scion")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	scionUserLookup = func(string) (*user.User, error) {
		return &user.User{Uid: "1000", Gid: "1000", HomeDir: home}, nil
	}

	sudoPath := plantSetuidSudo(t, root, "usr/bin")

	fixupRootfsForScionUser(root)

	info, err := os.Stat(sudoPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Error("sudo binary still setuid after fixupRootfsForScionUser")
	}
}

// sudoHardeningCallAllowlist is the parity guard for the sudo hardening:
// every other runtime this codebase targets (Docker, Kubernetes, Apple
// containers, rootless) keeps sudo and its setuid bit intact on purpose —
// root is not a security boundary there. Each of these names must only
// ever be REFERENCED (called, passed as a value, aliased — any use of the
// identifier at all, not just a direct call) from the one legitimate
// location(s) listed, keyed by name:
//
//	fixupRootfsForScionUser        <- the startupRootfsFixup/bootstrapRootfsFixup
//	                                   package vars' own initializers
//	startupRootfsFixup             <- runSubstrateServe (call site 1)
//	bootstrapRootfsFixup           <- substrateServeRootfsFixup (call site 2)
//	fixupRootfsForScion            <- fixupRootfsForScionUser
//	runStripSudoSetuidBits         <- fixupRootfsForScion
//	runRemoveSudoersGrants         <- fixupRootfsForScion
//	stripSudoSetuidBits            <- the runStripSudoSetuidBits package var's
//	                                   own initializer
//	removeSudoersGrants            <- the runRemoveSudoersGrants package var's
//	                                   own initializer
//	checkPrivilegeDropFeasible     <- substrateServePrivilegeDropChecker
//	substrateServePrivilegeDropChecker <- newSubstrateServeServer (wired into
//	                                   the Server as its PrivilegeDropChecker)
//	substrateServeRootfsFixup      <- newSubstrateServeServer (wired into the
//	                                   Server as its RootfsFixup)
//
// A location of the form "var:X" means "the initializer expression of the
// package-level var declared as X" — pinned to that one specific declaring
// var, not "any top-level var in any file": accepting any top-level value
// would let a *new* package-level var silently pick up one of these
// functions too. A plain
// function name means "anywhere in that function's body".
//
// This is deliberately about REFERENCES, not just calls: a mis-wiring can
// take the shape of a direct call (fixupRootfsForScionUser("/") inside
// RunInit), a call through an existing seam (startupRootfsFixup("/") inside
// RunInit — the most natural copy-paste of the legitimate call site), or a
// local alias (fx := fixupRootfsForScionUser; fx("/")) — the last of these
// is caught by flagging the alias ASSIGNMENT itself (an illegitimate
// reference to the tracked name), never by trying to track what the alias
// is later called as.
var sudoHardeningCallAllowlist = map[string]map[string]bool{
	"fixupRootfsForScionUser": {
		"var:startupRootfsFixup":   true,
		"var:bootstrapRootfsFixup": true,
	},
	"startupRootfsFixup": {
		"runSubstrateServe": true,
	},
	"bootstrapRootfsFixup": {
		"substrateServeRootfsFixup": true,
	},
	"fixupRootfsForScion": {
		"fixupRootfsForScionUser": true,
	},
	"runStripSudoSetuidBits": {
		"fixupRootfsForScion": true,
	},
	"runRemoveSudoersGrants": {
		"fixupRootfsForScion": true,
	},
	"stripSudoSetuidBits": {
		"var:runStripSudoSetuidBits": true,
	},
	"removeSudoersGrants": {
		"var:runRemoveSudoersGrants": true,
	},
	"checkPrivilegeDropFeasible": {
		"substrateServePrivilegeDropChecker": true,
	},
	"substrateServePrivilegeDropChecker": {
		"newSubstrateServeServer": true,
	},
	"substrateServeRootfsFixup": {
		"newSubstrateServeServer": true,
	},
}

// TestSudoHardeningOnlyReachableFromItsOwnWiring walks every *ast.Ident in
// every non-test .go file in this package and flags any occurrence of one
// of sudoHardeningCallAllowlist's tracked names — whether it's the function
// being called, a bare value passed or assigned, or anything else — whose
// location is not on that name's own allowlist. Declaring positions (a
// FuncDecl's own name; a ValueSpec's own declared names) are excluded, since
// those are definitions, not references. A future call, alias or
// pass-by-value added anywhere else in this package — including inside
// RunInit, which shares init.go with the legitimate
// checkPrivilegeDropFeasible definition, and including through an existing
// seam var rather than the underlying function directly — fails this test
// rather than silently changing non-substrate runtime behavior.
func TestSudoHardeningOnlyReachableFromItsOwnWiring(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var violations []string

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		// declaring collects the position of every *ast.Ident that is a
		// DEFINITION rather than a use: a FuncDecl's own name, and a
		// top-level ValueSpec's own declared names (the "X" in
		// "var X = ..."). These must never be checked against the
		// allowlist — the declaration of, say, fixupRootfsForScion itself
		// is not a "reference to fixupRootfsForScion" in the sense this
		// guard cares about.
		declaring := map[token.Pos]bool{}
		// varInitializerLocation maps a package-level ValueSpec value
		// expression's [start,end) position range to "var:<declared name>",
		// so a reference inside that initializer is attributed to the
		// specific var being declared, not to "package scope" generically.
		type varInit struct {
			pos, end token.Pos
			location string
		}
		var varInits []varInit
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				declaring[d.Name.Pos()] = true
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, n := range vs.Names {
						declaring[n.Pos()] = true
					}
					if len(vs.Names) == 1 && len(vs.Values) == 1 {
						varInits = append(varInits, varInit{
							pos:      vs.Values[0].Pos(),
							end:      vs.Values[0].End(),
							location: "var:" + vs.Names[0].Name,
						})
					}
				}
			}
		}

		// funcNameAt reports the name of the FuncDecl enclosing pos, or ""
		// if pos is not inside any function body. This codebase's
		// sudo-hardening call chain never nests these references inside a
		// closure, so (unlike rootexec/guard_test.go's funcLike machinery)
		// FuncDecl ranges alone are precise enough here.
		funcNameAt := func(pos token.Pos) string {
			var enclosing string
			var enclosingLen token.Pos
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				if fd.Body.Pos() <= pos && pos <= fd.Body.End() {
					if enclosing == "" || fd.Body.End()-fd.Body.Pos() < enclosingLen {
						enclosing = fd.Name.Name
						enclosingLen = fd.Body.End() - fd.Body.Pos()
					}
				}
			}
			return enclosing
		}

		// locationAt reports where pos is: a "var:X" package-level
		// initializer, an enclosing function's name, or "package scope" as
		// a last resort (a location no allowlist entry ever names, so it
		// fails closed rather than being silently treated as allowed).
		locationAt := func(pos token.Pos) string {
			for _, vi := range varInits {
				if vi.pos <= pos && pos < vi.end {
					return vi.location
				}
			}
			if fn := funcNameAt(pos); fn != "" {
				return fn
			}
			return "package scope"
		}

		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if declaring[id.Pos()] {
				return true
			}
			allowed, tracked := sudoHardeningCallAllowlist[id.Name]
			if !tracked {
				return true
			}
			where := locationAt(id.Pos())
			if !allowed[where] {
				line := fset.Position(id.Pos()).Line
				violations = append(violations, fmt.Sprintf(
					"%s:%d: %s referenced from %s, which is not on its sudo-hardening allowlist",
					name, line, id.Name, where))
			}
			return true
		})
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("found %d sudo-hardening reference(s) outside their allowed location:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

// TestFixupRootfsForScion_Enforced_RefusesAncestorSymlink is the core
// regression test: fixupRootfsForScion is reachable only from substrate-
// serve (fixupRootfsForScionUser -> startupRootfsFixup/bootstrapRootfsFixup
// in substrate_serve.go), so its own chownTreeRootOwned call is hardcoded
// to enforced (true) — see that call site's own comment. This proves that
// hardcoding actually matters: an ancestor-path symlink of home must be
// refused (nothing chowned through it), which only holds if the enforced,
// no-follow branch is really the one running.
func TestFixupRootfsForScion_Enforced_RefusesAncestorSymlink(t *testing.T) {
	origFilter := chownTreeRootOwnedFilter
	t.Cleanup(func() { chownTreeRootOwnedFilter = origFilter })
	chownTreeRootOwnedFilter = func(uint32) bool { return true }

	real := t.TempDir()
	actual := filepath.Join(real, "actual")
	if err := os.Mkdir(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(actual, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	homeLink := filepath.Join(parent, "home-link")
	if err := os.Symlink(real, homeLink); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(homeLink, "actual")

	markerBefore := ctimeOfFile(t, marker)
	time.Sleep(15 * time.Millisecond)

	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	fixupRootfsForScion(root, home, os.Getuid(), os.Getgid())

	markerAfter := ctimeOfFile(t, marker)
	if markerAfter != markerBefore {
		t.Error("marker's ctime advanced: the ancestor symlink was followed and chowned through — fixupRootfsForScion is not really enforced")
	}
}
