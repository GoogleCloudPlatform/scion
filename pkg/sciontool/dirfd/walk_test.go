//go:build linux

/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// ctimeSettle sleeps briefly so a subsequent chown(2) is guaranteed to bump
// ctime by a measurable amount. Some filesystems/clock sources this test
// suite runs on have coarse enough timestamp resolution that two syscalls
// issued back-to-back with no other work in between can otherwise land on
// the same recorded ctime even though a real ownership-changing chown ran.
func ctimeSettle() { time.Sleep(15 * time.Millisecond) }

// ctimeOf returns path's change time, as an opaque comparable value: chown(2)
// always bumps ctime even when it sets the same uid/gid a file already has
// (as this test's own uid/gid always are — an unprivileged process cannot
// chown to anyone else), so "ctime advanced" is a reliable proxy for "chown
// actually ran against this path" without needing real root.
func ctimeOf(t *testing.T, path string) syscall.Timespec {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no *syscall.Stat_t for %s", path)
	}
	return st.Ctim
}

func TestOpenDirNoFollow_MissingLeafIsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenDirNoFollow(filepath.Join(dir, "missing"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected errors.Is(err, os.ErrNotExist), got %v", err)
	}
}

// TestOpenDirNoFollow_MissingIntermediateComponentIsNotExist proves the
// errors.Is-not-os.IsNotExist distinction called out in OpenDirNoFollow's
// doc comment: when the missing component is an intermediate directory (not
// path's own leaf), the error comes back wrapped via OpenParentNoFollow's
// fmt.Errorf, which os.IsNotExist does not see through but errors.Is does.
func TestOpenDirNoFollow_MissingIntermediateComponentIsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenDirNoFollow(filepath.Join(dir, "missing-parent", "leaf"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected errors.Is(err, os.ErrNotExist), got %v", err)
	}
	if os.IsNotExist(err) {
		t.Log("os.IsNotExist now also recognizes this — the doc comment's warning may be stale")
	}
}

func TestOpenDirNoFollow_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDirNoFollow(link); err == nil {
		t.Fatal("expected an error opening a symlinked leaf, got nil")
	}
}

// TestChownTreeNoFollow_ChownsMatchingEntriesOnly proves the basic walk +
// filter behaviour: every entry (root, subdirectories, files) is visited,
// shouldChown decides which ones actually get chowned, and the walked/
// changed counts reflect that.
func TestChownTreeNoFollow_ChownsMatchingEntriesOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "chown-me"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "leave-me"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "chown-me"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := map[string]syscall.Timespec{}
	for _, p := range []string{"chown-me", "leave-me", "sub", "sub/chown-me"} {
		before[p] = ctimeOf(t, filepath.Join(root, p))
	}
	ctimeSettle()

	uid, gid := os.Getuid(), os.Getgid()
	walked, changed, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool {
		return true // stand-in for a real filter; exercised for real below.
	}, false, nil)
	if err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	// root + 4 entries.
	if walked != 5 {
		t.Errorf("walked = %d, want 5", walked)
	}
	if changed != 5 {
		t.Errorf("changed = %d, want 5", changed)
	}
	for _, p := range []string{"chown-me", "leave-me", "sub", "sub/chown-me"} {
		if ctimeOf(t, filepath.Join(root, p)) == before[p] {
			t.Errorf("%s: ctime did not advance, chown did not run", p)
		}
	}
}

// TestChownTreeNoFollow_FilterSkipsNonMatchingEntries proves shouldChown
// actually gates the chown, using ownership as the filter the way
// chownTreeRootOwned does (only uid==0 entries) — here inverted to "only
// entries NOT owned by the current uid" so the test's own files (which it
// owns) are provably left alone.
func TestChownTreeNoFollow_FilterSkipsNonMatchingEntries(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "file")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A subdirectory too, so the directory branch's own shouldChown check
	// (chownWalkDir) is exercised, not only the leaf branch's.
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	before := ctimeOf(t, target)
	subBefore := ctimeOf(t, sub)
	rootBefore := ctimeOf(t, root)
	ctimeSettle()

	uid, gid := os.Getuid(), os.Getgid()
	walked, changed, err := ChownTreeNoFollow(root, uid, gid, func(entryUID uint32) bool {
		return entryUID != uint32(os.Getuid()) // never true for our own files
	}, false, nil)
	if err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	if walked != 3 {
		t.Errorf("walked = %d, want 3 (root, file, sub)", walked)
	}
	if changed != 0 {
		t.Errorf("changed = %d, want 0 (filter should have skipped every entry)", changed)
	}
	if ctimeOf(t, target) != before {
		t.Error("file was chowned despite the filter returning false")
	}
	if ctimeOf(t, sub) != subBefore {
		t.Error("subdirectory was chowned despite the filter returning false")
	}
	if ctimeOf(t, root) != rootBefore {
		t.Error("root was chowned despite the filter returning false")
	}
}

