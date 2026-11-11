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
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ValidateRelPath returns an error unless p is a canonical, slash-separated
// relative path: non-empty, not "." itself, not absolute, already clean
// (path.Clean(p) == p), local on the current platform (filepath.IsLocal), with
// no ".." element and no backslash or NUL byte. The returned error describes
// the rule that failed and never includes p.
func ValidateRelPath(p string) error {
	switch {
	case p == "":
		return errors.New("path is empty")
	case strings.ContainsRune(p, 0):
		return errors.New("path contains a NUL byte")
	case strings.Contains(p, `\`):
		return errors.New("path contains a backslash")
	case path.IsAbs(p) || filepath.IsAbs(p):
		return errors.New("path is absolute")
	case p == ".":
		return errors.New("path names the root directory")
	case path.Clean(p) != p:
		return errors.New("path is not in canonical form")
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == ".." {
			return errors.New("path contains a parent directory element")
		}
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		return errors.New("path is not local")
	}
	return nil
}

// WriteFileInRoot writes data to the slash-separated relative path rel inside
// root, creating missing parent directories with mode 0755 and the file with
// perm. All filesystem operations go through root, so the file is only ever
// created inside the directory root was opened on.
func WriteFileInRoot(root *os.Root, rel string, data []byte, perm os.FileMode) error {
	name := filepath.FromSlash(rel)
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}
	return root.WriteFile(name, data, perm)
}
