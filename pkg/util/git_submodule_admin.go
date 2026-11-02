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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SubmoduleAdminDirs walks <gitDir>/modules and returns, as gitDir-relative
// slash paths in lexical walk order, every directory that directly contains a
// HEAD file (os.Stat, so a symlinked HEAD counts), including nested modules.
// This is the discovery the runtime uses to bind submodule admin dirs read-only,
// so a caller that checks dirs checks exactly the set that is bound.
//
// unsafe lists gitDir-relative slash paths that a fail-closed caller must
// refuse:
//   - modules itself when it is a symlink or not a directory;
//   - any symlink met while walking modules/ outside an admin dir's internals
//     (everything inside a discovered admin dir except its nested "modules"
//     subtree), since the walk does not descend into a symlinked directory that
//     git may still reach through a gitfile;
//   - <d>/config, <d>/config.worktree or <d>/worktrees/<n>/config.worktree
//     present but not a regular file, and <d>/worktrees/<n> that is a symlink.
//
// The directories on the chain from modules/ to each discovered admin dir are
// real directories by construction (the walk never descends a symlink).
// err reports the first walk error; the walk skips the failing entry and
// continues. An absent modules dir returns nil, nil, nil.
func SubmoduleAdminDirs(gitDir string) (dirs []string, unsafe []string, err error) {
	modulesDir := filepath.Join(gitDir, "modules")
	fi, lerr := os.Lstat(modulesDir)
	if lerr != nil {
		if errors.Is(lerr, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, lerr
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return nil, []string{"modules"}, nil
	}
	rel := func(path string) string {
		r, rerr := filepath.Rel(gitDir, path)
		if rerr != nil {
			return filepath.ToSlash(path)
		}
		return filepath.ToSlash(r)
	}
	// internal reports whether path lies inside a discovered admin dir's
	// internals: below its nearest admin ancestor and not in that ancestor's
	// nested modules subtree.
	internal := func(path string) bool {
		best := ""
		for _, d := range dirs {
			ad := filepath.Join(gitDir, filepath.FromSlash(d))
			if strings.HasPrefix(path, ad+string(filepath.Separator)) && len(ad) > len(best) {
				best = ad
			}
		}
		if best == "" {
			return false
		}
		first := strings.SplitN(strings.TrimPrefix(path, best+string(filepath.Separator)), string(filepath.Separator), 2)[0]
		return first != "modules"
	}
	walkErr := filepath.WalkDir(modulesDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if err == nil {
				err = werr
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if !internal(path) {
				unsafe = append(unsafe, rel(path))
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if _, serr := os.Stat(filepath.Join(path, "HEAD")); serr != nil {
			return nil
		}
		dirs = append(dirs, rel(path))
		unsafe = append(unsafe, unsafeSeedFiles(gitDir, path)...)
		return nil
	})
	if walkErr != nil && err == nil {
		err = walkErr
	}
	return dirs, unsafe, err
}

// unsafeSeedFiles returns, gitDir-relative, the config files under admin dir
// adminDir that are present but not regular files, and any worktrees/<n> that
// is a symlink.
func unsafeSeedFiles(gitDir, adminDir string) []string {
	var out []string
	rel := func(path string) string {
		r, err := filepath.Rel(gitDir, path)
		if err != nil {
			return filepath.ToSlash(path)
		}
		return filepath.ToSlash(r)
	}
	notRegular := func(path string) bool {
		fi, err := os.Lstat(path)
		return err == nil && !fi.Mode().IsRegular()
	}
	for _, name := range []string{"config", "config.worktree"} {
		if p := filepath.Join(adminDir, name); notRegular(p) {
			out = append(out, rel(p))
		}
	}
	entries, err := os.ReadDir(filepath.Join(adminDir, "worktrees"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		p := filepath.Join(adminDir, "worktrees", e.Name())
		if e.Type()&fs.ModeSymlink != 0 {
			out = append(out, rel(p))
			continue
		}
		if !e.IsDir() {
			continue
		}
		if cw := filepath.Join(p, "config.worktree"); notRegular(cw) {
			out = append(out, rel(cw))
		}
	}
	return out
}
