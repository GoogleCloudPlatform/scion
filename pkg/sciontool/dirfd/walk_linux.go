//go:build linux

/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// chownWalkTestHook, when non-nil, is invoked once right after this package
// opens a subdirectory encountered while walking (as a symlink-safe fd) and
// before it recurses into it. It receives the subdirectory's leaf name —
// purely informational, so a test can recognize "the directory it cares
// about" and act on that directory's entry in its parent (e.g. rename it
// away and plant a symlink in its place) — production code never derives
// any of its own operations from this name; every real fchownat/openat this
// package issues afterward stays relative to the fd it already holds, which
// is bound to the inode it was opened against and cannot be redirected by
// anything that happens to the directory's name in its parent afterward.
// This is what lets a test drive the exact "swap an intermediate directory
// for a symlink mid-walk" race deterministically instead of relying on a
// real, timing-dependent concurrent race. Always nil in production; tests
// must restore it to nil when done (it is a shared package-level var,
// unexported — this package's own tests are the only thing that may set it).
var chownWalkTestHook func(name string)

// chownLeafTestHook, when non-nil, fires in chownWalkChildren right after a
// non-directory entry has been resolved to an O_PATH fd and before that fd
// is fstat'd/fchown'd (i.e. immediately before chownWalkLeaf runs). It
// receives the entry's leaf name — purely informational, for the same
// reason and with the same "no real operation ever derives from it" and
// fd-immutability guarantee as chownWalkTestHook's doc comment describes.
// This is what lets a test drive "the leaf's name is swapped for something
// else immediately after this package resolved it to a descriptor"
// deterministically: because every decision after this point is made
// against the already-open fd, nothing the test does to the name at this
// point can change what gets chowned. Always nil in production; unexported.
var chownLeafTestHook func(name string)

// chownDirPreChownTestHook, when non-nil, fires in chownWalkDir right after
// a directory entry has been fstat'd and before it is fchown'd (i.e.
// strictly earlier than chownWalkTestHook, which fires after the chown
// decision, right before recursion). Same purpose as chownLeafTestHook,
// for the directory-entry chown itself rather than the leaf case. Always
// nil in production; unexported.
var chownDirPreChownTestHook func(name string)

