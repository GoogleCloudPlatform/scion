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

package util

import (
	"fmt"
	"os"
)

// openLstatVerified opens path with open only when it is a regular file,
// for platforms without O_NOFOLLOW. Lstat must report a regular file, so a
// symlink is refused before opening. After opening, fstat must report a
// regular file that os.SameFile matches, so a path swapped for a symlink or
// other link (a name-surrogate reparse point, such as a junction) before
// the open is refused. On unix the comparison is
// with the file Lstat saw. On Windows, os.SameFile reads the Lstat side's
// identity lazily, by re-opening path without following a reparse point, so
// it compares with the file at path at check time and a regular file renamed
// over path in between is accepted, as with O_NOFOLLOW. For a non-surrogate
// reparse point, Lstat records the identity eagerly. These Windows details
// are behaviour of the Go toolchain. On any refusal the opened file is closed
// and nil is returned.
func openLstatVerified(path string, open func(string) (*os.File, error)) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while it was being opened", path)
	}
	return f, nil
}
