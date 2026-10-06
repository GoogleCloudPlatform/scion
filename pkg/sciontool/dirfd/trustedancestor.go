/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// trustedAncestorOwnerUID is the uid a symlink AND its own containing
// directory must both be owned by for EnsureDirTrustedAncestorFollow to
// follow that symlink instead of refusing it. Production always leaves this
// at its zero value (root): only a root-owned link sitting in a root-owned,
// group/other-non-writable directory is a system-configured alias (e.g.
// "/var/run" -> "/run", "/home" -> "/var/home") that no workload can plant
// or redirect — nobody but root could have created or replaced it. A test
// overrides this to the test process's own real uid (mirroring the
// startReaper/runSetupHostUser seam pattern elsewhere in this codebase), so
// the trust check can be exercised end to end without CAP_CHOWN or running
// the test suite as root.
//
// This is deliberately simpler than rootowned.go's chainIsTrusted, which
// also accepts a caller's own effective uid as an alternative to uid 0 (for
// a rootless/keep-id deployment with no separate root/workload boundary).
// EnsureDirTrustedAncestorFollow runs on the staged-secrets write path,
// which only ever matters when sciontool init is root-context to begin
// with (os.Getuid() == 0 is Write's own gate before uid/gid are even
// resolved) — there is no rootless caller of this specific walk to
// accommodate, so the single fixed trusted uid is the simpler, narrower
// rule.
var trustedAncestorOwnerUID = 0

// SetTrustedAncestorOwnerUIDForTest overrides trustedAncestorOwnerUID for
// the duration of a test and returns a restore function, the same
// "override + return a restore closure" shape hub.SetTokenHome uses for its
// own resolver seam. Exported because the most faithful regression test for
// this walk's own motivating bug (a staged file secret whose target has a
// symlinked system ancestor) drives it indirectly, through
// pkg/stagedsecrets.Write — a different package, which cannot reach an
// unexported var here directly. A test creates its fixture symlinks owned
// by its own real uid (os.Getuid()) and calls this with that same uid, so
// the trust check has something real to find true without CAP_CHOWN or
// running as root.
func SetTrustedAncestorOwnerUIDForTest(uid int) (restore func()) {
	orig := trustedAncestorOwnerUID
	trustedAncestorOwnerUID = uid
	return func() { trustedAncestorOwnerUID = orig }
}

// maxTrustedAncestorSymlinkHops bounds how many trusted symlinks
// EnsureDirTrustedAncestorFollow will follow while resolving a single path,
// the same kind of guard maxSymlinkResolutionsUnderRoot gives the
// under-root walk and maxSymlinkHops gives VerifyRootOwnedExecutable's
// chain walk: a symlink cycle — even one made of individually-trusted
// links (a -> b, b -> a, both root-owned, both non-group/other-writable) —
// would otherwise spin this forever.
const maxTrustedAncestorSymlinkHops = 40

// ErrTooManyTrustedAncestorSymlinks is returned when a path's ancestor
// chain requires following more than maxTrustedAncestorSymlinkHops trusted
// symlinks to resolve, the ELOOP-equivalent for this walk.
var ErrTooManyTrustedAncestorSymlinks = errors.New("dirfd: too many symlinks resolving a trusted ancestor chain")

