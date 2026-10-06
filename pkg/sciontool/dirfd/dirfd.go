/*
Copyright 2026 The Scion Authors.
*/

// Package dirfd resolves a path into an open directory file descriptor by
// walking every path component with openat(2), so that a symlink swapped
// into ANY component of the path — not just the final one — is refused
// instead of followed.
//
// A plain O_NOFOLLOW on the final os.OpenFile call only protects the leaf:
// if an intermediate directory such as $HOME/.scion is itself replaced with
// a symlink (something a process that owns $HOME can always do), the
// kernel still resolves and follows that symlink while walking the rest of
// the path, because O_NOFOLLOW only applies to the last component. A
// process that runs as root for its whole life over a directory tree a
// less-privileged user owns (the sciontool init/substrate-serve pattern)
// needs every component checked, not just the last one.
//
// Callers get back an open fd for path's parent directory and path's own
// leaf name, and are expected to do every remaining operation — create,
// open, rename, chown — through the *at() syscalls relative to that fd,
// never through another path-based call, so nothing after the walk can be
// redirected by a symlink swapped in afterwards.
package dirfd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// OpenParentNoFollow resolves path's parent directory into an open,
// symlink-safe file descriptor. It walks every component from "/" down,
// opening each with O_DIRECTORY|O_NOFOLLOW, so a symlink planted at any
// component along the way makes the walk fail (ELOOP) instead of being
// followed. The caller owns the returned fd and must close it.
//
// path must be absolute (or resolvable via filepath.Abs against the
// current working directory); it does not need to exist yet — only its
// parent directories do. The leaf itself is never opened or stat'd here:
// that's left to the caller, since callers vary in whether the leaf should
// be created, read, or checked first.
func OpenParentNoFollow(path string) (dirFd int, leaf string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return -1, "", fmt.Errorf("dirfd: resolve %s: %w", path, err)
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return -1, "", fmt.Errorf("dirfd: refusing to resolve the root directory itself")
	}

	trimmed := strings.TrimPrefix(abs, string(filepath.Separator))
	parts := strings.Split(trimmed, string(filepath.Separator))
	leaf = parts[len(parts)-1]
	dirs := parts[:len(parts)-1]

	fd, err := syscall.Open(string(filepath.Separator), syscall.O_DIRECTORY|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("dirfd: open /: %w", err)
	}
	for _, name := range dirs {
		child, oerr := unix.Openat(fd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(fd)
		if oerr != nil {
			return -1, "", fmt.Errorf("dirfd: open %s: %w", name, oerr)
		}
		fd = child
	}
	return fd, leaf, nil
}

