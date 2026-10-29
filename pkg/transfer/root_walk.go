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
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync/atomic"
)

// ErrDirReplaced is wrapped by walkRoot's error when a subdirectory is
// replaced between the walk reading its entry and opening it.
var ErrDirReplaced = errors.New("directory replaced during the walk")

// walkRootBeforeOpenDir, when set (tests only), runs before walkRoot opens a
// subdirectory (rel is its slash-separated path).
var walkRootBeforeOpenDir atomic.Pointer[func(rel string)]

// walkRoot walks the tree of root depth-first in lexical order, calling fn
// for every entry below the root (rel is slash-separated, never "."), in the
// manner of fs.WalkDir: if fn returns fs.SkipDir for a directory, the walk
// does not descend into it; fs.SkipDir for a non-directory skips the rest of
// that entry's directory; fs.SkipAll stops the walk and walkRoot returns nil;
// any other error stops the walk and is returned.
//
// Each subdirectory is opened through root with a trailing path separator,
// so the kernel resolves it as a directory: a FIFO or other non-directory
// swapped in for it fails with ENOTDIR without being opened, so it cannot
// block. The opened directory must be the one its entry described
// (os.SameFile with the entry's Lstat-style info); a link or another
// directory swapped in fails with ErrDirReplaced. (fs.WalkDir over root.FS()
// would open a subdirectory with a blocking open that follows a link inside
// the root.)
func walkRoot(root *os.Root, fn func(rel string, d fs.DirEntry) error) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := walkRootDir(root, ".", f, fn); err != nil && !errors.Is(err, fs.SkipAll) {
		return err
	}
	return nil
}

// walkRootDir reads the open directory f (rel, already checked) and walks
// its entries. It closes f.
func walkRootDir(root *os.Root, rel string, f *os.File, fn func(rel string, d fs.DirEntry) error) error {
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("read directory %s: %w", rel, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		child := e.Name()
		if rel != "." {
			child = path.Join(rel, e.Name())
		}
		if err := fn(child, e); err != nil {
			if errors.Is(err, fs.SkipDir) {
				if e.IsDir() {
					continue
				}
				return nil // skip the rest of this directory
			}
			return err
		}
		if !e.IsDir() {
			continue
		}
		want, err := e.Info()
		if err != nil {
			return fmt.Errorf("stat directory %s: %w", child, err)
		}
		if hook := walkRootBeforeOpenDir.Load(); hook != nil {
			(*hook)(child)
		}
		sub, err := root.OpenFile(filepath.FromSlash(child)+string(filepath.Separator), os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("open directory %s: %w", child, err)
		}
		got, err := sub.Stat()
		if err != nil {
			_ = sub.Close()
			return fmt.Errorf("stat directory %s: %w", child, err)
		}
		if !got.IsDir() || !os.SameFile(want, got) {
			_ = sub.Close()
			return fmt.Errorf("%s: %w", child, ErrDirReplaced)
		}
		if err := walkRootDir(root, child, sub, fn); err != nil {
			return err
		}
	}
	return nil
}