// TestChownTreeNoFollow_SymlinkEntryNotFollowed proves a symlink INSIDE the
// tree pointing at a directory OUTSIDE the tree is never descended into —
// only the symlink itself is eligible for chowning (fchownat with
// AT_SYMLINK_NOFOLLOW chowns the link, never the target) — so the victim
// directory's own contents are completely untouched.
func TestChownTreeNoFollow_SymlinkEntryNotFollowed(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	victimFile := filepath.Join(victim, "secret")
	if err := os.WriteFile(victimFile, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	victimBefore := ctimeOf(t, victimFile)
	victimDirBefore := ctimeOf(t, victim)

	link := filepath.Join(root, "escape")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	// Without this, a chown that wrongly follows the symlink can land in the
	// same coarse-clock tick as the baselines above, leave ctime unchanged,
	// and let the assertions below pass by accident — a no-follow regression
	// here was observed to be caught only intermittently across many runs
	// without this settle.
	ctimeSettle()

	uid, gid := os.Getuid(), os.Getgid()
	if _, _, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, false, nil); err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}

	if ctimeOf(t, victimFile) != victimBefore {
		t.Error("victim file inside the symlink target was touched")
	}
	if ctimeOf(t, victim) != victimDirBefore {
		t.Error("victim directory (the symlink's target) was chowned — the symlink was followed")
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected the entry to still be a symlink")
	}
}

// TestChownTreeNoFollow_SymlinkToRegularFileEntryNotFollowed is the leaf
// counterpart of TestChownTreeNoFollow_SymlinkEntryNotFollowed: a symlink
// inside the tree pointing at a regular file outside it. The O_DIRECTORY
// attempt fails for it (ENOTDIR whether or not that open uses O_NOFOLLOW),
// so this entry is resolved only by the leaf openat(O_PATH|O_NOFOLLOW). If
// that open followed the link, the fd-based fchownat would chown the victim
// file itself. This test guards the leaf resolution independently of the
// directory one.
func TestChownTreeNoFollow_SymlinkToRegularFileEntryNotFollowed(t *testing.T) {
	root := t.TempDir()
	victimDir := t.TempDir()
	victimFile := filepath.Join(victimDir, "secret")
	if err := os.WriteFile(victimFile, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(victimFile, link); err != nil {
		t.Fatal(err)
	}
	victimBefore := ctimeOf(t, victimFile)
	linkBefore := ctimeOf(t, link)
	ctimeSettle()

	uid, gid := os.Getuid(), os.Getgid()
	walked, changed, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, false, nil)
	if err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	if walked != 2 || changed != 2 {
		t.Errorf("walked, changed = %d, %d; want 2, 2 (root + the symlink itself)", walked, changed)
	}
	if ctimeOf(t, victimFile) != victimBefore {
		t.Error("victim file (the symlink's target) was chowned: the leaf open followed the symlink")
	}
	if ctimeOf(t, link) == linkBefore {
		t.Error("the symlink itself was not chowned; expected the walk to chown the link, not skip it")
	}
}

