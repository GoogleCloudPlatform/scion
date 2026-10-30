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

package privatefs

import (
	"os"

	"golang.org/x/sys/windows"
)

// OpenNoFollow opens the existing file or directory rel through the walk:
// every component is opened relative to its parent's handle, the leaf with
// FILE_OPEN_REPARSE_POINT, and a reparse point at any component (leaf
// included, whatever its tag) is refused with ErrReparsePoint. The handle
// has READ_CONTROL and FILE_READ_ATTRIBUTES access only, and shares read,
// write and delete.
//
// It returns a handle for metadata decisions (owner, attributes); it makes
// no privacy decision — callers must not read secrets through it.
func (t *Tree) OpenNoFollow(rel string) (*os.File, error) {
	h, _, err := t.openLeaf(rel, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), t.display(rel)), nil
}
