/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestChainIsTrusted exercises the pure ownership/mode predicate directly,
// with fabricated uid/mode values — no real filesystem involved. This is
// the only way to cover "owned by neither root nor self" and "owned by root
// but group/other-writable" deterministically: a real component that fails
// the ownership half would require actually being a second, unrelated uid,
// which an unprivileged test process cannot fabricate on a real file.
func TestChainIsTrusted(t *testing.T) {
	const self = 1000
	tests := []struct {
		name    string
		uid     uint32
		mode    uint32
		selfUID uint32
		want    bool
	}{
		{"root owned, mode 0755", 0, 0o755, self, true},
		{"self owned, mode 0755", self, 0o755, self, true},
		{"self owned, mode 0700", self, 0o700, self, true},
		{"third party owned, mode 0755", 1234, 0o755, self, false},
		{"root owned, group writable", 0, 0o775, self, false},
		{"root owned, other writable", 0, 0o757, self, false},
		{"self owned, group writable", self, 0o775, self, false},
		{"self owned, other writable", self, 0o757, self, false},
		{"root owned, sticky and world writable (e.g. /tmp)", 0, 0o1777, self, false},
		{"root owned, sticky only, not writable", 0, 0o1755, self, true},
		{"third party owned, world writable", 1234, 0o777, self, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chainIsTrusted(tt.uid, tt.mode, tt.selfUID); got != tt.want {
				t.Errorf("chainIsTrusted(uid=%d, mode=%#o, self=%d) = %v, want %v", tt.uid, tt.mode, tt.selfUID, got, tt.want)
			}
		})
	}
}

