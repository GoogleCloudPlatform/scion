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

package hub

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync/atomic"
	"syscall"
)

// workspaceBeforeOpenHook, when set (tests only), runs immediately before
// openWorkspaceRegularForRead and openWorkspaceFileForWrite open name.
var workspaceBeforeOpenHook atomic.Pointer[func(name string)]

// openWorkspaceRegularForRead opens name under dir read-only without blocking
// on special files or following a final symlink, then verifies that the opened
// descriptor is a regular file matching expected (when non-nil).
func openWorkspaceRegularForRead(dir *os.Root, name string, expected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	if hook := workspaceBeforeOpenHook.Load(); hook != nil {
		(*hook)(name)
	}
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, fmt.Errorf("%w: target changed while opening it", errWorkspaceWriteRefused)
		}
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || (expected != nil && !os.SameFile(info, expected)) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: target changed while opening it", errWorkspaceWriteRefused)
	}
	return f, info, nil
}

// openWorkspaceFileForWrite opens name under dir for writing without truncating,
// without blocking on special files, and without following a final symlink.
// When exists is false, O_CREATE|O_EXCL ensures an entry appearing in between
// is refused rather than opened or followed.
func openWorkspaceFileForWrite(dir *os.Root, name string, exists bool) (*os.File, error) {
	if hook := workspaceBeforeOpenHook.Load(); hook != nil {
		(*hook)(name)
	}
	flag := os.O_WRONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
	if !exists {
		flag |= os.O_CREATE | os.O_EXCL
	}
	f, err := dir.OpenFile(name, flag, 0644)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrExist):
			return nil, fmt.Errorf("%w: target appeared while opening it", errWorkspaceWriteRefused)
		case errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.ELOOP):
			return nil, fmt.Errorf("%w: target is not a regular file", errWorkspaceWriteRefused)
		default:
			return nil, err
		}
	}
	return f, nil
}
