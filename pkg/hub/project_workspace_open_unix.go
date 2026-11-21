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

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// workspaceBeforeOpenHook, when set (tests only), runs immediately before
// openWorkspaceRegularForRead and openWorkspaceFileForWrite open name.
var workspaceBeforeOpenHook atomic.Pointer[func(name string)]

// openAtInRoot opens the slash-separated relative path name under dir using
// rootopen.OpenAt and translates path validation refusals to
// errWorkspaceWriteRefused.
func openAtInRoot(dir *os.Root, name string, flags int, perm uint32) (*os.File, error) {
	f, err := rootopen.OpenAt(dir, name, flags, perm)
	if err != nil {
		if errors.Is(err, rootopen.ErrRefused) {
			return nil, fmt.Errorf("%w: invalid workspace path", errWorkspaceWriteRefused)
		}
		return nil, err
	}
	return f, nil
}

// openWorkspaceRegularForRead opens name under dir read-only without blocking
// on special files or following any symlink component, then verifies that the
// opened descriptor is a regular file matching expected (when non-nil).
func openWorkspaceRegularForRead(dir *os.Root, name string, expected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	if hook := workspaceBeforeOpenHook.Load(); hook != nil {
		(*hook)(name)
	}
	f, info, err := rootopen.OpenRegularForRead(dir, name, expected)
	if err != nil {
		if errors.Is(err, rootopen.ErrRefused) {
			return nil, nil, fmt.Errorf("%w: target changed while opening it", errWorkspaceWriteRefused)
		}
		return nil, nil, err
	}
	return f, info, nil
}

// openWorkspaceFileForWrite opens name under dir for writing without truncating,
// without blocking on special files, and without following any symlink component.
// When exists is false, O_CREAT|O_EXCL ensures an entry appearing in between
// is refused rather than opened or followed.
func openWorkspaceFileForWrite(dir *os.Root, name string, exists bool) (*os.File, error) {
	if hook := workspaceBeforeOpenHook.Load(); hook != nil {
		(*hook)(name)
	}
	f, err := rootopen.OpenFileForWrite(dir, name, 0o644, exists)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrExist):
			return nil, fmt.Errorf("%w: target appeared while opening it", errWorkspaceWriteRefused)
		case errors.Is(err, rootopen.ErrRefused):
			return nil, fmt.Errorf("%w: target is not a regular file", errWorkspaceWriteRefused)
		default:
			return nil, err
		}
	}
	return f, nil
}