// EnsureDirTrustedAncestorFollow resolves path's full directory chain from
// "/" down, component by component, exactly like EnsureDirNoFollowUnderRoot
// and OpenParentNoFollow do (openat(2) with O_DIRECTORY|O_NOFOLLOW against
// each previous component's own already-open fd, never by re-parsing a path
// string) — except that a symlink at any component is not unconditionally
// refused. It is followed if, and only if, BOTH hold:
//
//   - the symlink's own directory entry is owned by trustedAncestorOwnerUID
//     (checked via Fstatat with AT_SYMLINK_NOFOLLOW — a Lstat-equivalent
//     that never dereferences the link itself); and
//   - the directory CONTAINING that symlink is also owned by
//     trustedAncestorOwnerUID and carries neither the group- nor the
//     other-write bit.
//
// Any symlink that fails either check is refused with an error naming the
// component, and nothing past it — on either side of the refused link — is
// ever opened, created, or chowned: the function returns immediately, so a
// caller can rely on nothing further having been written to the
// filesystem the instant this returns a non-nil error.
//
// A missing component (ENOENT) is created with mkdirat — 0755, never
// chowned to any uid, since every directory this call creates sits on a
// root-controlled system path, not workload territory — then reopened
// O_NOFOLLOW to continue the walk. This is what lets this function replace
// a path-based os.MkdirAll for an operator-configured destination outside
// any workload-controlled directory: like os.MkdirAll, missing ancestors
// are created; unlike it, a symlink swapped into the chain is never
// silently followed except where the trust check above explicitly allows
// it.
//
// This is NOT a general "resolve any symlink" helper. filepath.EvalSymlinks
// would follow a WORKLOAD-plantable ancestor too — a secret Target under a
// workload-writable directory with a symlinked component redirecting into,
// say, /etc — letting root create or truncate-in-place a file in a
// directory the workload chose instead of the operator. That is exactly
// the class of hole this package's no-follow walks exist to close. Trust
// here is decided per-component, by ownership and writability of both the
// link and its containing directory, never by "the kernel resolved it"
// alone.
//
// A symlink target is spliced into the walk and continues from "/" if
// absolute, or from the symlink's own containing directory if relative —
// matching how the kernel itself would continue resolving the rest of the
// original path after dereferencing one component. The returned fd is an
// open, O_DIRECTORY|O_NOFOLLOW descriptor for the final resolved directory;
// the caller is expected to write its own leaf through this SAME fd (see
// WriteAtNoFollowWithChown), never by re-resolving the original or
// resolved path as a string again, so nothing between this call returning
// and the leaf write can redirect the destination a second time. The
// caller owns the returned fd and must close it. A path of "/" itself
// resolves to an fd for the root directory.
func EnsureDirTrustedAncestorFollow(path string) (dirFd int, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return -1, fmt.Errorf("dirfd: resolve %s: %w", path, err)
	}
	abs = filepath.Clean(abs)

	rootFd, operr := syscall.Open(string(filepath.Separator), syscall.O_DIRECTORY|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if operr != nil {
		return -1, fmt.Errorf("dirfd: open /: %w", operr)
	}
	if abs == string(filepath.Separator) {
		// path is "/" itself (e.g. the parent of a file target such as
		// "/token"): there is no component to walk and "/" cannot be a
		// symlink, so the root fd just opened is the resolved directory.
		return rootFd, nil
	}

	// stack holds one open fd per path component already resolved, from
	// "/" (stack[0]) to the current directory (the last element). Kept as
	// a stack, not a single curFd, so a ".." in a relative symlink target
	// (e.g. "../run") can step back to an already-open, already-verified
	// ancestor fd instead of re-opening anything by path.
	stack := []int{rootFd}
	closeAbove := func(n int) {
		for len(stack) > n {
			_ = syscall.Close(stack[len(stack)-1])
			stack = stack[:len(stack)-1]
		}
	}
	defer func() {
		if err != nil {
			// closeAbove(0) pops and closes every fd on the stack,
			// including stack[0] (the root fd), leaving nothing open on
			// any error return path.
			closeAbove(0)
		}
	}()

	trimmed := strings.TrimPrefix(abs, string(filepath.Separator))
	queue := strings.Split(trimmed, string(filepath.Separator))
	hopsLeft := maxTrustedAncestorSymlinkHops

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		curFd := stack[len(stack)-1]

		switch name {
		case "", ".":
			continue
		case "..":
			if len(stack) > 1 {
				_ = syscall.Close(stack[len(stack)-1])
				stack = stack[:len(stack)-1]
			}
			continue
		}

		child, operr := unix.Openat(curFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		switch {
		case operr == nil:
			stack = append(stack, child)
			continue
		case operr == syscall.ENOENT:
			if merr := unix.Mkdirat(curFd, name, 0755); merr != nil && merr != syscall.EEXIST {
				return -1, fmt.Errorf("dirfd: mkdir %s: %w", name, merr)
			}
			child, operr = unix.Openat(curFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
			if operr != nil {
				return -1, fmt.Errorf("dirfd: open %s after create: %w", name, operr)
			}
			stack = append(stack, child)
			continue
		case operr != syscall.ELOOP && !isSymlinkAt(curFd, name):
			return -1, fmt.Errorf("dirfd: open %s: %w", name, operr)
		}

		// name is a confirmed symlink in the directory curFd is open on.
		trusted, terr := trustedAncestorSymlinkAt(curFd, name)
		if terr != nil {
			return -1, fmt.Errorf("dirfd: %s: %w", name, terr)
		}
		if !trusted {
			return -1, fmt.Errorf("dirfd: refusing untrusted ancestor symlink %s (not owned by uid %d, or its containing directory is not owned by uid %d or is group/other-writable)", name, trustedAncestorOwnerUID, trustedAncestorOwnerUID)
		}

		hopsLeft--
		if hopsLeft < 0 {
			return -1, fmt.Errorf("dirfd: %s: %w", name, ErrTooManyTrustedAncestorSymlinks)
		}

		target, rerr := readlinkAt(curFd, name)
		if rerr != nil {
			return -1, fmt.Errorf("dirfd: readlink %s: %w", name, rerr)
		}

		var spliced []string
		for _, part := range strings.Split(target, string(filepath.Separator)) {
			if part == "" || part == "." {
				continue
			}
			spliced = append(spliced, part)
		}
		if len(spliced) == 0 {
			return -1, fmt.Errorf("dirfd: symlink %s resolved to an empty target", name)
		}

		if filepath.IsAbs(target) {
			// Restart from "/": close every fd above the root, matching
			// the kernel's own behavior of re-anchoring at the filesystem
			// root for an absolute symlink target, then re-open a fresh
			// handle to "/" (the stack's own root fd is kept open and
			// reused rather than re-opened, since it was never a symlink
			// target itself — it's this walk's own starting point).
			closeAbove(1)
			queue = append(spliced, queue...)
			continue
		}

		// Relative target: continues from the symlink's own containing
		// directory (curFd, still on top of the stack — a relative target
		// never pops or pushes by itself; any ".." it contains is handled
		// by the ".." case above once it reaches the front of the queue).
		queue = append(spliced, queue...)
	}

	final := stack[len(stack)-1]
	for _, fd := range stack[:len(stack)-1] {
		_ = syscall.Close(fd)
	}
	return final, nil
}

// trustedAncestorSymlinkAt reports whether name, a confirmed symlink
// relative to dirFd, is safe for EnsureDirTrustedAncestorFollow to follow:
// the link's own directory entry and dirFd (the directory containing it)
// are both owned by trustedAncestorOwnerUID, and dirFd carries neither the
// group- nor the other-write bit. The symlink's own mode bits are not
// checked — a symlink's permission bits are fixed and meaningless on Linux,
// the same reasoning VerifyRootOwnedExecutable's symlink-hop check uses —
// only its ownership, and separately its containing directory's ownership
// and writability, say anything about who could have created or replaced
// it.
func trustedAncestorSymlinkAt(dirFd int, name string) (bool, error) {
	var linkSt unix.Stat_t
	if err := fstatatTrustedAncestor(dirFd, name, &linkSt, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, fmt.Errorf("stat %s: %w", name, err)
	}

	var dirSt unix.Stat_t
	if err := unix.Fstat(dirFd, &dirSt); err != nil {
		return false, fmt.Errorf("stat containing directory of %s: %w", name, err)
	}

	return symlinkTrustedByStat(linkSt.Uid, dirSt.Uid, uint32(dirSt.Mode), trustedAncestorOwnerUID), nil
}

// fstatatTrustedAncestor is trustedAncestorSymlinkAt's own call site for
// stat'ing the symlink's directory entry, as a package var instead of a
// direct unix.Fstatat call. Production always leaves this at its default;
// a test overrides it to report a fabricated owner for the link while
// leaving the containing directory's own, independently-fetched stat
// genuinely real, exercising the "the link's own owner must also be
// trusted" half of the decision — which an unprivileged test process
// cannot otherwise produce, since it can only create a symlink owned by
// its own uid, the same uid its containing directory is already owned by.
var fstatatTrustedAncestor = unix.Fstatat

// symlinkTrustedByStat is trustedAncestorSymlinkAt's decision, as a pure
// function of already-fetched stat fields: true only when linkUID and
// dirUID (the symlink's own owner, and the owner of the directory
// containing it) are BOTH equal to trusted, AND dirMode carries neither
// the group- nor the other-write bit. Every one of these three conditions
// is independently load-bearing — dropping any one of them, or checking
// only one of the two write bits, would accept a symlink a workload could
// have planted or redirected — which is exactly what
// TestSymlinkTrustedByStat's table pins row by row, each row changing
// only the one field its own guard depends on.
//
// Kept as a pure function of values, not one that stats anything itself,
// so it can be exercised by a table test with fabricated ownership/mode
// values, the same way rootowned.go's chainIsTrusted is tested, without
// requiring the test process itself to be root or to own a real root-owned
// directory.
func symlinkTrustedByStat(linkUID, dirUID uint32, dirMode uint32, trusted int) bool {
	return int(linkUID) == trusted && int(dirUID) == trusted && dirMode&(unix.S_IWGRP|unix.S_IWOTH) == 0
}