// TestChownTreeNoFollow_SurvivesIntermediateDirSwapMidWalk is the class's
// core regression test: a scion-uid process observes the walk enter a real
// subdirectory it owns, then — before the walk finishes with that
// subdirectory — renames it away and plants a symlink to a victim directory
// in its place. A name-based chown (e.g. filepath.Walk+os.Lchown(path))
// re-resolves the full path string on every Lchown call, so a deeper call
// would walk straight through a symlink freshly planted at an ancestor's
// name into the victim. The fd-based walk instead holds an open fd for the
// subdirectory from the moment it is entered, so nothing that happens to
// its *name* in the parent afterward can redirect any operation still in
// flight underneath it.
//
// The swap is injected via chownWalkTestHook, fired right after the walk
// opens the subdirectory's fd and before it processes that subdirectory's
// own entries — the same window the class's real, timing-dependent exploit
// needs, made deterministic exactly like writeEnvFileAfterWriteForTest
// does for writeEnvFile's directory-chown race.
func TestChownTreeNoFollow_SurvivesIntermediateDirSwapMidWalk(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	// The victim's file is deliberately named "inner", matching the real
	// subdirectory's own entry below: a name-based chown reconstructs the
	// path string "root/cache/inner" and re-lstats/re-lchowns it, so it only
	// actually lands on the victim when the victim happens to have an entry
	// by the same name the walk was already expecting to find there.
	victimMarker := filepath.Join(victim, "inner")
	if err := os.WriteFile(victimMarker, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	victimMarkerBefore := ctimeOf(t, victimMarker)
	victimBefore := ctimeOf(t, victim)

	subdir := filepath.Join(root, "cache")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	innerFile := filepath.Join(subdir, "inner")
	if err := os.WriteFile(innerFile, []byte("real content"), 0o644); err != nil {
		t.Fatal(err)
	}
	innerBefore := ctimeOf(t, innerFile)
	ctimeSettle()

	moved := filepath.Join(root, "cache.moved")
	var swapped bool
	chownWalkTestHook = func(name string) {
		if swapped || name != "cache" {
			return
		}
		swapped = true
		if err := os.Rename(subdir, moved); err != nil {
			t.Errorf("swap: rename: %v", err)
			return
		}
		if err := os.Symlink(victim, subdir); err != nil {
			t.Errorf("swap: symlink: %v", err)
		}
	}
	t.Cleanup(func() { chownWalkTestHook = nil })

	uid, gid := os.Getuid(), os.Getgid()
	if _, _, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, false, nil); err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	if !swapped {
		t.Fatal("test hook never fired — test is not exercising the intended window")
	}

	// The victim must be completely untouched.
	if ctimeOf(t, victimMarker) != victimMarkerBefore {
		t.Error("victim marker file was chowned through the swapped-in symlink")
	}
	if ctimeOf(t, victim) != victimBefore {
		t.Error("victim directory was chowned through the swapped-in symlink")
	}

	// "cache" in root must still be the symlink the swap planted — the walk
	// must not have unlinked or replaced it.
	linkInfo, err := os.Lstat(subdir)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected root/cache to still be the symlink the swap planted")
	}

	// The real subdirectory, now sitting at its moved-away name, is the one
	// that should have kept being walked and chowned via the fd the walk
	// already held before the swap. root/cache is now the swapped-in
	// symlink, so the original file's current path is moved/inner.
	movedInnerFile := filepath.Join(moved, "inner")
	if ctimeOf(t, movedInnerFile) == innerBefore {
		t.Error("the original subdirectory's own content was not chowned — the walk lost its held fd across the swap")
	}
}

