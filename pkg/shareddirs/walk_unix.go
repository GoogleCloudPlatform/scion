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

//go:build unix

package shareddirs

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// EnsureLeaf race-freely creates (or verifies) the directory chain for
// rel = "<subpath_root>/<projectID>/shared-dirs/<name>" under hostBase,
// using openat(2)/mkdirat(2) with O_NOFOLLOW at every component -- leaf or
// intermediate, pointing inside the export or outside it -- so opening or
// creating a component and refusing a symlink there happen atomically in
// the same syscall, with no separate stat-then-open step for a concurrent
// symlink swap to land between. It applies the leaf modes/ACL hardening
// (chmod 0o2775 + default ACL) when it creates the leaf itself.
//
// This is the SINGLE place both the broker (resolveSharedDirs) and the hub
// file browser create/finalize a shared dir leaf, so a shared dir either of
// them causes to exist is indistinguishable and always gets the same
// treatment.
//
// Phase 2 item 2 (leaf modes/ACL hardening, ptone/scion#1794): every
// INTERMEDIATE component this call creates is created at mode 0o755 (no
// group/other write bit from the moment it exists, closing the brief window
// a 0o775-then-fchmod sequence would otherwise leave) and then explicitly
// fchmod'd to intermediateMode (0o2755: setgid, still no group-write) on the
// fd. The leaf itself is chmod'd to 0o2775 and given the default ACL only
// when this call creates it (design §3.5(5)).
//
// If the leaf's chmod or a non-ENOTSUP ACL error occurs for a leaf THIS call
// created, the leaf is removed (Unlinkat AT_REMOVEDIR) before returning the
// error, so a retry starts clean instead of getting permanently stuck at
// alreadyExisted=true with no group-write and no ACL.
//
// Returns the leaf's open fd (caller must close via CloseFd) and whether it
// already existed before this call.
func EnsureLeaf(hostBase, rel string) (leafFd int, alreadyExisted bool, err error) {
	parentFd, leafName, fd, existed, walkErr := createViaComponentWalk(hostBase, rel, 0o2755)
	if walkErr != nil {
		return -1, false, walkErr
	}
	if existed {
		_ = unix.Close(parentFd)
		return fd, true, nil
	}

	if chmodErr := chmodLeaf(fd, 0o2775); chmodErr != nil {
		_ = CloseFd(fd)
		_ = unix.Unlinkat(parentFd, leafName, unix.AT_REMOVEDIR)
		_ = unix.Close(parentFd)
		return -1, false, fmt.Errorf("chmod shared dir: %w", chmodErr)
	}
	if aclErr := setLeafDefaultACL(fd); aclErr != nil {
		_ = CloseFd(fd)
		_ = unix.Unlinkat(parentFd, leafName, unix.AT_REMOVEDIR)
		_ = unix.Close(parentFd)
		return -1, false, fmt.Errorf("set default ACL on shared dir: %w", aclErr)
	}
	_ = unix.Close(parentFd)
	return fd, false, nil
}

// chmodLeaf and setLeafDefaultACL are indirected through package vars purely
// so a test can inject a failure (EINVAL, EPERM) at either finalization step
// and verify EnsureLeaf's rollback -- removing the leaf it just created, but
// never an intermediate -- without needing a real
// filesystem condition that produces one. Production always uses ChmodFd
// and SetLeafDefaultACL themselves; nothing here overrides them outside
// tests.
var (
	chmodLeaf         = ChmodFd
	setLeafDefaultACL = SetLeafDefaultACL
)