// selfOwnedTrustedDir creates a fresh, self-owned, non-group/other-writable
// directory to anchor a chain-verification test's "happy path" component(s)
// under. It deliberately does NOT use t.TempDir() (which resolves under
// os.TempDir(), i.e. "/tmp" on every system this suite runs on — world-
// writable by design, so it fails the very check under test on its own,
// before the test's own fixture is ever reached). Anchoring instead under
// this process's real home directory means every ancestor from "/" down is
// either root-owned (e.g. "/", "/home") or owned by this process's own uid
// (the home directory itself) and not group/other-writable — exactly the
// condition OpenParentNoFollowRootOwned requires — without needing real
// root privilege to construct.
//
// Skips the test if $HOME can't be resolved or isn't usable for this. The
// created directory is removed via t.Cleanup.
func selfOwnedTrustedDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("cannot resolve a real home directory to anchor a root/self-owned chain under")
	}
	dir, err := os.MkdirTemp(home, ".dirfd-rootowned-test-*")
	if err != nil {
		t.Skipf("cannot create a test directory under %s: %v", home, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestOpenParentNoFollowRootOwned_RefusesWorldWritableAncestor proves the
// walk fails closed against a REAL ancestor that is merely writable by
// everyone else — not a symlink, not even wrong-owner, just too permissive
// — the exact shape of risk this guards against (a workload-writable
// parent letting an entry be renamed/replaced out from under root).
func TestOpenParentNoFollowRootOwned_RefusesWorldWritableAncestor(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	writable := filepath.Join(base, "writable")
	if err := os.Mkdir(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	// os.Mkdir's mode argument is masked by the process umask; force the
	// exact bits this test needs regardless of what umask happens to be.
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}

	_, _, err := OpenParentNoFollowRootOwned(filepath.Join(writable, "leaf"))
	if err == nil {
		t.Fatal("expected OpenParentNoFollowRootOwned to refuse a world-writable ancestor, got nil error")
	}
}

// TestOpenParentNoFollowRootOwned_RefusesGroupWritableAncestor is the
// group-write half of the same property.
func TestOpenParentNoFollowRootOwned_RefusesGroupWritableAncestor(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	writable := filepath.Join(base, "group-writable")
	if err := os.Mkdir(writable, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o775); err != nil {
		t.Fatal(err)
	}

	_, _, err := OpenParentNoFollowRootOwned(filepath.Join(writable, "leaf"))
	if err == nil {
		t.Fatal("expected OpenParentNoFollowRootOwned to refuse a group-writable ancestor, got nil error")
	}
}

// TestOpenParentNoFollowRootOwned_SucceedsOnTrustedChain proves the happy
// path: a real chain that is entirely root- or self-owned and free of
// group/other write succeeds, returning a usable parent fd.
func TestOpenParentNoFollowRootOwned_SucceedsOnTrustedChain(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	nested := filepath.Join(base, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	fd, leaf, err := OpenParentNoFollowRootOwned(filepath.Join(nested, "leaf"))
	if err != nil {
		t.Fatalf("OpenParentNoFollowRootOwned: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if leaf != "leaf" {
		t.Errorf("leaf = %q, want %q", leaf, "leaf")
	}
}

// TestEnsureDirNoFollowRootOwned_CreatesAndVerifiesLeaf proves
// EnsureDirNoFollowRootOwned creates a missing leaf under a trusted chain,
// at the requested mode, and that a second call against the now-existing
// leaf succeeds idempotently (Mkdirat's EEXIST is tolerated).
func TestEnsureDirNoFollowRootOwned_CreatesAndVerifiesLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	target := filepath.Join(base, "private")

	f, err := EnsureDirNoFollowRootOwned(target, 0o700)
	if err != nil {
		t.Fatalf("EnsureDirNoFollowRootOwned (create): %v", err)
	}
	_ = f.Close()

	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("target is not a directory: %v", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %#o, want 0700", perm)
	}

	f2, err := EnsureDirNoFollowRootOwned(target, 0o700)
	if err != nil {
		t.Fatalf("EnsureDirNoFollowRootOwned (idempotent second call): %v", err)
	}
	_ = f2.Close()
}

// TestEnsureDirNoFollowRootOwned_RefusesSymlinkedLeaf proves a pre-existing
// symlink at the leaf name is refused outright, never followed and never
// silently replaced.
func TestEnsureDirNoFollowRootOwned_RefusesSymlinkedLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	victim := filepath.Join(base, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "leaf")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureDirNoFollowRootOwned(target, 0o700); err == nil {
		t.Fatal("expected EnsureDirNoFollowRootOwned to refuse a symlinked leaf, got nil error")
	}

	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("leaf is no longer a symlink — it must be refused, not replaced")
	}
}

// TestEnsureDirNoFollowRootOwned_RefusesPreExistingBadModeLeaf proves the
// leaf's own fstat-and-check — not just the parent chain — is what actually
// runs: a leaf that already exists, under an entirely trusted chain, but is
// itself group/other-writable, must still be refused rather than handed
// back as a usable directory.
func TestEnsureDirNoFollowRootOwned_RefusesPreExistingBadModeLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	target := filepath.Join(base, "leaf")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Chmod after creation, not via Mkdir's own (umask-masked) mode
	// argument, to force the exact world-writable bits this test needs and
	// to exercise the re-check against an already-existing directory
	// (the create path's own Mkdirat call gets EEXIST here, tolerated —
	// this test is about what happens after that).
	if err := os.Chmod(target, 0o777); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureDirNoFollowRootOwned(target, 0o700); err == nil {
		t.Fatal("expected EnsureDirNoFollowRootOwned to refuse an existing, group/other-writable leaf, got nil error")
	}
}

// TestEnsureDirNoFollowRootOwned_ReChmodsPreExistingLooseModeLeaf proves mode
// is enforced even on a leaf that already existed before this call: a
// directory left at 0755 (not group/other-writable, so it passes the trust
// check, but looser than the caller's requested mode) is tightened to
// exactly the requested mode rather than left as whatever it already was.
// mkdirat(2)'s own mode argument only ever applies at creation, so without
// an explicit chmod a pre-existing leaf's mode would silently survive
// unchanged forever.
func TestEnsureDirNoFollowRootOwned_ReChmodsPreExistingLooseModeLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	target := filepath.Join(base, "leaf")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	f, err := EnsureDirNoFollowRootOwned(target, 0o700)
	if err != nil {
		t.Fatalf("EnsureDirNoFollowRootOwned: %v", err)
	}
	_ = f.Close()

	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %#o, want 0700 (a pre-existing 0755 leaf must be tightened, not left as-is)", perm)
	}
}