func TestRemoveContentsNoFollow_RemovesEverythingExceptKept(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "keep"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "sub", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "nested", "f"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := OpenDirNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	removed, err := RemoveContentsNoFollow(dir, func(name string) bool { return name == "keep" }, nil)
	if err != nil {
		t.Fatalf("RemoveContentsNoFollow: %v", err)
	}
	if removed != 3 { // "a", "b", "sub" (nested contents don't count as top-level)
		t.Errorf("removed = %d, want 3", removed)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "keep" {
		t.Errorf("remaining entries = %v, want [keep]", names)
	}
}

func TestRemoveContentsNoFollow_RefusesSymlinkedSubdirEntry(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	sentinel := filepath.Join(victim, "sentinel")
	if err := os.WriteFile(sentinel, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "escape")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	dir, err := OpenDirNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	// A symlink entry is not a directory (unix.S_IFDIR won't match its own
	// lstat), so it takes the non-directory unlinkat branch and is removed
	// as a link — but the victim it pointed at must survive.
	if _, err := RemoveContentsNoFollow(dir, nil, nil); err != nil {
		t.Fatalf("RemoveContentsNoFollow: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("victim sentinel file should survive: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected the symlink entry itself to be removed, lstat err = %v", err)
	}
}

// TestRemoveContentsNoFollow_SurvivesSubdirSwapMidWalk mirrors
// TestChownTreeNoFollow_SurvivesIntermediateDirSwapMidWalk for the delete
// walk: a scion-uid process swaps a subdirectory already entered by
// the walk for a symlink to a victim directory before the walk finishes
// deleting that subdirectory's own contents. The victim must never be
// touched, and the walk must keep operating on the fd it already holds for
// the real (now moved-away) subdirectory.
func TestRemoveContentsNoFollow_SurvivesSubdirSwapMidWalk(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	// Named "inner" to match the real subdirectory's own entry below — a
	// name-based delete re-joins "gcloud-sub/inner" as a string and
	// re-resolves it, so it only lands on the victim when the victim
	// happens to have a same-named entry.
	victimMarker := filepath.Join(victim, "inner")
	if err := os.WriteFile(victimMarker, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	subdir := filepath.Join(root, "gcloud-sub")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	innerFile := filepath.Join(subdir, "inner")
	if err := os.WriteFile(innerFile, []byte("real content"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(root, "gcloud-sub.moved")
	var swapped bool
	removeWalkTestHook = func(name string) {
		if swapped || name != "gcloud-sub" {
			return
		}
		swapped = true
		if err := os.Rename(subdir, moved); err != nil {
			t.Errorf("swap: rename: %v", err)
			return
		}
		if err := os.Symlink(victim, subdir); err != nil {
			t.Errorf("swap: symlink: %v", err)
		}
	}
	t.Cleanup(func() { removeWalkTestHook = nil })

	dir, err := OpenDirNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	if _, err := RemoveContentsNoFollow(dir, nil, nil); err != nil {
		t.Fatalf("RemoveContentsNoFollow: %v", err)
	}
	if !swapped {
		t.Fatal("test hook never fired — test is not exercising the intended window")
	}

	if _, err := os.Stat(victimMarker); err != nil {
		t.Fatalf("victim marker must survive: %v", err)
	}
	if entries, err := os.ReadDir(victim); err != nil || len(entries) != 1 {
		t.Errorf("victim directory contents changed: entries=%v err=%v", entries, err)
	}

	// root/gcloud-sub must still be the symlink the swap planted.
	linkInfo, err := os.Lstat(subdir)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected root/gcloud-sub to still be the symlink the swap planted")
	}

	// The real, moved-away subdirectory's own contents should have been
	// deleted via the fd the walk already held — proving it kept operating
	// on the original directory, not the swapped-in symlink. root/gcloud-sub
	// is now the swapped-in symlink, so the original file's current path is
	// moved/inner.
	movedInnerFile := filepath.Join(moved, "inner")
	if _, err := os.Stat(movedInnerFile); !os.IsNotExist(err) {
		t.Errorf("expected the original subdirectory's inner file to be removed, got err=%v", err)
	}
}

// TestChownTreeNoFollow_ReportsPerEntryChownFailureViaOnErr proves per-entry
// chown failures are surfaced through onErr rather than silently discarded,
// for BOTH the leaf branch (chownWalkLeaf) and the directory branch
// (chownWalkDir) — a subdirectory entry exercises the latter. Chowning to
// uid 0 (root) as a non-root test process is guaranteed to fail with EPERM
// for the root directory itself (surfaced via the returned err), for a
// leaf child entry, and for a subdirectory entry (the directory branch
// reports it via onErr too, not silently swallowed).
func TestChownTreeNoFollow_ReportsPerEntryChownFailureViaOnErr(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chowning to uid 0 would trivially succeed")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	var onErrCalls []string
	_, _, err := ChownTreeNoFollow(root, 0, 0, func(uint32) bool { return true }, false, func(name string, e error) {
		onErrCalls = append(onErrCalls, name)
	})
	if err == nil {
		t.Fatal("expected chowning root to uid 0 as non-root to fail")
	}
	for _, want := range []string{"child", "subdir"} {
		found := false
		for _, name := range onErrCalls {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected onErr to be called for %q, got calls: %v", want, onErrCalls)
		}
	}
}

// TestChownTreeNoFollow_GuardHardlinksSkipsMultiplyLinkedRegularFile proves
// the hard-link guard: a workload can pre-plant a hard link to a file it
// does not own (hard-linking only needs write access to the directory the
// link is created in), so a regular file with more than one link is skipped
// rather than chowned when guardHardlinks is true, and reported via onErr.
func TestChownTreeNoFollow_GuardHardlinksSkipsMultiplyLinkedRegularFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "hardlink")
	if err := os.Link(target, link); err != nil {
		t.Fatal(err)
	}
	before := ctimeOf(t, target)
	ctimeSettle()

	reported := map[string]bool{}
	uid, gid := os.Getuid(), os.Getgid()
	_, changed, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, true, func(name string, e error) {
		if e == ErrHardlinkedRegularFile {
			reported[name] = true
		}
	})
	if err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	if !reported["target"] || !reported["hardlink"] {
		t.Fatalf("expected onErr to report ErrHardlinkedRegularFile for both entries, got: %v", reported)
	}
	if changed != 1 {
		// root itself is still chowned normally (it isn't a regular file,
		// so the hard-link guard never applies to it) — only "target" and
		// "hardlink" (the two entries sharing the guarded inode) must be
		// skipped.
		t.Errorf("changed = %d, want 1 (root only — both hard-linked entries must be skipped)", changed)
	}
	if ctimeOf(t, target) != before {
		t.Error("target file's ctime advanced — it was chowned despite the hard-link guard")
	}
}

// TestChownTreeNoFollow_GuardHardlinksDisabledStillChownsMultiplyLinkedFile
// proves guardHardlinks=false preserves the historical (non-enforced)
// behaviour: a hard-linked regular file is chowned like any other entry.
func TestChownTreeNoFollow_GuardHardlinksDisabledStillChownsMultiplyLinkedFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "hardlink")
	if err := os.Link(target, link); err != nil {
		t.Fatal(err)
	}
	before := ctimeOf(t, target)
	ctimeSettle()

	uid, gid := os.Getuid(), os.Getgid()
	_, _, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, false, nil)
	if err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	if ctimeOf(t, target) == before {
		t.Error("expected the hard-linked file to be chowned when guardHardlinks is false")
	}
}

