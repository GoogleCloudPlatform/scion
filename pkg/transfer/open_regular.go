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

package transfer

import (
	"errors"
	"os"
	"sync/atomic"
)

// ErrNotRegularFile is wrapped by OpenRegularFile's error when the path is
// a link, or another entry that is not a regular file.
var ErrNotRegularFile = errors.New("not a regular file")

// ErrFileReplaced is wrapped by OpenRegularFile's error when the file
// opened is not the one its Lstat saw (it was replaced in between).
var ErrFileReplaced = errors.New("replaced while it was opened")

// openRegularAfterLstat, when set (tests only), runs between
// OpenRegularFile's Lstat and its open.
var openRegularAfterLstat atomic.Pointer[func(rel string)]

// OpenRegularFile opens rel, a path relative to root, for reading, only if
// it is a regular file:
//   - root.Lstat must show a regular file, so a link is not followed and a
//     FIFO, device or socket is never opened;
//   - the open is non-blocking (readOnlyFlags), so a FIFO swapped in after
//     the Lstat does not block it;
//   - the opened file must be a regular file and the same file the Lstat
//     saw (os.SameFile).
//
// A link or another non-regular entry returns an error wrapping
// ErrNotRegularFile; a file replaced between the Lstat and the open returns
// one wrapping ErrFileReplaced. Other failures of the Lstat, the open or the
// stat are returned as the os package reports them. On success it returns
// the open file and its FileInfo; the caller must close the file.
func OpenRegularFile(root *os.Root, rel string) (*os.File, os.FileInfo, error) {
	before, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, &os.PathError{Op: "open", Path: rel, Err: ErrNotRegularFile}
	}
	if hook := openRegularAfterLstat.Load(); hook != nil {
		(*hook)(rel)
	}
	f, err := root.OpenFile(rel, readOnlyFlags, 0)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, &os.PathError{Op: "open", Path: rel, Err: ErrNotRegularFile}
	}
	if !os.SameFile(before, opened) {
		_ = f.Close()
		return nil, nil, &os.PathError{Op: "open", Path: rel, Err: ErrFileReplaced}
	}
	return f, opened, nil
}