// TestOpenParentNoFollowRootOwned_RefusesSymlinkedAncestor proves an
// ancestor component that is a symlink — even one that points at an
// otherwise entirely trusted, self-owned directory — is refused by the
// O_NOFOLLOW open at that component, never silently followed into the
// directory it happens to point at.
func TestOpenParentNoFollowRootOwned_RefusesSymlinkedAncestor(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	_, _, err := OpenParentNoFollowRootOwned(filepath.Join(link, "leaf"))
	if err == nil {
		t.Fatal("expected OpenParentNoFollowRootOwned to refuse a symlinked ancestor even when its target is trusted, got nil error")
	}
}

// TestEnsureDirNoFollowRootOwned_RootOwnedChain is the genuine root-only
// happy path: with a real root-owned, non-writable ancestor chain (this
// process's own euid IS 0 in that case, so chainIsTrusted's uid==0 branch is
// what's actually exercised, not the uid==self escape valve every other
// test in this file relies on). Skips unless running as root.
func TestEnsureDirNoFollowRootOwned_RootOwnedChain(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create and verify a genuinely root-owned (uid 0) chain")
	}
	base, err := os.MkdirTemp("/root", "dirfd-rootowned-test-*")
	if err != nil {
		t.Skipf("could not create a fixture under /root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(base, "private")
	f, err := EnsureDirNoFollowRootOwned(target, 0o700)
	if err != nil {
		t.Fatalf("EnsureDirNoFollowRootOwned: %v", err)
	}
	defer func() { _ = f.Close() }()
}

// TestAmbientTempDirTrusted_AcceptsTrustedDirUnderTrustedParent is the
// positive path achievable without real root: a self-owned directory, free
// of the group/other-write bits, under an entirely trusted ancestor chain
// must be accepted (chainIsTrusted's ordinary rule — no sticky bit
// involved).
func TestAmbientTempDirTrusted_AcceptsTrustedDirUnderTrustedParent(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	dir := filepath.Join(base, "tmp-like")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if !AmbientTempDirTrusted(dir) {
		t.Error("AmbientTempDirTrusted() = false, want true (self-owned, non-group/other-writable dir under a trusted parent)")
	}
}

// TestAmbientTempDirTrusted_AcceptsStickyRootOwnedDir is the genuine
// root-only happy path for the sticky-bit branch (real "/tmp"'s own
// shape): a sticky, world-writable directory is only accepted when it is
// actually owned by uid 0 — this process's own euid must BE 0 to construct
// that fixture at all, so this skips otherwise, the same convention
// TestEnsureDirNoFollowRootOwned_RootOwnedChain uses.
func TestAmbientTempDirTrusted_AcceptsStickyRootOwnedDir(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create and own a genuinely root-owned sticky dir")
	}
	base, err := os.MkdirTemp("/root", "dirfd-ambienttmp-test-*")
	if err != nil {
		t.Skipf("could not create a fixture under /root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "tmp-like")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	// os.Chmod's mode argument is an os.FileMode, whose special bits
	// (os.ModeSticky here) are encoded at different bit positions than the
	// raw octal 0o1000 — passing a bare 0o1777 silently drops the sticky
	// bit instead of setting it.
	if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}

	if !AmbientTempDirTrusted(dir) {
		t.Error("AmbientTempDirTrusted() = false, want true (sticky, root-owned dir under a trusted parent)")
	}
}

// TestAmbientTempDirTrusted_RefusesWhenParentIsWorkloadWritable proves the
// ancestor-chain check actually runs: a directory that would be accepted
// entirely on its own merits (self-owned, mode 0755 — no sticky bit needed
// at all) is still refused when its PARENT is workload-writable, since the
// workload could rename the directory itself out of the way and plant a
// symlink at the same name before it is ever used. The leaf is
// deliberately NOT also independently untrusted: a self-owned 0o1777 leaf
// would fail the leaf's own chainIsTrusted check regardless of the parent,
// meaning that fixture would refuse for the wrong reason even with the
// ancestor-chain check removed entirely.
func TestAmbientTempDirTrusted_RefusesWhenParentIsWorkloadWritable(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	untrustedParent := filepath.Join(base, "untrusted-parent")
	if err := os.Mkdir(untrustedParent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(untrustedParent, 0o777); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(untrustedParent, "tmp-like")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if AmbientTempDirTrusted(dir) {
		t.Error("AmbientTempDirTrusted() = true, want false (parent is group/other-writable, even though the leaf alone would pass)")
	}
}

// TestAmbientTempDirTrusted_RefusesSelfOwnedStickyWorldWritableLeaf proves
// the sticky-bit exemption is for uid 0 ONLY, never self-owned: a sticky,
// world-writable directory under a fully trusted parent is refused when it
// is merely self-owned (this test process's own uid, not literally root) —
// the exemption exists for real "/tmp" (root-owned, sticky, world-writable)
// specifically, not as a general "sticky bit means trusted" rule that would
// let a non-root runtime's own workload-writable, sticky-but-self-owned
// directory in.
func TestAmbientTempDirTrusted_RefusesSelfOwnedStickyWorldWritableLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	dir := filepath.Join(base, "tmp-like")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	// See TestAmbientTempDirTrusted_AcceptsStickyRootOwnedDir's comment: a
	// bare 0o1777 passed to os.Chmod does not set the sticky bit at all
	// (os.ModeSticky is a different bit position than raw octal 0o1000),
	// which would make this fixture pass for the wrong reason (plain
	// world-writable, never reaching the sticky branch under test either
	// way).
	if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}

	if AmbientTempDirTrusted(dir) {
		t.Error("AmbientTempDirTrusted() = true, want false (sticky+world-writable but only self-owned, not uid 0)")
	}
}