// TestChownTreeNoFollow_DepthCapStopsDescendingAndReportsViaOnErr proves the
// depth cap: a directory at or beyond the cap is itself still visited and
// chowned, but the walk does not descend into it, and onErr is told why.
func TestChownTreeNoFollow_DepthCapStopsDescendingAndReportsViaOnErr(t *testing.T) {
	origDepth := maxWalkDepth
	maxWalkDepth = 2
	t.Cleanup(func() { maxWalkDepth = origDepth })

	root := t.TempDir()
	level1 := filepath.Join(root, "level1")
	level2 := filepath.Join(level1, "level2")
	tooDeepFile := filepath.Join(level2, "too-deep")
	if err := os.MkdirAll(level2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tooDeepFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tooDeepBefore := ctimeOf(t, tooDeepFile)
	ctimeSettle()

	var gotCutoff bool
	uid, gid := os.Getuid(), os.Getgid()
	walked, _, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, false, func(name string, e error) {
		if e == ErrMaxWalkDepthExceeded {
			gotCutoff = true
		}
	})
	if err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	if !gotCutoff {
		t.Fatal("expected onErr to report ErrMaxWalkDepthExceeded")
	}
	// root, level1, level2 — not too-deep-file, which is beyond the cap.
	if walked != 3 {
		t.Errorf("walked = %d, want 3 (root, level1, level2 — not the file beyond the cap)", walked)
	}
	if ctimeOf(t, tooDeepFile) != tooDeepBefore {
		t.Error("expected the file beyond the depth cap to be left untouched")
	}
}

