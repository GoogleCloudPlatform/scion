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

//go:build !windows

package pathcanon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns the canonical form of p: absolute (filepath.Abs, which
// also cleans it lexically), with the deepest existing ancestor resolved
// through filepath.EvalSymlinks and the not-yet-existing tail appended
// after ValidName accepts each tail component.
func Resolve(p string) (string, error) {
	if p == "" {
		return "", invalid(p, "empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cur := abs
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				if err := ValidName(tail[i]); err != nil {
					return "", err
				}
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// EvalSymlinks found nothing. If cur itself exists (Lstat does not
		// follow its last component), it is a symlink whose target is
		// missing, which must never become tail.
		if _, lerr := os.Lstat(cur); lerr == nil {
			return "", fmt.Errorf("resolve %s: %w", cur, ErrDanglingLink)
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			return "", lerr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// ValidName accepts a name that becomes exactly one path component. It
// rejects the empty name, ".", "..", a name holding a separator, and a
// name holding a NUL byte.
func ValidName(name string) error {
	switch {
	case name == "":
		return invalid(name, "empty name")
	case name == "." || name == "..":
		return invalid(name, "not a file name")
	case strings.ContainsRune(name, '/'):
		return invalid(name, "contains a path separator")
	case strings.ContainsRune(name, 0):
		return invalid(name, "contains a NUL byte")
	}
	return nil
}

// ValidInputPath accepts a caller-supplied whole path. It rejects the
// empty path and a path holding a NUL byte.
func ValidInputPath(p string) error {
	switch {
	case p == "":
		return invalid(p, "empty path")
	case strings.ContainsRune(p, 0):
		return invalid(p, "contains a NUL byte")
	}
	return nil
}

// within compares two Resolve outputs: q is r or below it.
func within(r, q string) bool {
	rel, err := filepath.Rel(r, q)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}