// ChownTreeNoFollow walks root and everything beneath it, deciding for each
// entry — via shouldChown, given that entry's own owning uid — whether to
// chown it to uid:gid. It never follows a symlink and never re-resolves a
// name once it has been resolved to a file descriptor: root itself is
// opened once via OpenDirNoFollow; every other entry is opened exactly once,
// relative to its parent's already-open directory descriptor
// (openat(O_DIRECTORY|O_NOFOLLOW) for a directory, or
// openat(O_PATH|O_NOFOLLOW) for anything else), and every subsequent
// decision — the ownership check, the hard-link guard, the chown itself —
// is made against THAT descriptor (fstat(fd) / fchownat(fd, "",
// AT_EMPTY_PATH)), never by looking the name up again. This closes the
// window a stat-then-act-by-name approach would otherwise leave open: with
// only one resolve per entry, there is no second name lookup for a
// workload's concurrent rename/symlink-swap of that same name to land in
// between.
//
// A symlink entry is opened O_PATH|O_NOFOLLOW (which succeeds without
// following it or requiring its target to exist) and is itself eligible for
// chowning — fchownat with AT_EMPTY_PATH on an O_NOFOLLOW-opened descriptor
// chowns the symlink, never its target — but is never descended into.
//
// When guardHardlinks is true, a regular file with more than one hard link
// is skipped (reported via onErr as ErrHardlinkedRegularFile) rather than
// chowned: see ErrHardlinkedRegularFile's doc comment for why. Directories
// are never subject to this check.
//
// onErr, if non-nil, is called for every per-entry problem that does not
// abort the walk: a failed chown, a hard-link guard skip, a depth-cap
// cutoff (see maxWalkDepth), a directory listing failure (reported with the
// name "." — there is no single entry name to blame for failing to list a
// directory at all), or any per-entry open/stat failure other than the
// ordinary "it vanished between listing and this call" case (ENOENT, which
// is not reported — that is an expected race with the directory's own
// contents, not a problem with this package's handling of it). It receives
// the entry's leaf name only — never a full path, and never file content —
// so a caller logging it cannot leak anything beyond a bare filename. onErr
// may be nil, in which case these events are silently discarded (matching
// the historical behaviour before this parameter existed); callers that
// want them logged should pass a closure that does so.
//
// Returns the number of entries visited (including root itself) and the
// number actually chowned. Only an error opening or stat'ing root itself is
// returned as the third value; every deeper problem goes through onErr
// instead, so the walk always completes and reports its full counts even
// when individual entries fail.
//
// O_PATH and fchownat's AT_EMPTY_PATH are Linux-specific kernel features
// with no portable equivalent, which is why this implementation is built
// only for GOOS=linux; see walk_other.go for every other platform.
func ChownTreeNoFollow(root string, uid, gid int, shouldChown func(entryUID uint32) bool, guardHardlinks bool, onErr func(name string, err error)) (walked, changed int, err error) {
	rootFile, err := OpenDirNoFollow(root)
	if err != nil {
		return 0, 0, fmt.Errorf("dirfd: open %s: %w", root, err)
	}
	defer func() { _ = rootFile.Close() }()

	var rootSt unix.Stat_t
	if err := unix.Fstat(int(rootFile.Fd()), &rootSt); err != nil {
		return 0, 0, fmt.Errorf("dirfd: stat %s: %w", root, err)
	}
	walked++
	if shouldChown(rootSt.Uid) {
		// rootFile is an already-open fd bound to this exact inode: fchown
		// on it, never a path-based chown that could be redirected by
		// whatever root's own directory entry becomes afterward.
		if cerr := rootFile.Chown(uid, gid); cerr != nil {
			err = fmt.Errorf("dirfd: chown %s: %w", root, cerr)
		} else {
			changed++
		}
	}

	w, c := chownWalkChildren(rootFile, uid, gid, shouldChown, guardHardlinks, onErr, 1)
	walked += w
	changed += c
	return walked, changed, err
}