// TestRemoveContentsNoFollow_DepthCapStopsDescendingAndReportsViaOnErr
// mirrors the chown depth-cap test for the delete walk.
func TestRemoveContentsNoFollow_DepthCapStopsDescendingAndReportsViaOnErr(t *testing.T) {
	origDepth := maxWalkDepth
	maxWalkDepth = 2
	t.Cleanup(func() { maxWalkDepth = origDepth })

	root := t.TempDir()
	level1 := filepath.Join(root, "level1")
	level2 := filepath.Join(level1, "level2")
	tooDeepFile := filepath.Join(level2, "too-deep")
	if err := os.MkdirAll(level2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tooDeepFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := OpenDirNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	var gotCutoff bool
	if _, err := RemoveContentsNoFollow(dir, nil, func(name string, e error) {
		if e == ErrMaxWalkDepthExceeded {
			gotCutoff = true
		}
	}); err != nil {
		t.Fatalf("RemoveContentsNoFollow: %v", err)
	}
	if !gotCutoff {
		t.Fatal("expected onErr to report ErrMaxWalkDepthExceeded")
	}
	if _, err := os.Stat(tooDeepFile); err != nil {
		t.Errorf("expected the file beyond the depth cap to survive: %v", err)
	}
}

// TestChownTreeNoFollow_LeafSwapAfterResolveDoesNotRedirectChown is the
// core regression test for the single-resolve TOCTOU closure: once a
// non-directory entry has been resolved to an O_PATH fd, nothing that
// happens to its NAME afterward — including being replaced with a hard
// link to a victim file — can redirect the chown that fd is about to
// receive. Exercised with the hard-link guard both off and on: the guard
// decides based on the PRE-swap inode's Nlink (1, since the swap hasn't
// happened yet when the fd is resolved), so it must not affect this
// property either way.
func TestChownTreeNoFollow_LeafSwapAfterResolveDoesNotRedirectChown(t *testing.T) {
	for _, guard := range []bool{false, true} {
		t.Run(fmt.Sprintf("guardHardlinks=%v", guard), func(t *testing.T) {
			root, victimDir := t.TempDir(), t.TempDir()
			victim := filepath.Join(victimDir, "victim")
			if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(root, "leaf")
			if err := os.WriteFile(leaf, []byte("l"), 0o644); err != nil {
				t.Fatal(err)
			}
			victimBefore := ctimeOf(t, victim)
			movedBefore := ctimeOf(t, leaf)
			ctimeSettle()

			var fired bool
			chownLeafTestHook = func(name string) {
				if name != "leaf" || fired {
					return
				}
				fired = true
				if err := os.Rename(leaf, leaf+".moved"); err != nil {
					t.Errorf("swap: rename: %v", err)
					return
				}
				// rename(2) itself bumps the renamed inode's ctime;
				// re-snapshot AFTER the rename so the held-fd assertion
				// below ("the original leaf was chowned") can't pass
				// vacuously just because the rename already advanced it.
				movedBefore = ctimeOf(t, leaf+".moved")
				if err := os.Link(victim, leaf); err != nil {
					t.Errorf("swap: link: %v", err)
					return
				}
				// link(2) itself bumps the victim's ctime (Nlink changed);
				// re-snapshot so the assertion below only catches a chown,
				// not the link creation.
				victimBefore = ctimeOf(t, victim)
				ctimeSettle()
			}
			t.Cleanup(func() { chownLeafTestHook = nil })

			uid, gid := os.Getuid(), os.Getgid()
			_, _, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, guard, nil)
			if err != nil {
				t.Fatalf("ChownTreeNoFollow: %v", err)
			}
			if !fired {
				t.Fatal("test hook never fired — test is not exercising the intended window")
			}

			if ctimeOf(t, victim) != victimBefore {
				t.Error("victim file was chowned via the leaf name swapped in after resolve")
			}
			if ctimeOf(t, leaf+".moved") == movedBefore {
				t.Error("the original (held-fd) leaf was not chowned")
			}
		})
	}
}

// TestChownTreeNoFollow_DirEntrySwapAfterFstatDoesNotRedirectChown mirrors
// the leaf test above for a directory entry: once chownWalkDir has fstat'd
// the entry, swapping its name in the parent for a symlink to a victim
// directory — immediately before the fchownat call — must not redirect
// that chown. This closes the same TOCTOU shape one level earlier than
// TestChownTreeNoFollow_SurvivesIntermediateDirSwapMidWalk (which proves
// recursion survives a later swap); this one proves the directory's OWN
// chown does.
func TestChownTreeNoFollow_DirEntrySwapAfterFstatDoesNotRedirectChown(t *testing.T) {
	root, victimDir := t.TempDir(), t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "sub.moved")

	victimBefore := ctimeOf(t, victimDir)
	subBefore := ctimeOf(t, sub)
	ctimeSettle()

	var fired bool
	chownDirPreChownTestHook = func(name string) {
		if name != "sub" || fired {
			return
		}
		fired = true
		if err := os.Rename(sub, moved); err != nil {
			t.Errorf("swap: rename: %v", err)
			return
		}
		// rename(2) itself bumps the renamed inode's ctime; re-snapshot
		// AFTER the rename so the held-fd assertion below ("the original
		// subdirectory was chowned") can't pass vacuously just because the
		// rename already advanced it — snapshotting before the rename would
		// make the assertion below vacuously unable to fail.
		subBefore = ctimeOf(t, moved)
		if err := os.Symlink(victimDir, sub); err != nil {
			t.Errorf("swap: symlink: %v", err)
		}
		ctimeSettle()
	}
	t.Cleanup(func() { chownDirPreChownTestHook = nil })

	uid, gid := os.Getuid(), os.Getgid()
	_, _, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, false, nil)
	if err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	if !fired {
		t.Fatal("test hook never fired — test is not exercising the intended window")
	}

	if ctimeOf(t, victimDir) != victimBefore {
		t.Error("victim directory was chowned via the symlink swapped in after fstat")
	}
	if ctimeOf(t, moved) == subBefore {
		t.Error("the original (held-fd) subdirectory was not chowned")
	}
	linkInfo, err := os.Lstat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected root/sub to still be the symlink the swap planted")
	}
}

