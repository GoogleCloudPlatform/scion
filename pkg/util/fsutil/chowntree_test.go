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

package fsutil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
)

// chownCall records one invocation of a fake chownFunc.
type chownCall struct {
	path     string
	uid, gid int
}

// recordingChown returns a chownFunc that records every call it receives
// (via calls) instead of touching the filesystem, plus an optional errFor
// hook so tests can make specific paths fail.
func recordingChown(calls *[]chownCall, errFor func(path string) error) chownFunc {
	return func(path string, uid, gid int) error {
		*calls = append(*calls, chownCall{path, uid, gid})
		if errFor != nil {
			return errFor(path)
		}
		return nil
	}
}

func TestChownTree_RefusesUnsafeRootWithoutTouchingAnything(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "etc"))
	mustWriteFile(t, filepath.Join(dir, "etc", "passwd"), "root:x:0:0:root:/root:/bin/sh\n")
	mustMkdirAll(t, filepath.Join(dir, "usr", "bin"))
	mustMkdirAll(t, filepath.Join(dir, "proc"))

	var calls []chownCall
	err := chownTree(context.Background(), dir, 1000, 1000, nil, recordingChown(&calls, nil), statDeviceOf)
	if !errors.Is(err, ErrFilesystemRootLookalike) {
		t.Fatalf("chownTree(%q) = %v, want ErrFilesystemRootLookalike", dir, err)
	}
	if len(calls) != 0 {
		t.Fatalf("chown called %d times despite refusal: %+v", len(calls), calls)
	}
}

func TestChownTree_ChownsEveryEntry(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "sub"))
	mustWriteFile(t, filepath.Join(dir, "sub", "file.txt"), "hi\n")

	var calls []chownCall
	err := chownTree(context.Background(), dir, 1000, 2000, nil, recordingChown(&calls, nil), statDeviceOf)
	if err != nil {
		t.Fatalf("chownTree: %v", err)
	}

	var gotPaths []string
	for _, c := range calls {
		if c.uid != 1000 || c.gid != 2000 {
			t.Errorf("call %+v has wrong uid/gid", c)
		}
		gotPaths = append(gotPaths, c.path)
	}
	sort.Strings(gotPaths)
	want := []string{dir, filepath.Join(dir, "sub"), filepath.Join(dir, "sub", "file.txt")}
	sort.Strings(want)
	if len(gotPaths) != len(want) {
		t.Fatalf("chowned paths = %v, want %v", gotPaths, want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Errorf("chowned paths = %v, want %v", gotPaths, want)
			break
		}
	}
}

// TestChownTree_DoesNotCrossDeviceBoundary proves the walk skips an entry
// whose device differs from root's, without needing a real mount: deviceOf
// is faked per-path, so the boundary is exercised deterministically and the
// test can actually fail if the skip logic regresses.
func TestChownTree_DoesNotCrossDeviceBoundary(t *testing.T) {
	dir := t.TempDir()
	otherDeviceDir := filepath.Join(dir, "other-device")
	mustMkdirAll(t, otherDeviceDir)
	otherDeviceFile := filepath.Join(otherDeviceDir, "file.txt")
	mustWriteFile(t, otherDeviceFile, "hi\n")
	sameDeviceFile := filepath.Join(dir, "same-device.txt")
	mustWriteFile(t, sameDeviceFile, "hi\n")

	const rootDev, otherDev uint64 = 1, 2
	fakeDeviceOf := func(path string, info fs.FileInfo) (uint64, bool) {
		if path == otherDeviceDir || path == otherDeviceFile {
			return otherDev, true
		}
		return rootDev, true
	}

	var calls []chownCall
	err := chownTree(context.Background(), dir, 1000, 1000, nil, recordingChown(&calls, nil), fakeDeviceOf)
	if err != nil {
		t.Fatalf("chownTree: %v", err)
	}

	for _, c := range calls {
		if c.path == otherDeviceDir || c.path == otherDeviceFile {
			t.Errorf("chown called on entry across the device boundary: %+v", c)
		}
	}
	var gotPaths []string
	for _, c := range calls {
		gotPaths = append(gotPaths, c.path)
	}
	sort.Strings(gotPaths)
	want := []string{dir, sameDeviceFile}
	sort.Strings(want)
	if len(gotPaths) != len(want) {
		t.Fatalf("chowned paths = %v, want exactly %v (other-device entries must be skipped)", gotPaths, want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Errorf("chowned paths = %v, want %v", gotPaths, want)
			break
		}
	}
}

