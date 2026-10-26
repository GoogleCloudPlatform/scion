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

package hubprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// ensurePrivateDir creates dir, which must be base or below it, with mode
// 0700, and returns the private tree anchored at base. base is
// ~/.scion/hub-profiles: it and every component from it down to dir is
// inspected with Lstat, created when missing, and refused when it is a
// symlink or not a directory, so an existing directory below a symlinked
// component is never followed. Components above base (such as ~/.scion)
// are created with MkdirAll when missing and not inspected.
//
// This Lstat-per-component walk is the unix implementation;
// privatedir_windows.go walks handles with privatefs.Tree.MkdirAll.
func ensurePrivateDir(base, dir string) (*privatefs.Tree, error) {
	rel, err := privateRel(base, dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			return nil, err
		}
	}
	cur := base
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			if parts[i] == ".." || parts[i] == "" {
				return nil, fmt.Errorf("refusing %s: not below %s", dir, base)
			}
			cur = filepath.Join(cur, parts[i])
		}
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
			fi, err = os.Lstat(cur)
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing %s: it is a symlink", cur)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("refusing %s: it is not a directory", cur)
		}
	}
	return privatefs.OpenTree(base, privatefs.TreeOptions{})
}