// CreateExclAt creates name in the directory referenced by dirFd with
// O_CREAT|O_EXCL|O_NOFOLLOW, so a pre-existing entry (including a symlink)
// at name is refused rather than truncated or followed. The returned fd is
// close-on-exec (see OpenAt's doc comment for why that's forced here).
func CreateExclAt(dirFd int, name string, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(dirFd, name,
		syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(mode))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// OpenAt opens name in the directory referenced by dirFd with the given
// flags and mode via openat(2). Callers that need the no-follow guarantee
// must include syscall.O_NOFOLLOW in flags themselves; this helper doesn't
// force that, since some callers (e.g. walking further into a
// subdirectory) legitimately want other flag combinations.
//
// O_CLOEXEC is always forced in, regardless of flags: unlike os.OpenFile,
// the raw syscall.Openat this wraps does not set it, and every fd this
// package hands back either sits behind a privilege boundary (a cached log
// fd, a token read/write fd) or is meant to be used and closed within this
// process — never leaked into a child this process execs, which on
// Substrate is the workload itself running with dropped privileges.
func OpenAt(dirFd int, name string, flags int, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(dirFd, name, flags|syscall.O_CLOEXEC, uint32(mode))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// EnsureDirNoFollow resolves path via OpenParentNoFollow, creates the leaf
// as a directory if it doesn't already exist (mkdirat, so nothing can be
// planted there as a side effect of the create itself), and returns an
// open O_DIRECTORY|O_NOFOLLOW fd for it — refusing a symlink at the leaf
// exactly like every other op in this package. The caller owns the
// returned fd and must close it.
//
// Because the fd is resolved once and returned to the caller, an operation
// against it (e.g. Fchown) stays correct even if something later replaces
// the directory's entry in its parent: a file descriptor refers to the
// inode it was opened against, not the path used to open it, so there is
// no window between "ensure the directory exists" and "operate on it" for
// a path-based swap to land in.
func EnsureDirNoFollow(path string, mode os.FileMode) (*os.File, error) {
	dirFd, leaf, err := OpenParentNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	if err := unix.Mkdirat(dirFd, leaf, uint32(mode)); err != nil && err != syscall.EEXIST {
		return nil, fmt.Errorf("dirfd: mkdir %s: %w", path, err)
	}

	return OpenAt(dirFd, leaf, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY, 0)
}

// EnsureDirNoFollowUnderRoot ensures that path exists as a directory,
// anchored under root: it reports underRoot=false (with a nil error) without
// touching anything when path does not resolve under root at all — a
// ".."-relative escape, a sibling that merely shares root's own string
// prefix, or anything relUnderRoot cannot express as a descendant of root —
// so a caller with a legitimate reason to create a directory outside root
// (e.g. an operator-configured absolute path) can fall back to its own
// handling for that case. path EQUAL to root itself is reported as
// underRoot=true: root is trivially "under" itself, the same way a file
// directly inside $HOME (dir == $HOME) is a legitimate under-home target,
// not an escape. relUnderRoot's own rel=="." refusal exists for a different
// caller (ReadUnderRootNoFollow, where reading root itself as a leaf file
// makes no sense) and says nothing about this case, so it is checked before
// ever calling relUnderRoot.
//
// When path DOES resolve under root, this walks from root (opened once via
// EnsureDirTrustedAncestorFollow, so a trusted symlink at root's own leaf or
// any of root's own ancestors is followed rather than refused — see that
// function's own doc comment) down to path one component at a time, exactly
// like OpenParentNoFollow's walk: each component is opened with
// O_DIRECTORY|O_NOFOLLOW relative to the previous component's own already-
// open fd, never by re-parsing a path string. A missing component is
// created with mkdirat (so nothing can be planted there as a side effect of
// the create itself) and then reopened no-follow; a component that already
// exists as a symlink or as any non-directory is refused before anything —
// including a chown — happens to it. Only a component this call itself
// creates is chowned (fchown on that component's own open fd, never a
// path-based os.Chown) to uid:gid, when uid > 0; a pre-existing component's
// ownership is left exactly as it was. mode is the permission bits used for
// any component this call creates.
//
// This is what closes the gap a path-based os.MkdirAll + os.Chown leaves
// open for a directory chain under a directory the WORKLOAD controls (e.g.
// $HOME): os.MkdirAll silently succeeds when a component is a symlink to an
// existing directory, and a subsequent os.Chown follows it — so a workload
// that plants, say, $HOME/.scion -> /etc ahead of a restart can have root
// chown /etc to the workload's own uid before this package's own leaf-write
// guards ever run. Once every component is confirmed real and either
// pre-existing (untouched) or freshly created (owned by uid:gid), there is
// nothing left along the chain for a symlink swap to redirect.
//
// On success, dirFd is an open, symlink-safe O_DIRECTORY fd for path
// itself: the caller writes its own leaf through this SAME fd (e.g. via
// WriteAtNoFollowWithChown), never by re-resolving path as a string again —
// which matters here specifically because root may have been reached
// through a trusted symlink EnsureDirTrustedAncestorFollow followed, and a
// fresh path-based walk (OpenParentNoFollow's strict no-follow) would
// refuse that same symlink the second time around. The caller owns dirFd
// and must close it; dirFd is -1 whenever err is non-nil or underRoot is
// false.
func EnsureDirNoFollowUnderRoot(root, path string, mode os.FileMode, uid, gid int) (dirFd int, underRoot bool, err error) {
	if absRoot, aerr := filepath.Abs(root); aerr == nil {
		if absPath, aerr := filepath.Abs(path); aerr == nil && filepath.Clean(absRoot) == filepath.Clean(absPath) {
			// path IS root itself (e.g. a file secret directly inside
			// $HOME): nothing to create or chown along an empty component
			// list, but root itself must still resolve to a real directory
			// before this reports it as usable. root's own chain is walked
			// via EnsureDirTrustedAncestorFollow, not a strict no-follow
			// open: root (e.g. $HOME) may itself be reached through a
			// root-owned system symlink (an image with "/home" ->
			// "/var/home"), which is not workload-plantable and must not
			// break this the way a strict no-follow walk would.
			rootFd, oerr := EnsureDirTrustedAncestorFollow(root)
			if oerr != nil {
				return -1, true, fmt.Errorf("dirfd: open root %s: %w", root, oerr)
			}
			return rootFd, true, nil
		}
	}

	rel, rerr := relUnderRoot(root, path)
	if rerr != nil {
		return -1, false, nil
	}

	// root's own chain, like the root-equals-path case above, is walked
	// via EnsureDirTrustedAncestorFollow so a root-owned system symlink
	// anywhere in root's own path — an ancestor (an image with "/home" ->
	// "/var/home") or root's own final component — does not break this the
	// way a strict no-follow walk would. Everything BELOW root, from here
	// down to path, still goes through the strict no-follow walk below:
	// only root's own resolution tolerates a trusted symlink.
	curFd, oerr := EnsureDirTrustedAncestorFollow(root)
	if oerr != nil {
		return -1, true, fmt.Errorf("dirfd: open root %s: %w", root, oerr)
	}
	closeOnReturn := true
	defer func() {
		if closeOnReturn {
			_ = syscall.Close(curFd)
		}
	}()

	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		child, operr := unix.Openat(curFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		created := false
		switch operr {
		case nil:
			// A real, pre-existing directory: leave its ownership alone.
		case syscall.ENOENT:
			if merr := unix.Mkdirat(curFd, name, uint32(mode)); merr != nil && merr != syscall.EEXIST {
				return -1, true, fmt.Errorf("dirfd: mkdir %s: %w", name, merr)
			}
			child, operr = unix.Openat(curFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
			if operr != nil {
				return -1, true, fmt.Errorf("dirfd: open %s after create: %w", name, operr)
			}
			created = true
		case syscall.ELOOP:
			return -1, true, fmt.Errorf("dirfd: refusing %s: existing entry is a symlink", name)
		case syscall.ENOTDIR:
			return -1, true, fmt.Errorf("dirfd: refusing %s: existing entry is not a directory", name)
		default:
			return -1, true, fmt.Errorf("dirfd: open %s: %w", name, operr)
		}

		_ = syscall.Close(curFd)
		curFd = child

		if created && uid > 0 {
			// fchown on the fd this call just created and opened, never a
			// path-based os.Chown.
			if cerr := unix.Fchown(curFd, uid, gid); cerr != nil {
				return -1, true, fmt.Errorf("dirfd: chown %s: %w", name, cerr)
			}
		}
	}
	closeOnReturn = false
	return curFd, true, nil
}

// RenameAt renames oldName to newName, both resolved relative to dirFd via
// renameat(2). Both names live in the same directory in every caller today.
func RenameAt(dirFd int, oldName, newName string) error {
	return unix.Renameat(dirFd, oldName, dirFd, newName)
}

// UnlinkAt removes name relative to dirFd via unlinkat(2).
func UnlinkAt(dirFd int, name string) error {
	return unix.Unlinkat(dirFd, name, 0)
}

// RefuseSymlinkOrNonRegularAt reports an error if name already exists
// relative to dirFd as a symlink or as any non-regular file. A missing
// name is not an error (returns nil), matching os.IsNotExist semantics.
//
// This opens the entry (never following a symlink, and non-blocking so a
// planted FIFO can't hang the check) purely to fstat it, then closes it
// immediately; it is a refusal check, not a read. The fd is close-on-exec
// for the brief window it's open, in case another goroutine execs a child
// concurrently.
func RefuseSymlinkOrNonRegularAt(dirFd int, name string) error {
	fd, err := unix.Openat(dirFd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("refusing %s: existing entry is a symlink", name)
		}
		return fmt.Errorf("refusing %s: %w", name, err)
	}
	defer func() { _ = syscall.Close(fd) }()

	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("refusing %s: %w", name, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fmt.Errorf("refusing %s: existing entry is not a regular file", name)
	}
	return nil
}
