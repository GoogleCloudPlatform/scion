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

//go:build linux

package provision

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames oldpath to newpath, failing with fs.ErrExist if
// newpath exists. It uses renameat2(RENAME_NOREPLACE), and falls back to a
// check followed by a rename where the filesystem does not support it.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrExist}
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.ENOTSUP):
		return renameNoReplaceFallback(oldpath, newpath)
	default:
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
}
