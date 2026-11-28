//go:build unix

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

package hub

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// openRuntimeTokenParent opens the parent directory of dir with strict
// no-follow and root/self-owned non-group/world-writable chain checks.
// When the immediate parent (e.g. /run/sciontool for /run/sciontool/auth) does
// not yet exist, it creates that single missing parent with mode 0755 (so
// sibling files like /run/sciontool/credential-state.json remain reachable by
// sciontool doctor) after verifying its own ancestor chain, and never
// chmods/chowns an already-existing parent directory.
func openRuntimeTokenParent(dir, base string, overridden bool, euid int) (int, string, error) {
	openParent := func(target string) (int, string, error) {
		switch {
		case base == "/":
			return dirfd.OpenParentNoFollowRootOwned(target)
		case base != "":
			return dirfd.OpenParentNoFollowOwnedBy(base, target, uint32(euid)) //nolint:gosec // euid is non-negative
		case overridden:
			return dirfd.OpenParentNoFollowOwnedBy(filepath.Dir(target), target, uint32(euid)) //nolint:gosec // euid is non-negative
		default:
			return dirfd.OpenParentNoFollowRootOwned(target)
		}
	}

	parentFd, leaf, err := openParent(dir)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return parentFd, leaf, err
	}

	// One-level parent creation (e.g. create /run/sciontool at 0755 when
	// creating /run/sciontool/auth on a fresh container). Never touch an
	// already-existing directory.
	parentDir := filepath.Dir(dir)
	grandFd, parentName, gerr := openParent(parentDir)
	if gerr != nil {
		return -1, "", gerr
	}
	mkErr := unix.Mkdirat(grandFd, parentName, 0o755)
	if mkErr == nil {
		if pd, perr := dirfd.OpenAt(grandFd, parentName, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY, 0); perr == nil {
			if euid == 0 {
				_ = pd.Chown(0, 0)
			}
			_ = pd.Chmod(0o755)
			_ = pd.Close()
		}
	}
	_ = dirfd.Close(grandFd)
	if mkErr != nil && !errors.Is(mkErr, syscall.EEXIST) {
		return -1, "", fmt.Errorf("mkdir %s: %w", parentDir, mkErr)
	}
	return openParent(dir)
}

// ensureRuntimeTokenDir creates (if absent) and verifies the leaf directory of
// runtimePath (e.g. /run/sciontool/auth) through dirfd no-follow, verifying
// that every ancestor along the chain is root-owned (or owned by this process)
// and free of group/other write bits.
//
// If the leaf directory is freshly created by Mkdirat, it is set to 0700
// root:root (when euid==0). If it already exists, it is NEVER chmodded or
// chowned: it must already be owned by this process (root:root when euid==0)
// with exact mode 0700, otherwise an error is returned so the caller falls
// back to in-memory storage.
func ensureRuntimeTokenDir(runtimePath string) error {
	dir := filepath.Dir(runtimePath)
	euid := os.Geteuid()

	tokenStoreMu.RLock()
	base := runtimeTokenBase
	overridden := runtimeStoreOverridden
	tokenStoreMu.RUnlock()

	parentFd, leaf, err := openRuntimeTokenParent(dir, base, overridden, euid)
	if err != nil {
		return fmt.Errorf("verify runtime token parent for %s: %w", dir, err)
	}
	defer func() { _ = dirfd.Close(parentFd) }()

	mkErr := unix.Mkdirat(parentFd, leaf, 0o700)
	created := mkErr == nil
	if mkErr != nil && !errors.Is(mkErr, syscall.EEXIST) {
		return fmt.Errorf("mkdir runtime token dir %s: %w", dir, mkErr)
	}

	d, err := dirfd.OpenAt(parentFd, leaf, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open runtime token dir %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(d.Fd()), &st); err != nil {
		return fmt.Errorf("stat runtime token dir %s: %w", dir, err)
	}
	if int(st.Uid) != euid {
		return fmt.Errorf("runtime token dir %s owned by uid %d, expected %d", dir, st.Uid, euid)
	}
	if created {
		if euid == 0 {
			if err := d.Chown(0, 0); err != nil {
				return fmt.Errorf("chown runtime token dir %s to 0:0: %w", dir, err)
			}
		}
		if err := d.Chmod(0o700); err != nil {
			return fmt.Errorf("chmod runtime token dir %s to 0700: %w", dir, err)
		}
		return nil
	}

	// Pre-existing directory: require exact owner and 0700 mode; never mutate it.
	if euid == 0 && st.Gid != 0 {
		return fmt.Errorf("pre-existing runtime token dir %s has gid %d, expected 0", dir, st.Gid)
	}
	if perm := st.Mode & 0o777; perm != 0o700 {
		return fmt.Errorf("pre-existing runtime token dir %s has mode %04o, expected 0700", dir, perm)
	}
	return nil
}