// TestAmbientTempDirTrusted_RefusesSymlinkedLeaf proves a symlink planted at
// the checked path itself is refused, never followed.
func TestAmbientTempDirTrusted_RefusesSymlinkedLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(real, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if AmbientTempDirTrusted(link) {
		t.Error("AmbientTempDirTrusted() = true, want false (path itself is a symlink)")
	}
}

// mustWriteExecutable creates an executable regular file at path with the
// given content, for VerifyRootOwnedExecutable's fixtures.
func mustWriteExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyRootOwnedExecutable_AcceptsPlainTrustedRegularFile is the
// no-symlinks-at-all base case.
func TestVerifyRootOwnedExecutable_AcceptsPlainTrustedRegularFile(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	bin := filepath.Join(base, "tool")
	mustWriteExecutable(t, bin, "#!/bin/sh\necho ok\n")

	if err := VerifyRootOwnedExecutable(bin); err != nil {
		t.Errorf("VerifyRootOwnedExecutable(%q) = %v, want nil", bin, err)
	}
}

// TestVerifyRootOwnedExecutable_RefusesGroupWritableLeaf proves the leaf's
// OWN chainIsTrusted check still runs even once every directory leading to
// it has already passed: a group-writable regular file — sitting directly
// in an otherwise fully-trusted directory, no symlink involved at all —
// must still be refused. Directory-chain trust alone (what
// OpenParentNoFollowRootOwned's walk verifies) says nothing about whether
// the workload can overwrite the leaf file's own content.
func TestVerifyRootOwnedExecutable_RefusesGroupWritableLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	bin := filepath.Join(base, "tool")
	mustWriteExecutable(t, bin, "#!/bin/sh\necho ok\n")
	if err := os.Chmod(bin, 0o775); err != nil {
		t.Fatal(err)
	}

	if err := VerifyRootOwnedExecutable(bin); err == nil {
		t.Error("VerifyRootOwnedExecutable() = nil, want an error (group-writable leaf)")
	}
}