// TestChownTree_SkipDirPreventsDescentAcrossDeviceBoundary proves the walk
// stops *descending* at a device boundary (fs.SkipDir), not just that each
// individual entry's own device is checked. TestChownTree_DoesNotCrossDeviceBoundary
// above fakes the other device for every path under the boundary, so it
// cannot tell a real SkipDir from a walk that still descends but happens to
// skip every entry it finds there. Here, only the boundary directory itself
// reports a different device; a file nested inside it reports the *root's*
// own device. If the walk incorrectly kept descending (e.g. fs.SkipDir
// replaced with nil), that nested file's own device check would say "same
// as root" and it would be chowned. It must not be.
func TestChownTree_SkipDirPreventsDescentAcrossDeviceBoundary(t *testing.T) {
	dir := t.TempDir()
	otherDeviceDir := filepath.Join(dir, "other-device")
	mustMkdirAll(t, otherDeviceDir)
	nestedSameDeviceFile := filepath.Join(otherDeviceDir, "nested-reports-root-device.txt")
	mustWriteFile(t, nestedSameDeviceFile, "hi\n")

	const rootDev, otherDev uint64 = 1, 2
	fakeDeviceOf := func(path string, info fs.FileInfo) (uint64, bool) {
		if path == otherDeviceDir {
			return otherDev, true
		}
		// Every other path, including the nested file, reports the root's
		// own device -- as it would if descent into otherDeviceDir were
		// never supposed to happen in the first place, but did.
		return rootDev, true
	}

	var calls []chownCall
	err := chownTree(context.Background(), dir, 1000, 1000, nil, recordingChown(&calls, nil), fakeDeviceOf)
	if err != nil {
		t.Fatalf("chownTree: %v", err)
	}

	for _, c := range calls {
		if c.path == nestedSameDeviceFile {
			t.Errorf("chown called on %s: the walk descended into a directory on another device instead of skipping it entirely", nestedSameDeviceFile)
		}
	}
}

// TestChownTree_AggregatesPerEntryErrors proves a failure on one entry does
// not stop the walk, and all failures are reported together.
func TestChownTree_AggregatesPerEntryErrors(t *testing.T) {
	dir := t.TempDir()
	failPath := filepath.Join(dir, "fails.txt")
	okPath := filepath.Join(dir, "ok.txt")
	mustWriteFile(t, failPath, "x\n")
	mustWriteFile(t, okPath, "x\n")

	sentinel := errors.New("boom")
	var calls []chownCall
	chown := recordingChown(&calls, func(path string) error {
		if path == failPath {
			return sentinel
		}
		return nil
	})

	err := chownTree(context.Background(), dir, 1000, 1000, nil, chown, statDeviceOf)
	if err == nil {
		t.Fatal("chownTree() = nil, want an aggregated error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("chownTree() = %v, want it to wrap the per-entry sentinel", err)
	}

	// The walk must not have stopped at the failing entry: the other file
	// (and the root dir) must still have been chowned.
	touched := map[string]bool{}
	for _, c := range calls {
		touched[c.path] = true
	}
	if !touched[okPath] {
		t.Errorf("ok.txt was not chowned; walk stopped at the first error instead of continuing")
	}
	if !touched[dir] {
		t.Errorf("root dir was not chowned")
	}
}

// TestChownTree_JoinedErrorPreservesDistinctFailures documents why a caller
// must not decide "the root is missing" by testing errors.Is(err,
// fs.ErrNotExist) against ChownTree's returned error: errors.Is on a joined
// error is true if any member matches, not all of them. Here one entry
// vanishes (fs.ErrNotExist) and a different entry fails a different way; the
// joined error must satisfy errors.Is against both, so a caller checking
// only for fs.ErrNotExist and treating a match as "nothing to report" would
// silently drop the other, unrelated failure. Callers that need a
// missing-root fast path must check the root itself before calling this
// function, not inspect its result afterward.
func TestChownTree_JoinedErrorPreservesDistinctFailures(t *testing.T) {
	dir := t.TempDir()
	vanished := filepath.Join(dir, "vanished.txt")
	otherFailure := filepath.Join(dir, "other-failure.txt")
	mustWriteFile(t, vanished, "x\n")
	mustWriteFile(t, otherFailure, "x\n")

	sentinel := errors.New("read-only filesystem")
	chown := recordingChown(&[]chownCall{}, func(path string) error {
		switch path {
		case vanished:
			return fs.ErrNotExist
		case otherFailure:
			return sentinel
		default:
			return nil
		}
	})

	err := chownTree(context.Background(), dir, 1000, 1000, nil, chown, statDeviceOf)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("chownTree() = %v, want it to wrap fs.ErrNotExist", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("chownTree() = %v, want it to also wrap the unrelated failure -- "+
			"a caller checking only errors.Is(err, fs.ErrNotExist) would otherwise miss it", err)
	}
}

func TestChownTree_ContextCancellationStopsWalk(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "a.txt"), "x\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the walk starts

	var calls []chownCall
	err := chownTree(ctx, dir, 1000, 1000, nil, recordingChown(&calls, nil), statDeviceOf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("chownTree() = %v, want it to wrap context.Canceled", err)
	}
}