// TestChownTreeNoFollow_DirEntrySwapToHardlinkedFileDoesNotRedirectChown is
// the real security property the swap test above only established
// indirectly: the entry swapped in at a directory's name after it has been
// resolved and fstat'd need not be another directory or a symlink — it can
// be a hard link to a REGULAR victim file. A name-based
// fchownat(parentFd, name, AT_SYMLINK_NOFOLLOW) would chown the victim
// through that hard link, bypassing BOTH the hard-link guard (which is
// only ever checked in the leaf branch, never the directory branch — a
// directory can't itself be hard-linked, but the workload can swap the
// NAME the directory branch is about to act on for something that can be)
// and the shouldChown decision (made on the pre-swap directory's fstat,
// not on the victim's). The fd-based AT_EMPTY_PATH chown, resolved before
// any of this happens, is immune. guardHardlinks=true is deliberately
// exercised here: if the swapped-in hard link were ever visited as an
// ordinary leaf entry after the swap, the guard would skip it, so any
// post-swap ctime bump on the victim can only come from the directory
// branch's own chown landing on it.
func TestChownTreeNoFollow_DirEntrySwapToHardlinkedFileDoesNotRedirectChown(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim-src") // same filesystem so link(2) works
	if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	var victimBefore syscall.Timespec
	fired := false
	chownDirPreChownTestHook = func(name string) {
		if name != "sub" || fired {
			return
		}
		fired = true
		if err := os.Rename(sub, filepath.Join(root, "sub.moved")); err != nil {
			t.Errorf("swap: rename: %v", err)
			return
		}
		if err := os.Link(victim, sub); err != nil {
			t.Errorf("swap: link: %v", err)
			return
		}
		// link(2) itself bumps the victim's ctime (Nlink changed);
		// re-snapshot so the assertion below only catches a chown, not the
		// link creation.
		victimBefore = ctimeOf(t, victim)
		ctimeSettle()
	}
	t.Cleanup(func() { chownDirPreChownTestHook = nil })

	uid, gid := os.Getuid(), os.Getgid()
	if _, _, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, true, nil); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("test hook never fired — test is not exercising the intended window")
	}
	if ctimeOf(t, victim) != victimBefore {
		t.Error("directory-branch chown landed on a hard-linked regular file swapped in after fstat")
	}
}

// TestRemoveContentsNoFollow_RefusesSymlinkSwappedInBeforeSubdirOpen is the
// core regression test: the ONLY thing that stops a symlink swapped into a
// subdirectory's name — after Fstatat has already classified it as a
// directory, but before the O_NOFOLLOW openat resolves it for real — from
// being followed is that openat's own O_NOFOLLOW flag. Without it, this
// would open (and then empty) the victim directory the symlink points at.
func TestRemoveContentsNoFollow_RefusesSymlinkSwappedInBeforeSubdirOpen(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	sentinel := filepath.Join(victim, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	var fired bool
	removeWalkPreOpenTestHook = func(name string) {
		if name != "sub" || fired {
			return
		}
		fired = true
		if err := os.RemoveAll(sub); err != nil {
			t.Errorf("swap: remove: %v", err)
			return
		}
		if err := os.Symlink(victim, sub); err != nil {
			t.Errorf("swap: symlink: %v", err)
		}
	}
	t.Cleanup(func() { removeWalkPreOpenTestHook = nil })

	dir, err := OpenDirNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	if _, err := RemoveContentsNoFollow(dir, nil, nil); err != nil {
		t.Fatalf("RemoveContentsNoFollow: %v", err)
	}
	if !fired {
		t.Fatal("test hook never fired — test is not exercising the intended window")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("victim sentinel must survive: %v", err)
	}
	linkInfo, err := os.Lstat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected root/sub to still be the symlink the swap planted")
	}
}

