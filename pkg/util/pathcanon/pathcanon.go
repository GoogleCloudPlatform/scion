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

// Package pathcanon is the single place where scion canonicalises file
// system paths, validates caller-supplied names, and decides containment.
//
// The rule it exists to enforce: a containment decision is made only on
// the canonical form returned by Resolve, never on a caller-supplied
// string. Callers must not compare paths as strings, call filepath.Rel to
// decide containment, or special-case an alias form (8.3 short names,
// alternate data streams, device names, \\?\ and UNC prefixes, case,
// trailing dots and spaces, mixed separators). Each of those is handled
// here, once.
//
//   - Resolve returns the canonical form of a path.
//   - ValidName is the only validator of a caller-supplied name that
//     becomes one path component.
//   - ValidInputPath is the only validator of a caller-supplied whole path.
//   - Within decides containment, on Resolve output only.
package pathcanon

import (
	"errors"
	"fmt"
)

// ErrInvalidName is returned (wrapped) by ValidName, ValidInputPath and
// Resolve for a name or path that is refused.
var ErrInvalidName = errors.New("invalid path name")

// ErrDanglingLink is returned (wrapped) by Resolve and Within when a
// component of the path exists but cannot be followed: a symlink or
// junction whose target is missing. Its name is never treated as a
// not-yet-existing tail, because creating below it would create below the
// link's target, wherever that is.
var ErrDanglingLink = errors.New("path component is a link whose target does not exist")

// Within reports whether p is root or lies below it. Both sides go through
// Resolve, so links, and on Windows every alias form, are resolved before
// the comparison. A dangling link in either path is an error
// (ErrDanglingLink), never a not-yet-existing tail. The comparison is per
// path component: case-insensitive on Windows (ordinal, through the OS
// uppercase table, the rule NTFS applies to names), exact elsewhere.
func Within(root, p string) (bool, error) {
	r, err := Resolve(root)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", root, err)
	}
	q, err := Resolve(p)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", p, err)
	}
	return within(r, q), nil
}

func invalid(name, why string) error {
	return fmt.Errorf("%w %q: %s", ErrInvalidName, name, why)
}