// SetLeafFinalizationHooksForTest overrides EnsureLeaf's two finalization
// steps (chmod and default-ACL) for the duration of a test, returning a
// restore function the caller must invoke (typically via t.Cleanup) to put
// the real implementations back. Exported so a test in a different package
// (e.g. pkg/hub, exercising EnsureLeaf only indirectly through a handler)
// can inject a finalization failure without a copy of pkg/shareddirs'
// unexported seam vars. A nil argument leaves that hook unchanged.
//
// Production code never calls this.
func SetLeafFinalizationHooksForTest(chmod func(fd int, mode uint32) error, acl func(fd int) error) (restore func()) {
	oldChmod, oldACL := chmodLeaf, setLeafDefaultACL
	if chmod != nil {
		chmodLeaf = chmod
	}
	if acl != nil {
		setLeafDefaultACL = acl
	}
	return func() {
		chmodLeaf, setLeafDefaultACL = oldChmod, oldACL
	}
}

// createViaComponentWalk walks every component of rel under hostBase,
// creating missing intermediates at intermediateMode (fchmod'd on the fd,
// never trusted to mkdirat's mode argument), and returns the leaf's PARENT
// fd (still open — the caller owns closing it) alongside the leaf's own fd
// and name, so a caller (EnsureLeaf) can roll back a partially-finalized
// leaf via Unlinkat(parentFd, leafName, ...) without re-walking.
func createViaComponentWalk(hostBase, rel string, intermediateMode uint32) (parentFd int, leafName string, leafFd int, alreadyExisted bool, err error) {
	baseFd, err := unix.Open(hostBase, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", -1, false, fmt.Errorf("open host base %q: %w", hostBase, err)
	}

	parts := strings.Split(filepath.ToSlash(rel), "/")
	currentFd := baseFd
	for _, comp := range parts[:len(parts)-1] {
		// Intermediates are created with NO group/other write bit from the
		// moment they exist (0o755, not 0o775), closing the window between
		// mkdirat and the explicit fchmod below during
		// which a concurrent, differently-permissioned creator could
		// otherwise plant a real (group-writable) directory here.
		nextFd, existed, walkErr := openOrCreateDirNoFollow(currentFd, comp, 0o755)
		if walkErr != nil {
			_ = unix.Close(currentFd)
			return -1, "", -1, false, walkErr
		}
		if !existed {
			if chmodErr := unix.Fchmod(nextFd, intermediateMode); chmodErr != nil {
				_ = unix.Close(nextFd)
				_ = unix.Close(currentFd)
				return -1, "", -1, false, fmt.Errorf("chmod path component %q: %w", comp, chmodErr)
			}
		} else {
			warnIfIntermediateUnsafe(nextFd, comp)
		}
		_ = unix.Close(currentFd)
		currentFd = nextFd
	}

	leafComp := parts[len(parts)-1]
	fd, existed, walkErr := openOrCreateDirNoFollow(currentFd, leafComp, 0o775)
	if walkErr != nil {
		_ = unix.Close(currentFd)
		return -1, "", -1, false, walkErr
	}
	return currentFd, leafComp, fd, existed, nil
}

var intermediateUnsafeWarnOnce sync.Once

// geteuid is indirected through a package var so a test can simulate "this
// intermediate is not owned by this process" without
// needing a second real uid available in the test environment -- catching a
// regression that flips warnIfIntermediateUnsafe's mode-check/owner-check
// boolean operator (AND vs OR), which a test asserting only the mode half
// of the condition can't distinguish from the correct behavior. Production
// never overrides this outside tests.
var geteuid = unix.Geteuid

// warnIfIntermediateUnsafe checks a pre-existing intermediate component and
// logs a single process-wide warning — never refuses — if it is
// group/other-writable or not owned by this process's effective uid. This
// covers directories created before the leaf modes/ACL hardening shipped
// (mode 0o2775, no group-write hardening) or anything else that predates
// this walk; fixing them is the documented, symlink-safe manual recipe (see
// docs/deploy/hybrid-tier.md), not something this call does automatically.
func warnIfIntermediateUnsafe(fd int, comp string) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return
	}
	if st.Mode&0o022 == 0 && int(st.Uid) == geteuid() {
		return
	}
	intermediateUnsafeWarnOnce.Do(func() {
		slog.Warn("server.shared_dir_storage: an existing upper directory is group/other-writable or not owned by this broker; "+
			"the leaf modes/ACL hardening does not apply retroactively -- see the manual fix-up recipe in docs/deploy/hybrid-tier.md",
			"component", comp, "mode", fmt.Sprintf("%#o", st.Mode&0o7777), "uid", st.Uid)
	})
}