// TestVerifyRootOwnedExecutable_RefusesOtherWritableLeaf is the other-write
// counterpart above.
func TestVerifyRootOwnedExecutable_RefusesOtherWritableLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	bin := filepath.Join(base, "tool")
	mustWriteExecutable(t, bin, "#!/bin/sh\necho ok\n")
	if err := os.Chmod(bin, 0o757); err != nil {
		t.Fatal(err)
	}

	if err := VerifyRootOwnedExecutable(bin); err == nil {
		t.Error("VerifyRootOwnedExecutable() = nil, want an error (other-writable leaf)")
	}
}

// TestVerifyRootOwnedExecutable_RefusesDirectoryLeaf proves the candidate
// must actually be a regular file: a directory sitting at the candidate's
// own name — otherwise indistinguishable from a trusted leaf by ownership
// and mode alone — must still be refused.
func TestVerifyRootOwnedExecutable_RefusesDirectoryLeaf(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	dir := filepath.Join(base, "tool")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := VerifyRootOwnedExecutable(dir); err == nil {
		t.Error("VerifyRootOwnedExecutable() = nil, want an error (candidate is a directory, not a regular file)")
	}
}

// TestVerifyRootOwnedExecutable_FollowsMultiHopSymlinkChainWhenAllTrusted
// mirrors Debian's real iptables layout ("iptables" -> "alternatives/
// iptables" -> "xtables-nft-multi", two hops) entirely under a trusted
// chain, and proves the walk follows both hops and accepts the real
// destination — the shape rootexec.Resolve depends on to keep working for
// any multi-call binary reached through a legitimate root-installed
// alternatives-style symlink.
func TestVerifyRootOwnedExecutable_FollowsMultiHopSymlinkChainWhenAllTrusted(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	altDir := filepath.Join(base, "alternatives")
	if err := os.Mkdir(altDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(base, "xtables-nft-multi")
	mustWriteExecutable(t, dest, "#!/bin/sh\necho dispatch\n")

	hop1 := filepath.Join(altDir, "iptables")
	if err := os.Symlink(dest, hop1); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(base, "iptables")
	if err := os.Symlink(hop1, candidate); err != nil {
		t.Fatal(err)
	}

	if err := VerifyRootOwnedExecutable(candidate); err != nil {
		t.Errorf("VerifyRootOwnedExecutable(%q) = %v, want nil (fully trusted 2-hop chain)", candidate, err)
	}
}

// TestVerifyRootOwnedExecutable_RefusesHopThroughUntrustedDir proves that
// even a symlink chain that eventually reaches a trusted destination is
// refused if any INTERMEDIATE hop's own containing directory is not
// trusted — following a legitimate-looking alternatives chain must not
// smuggle a workload-writable directory into the trusted result.
func TestVerifyRootOwnedExecutable_RefusesHopThroughUntrustedDir(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	untrustedDir := filepath.Join(base, "untrusted")
	if err := os.Mkdir(untrustedDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(untrustedDir, 0o777); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(base, "real-tool")
	mustWriteExecutable(t, dest, "#!/bin/sh\necho ok\n")

	hop1 := filepath.Join(untrustedDir, "tool")
	if err := os.Symlink(dest, hop1); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(base, "tool")
	if err := os.Symlink(hop1, candidate); err != nil {
		t.Fatal(err)
	}

	if err := VerifyRootOwnedExecutable(candidate); err == nil {
		t.Error("VerifyRootOwnedExecutable() = nil, want an error (intermediate hop lives in an untrusted directory)")
	}
}

// TestVerifyRootOwnedExecutable_RefusesSymlinkLoop proves a symlink cycle
// fails closed instead of looping forever.
func TestVerifyRootOwnedExecutable_RefusesSymlinkLoop(t *testing.T) {
	base := selfOwnedTrustedDir(t)
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}

	if err := VerifyRootOwnedExecutable(a); err == nil {
		t.Error("VerifyRootOwnedExecutable() = nil, want an error (symlink loop)")
	}
}
