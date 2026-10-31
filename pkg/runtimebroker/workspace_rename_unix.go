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

package runtimebroker

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// renameSiblingDir atomically renames the directory oldName onto newName,
// both entries of the directory parent, with renameat(2) on one parent
// descriptor. newName must be absent or an empty directory, which POSIX
// rename replaces atomically (NFS performs it on the server); os.Rename
// cannot do this, because it refuses an existing directory as the target.
// The parent is opened with O_NOFOLLOW, so a symlink in its place is
// refused.
func renameSiblingDir(parent, oldName, newName string) error {
	fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open the workspace parent: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Renameat(fd, oldName, fd, newName); err != nil {
		return fmt.Errorf("rename the staged workspace into place: %w", err)
	}
	return nil
}