// TestOwnedByUID_MatchesCurrentUIDOnly is a direct unit test on the
// production filter itself (not a copy of it), so a regression in the real
// predicate fails this test.
func TestOwnedByUID_MatchesCurrentUIDOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	mustWriteFile(t, path, "x\n")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}

	if !ownedByUID(os.Getuid())(info) {
		t.Errorf("ownedByUID(%d) = false for a file owned by %d, want true", os.Getuid(), os.Getuid())
	}
	const impossibleUID = 999999
	if ownedByUID(impossibleUID)(info) {
		t.Errorf("ownedByUID(%d) = true for a file not owned by that uid, want false", impossibleUID)
	}
}

// TestChownTreeOwnedByUID_OnlyTouchesMatchingEntries covers the narrower
// entry point used by cmd/sciontool/commands/init.go's post-pre-start-hook
// fix-up, which must only re-own files a specific (root-run) process
// created, not everything under root. It exercises chownTree with the real
// ownedByUID predicate (not a local copy), so a regression there fails this
// test too.
func TestChownTreeOwnedByUID_OnlyTouchesMatchingEntries(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")
	mustWriteFile(t, fileA, "x\n")
	mustWriteFile(t, fileB, "x\n")

	// Every real file here is owned by the current test process, so
	// filtering on that uid must touch everything -- this proves the filter
	// is actually consulted, not a no-op.
	var calls []chownCall
	err := chownTree(context.Background(), dir, 1000, 1000, ownedByUID(os.Getuid()), recordingChown(&calls, nil), statDeviceOf)
	if err != nil {
		t.Fatalf("chownTree: %v", err)
	}

	touched := map[string]bool{}
	for _, c := range calls {
		touched[c.path] = true
	}
	if !touched[fileA] || !touched[fileB] || !touched[dir] {
		t.Fatalf("expected all entries touched when filter matches current uid; got %v", calls)
	}
}

func TestChownTreeOwnedByUID_SkipsNonMatchingEntries(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "file.txt"), "x\n")

	// No real file is owned by UID 999999; the filter should reject every
	// entry (the root dir is walked but the filter still applies to it too).
	var calls []chownCall
	err := chownTree(context.Background(), dir, 1000, 1000, ownedByUID(999999), recordingChown(&calls, nil), statDeviceOf)
	if err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("expected no entries touched (none owned by uid 999999), got %+v", calls)
	}
}

// TestChownTreeOwnedByUID_PublicAPI proves ChownTreeOwnedByUID actually
// applies the ownerUID filter, not just that it returns nil. The
// non-matching case below uses a *different* target uid/gid than the file
// currently has, specifically so a dropped or broken filter cannot pass by
// coincidence:
//   - with the filter intact, the entry is skipped: err is nil and
//     ownership is unchanged.
//   - with the filter dropped (chowning everything) or otherwise broken,
//     chownTree attempts Lchown(path, os.Getuid()+1, os.Getgid()+1).
//     Unprivileged, that fails with EPERM, so err is non-nil; as root, it
//     would succeed and ownership would visibly change. Either way, the
//     assertions below catch it.
//
// (The matching-uid case necessarily chowns to the file's own current
// owner, since every file this test creates is already owned by
// os.Getuid()/os.Getgid() -- it cannot by itself prove the filter is
// consulted, only that the call succeeds; that half is covered together
// with the non-matching case here.)
func TestChownTreeOwnedByUID_PublicAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	mustWriteFile(t, path, "x\n")

	before, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	if err := ChownTreeOwnedByUID(context.Background(), dir, os.Getuid(), os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("ChownTreeOwnedByUID (matching uid): %v", err)
	}
	afterMatch, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat after matching-uid call: %v", err)
	}
	matchStat := afterMatch.Sys().(*syscall.Stat_t)
	if int(matchStat.Uid) != os.Getuid() || int(matchStat.Gid) != os.Getgid() {
		t.Errorf("matching-uid call did not chown %s: uid=%d gid=%d", path, matchStat.Uid, matchStat.Gid)
	}

	const impossibleUID = 999999
	targetUID, targetGID := os.Getuid()+1, os.Getgid()+1
	err = ChownTreeOwnedByUID(context.Background(), dir, impossibleUID, targetUID, targetGID)
	if err != nil {
		t.Fatalf("ChownTreeOwnedByUID (non-matching uid) = %v, want nil: the filter should skip every entry", err)
	}
	afterSkip, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat after non-matching-uid call: %v", err)
	}
	skipStat := afterSkip.Sys().(*syscall.Stat_t)
	if skipStat.Uid != beforeStat.Uid || skipStat.Gid != beforeStat.Gid {
		t.Errorf("non-matching-uid call changed ownership despite the filter: before uid=%d gid=%d, after uid=%d gid=%d",
			beforeStat.Uid, beforeStat.Gid, skipStat.Uid, skipStat.Gid)
	}
}

