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

package metadata

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// readShutdownToken reads the shutdown token writeShutdownToken wrote,
// without following a symlink at any path component, without blocking on a
// planted FIFO, and without treating anything but a single-link regular
// file owned by this process as a source of trust: the token path is a
// predictable name under os.TempDir(), so a shared, world-writable
// directory lets another user plant something there before this process's
// own writeShutdownToken has run (e.g. between init runs bootstrap
// resets), and shutdownExisting sends whatever it reads here to another
// process's authenticated endpoint. O_NONBLOCK keeps a FIFO's open(2) from
// blocking forever waiting for a writer; the Nlink and Uid checks refuse a
// hardlink to (or a file planted by) something other than this process.
func readShutdownToken(path string) ([]byte, error) {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	f, err := dirfd.OpenAt(dirFd, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("shutdown token path %s is not a single-link regular file owned by this process", path)
	}
	return io.ReadAll(io.LimitReader(f, shutdownTokenMaxBytes))
}

func writeShutdownToken(path, token string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(token + "\n"); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