// openOrCreateDirNoFollow opens the directory component `comp` under the
// directory referenced by parentFd, refusing to follow it if it is a
// symlink (O_NOFOLLOW) or use it if it is not a directory (O_DIRECTORY). If
// the component does not exist, it creates it via mkdirat (at mode) and
// reopens it the same way. A concurrent creator winning the mkdirat race
// (EEXIST) is not an error — the reopen with O_NOFOLLOW still refuses a
// symlink even if the entry a racing process left behind is one.
func openOrCreateDirNoFollow(parentFd int, comp string, mode uint32) (fd int, existed bool, err error) {
	const openFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

	fd, err = unix.Openat(parentFd, comp, openFlags, 0)
	if err == nil {
		return fd, true, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return -1, false, classifyComponentOpenError(parentFd, comp, err)
	}

	if mkErr := unix.Mkdirat(parentFd, comp, mode); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
		return -1, false, fmt.Errorf("mkdir path component %q: %w", comp, mkErr)
	}
	fd, err = unix.Openat(parentFd, comp, openFlags, 0)
	if err != nil {
		return -1, false, classifyComponentOpenError(parentFd, comp, err)
	}
	return fd, false, nil
}

// classifyComponentOpenError turns the errno openat(O_NOFOLLOW|O_DIRECTORY)
// returns for "this component isn't usable" into a clear wording.
//
// On Linux (verified empirically; POSIX leaves the exact errno
// implementation-defined here), combining O_DIRECTORY with O_NOFOLLOW on a
// symlink reports ENOTDIR, not ELOOP — the kernel checks "is this a
// directory" before "is this a symlink we were told not to follow", so a
// symlink and a plain regular file both blocking the path produce the same
// errno. Handle both: on ELOOP (some platforms) it's unambiguously a
// symlink; on ENOTDIR, Lstat the component relative to parentFd (via
// Fstatat with AT_SYMLINK_NOFOLLOW, so this diagnostic step is itself
// race-safe) to tell a symlink from a genuine non-directory and produce an
// accurate message either way. This is purely for error-message quality —
// the operation has already been correctly refused by the O_NOFOLLOW open
// regardless of which branch below fires.
func classifyComponentOpenError(parentFd int, comp string, err error) error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return fmt.Errorf("path component %q is a symlink; refusing to use a symlinked path", comp)
	case errors.Is(err, unix.ENOTDIR):
		var st unix.Stat_t
		if statErr := unix.Fstatat(parentFd, comp, &st, unix.AT_SYMLINK_NOFOLLOW); statErr == nil &&
			st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("path component %q is a symlink; refusing to use a symlinked path", comp)
		}
		return fmt.Errorf("path component %q exists but is not a directory", comp)
	default:
		return fmt.Errorf("open path component %q: %w", comp, err)
	}
}

// ChmodFd sets a directory's mode via fchmod on the already-open file
// descriptor, so the mode change always lands on the exact directory this
// package's walk verified, regardless of anything that happens to the path
// afterward. mode is a traditional Unix mode_t value (e.g. 0o2775 for
// rwxrwsr-x), NOT an os.FileMode: Go's os.FileMode encodes its
// setgid/setuid/sticky bits at different bit positions than the kernel's
// mode_t, so passing an os.FileMode value to a raw syscall wrapper would
// silently target the wrong bits.
func ChmodFd(fd int, mode uint32) error {
	return unix.Fchmod(fd, mode)
}

// CloseFd closes a file descriptor returned by EnsureLeaf. Errors are
// deliberately ignored by most callers (a close failure after a successful
// chmod doesn't invalidate the mkdir/chmod that already happened), but the
// return value is available for callers that want to check it.
func CloseFd(fd int) error {
	return unix.Close(fd)
}
