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

package provision

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// readSmallFileNoFollow reads the regular file at path, at most max bytes:
// it is opened with O_NOFOLLOW (a symlink at the last component is refused)
// and O_NONBLOCK (a FIFO never blocks the open), the opened descriptor must
// be a regular file, and the read is bounded (readBoundedFile).
func readSmallFileNoFollow(path string, max int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s without following symlinks: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return readBoundedFile(f, max)
}

// openDirNoFollow opens the directory at path for reading its entries,
// refusing a symlink at the last component (O_NOFOLLOW) and anything that is
// not a directory (O_DIRECTORY). A missing directory is an fs.ErrNotExist
// error.
func openDirNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open directory %s without following symlinks: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}
