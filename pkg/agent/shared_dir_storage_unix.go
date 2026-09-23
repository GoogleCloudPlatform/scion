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

package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// createSharedDirViaComponentWalk race-freely creates (or verifies) the
// directory chain for rel = "<subpath_root>/<projectID>/shared-dirs/<name>"
// under hostBase, refusing to follow a symlink at ANY component — leaf or
// intermediate, pointing inside the export or outside it — before ever
// creating or touching anything past it.
//
// Round 5 review finding C1=T1=S-L2 (also numbered C2=T2=S-L1 across the
// three r5 reports): the earlier os.Root-based approach only refused a
// traversal that would ESCAPE the root; it happily followed a relative
// symlink that stayed inside it (e.g. one project's shared-dirs pointing at
// another's), so a real mkdir+chmod could land inside a victim project's
// tree, or at the export root, before resolveSharedDirs' final equality
// check ever ran. openat(2)/mkdirat(2) with O_NOFOLLOW refuse to open or
// traverse through a symlink at all, atomically as part of the single
// syscall — there is no separate stat-then-open step for a concurrent
// symlink swap to land between, which is what made the os.Root version
// racy (see the R4/R5 security probes: 6,603/20,000 and 7,032/20,000
// escapes respectively; this walk is what finally gets both to 0).
//
// This supersedes os.Root for this path entirely — os.Root is no longer
// used anywhere in shared_dir_storage.go.
//
// Round 5 review nit T4: a mid-walk swap (an attacker replacing a plain
// directory with a symlink between our openat of the parent and the
// openat/mkdirat of the child) is refused deterministically by the same
// O_NOFOLLOW open on the child — there is no separate window to land in,
// since the kernel resolves and checks the final component atomically as
// part of the openat/mkdirat call itself. There is deliberately no unit
// test that forces this exact interleaving: doing so would require a
// production-code test hook solely to pause the walk between components,
// which adds real production-code complexity for a property that syscall
// atomicity already guarantees and that the r5 security probe already
// verifies empirically (reviews/r5-security-probe_test.go.txt, run
// 20,000 times against the pre-rewrite os.Root code and again against this
// walk: 0/20,000 escapes and 0/20,000 stray chmods here, versus thousands
// under os.Root — see the self-check numbers reported alongside this PR).
//
// Returns the leaf's open file descriptor (the caller must close it via
// closeSharedDirFd) and whether the leaf already existed before this call,
// so the caller can decide whether to chmod it — only a leaf THIS call
// created is ever chmod'd (design §3.5(5); round 3 review item 13).
// Intermediate components get the same mkdir mode as the leaf (0o775); the
// caller chmods only the leaf.
func createSharedDirViaComponentWalk(hostBase, rel string) (leafFd int, alreadyExisted bool, err error) {
	baseFd, err := unix.Open(hostBase, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, false, fmt.Errorf("open host base %q: %w", hostBase, err)
	}

	currentFd := baseFd
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, comp := range parts {
		nextFd, existed, walkErr := openOrCreateDirNoFollow(currentFd, comp)
		_ = unix.Close(currentFd)
		if walkErr != nil {
			return -1, false, walkErr
		}
		currentFd = nextFd
		alreadyExisted = existed
	}
	return currentFd, alreadyExisted, nil
}

// openOrCreateDirNoFollow opens the directory component `comp` under the
// directory referenced by parentFd, refusing to follow it if it is a
// symlink (O_NOFOLLOW) or use it if it is not a directory (O_DIRECTORY). If
// the component does not exist, it creates it via mkdirat and reopens it
// the same way. A concurrent creator winning the mkdirat race (EEXIST) is
// not an error — the reopen with O_NOFOLLOW still refuses a symlink even if
// the entry a racing process left behind is one.
func openOrCreateDirNoFollow(parentFd int, comp string) (fd int, existed bool, err error) {
	const openFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

	fd, err = unix.Openat(parentFd, comp, openFlags, 0)
	if err == nil {
		return fd, true, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return -1, false, classifyComponentOpenError(parentFd, comp, err)
	}

	if mkErr := unix.Mkdirat(parentFd, comp, 0o775); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
		return -1, false, fmt.Errorf("mkdir path component %q: %w", comp, mkErr)
	}
	fd, err = unix.Openat(parentFd, comp, openFlags, 0)
	if err != nil {
		return -1, false, classifyComponentOpenError(parentFd, comp, err)
	}
	return fd, false, nil
}

// classifyComponentOpenError turns the errno openat(O_NOFOLLOW|O_DIRECTORY)
// returns for "this component isn't usable" into the clear wording
// resolveSharedDirs used before this rewrite (round 4 review nit C4/C5).
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

// chmodSharedDirFd sets the leaf's mode via fchmod on the already-open file
// descriptor — never a path-based chmod, which could be raced onto a
// different inode after the fd was opened (round 5 disposition item 2).
// mode is a traditional Unix mode_t value (e.g. 0o2775 for rwxrwsr-x), NOT
// an os.FileMode: Go's os.FileMode encodes its setgid/setuid/sticky bits at
// different bit positions than the kernel's mode_t, so passing an
// os.FileMode value to a raw syscall wrapper would silently target the
// wrong bits (this exact bug bit an earlier version of this file — see
// git history / round 1-4 review notes on os.ModeSetgid vs 0o2000).
func chmodSharedDirFd(fd int, mode uint32) error {
	return unix.Fchmod(fd, mode)
}

// closeSharedDirFd closes a file descriptor returned by
// createSharedDirViaComponentWalk. Errors are deliberately ignored by most
// callers (a close failure after a successful chmod doesn't invalidate the
// mkdir/chmod that already happened), but the return value is available for
// callers that want to check it.
func closeSharedDirFd(fd int) error {
	return unix.Close(fd)
}
