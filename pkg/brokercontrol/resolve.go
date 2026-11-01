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

package brokercontrol

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// caseInsensitive reports whether the platform's default filesystem is
// case-insensitive. It only ever widens refusals. A variable so tests can
// exercise the case-insensitive comparisons on any host.
var caseInsensitive = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

const sep = string(filepath.Separator)

// resolvePhysical returns the absolute path p with every symlink resolved.
// It does not clean p lexically first: filepath.EvalSymlinks applies a ".."
// to the already-resolved prefix, so a ".." after a symlink goes to the link
// target's parent, as it does for the kernel. When p does not exist, the
// deepest existing ancestor is resolved and the missing components are
// appended (they cannot be symlinks, as they do not exist). It fails for a
// relative p, a dangling symlink, a ".." among the missing components, a
// component that is not a directory, a permission error, and any other
// resolution error: the caller treats every failure as a refusal.
func resolvePhysical(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q is not an absolute path", p)
	}
	vol := filepath.VolumeName(p)
	parts := strings.Split(p[len(vol):], sep)
	for i := len(parts); i > 0; i-- {
		prefix := vol + strings.Join(parts[:i], sep)
		if prefix == vol {
			prefix = vol + sep
		}
		resolved, err := filepath.EvalSymlinks(prefix)
		if err == nil {
			missing := parts[i:]
			for _, m := range missing {
				if m == ".." {
					return "", fmt.Errorf("%q has a \"..\" after a component that does not exist", p)
				}
			}
			return filepath.Clean(filepath.Join(append([]string{resolved}, missing...)...)), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(prefix); lerr == nil {
			return "", fmt.Errorf("%s is a symlink whose target does not exist", prefix)
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			return "", lerr
		}
	}
	return "", fmt.Errorf("no part of %q exists", p)
}

// pathEqual reports whether a and b name the same path, by spelling: exact,
// or case-insensitively on a case-insensitive platform. For refusals only.
func pathEqual(a, b string) bool {
	return a == b || (caseInsensitive && strings.EqualFold(a, b))
}

// strictlyUnder reports whether p is a strict descendant of base, by
// spelling (case-folded on a case-insensitive platform). Both are clean and
// absolute. For refusals only.
func strictlyUnder(p, base string) bool {
	return strictlyUnderFold(p, base, caseInsensitive)
}

func strictlyUnderFold(p, base string, fold bool) bool {
	prefix := base
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	if fold {
		p, prefix = strings.ToLower(p), strings.ToLower(prefix)
	}
	return len(p) > len(prefix) && strings.HasPrefix(p, prefix)
}

// exactlyAtOrUnder reports whether p is base or below it with an exact,
// case-preserving spelling, which is what an allow-list decision requires.
func exactlyAtOrUnder(p, base string) bool {
	return p == base || strictlyUnderFold(p, base, false)
}

// identityAtOrAbove reports whether the existing file at p is the same file
// (os.SameFile) as target or as one of target's existing ancestors. It finds
// another spelling of an ancestor relation: a case variant, a second bind
// mount of the same directory, or a hard link to a file. It reports false
// when p does not exist.
func identityAtOrAbove(p, target string) bool {
	pi, err := os.Stat(p)
	if err != nil {
		return false
	}
	for dir := target; ; {
		if fi, err := os.Stat(dir); err == nil && os.SameFile(fi, pi) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