// chownWalkChildren lists dir's entries and resolves each exactly once —
// see ChownTreeNoFollow's doc comment for why this matters — to decide
// whether to chown it and whether to recurse. dir is the sole *os.File
// wrapping its fd for its entire lifetime in this walk (opened once by the
// caller or by this function's own Openat below, closed exactly once by
// whichever of the two owns it) — never re-wrapped, so there is never a
// second *os.File whose GC finalizer could close the fd out from under the
// other.
func chownWalkChildren(dir *os.File, uid, gid int, shouldChown func(entryUID uint32) bool, guardHardlinks bool, onErr func(string, error), depth int) (walked, changed int) {
	dirFd := int(dir.Fd())
	names, err := dir.Readdirnames(-1)
	if err != nil {
		// "." stands in for "this directory itself" — there is no single
		// entry name to blame for a failure to list it at all.
		if onErr != nil {
			onErr(".", err)
		}
		return 0, 0
	}

	for _, name := range names {
		// Try it as a directory first: openat(O_DIRECTORY|O_NOFOLLOW)
		// succeeds only for a real, non-symlink directory, and the fd it
		// returns is simultaneously the one used to fstat it, chown it (via
		// AT_EMPTY_PATH), and list its own children — one resolve of name,
		// reused for everything.
		if childFd, operr := unix.Openat(dirFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0); operr == nil {
			w, c := chownWalkDir(childFd, name, uid, gid, shouldChown, guardHardlinks, onErr, depth)
			walked += w
			changed += c
			continue
		}

		// Not a (non-symlink) directory: openat(O_PATH|O_NOFOLLOW) resolves
		// name exactly once more, succeeding for a regular file, a symlink
		// (without following it — O_PATH|O_NOFOLLOW never dereferences the
		// final component), or another special file type. The same fd is
		// then used for both the fstat and the fchown below, so there is
		// still only one resolve of name for this entry overall (the
		// O_DIRECTORY attempt above and this one are alternatives, not a
		// stat-then-act pair on the same open).
		pfd, operr := unix.Openat(dirFd, name, unix.O_PATH|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if operr != nil {
			// A plain "vanished between listing and this open" (ENOENT) is
			// not reported — that's an ordinary, expected race with the
			// directory's own contents, not a problem with this package's
			// handling of it. Anything else (EMFILE/ENFILE — the exact
			// fd-exhaustion signal the depth cap's own doc describes,
			// EACCES, etc.) is reported: it means an entry was silently
			// skipped for a reason worth knowing about.
			if onErr != nil && !errors.Is(operr, unix.ENOENT) {
				onErr(name, operr)
			}
			continue
		}
		if chownLeafTestHook != nil {
			chownLeafTestHook(name)
		}
		w, c := chownWalkLeaf(pfd, name, uid, gid, shouldChown, guardHardlinks, onErr)
		walked += w
		changed += c
	}
	return walked, changed
}

// chownWalkDir handles one child already confirmed (by a successful
// openat(O_DIRECTORY|O_NOFOLLOW)) to be a real directory: fstat, chown
// decision, depth-cap check, and recursion all happen against childFd,
// which this function closes before returning.
func chownWalkDir(childFd int, name string, uid, gid int, shouldChown func(entryUID uint32) bool, guardHardlinks bool, onErr func(string, error), depth int) (walked, changed int) {
	child := os.NewFile(uintptr(childFd), name)
	defer func() { _ = child.Close() }()

	var st unix.Stat_t
	if serr := unix.Fstat(childFd, &st); serr != nil {
		if onErr != nil {
			onErr(name, serr)
		}
		return 0, 0
	}
	walked = 1
	if shouldChown(st.Uid) {
		if chownDirPreChownTestHook != nil {
			chownDirPreChownTestHook(name)
		}
		if cerr := unix.Fchownat(childFd, "", uid, gid, unix.AT_EMPTY_PATH); cerr != nil {
			if onErr != nil {
				onErr(name, cerr)
			}
		} else {
			changed = 1
		}
	}

	if depth >= maxWalkDepth {
		if onErr != nil {
			onErr(name, ErrMaxWalkDepthExceeded)
		}
		return walked, changed
	}

	if chownWalkTestHook != nil {
		chownWalkTestHook(name)
	}
	w, c := chownWalkChildren(child, uid, gid, shouldChown, guardHardlinks, onErr, depth+1)
	return walked + w, changed + c
}

// chownWalkLeaf handles one child already confirmed (by a successful
// openat(O_PATH|O_NOFOLLOW)) not to be a directory it could descend into:
// fstat, the hard-link guard, and the chown decision all happen against
// pfd, which this function closes before returning. It never recurses,
// since a non-directory has no children.
func chownWalkLeaf(pfd int, name string, uid, gid int, shouldChown func(entryUID uint32) bool, guardHardlinks bool, onErr func(string, error)) (walked, changed int) {
	defer func() { _ = syscall.Close(pfd) }()

	var st unix.Stat_t
	if serr := unix.Fstat(pfd, &st); serr != nil {
		if onErr != nil {
			onErr(name, serr)
		}
		return 0, 0
	}
	walked = 1

	if guardHardlinks && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink > 1 {
		if onErr != nil {
			onErr(name, ErrHardlinkedRegularFile)
		}
		return walked, 0
	}

	if !shouldChown(st.Uid) {
		return walked, 0
	}
	if cerr := unix.Fchownat(pfd, "", uid, gid, unix.AT_EMPTY_PATH); cerr != nil {
		if onErr != nil {
			onErr(name, cerr)
		}
		return walked, 0
	}
	return walked, 1
}