// TakeHandoffToken reads the broker handoff file (<scion home>/.scion/scion-token)
// through a no-follow, size-bounded, single-link regular-file reader, and
// unlinks it unconditionally (even when the entry is a symlink, FIFO, hardlink,
// oversize file, or contains invalid text).
//
// Return semantics:
//   - Missing file or missing .scion directory: ("", fs.ErrNotExist)
//   - Unlink failed (e.g. unwritable directory, directory planted at leaf, or
//     symlinked .scion parent): ("", error wrapping ErrHandoffUnlinkFailed)
//   - Entry refused by reader (symlink, FIFO, hardlink, oversize) AND unlink
//     succeeded: ("", readErr)
//   - Regular file read and unlinked: (strings.TrimSpace(content), nil)
func TakeHandoffToken() (string, error) {
	if testing.Testing() && !tokenHomeOverridden {
		panic("scion/hub: TakeHandoffToken called during a test without SetTokenHome()")
	}
	return takeHandoffFile(HandoffTokenFilePath())
}

// takeHandoffFile implements the no-follow read + unconditional unlink
// semantics for a handoff credential file.
func takeHandoffFile(path string) (string, error) {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fs.ErrNotExist
		}
		return "", fmt.Errorf("%w: open parent of %s: %w", ErrHandoffUnlinkFailed, path, err)
	}
	defer func() { _ = dirfd.Close(dirFd) }()

	data, readErr := readTokenAt(dirFd, leaf, path)
	if errors.Is(readErr, fs.ErrNotExist) {
		return "", fs.ErrNotExist
	}

	// Unlink unconditionally regardless of readErr (symlink, FIFO, oversize,
	// hardlink, or invalid content). Never follows symlinks.
	if unlinkErr := dirfd.UnlinkAt(dirFd, leaf); unlinkErr != nil && !errors.Is(unlinkErr, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s: %w", ErrHandoffUnlinkFailed, path, unlinkErr)
	}

	if readErr != nil {
		return "", readErr
	}
	return strings.TrimSpace(data), nil
}

// RemoveStaleHandoffTmpFile removes any leftover <scion home>/.scion/scion-token.tmp
// without following symlinks. Missing parent or file is a no-op.
func RemoveStaleHandoffTmpFile() error {
	if testing.Testing() && !tokenHomeOverridden {
		panic("scion/hub: RemoveStaleHandoffTmpFile called during a test without SetTokenHome()")
	}
	return removeHandoffSiblingNoFollow(HandoffTokenTmpFilePath())
}

func removeHandoffSiblingNoFollow(path string) error {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: open parent of %s: %w", ErrHandoffUnlinkFailed, path, err)
	}
	defer func() { _ = dirfd.Close(dirFd) }()

	if err := dirfd.UnlinkAt(dirFd, leaf); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s: %w", ErrHandoffUnlinkFailed, path, err)
	}
	return nil
}

// VerifyHandoffTokenAbsent asserts via dirfd no-follow that the handoff token
// path does not exist (as any entry type: file, symlink, FIFO, or directory).
// Returns nil when absent, or an error wrapping ErrHandoffUnlinkFailed.
func VerifyHandoffTokenAbsent() error {
	if testing.Testing() && !tokenHomeOverridden {
		panic("scion/hub: VerifyHandoffTokenAbsent called during a test without SetTokenHome()")
	}
	return verifyHandoffAbsent(HandoffTokenFilePath())
}

// TakeHandoffTransportToken reads the broker transport-token handoff file
// (<scion home>/.scion/transport-token) through the same no-follow,
// size-bounded, single-link regular-file reader as TakeHandoffToken and
// unlinks it unconditionally.
func TakeHandoffTransportToken() (string, error) {
	if testing.Testing() && !tokenHomeOverridden {
		panic("scion/hub: TakeHandoffTransportToken called during a test without SetTokenHome()")
	}
	return takeHandoffFile(HandoffTransportTokenFilePath())
}

// RemoveStaleHandoffTransportTmpFile removes any leftover
// <scion home>/.scion/transport-token.tmp without following symlinks.
func RemoveStaleHandoffTransportTmpFile() error {
	if testing.Testing() && !tokenHomeOverridden {
		panic("scion/hub: RemoveStaleHandoffTransportTmpFile called during a test without SetTokenHome()")
	}
	return removeHandoffSiblingNoFollow(HandoffTransportTokenTmpFilePath())
}

// VerifyHandoffTransportTokenAbsent asserts via dirfd no-follow that the
// handoff transport token path does not exist.
func VerifyHandoffTransportTokenAbsent() error {
	if testing.Testing() && !tokenHomeOverridden {
		panic("scion/hub: VerifyHandoffTransportTokenAbsent called during a test without SetTokenHome()")
	}
	return verifyHandoffAbsent(HandoffTransportTokenFilePath())
}

func verifyHandoffAbsent(path string) error {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: open parent of %s: %w", ErrHandoffUnlinkFailed, path, err)
	}
	defer func() { _ = dirfd.Close(dirFd) }()

	var st unix.Stat_t
	err = unix.Fstatat(dirFd, leaf, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: stat %s: %w", ErrHandoffUnlinkFailed, path, err)
	}
	return fmt.Errorf("%w: handoff path %s still exists", ErrHandoffUnlinkFailed, path)
}
