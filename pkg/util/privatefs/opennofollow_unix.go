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

package privatefs

import (
	"os"
	"syscall"
)

// OpenNoFollow opens the existing file or directory rel read-only, never
// following a symlink at rel (O_NOFOLLOW; a symlink is refused with
// ErrReparsePoint). O_NONBLOCK keeps a FIFO planted at rel from blocking
// the open.
//
// It returns a handle for metadata decisions (owner, attributes); it makes
// no privacy decision — callers must not read secrets through it.
func (t *Tree) OpenNoFollow(rel string) (*os.File, error) {
	path, err := t.path(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, linkErr(path, err)
	}
	return f, nil
}
