/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// OpenDirNoFollow resolves path via OpenParentNoFollow and opens the leaf
// itself with O_DIRECTORY|O_NOFOLLOW — refusing (not following) a symlink at
// the leaf, and refusing anything that isn't a directory. Unlike
// EnsureDirNoFollow, it never creates path: a missing path is reported as an
// error satisfying errors.Is(err, os.ErrNotExist), exactly like os.Open
// would, so callers that treat "no such directory" as a legitimate no-op
// (e.g. "nothing to clean up yet") keep that behaviour. The caller owns the
// returned fd and must close it.
//
// Use errors.Is, not os.IsNotExist, to check this: when the missing
// component is one of path's intermediate directories rather than its own
// leaf, the error comes back wrapped (via OpenParentNoFollow's fmt.Errorf),
// and os.IsNotExist only unwraps the specific *PathError/*LinkError/
// *SyscallError types, not an arbitrary %w chain.
func OpenDirNoFollow(path string) (*os.File, error) {
	dirFd, leaf, err := OpenParentNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	return OpenAt(dirFd, leaf, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY, 0)
}

// maxWalkDepth bounds the recursion ChownTreeNoFollow and
// RemoveContentsNoFollow perform. Both hold one open file descriptor per
// level of nesting for the lifetime of that level's recursive call, and
// recurse on the Go call stack; without a cap, a pathologically deep
// hostile directory tree (a scion-uid process nesting many thousands of
// directories under a path root's own walk covers) could exhaust the
// process's file descriptor limit, silently truncating the walk partway
// through with no signal beyond whatever the caller's onErr callback logs
// for the entries at the cutoff. 1024 is far deeper than any legitimate
// directory tree this package walks ($HOME, /workspace, a gcloud config
// directory) is expected to have.
// A package var, not a const, so a test can lower it temporarily to exercise
// the cutoff without actually building a many-thousand-directory tree on
// disk. Production code never changes it.
var maxWalkDepth = 1024

// ErrHardlinkedRegularFile is reported (via a walk's onErr callback) instead
// of chowning a regular file that has more than one hard link. A workload
// process can pre-plant a hard link to an unrelated (possibly root-owned)
// file it does not itself own — hard-linking only requires write access to
// the directory the link is created in, not ownership of the target — so a
// tree-wide chown that didn't check this could be tricked into handing an
// unrelated file to the workload. Skipping any regular file with Nlink > 1
// closes that gap. Not applied to directories, which legitimately have
// Nlink >= 2 (from their own "." and every subdirectory's "..").
//
// Declared here (rather than alongside ChownTreeNoFollow's own
// implementation) so it has one definition on every platform: callers that
// compare a walk's onErr error against this sentinel — e.g. via errors.Is —
// need it to exist and compile the same way regardless of which platform's
// ChownTreeNoFollow they end up linking against.
var ErrHardlinkedRegularFile = errors.New("dirfd: regular file has more than one hard link, refusing to chown")

// ErrMaxWalkDepthExceeded is reported (via a walk's onErr callback) when a
// directory is encountered at or beyond maxWalkDepth; that directory's own
// entry is still visited and chowned (if eligible) as a normal entry of its
// parent, but the walk does not descend into it.
var ErrMaxWalkDepthExceeded = errors.New("dirfd: max walk depth exceeded, not descending further")

// removeWalkTestHook, when non-nil, is invoked once right after this
// package opens a subdirectory it is about to empty and remove, before it
// lists/removes that subdirectory's own contents. Same purpose and same
// fd-holding guarantee as ChownTreeNoFollow's own walk test hooks. Always
// nil in production; unexported, this package's own tests are the only
// thing that may set it.
var removeWalkTestHook func(name string)

// removeWalkPreOpenTestHook, when non-nil, fires right after Fstatat has
// classified an entry as a directory and right before the
// O_NOFOLLOW openat that resolves it for real. This is the one window
// removeWalkTestHook (which fires after that openat) cannot cover: it lets
// a test prove that a symlink swapped into name's place in exactly that
// gap is refused (openat with O_NOFOLLOW fails, ELOOP) rather than
// followed, since O_NOFOLLOW — not the earlier Fstatat classification — is
// the only thing standing between this window and following the swap.
// Always nil in production; unexported.
var removeWalkPreOpenTestHook func(name string)

// RemoveContentsNoFollow removes every entry inside dir except those for
// which keep(name) returns true, without ever following a symlink or
// re-resolving a path: dir is an already-open fd, every removal is
// unlinkat(dirFd, name) (or unlinkat(dirFd, name, AT_REMOVEDIR) once a
// subdirectory's own contents are gone) relative to that fd, and every
// subdirectory is opened relative to its parent's held fd
// (openat(O_DIRECTORY|O_NOFOLLOW)) before being emptied the same way. This
// never re-resolves dir's path again after the caller opened it, so a
// symlink or directory swap planted at dir's own entry in its parent after
// that open cannot redirect anything this function does. It does not remove
// dir itself. Recursion is bounded by maxWalkDepth, reported via onErr.
//
// onErr, if non-nil, is called for every per-entry problem that does not
// abort the cleanup: a failed removal, a depth-cap cutoff, or a per-entry
// stat/open failure other than the ordinary "it vanished" case (ENOENT,
// not reported). It receives the entry's leaf name only. onErr may be nil,
// in which case these events are silently discarded, matching the
// historical os.RemoveAll-per-entry loop's best-effort behaviour.
func RemoveContentsNoFollow(dir *os.File, keep func(name string) bool, onErr func(name string, err error)) (removed int, err error) {
	return removeWalkChildren(dir, keep, onErr, 1)
}

func removeWalkChildren(dir *os.File, keep func(name string) bool, onErr func(string, error), depth int) (removed int, err error) {
	dirFd := int(dir.Fd())
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return 0, err
	}

	for _, name := range names {
		if keep != nil && keep(name) {
			continue
		}
		var st unix.Stat_t
		if serr := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
			if onErr != nil && !errors.Is(serr, unix.ENOENT) {
				onErr(name, serr)
			}
			continue
		}

		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			if depth >= maxWalkDepth {
				if onErr != nil {
					onErr(name, ErrMaxWalkDepthExceeded)
				}
				continue
			}
			if removeWalkPreOpenTestHook != nil {
				removeWalkPreOpenTestHook(name)
			}
			childFd, oerr := unix.Openat(dirFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
			if oerr != nil {
				// No longer a plain directory (raced out from under us,
				// e.g. swapped for a symlink) — refuse rather than follow;
				// skip this entry entirely, leaving it in place.
				if onErr != nil && !errors.Is(oerr, unix.ENOENT) {
					onErr(name, oerr)
				}
				continue
			}
			child := os.NewFile(uintptr(childFd), name)
			if removeWalkTestHook != nil {
				removeWalkTestHook(name)
			}
			if _, rerr := removeWalkChildren(child, nil, onErr, depth+1); rerr != nil {
				_ = child.Close()
				if onErr != nil {
					onErr(name, rerr)
				}
				continue
			}
			_ = child.Close()
			if uerr := unix.Unlinkat(dirFd, name, unix.AT_REMOVEDIR); uerr != nil {
				if onErr != nil {
					onErr(name, uerr)
				}
				continue
			}
		} else {
			if uerr := unix.Unlinkat(dirFd, name, 0); uerr != nil {
				if onErr != nil {
					onErr(name, uerr)
				}
				continue
			}
		}
		removed++
	}
	return removed, nil
}
