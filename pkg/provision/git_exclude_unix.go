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

package provision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openGitExclude opens hostPath/.git/info/exclude for reading and appending
// without following a symlink at .git, info or exclude: each component is
// opened with openat relative to its parent's descriptor and O_NOFOLLOW, so
// none of them can be redirected by a symlink, or swapped for one between
// a check and the open. info is created if missing; exclude is created if
// missing and must be a regular file (O_NONBLOCK keeps a FIFO planted there
// from blocking the open; it is then refused).
func openGitExclude(hostPath string) (*os.File, error) {
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	rootFd, err := unix.Open(hostPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", hostPath, err)
	}
	defer func() { _ = unix.Close(rootFd) }()
	gitDir := filepath.Join(hostPath, ".git")
	gitFd, err := unix.Openat(rootFd, ".git", dirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s without following symlinks: %w", gitDir, err)
	}
	defer func() { _ = unix.Close(gitFd) }()

	infoDir := filepath.Join(gitDir, "info")
	infoFd, err := unix.Openat(gitFd, "info", dirFlags, 0)
	if errors.Is(err, unix.ENOENT) {
		if mkErr := unix.Mkdirat(gitFd, "info", 0o755); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
			return nil, fmt.Errorf("mkdir %s: %w", infoDir, mkErr)
		}
		infoFd, err = unix.Openat(gitFd, "info", dirFlags, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s without following symlinks: %w", infoDir, err)
	}
	defer func() { _ = unix.Close(infoFd) }()

	excludePath := filepath.Join(infoDir, "exclude")
	fd, err := unix.Openat(infoFd, "exclude", unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s without following symlinks: %w", excludePath, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("stat %s: %w", excludePath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%s is not a regular file", excludePath)
	}
	return os.NewFile(uintptr(fd), excludePath), nil
}
