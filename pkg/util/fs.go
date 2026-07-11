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
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// CopyDir recursively copies a directory tree, attempting to preserve permissions.
// Source directory must exist. Python bytecode artifacts (__pycache__ directories
// and .pyc files) are skipped to avoid propagating stale, potentially root-owned
// bytecode into agent homes.
func CopyDir(src string, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		base := filepath.Base(path)

		if info.IsDir() && base == "__pycache__" {
			return filepath.SkipDir
		}
		if !info.IsDir() && strings.HasSuffix(base, ".pyc") {
			return nil
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(target, data, info.Mode())
	})
}

// CopyFile copies a single file from src to dst.
func CopyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, info.Mode())
}

// MakeWritableRecursive recursively makes all files and directories in the path writable by the user.
func MakeWritableRecursive(path string) error {
	var totalFiles, chmodCount int
	err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		totalFiles++
		if info.Mode().Perm()&0200 == 0 {
			chmodCount++
			return os.Chmod(path, info.Mode().Perm()|0200)
		}
		return nil
	})
	Debugf("MakeWritableRecursive: walked %d files, chmod'd %d", totalFiles, chmodCount)
	return err
}

// RemoveAllSafe removes a directory tree in a single pass, handling
// symlinks and permissions inline to avoid the overhead of separate walks.
//
// It works in three phases:
//  1. Walk the tree with filepath.WalkDir (uses Lstat, never follows
//     symlinks). During the walk, symlinks are removed immediately and
//     read-only directories are made writable so their contents can be
//     listed and deleted.
//  2. Regular files collected during the walk are deleted.
//  3. Directories are removed bottom-up (deepest first).
//
// Symlink removal uses removeSymlinkSafe which avoids triggering macOS
// autofs timeouts on dangling symlinks pointing to container-internal
// paths (e.g. /home/scion/...).
func RemoveAllSafe(root string) error {
	Debugf("RemoveAllSafe: starting removal of %s", root)
	start := time.Now()
	var files []string
	var dirs []string
	var symlinkCount int
	var firstErr error

	var lastDir string
	var dirStart time.Time

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		// Track time spent per directory to identify slow ReadDir calls.
		currentDir := filepath.Dir(path)
		if currentDir != lastDir {
			if lastDir != "" && time.Since(dirStart) > 100*time.Millisecond {
				Debugf("RemoveAllSafe: slow dir listing+processing: %v for %s", time.Since(dirStart), filepath.Base(lastDir))
			}
			lastDir = currentDir
			dirStart = time.Now()
		}

		if err != nil {
			if os.IsPermission(err) {
				// Make the parent directory writable and retry.
				parent := filepath.Dir(path)
				if chErr := os.Chmod(parent, 0700); chErr == nil {
					// Re-stat to determine the entry type.
					info, stErr := os.Lstat(path)
					if stErr != nil {
						return nil // skip this entry
					}
					if info.Mode()&os.ModeSymlink != 0 {
						removeSymlinkSafe(path)
						symlinkCount++
						return nil
					}
					if info.IsDir() {
						_ = os.Chmod(path, 0700)
						dirs = append(dirs, path)
						return nil
					}
					files = append(files, path)
					return nil
				}
			}
			return nil // skip entries we cannot access
		}

		if d.Type()&os.ModeSymlink != 0 {
			// Remove symlinks immediately during the walk to prevent
			// any later operation from touching their targets.
			removeSymlinkSafe(path)
			symlinkCount++
			return nil
		}

		if d.IsDir() {
			// Ensure the directory is writable so we can list/delete contents.
			info, infoErr := d.Info()
			if infoErr == nil && info.Mode().Perm()&0700 != 0700 {
				_ = os.Chmod(path, 0700)
			}
			dirs = append(dirs, path)
			return nil
		}

		files = append(files, path)
		return nil
	})
	if walkErr != nil && firstErr == nil {
		firstErr = walkErr
	}
	Debugf("RemoveAllSafe: walk completed in %v (symlinks: %d, files: %d, dirs: %d)", time.Since(start), symlinkCount, len(files), len(dirs))

	// Phase 2: remove regular files.
	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			if os.IsPermission(err) {
				_ = os.Chmod(filepath.Dir(f), 0700)
				err = os.Remove(f)
			}
			if err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
		}
	}

	// Phase 3: remove directories bottom-up (deepest paths first).
	// Since WalkDir visits in lexical order (parent before children),
	// reversing gives us children before parents.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil && !os.IsNotExist(err) {
			if os.IsPermission(err) {
				_ = os.Chmod(dirs[i], 0700)
				err = os.Remove(dirs[i])
			}
			if err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
		}
	}
	Debugf("RemoveAllSafe: completed in %v", time.Since(start))

	return firstErr
}

// removeSymlinkSafe removes a symlink without triggering macOS autofs.
//
// On macOS, calling unlink() on a symlink whose target is under an autofs
// mount (e.g. /home/scion/...) can trigger the automounter, causing a
// multi-second timeout while macOS tries to resolve the nonexistent
// container-internal path.
//
// This function uses syscall.Unlinkat with a parent directory fd to avoid
// full path resolution. It also tries an atomic rename-over as a second
// strategy. Debug timing is included to diagnose which approach works.
func removeSymlinkSafe(path string) {
	start := time.Now()
	dir := filepath.Dir(path)
	name := filepath.Base(path)

	// Strategy 1: unlinkat with parent directory fd.
	// This avoids full path resolution — the kernel operates directly on
	// the directory entry via the fd, without resolving the symlink target.
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY, 0)
	if err == nil {
		err = unix.Unlinkat(dirFD, name, 0)
		_ = syscall.Close(dirFD)
		if err == nil {
			elapsed := time.Since(start)
			if elapsed > 100*time.Millisecond {
				Debugf("removeSymlinkSafe: unlinkat took %v for %s", elapsed, name)
			}
			return
		}
		Debugf("removeSymlinkSafe: unlinkat failed for %s: %v", name, err)
	}

	// Strategy 2: rename a temp file over the symlink, then remove the
	// regular file. rename(2) operates on directory entries without
	// resolving symlink targets.
	f, tmpErr := os.CreateTemp(dir, ".symrm.*")
	if tmpErr == nil {
		tmp := f.Name()
		_ = f.Close()
		if os.Rename(tmp, path) == nil {
			_ = os.Remove(path)
			elapsed := time.Since(start)
			if elapsed > 100*time.Millisecond {
				Debugf("removeSymlinkSafe: rename-over took %v for %s", elapsed, name)
			}
			return
		}
		_ = os.Remove(tmp)
	}

	// Strategy 3: direct os.Remove (may trigger autofs on macOS).
	Debugf("removeSymlinkSafe: falling back to os.Remove for %s", name)
	if rmErr := os.Remove(path); rmErr != nil && os.IsPermission(rmErr) {
		_ = os.Chmod(dir, 0700)
		_ = os.Remove(path)
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		Debugf("removeSymlinkSafe: os.Remove took %v for %s", elapsed, name)
	}
}