// TestChownTreeNoFollow_ReportsPerEntryOpenFailureViaOnErr guards the chown
// walk's "non-ENOENT open/stat failure" onErr reporting: a real,
// deterministic, non-fault-injected trigger. Removing search (execute)
// permission on a directory makes every openat/fstatat relative to it fail
// EACCES for anything inside it, for a non-root process — no seam needed.
// Proves both that onErr fires with the entry's name and the real error,
// and that the entry is left un-chowned (fail-safe: a skipped entry is
// never acted on).
func TestChownTreeNoFollow_ReportsPerEntryOpenFailureViaOnErr(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: root bypasses the permission check this test relies on")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fBefore := ctimeOf(t, f)
	ctimeSettle()

	// Read-but-no-search: os.ReadDir(sub) (and this package's own
	// Readdirnames on an already-open fd) still lists "f", but any
	// openat/fstatat relative to sub's fd trying to resolve "f" fails
	// EACCES — sub's own listing succeeded, its contents did not resolve.
	if err := os.Chmod(sub, 0o600); err != nil {
		t.Fatal(err)
	}

	var onErrCalls []string
	var gotEACCES bool
	uid, gid := os.Getuid(), os.Getgid()
	_, _, err := ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, false, func(name string, e error) {
		onErrCalls = append(onErrCalls, name)
		if name == "f" && e == syscall.EACCES {
			gotEACCES = true
		}
	})
	// Restore before any further path-based inspection (including this
	// test's own ctimeOf call below), which would otherwise also be
	// refused by the same permission this test just applied.
	_ = os.Chmod(sub, 0o755)
	if err != nil {
		t.Fatalf("ChownTreeNoFollow: %v", err)
	}
	if !gotEACCES {
		t.Errorf("expected onErr to be called with \"f\"/EACCES, got calls: %v", onErrCalls)
	}
	if ctimeOf(t, f) != fBefore {
		t.Error("f was chowned despite the open failure — a skipped entry must be left un-chowned")
	}
}

// TestRemoveContentsNoFollow_ReportsPerEntryStatFailureViaOnErr is the
// remove-walk twin: the same real EACCES trigger, this time hitting
// RemoveContentsNoFollow's own Fstatat call. Proves onErr fires and that
// the entry survives (fail-safe: a skipped entry is never deleted).
func TestRemoveContentsNoFollow_ReportsPerEntryStatFailureViaOnErr(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: root bypasses the permission check this test relies on")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := OpenDirNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	if err := os.Chmod(sub, 0o600); err != nil {
		t.Fatal(err)
	}

	var onErrCalls []string
	var gotEACCES bool
	if _, err := RemoveContentsNoFollow(dir, nil, func(name string, e error) {
		onErrCalls = append(onErrCalls, name)
		if name == "f" && e == syscall.EACCES {
			gotEACCES = true
		}
	}); err != nil {
		_ = os.Chmod(sub, 0o755)
		t.Fatalf("RemoveContentsNoFollow: %v", err)
	}
	// Restore before inspecting: os.Stat(f) below needs search permission
	// on sub too.
	_ = os.Chmod(sub, 0o755)

	if !gotEACCES {
		t.Errorf("expected onErr to be called with \"f\"/EACCES, got calls: %v", onErrCalls)
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("f should survive (fail-safe: a skipped entry is never deleted): %v", err)
	}
}

// TestChownTreeNoFollow_RootSwapAfterOpenDoesNotRedirectChown proves root's
// own chown acts on the file descriptor ChownTreeNoFollow already opened,
// not on whatever directory entry root's path currently names. The
// caller-supplied shouldChown callback runs between root's open/fstat and
// root's chown, so this test uses it to swap root's directory entry at
// exactly that point, with no production hook involved: a path-based chown
// would then re-resolve the (now missing) name and fail, while an
// fd-based chown still lands on the held inode.
func TestChownTreeNoFollow_RootSwapAfterOpenDoesNotRedirectChown(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	first := true
	should := func(uint32) bool {
		if first {
			first = false
			if err := os.Rename(root, filepath.Join(parent, "moved")); err != nil {
				t.Fatal(err)
			}
		}
		return true
	}
	walked, changed, err := ChownTreeNoFollow(root, os.Getuid(), os.Getgid(), should, true, nil)
	if err != nil || walked != 1 || changed != 1 {
		t.Fatalf("ChownTreeNoFollow = (%d, %d, %v), want (1, 1, nil): root chown must act on the held fd, not re-resolve root's path", walked, changed, err)
	}
}
