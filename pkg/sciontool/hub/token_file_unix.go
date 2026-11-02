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
	"fmt"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// ChownTokenFile fixes the ownership of an already-written token file to
// uid:gid — used when the token file was written by another process (the
// host-side agent manager, before this container started) and this
// (root) process only needs to hand it off to the scion user, not rewrite
// its content.
//
// It resolves the token file's parent directory the same symlink-safe way
// WriteFileNoFollowChown does (dirfd.OpenParentNoFollow), then opens the
// leaf itself without following a symlink, and refuses to chown anything
// but a single-link regular file — a hardlink to a root-owned file would
// otherwise pass a bare "is this a regular file" check and hand that file
// to the scion user. This check always applies, on every runtime. The
// chown is fchown on that open fd, never a path-based chown that a symlink
// swapped in afterwards could redirect.
//
// When EnforceTokenFileOwnerChecks(true) has been called (substrate only),
// it additionally requires the file's current owner to be root or the
// containing directory's own owner before chowning it — the same rule
// readTokenFileGuarded applies, gated the same way and for the same
// reason: it isn't provably safe to require on every runtime's
// token-provisioning path, only on the one that requires privilege drop.
func ChownTokenFile(uid, gid int) error {
	path := TokenFilePath()
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		return fmt.Errorf("failed to open parent directory of token file: %w", err)
	}
	defer func() { _ = dirfd.Close(dirFd) }()

	var dirSt syscall.Stat_t
	if err := syscall.Fstat(dirFd, &dirSt); err != nil {
		return fmt.Errorf("failed to stat parent directory of token file: %w", err)
	}

	f, err := dirfd.OpenAt(dirFd, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("failed to open token file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return fmt.Errorf("failed to stat token file: %w", err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 {
		return fmt.Errorf("refusing to chown token file: not a single-link regular file")
	}
	if enforceTokenFileOwnerChecks.Load() && st.Uid != 0 && st.Uid != dirSt.Uid {
		return fmt.Errorf("refusing to chown token file: unexpected owner")
	}
	if err := f.Chown(uid, gid); err != nil {
		return fmt.Errorf("failed to chown token file: %w", err)
	}
	return nil
}

// readTokenAt applies the token-file checks to leaf, relative to the
// already-resolved parent directory dirFd; path is used only in errors.
func readTokenAt(dirFd int, leaf, path string) (string, error) {
	value, _, err := readTokenAtStat(dirFd, leaf, path)
	return value, err
}

// readTokenAtStat is readTokenAt that also returns the fstat of the file
// it read, taken on the same descriptor.
func readTokenAtStat(dirFd int, leaf, path string) (string, dirfd.Stat, error) {
	var dirSt syscall.Stat_t
	if err := syscall.Fstat(dirFd, &dirSt); err != nil {
		return "", dirSt, fmt.Errorf("failed to stat parent directory of %s: %w", path, err)
	}

	data, st, err := dirfd.ReadAtNoFollowStat(dirFd, leaf, tokenFileMaxBytes)
	if err != nil {
		return "", st, fmt.Errorf("%s: %w", path, err)
	}
	if enforceTokenFileOwnerChecks.Load() && st.Uid != 0 && st.Uid != dirSt.Uid {
		return "", st, fmt.Errorf("refusing to read %s: unexpected owner", path)
	}
	return string(data), st, nil
}
