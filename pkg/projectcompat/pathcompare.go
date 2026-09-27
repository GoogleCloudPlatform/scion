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

package projectcompat

import "path/filepath"

// ResolvePathForCompare returns path fully resolved: first made absolute
// (against the current working directory, matching how the rest of the
// standard library treats a relative path) so a relative and an absolute
// path naming the same location compare equal — this also cleans the path
// lexically, so a ".." segment is collapsed before symlinks are resolved,
// rather than being applied physically against whatever the preceding
// component resolves to — then resolved through filepath.EvalSymlinks, so a
// path recorded before a directory was renamed to a new name (with a symlink
// left behind at the old one) compares equal to its canonical replacement.
// When path no longer resolves — already renamed away, or never created — it
// falls back to filepath.Clean, so two equally unresolvable paths still
// compare consistently instead of erroring.
func ResolvePathForCompare(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// ResolvedPathEqual reports whether a and b identify the same filesystem
// location, comparing them through ResolvePathForCompare.
func ResolvedPathEqual(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return ResolvePathForCompare(a) == ResolvePathForCompare(b)
}

// LabelValuesMatch reports whether actual, the value read for label key,
// matches the wanted filter value v. LabelProjectPath is compared through
// ResolvedPathEqual: a running agent's recorded project path may still be a
// path from before its project directory was renamed, while a caller
// filtering by project path supplies the canonical, resolved one. Every
// other label is compared exactly.
func LabelValuesMatch(key, actual, want string) bool {
	if key == LabelProjectPath {
		return ResolvedPathEqual(actual, want)
	}
	return actual == want
}

// ResolvedPathHasPrefix reports whether path, once resolved through
// ResolvePathForCompare, lies inside prefix (also resolved), as a directory
// boundary — that is, path equals prefix or is nested under it.
func ResolvedPathHasPrefix(path, prefix string) bool {
	if path == "" || prefix == "" {
		return false
	}
	resolvedPath := ResolvePathForCompare(path)
	resolvedPrefix := ResolvePathForCompare(prefix)
	if resolvedPath == resolvedPrefix {
		return true
	}
	if len(resolvedPrefix) > 0 && resolvedPrefix[len(resolvedPrefix)-1] == filepath.Separator {
		// resolvedPrefix is the filesystem root ("/" on Unix): filepath.Clean
		// (via EvalSymlinks or the Clean fallback) leaves the trailing
		// separator only for the root, and that separator already marks the
		// directory boundary, so requiring a second one after it would wrongly
		// refuse every direct child of root.
		return len(resolvedPath) > len(resolvedPrefix) && resolvedPath[:len(resolvedPrefix)] == resolvedPrefix
	}
	return len(resolvedPath) > len(resolvedPrefix) &&
		resolvedPath[len(resolvedPrefix)] == filepath.Separator &&
		resolvedPath[:len(resolvedPrefix)] == resolvedPrefix
}
