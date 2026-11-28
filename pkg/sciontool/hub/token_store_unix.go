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

// ensureRuntimeTokenDir creates (if absent) and verifies the parent directory
// of runtimePath through dirfd no-follow, enforcing mode 0700 and ownership by
// the current effective UID (root:root when euid==0). Never follows symlinks.
func ensureRuntimeTokenDir(runtimePath string) error {
	dir := filepath.Dir(runtimePath)
	d, err := dirfd.EnsureDirNoFollow(dir, 0o700)
	if err != nil {
		return fmt.Errorf("ensure runtime token dir %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(d.Fd()), &st); err != nil {
		return fmt.Errorf("stat runtime token dir %s: %w", dir, err)
	}
	euid := os.Geteuid()
	if int(st.Uid) != euid {
		return fmt.Errorf("runtime token dir %s owned by uid %d, expected %d", dir, st.Uid, euid)
	}
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