// TestChownTree_MissingRootReturnsError documents and locks in the policy
// stated in ChownTree's doc comment: a nonexistent root is an error (wrapping
// fs.ErrNotExist), matching plain `chown -R`. Callers for whom a missing
// root means "nothing to fix up" must check for that themselves.
func TestChownTree_MissingRootReturnsError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	err := ChownTree(context.Background(), missing, os.Getuid(), os.Getgid())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ChownTree(%q) = %v, want it to wrap fs.ErrNotExist", missing, err)
	}
}

// TestChownTree_EndToEnd exercises the public entry point with the real
// os.Lchown and the real device lookup, proving the whole thing works
// together, not just its mocked pieces.
func TestChownTree_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "sub"))
	mustWriteFile(t, filepath.Join(dir, "sub", "file.txt"), "hi\n")

	if err := ChownTree(context.Background(), dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("ChownTree: %v", err)
	}
}

// TestChownTree_DoesNotFollowDanglingSymlink is the deterministic no-follow
// guard: a symlink to a path that does not exist anywhere. os.Lchown re-owns
// the link itself and never resolves the target, so this succeeds; if the
// walk ever dereferenced the link (os.Chown instead of os.Lchown), the
// resolve would fail with ENOENT and the walk would report a non-nil error.
// This needs no ctime comparison or timing of any kind, so it is reliable
// under the filesystem's timestamp granularity, and it passes identically
// whether the process is privileged or not: an unprivileged os.Lchown to the
// file's own uid/gid always succeeds, and so does a privileged one.
func TestChownTree_DoesNotFollowDanglingSymlink(t *testing.T) {
	treeRoot := t.TempDir()
	target := filepath.Join(treeRoot, "does-not-exist")
	linkPath := filepath.Join(treeRoot, "dangling-link")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := ChownTree(context.Background(), treeRoot, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("ChownTree(%q) = %v, want nil: a dangling symlink must be re-owned itself (Lchown), not resolved (Chown, which would fail with ENOENT)", treeRoot, err)
	}
}

// TestChownTree_SymlinkNeverDereferenced is a secondary, real-filesystem
// check alongside TestChownTree_DoesNotFollowDanglingSymlink above (which is
// the guard that can actually fail without help): it chowns to the file's
// own uid/gid and checks that a symlink's target outside the tree keeps its
// original ctime. Because inode ctimes come from the kernel's coarse clock
// (a few ms per tick on this filesystem), a same-tick same-uid dereference
// would leave ctime unchanged even if the link *were* followed, so a short
// sleep before the chown is needed for this comparison to be able to fail at
// all -- confirmed empirically: without the sleep, swapping the
// implementation's os.Lchown for os.Chown still passes this test; with it,
// that swap fails it as expected.
func TestChownTree_SymlinkNeverDereferenced(t *testing.T) {
	treeRoot := t.TempDir()
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "outside-target.txt")
	if err := os.WriteFile(outsideTarget, []byte("do not touch"), 0644); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	linkPath := filepath.Join(treeRoot, "link-to-outside")
	if err := os.Symlink(outsideTarget, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	before, err := os.Lstat(outsideTarget)
	if err != nil {
		t.Fatalf("lstat outside target before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	// See the doc comment: without this, a same-tick dereference would be
	// indistinguishable from no dereference at all.
	time.Sleep(50 * time.Millisecond)

	if err := ChownTree(context.Background(), treeRoot, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("ChownTree: %v", err)
	}

	after, err := os.Lstat(outsideTarget)
	if err != nil {
		t.Fatalf("lstat outside target after: %v", err)
	}
	afterStat := after.Sys().(*syscall.Stat_t)
	if beforeStat.Ctim != afterStat.Ctim {
		t.Errorf("outside target ctime changed (target was touched, meaning the symlink was dereferenced): before %+v, after %+v",
			beforeStat.Ctim, afterStat.Ctim)
	}
}
