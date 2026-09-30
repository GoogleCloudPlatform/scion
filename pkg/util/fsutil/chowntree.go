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

package fsutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// chownFunc performs the ownership change on one filesystem entry. Tests
// substitute a recording function in place of osLchown.
type chownFunc func(path string, uid, gid int) error

// deviceOfFunc reports the device number backing path, given its already
// stat'd info. Tests substitute a fake to simulate a filesystem/mount
// boundary without a real mount.
type deviceOfFunc func(path string, info fs.FileInfo) (dev uint64, ok bool)

func osLchown(path string, uid, gid int) error {
	return os.Lchown(path, uid, gid)
}

func statDeviceOf(_ string, info fs.FileInfo) (uint64, bool) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(sys.Dev), true
}

// ChownTree recursively changes ownership of every entry under root
// (root included) to uid:gid.
//
// Policy:
//   - root must pass CheckRoot or this returns that error without touching
//     anything.
//   - root must exist: a missing root is reported as an error (wrapping
//     fs.ErrNotExist), matching plain `chown -R`'s own behavior on a
//     nonexistent path. This error can be an errors.Join of every per-entry
//     failure the walk hit, and errors.Is on a joined error is true if *any*
//     one member matches — so a caller must not decide "the root is missing"
//     by testing errors.Is(err, fs.ErrNotExist) on what this function
//     returns; that would also match, and hide, an unrelated real failure
//     whenever some other entry happens to vanish mid-walk. A caller for
//     whom a missing root legitimately means "nothing to fix up" (e.g. an
//     optional, image-dependent directory) must check root's own existence
//     with os.Lstat *before* calling this function, as
//     cmd/sciontool/commands/init.go's chownTreeRootOwned does, and only
//     call it once root is known to exist.
//   - the walk never crosses a filesystem (device) boundary: an entry whose
//     device differs from root's own device is left untouched, and if it is
//     a directory, not descended into.
//   - every entry is Lchown'd, never Chown'd: a symlink is re-owned itself
//     and its target is never touched or dereferenced.
//   - a failure on one entry does not stop the walk. All per-entry failures
//     (and any walk-enumeration failure, e.g. permission denied listing a
//     directory) are collected and returned together via errors.Join once
//     the walk finishes — this matches plain `chown -R`'s continue-on-error
//     behavior, rather than stopping at the first bad entry.
//   - ctx is checked between entries; a cancelled/timed-out context stops
//     the walk early, and that error is included in the returned join.
func ChownTree(ctx context.Context, root string, uid, gid int) error {
	return chownTree(ctx, root, uid, gid, nil, osLchown, statDeviceOf)
}

// ChownTreeOwnedByUID is like ChownTree, but leaves an entry untouched
// unless it is currently owned by ownerUID. This is for fix-ups that must
// only re-own files a specific process (e.g. a root-run provisioning hook)
// created, not everything under root.
func ChownTreeOwnedByUID(ctx context.Context, root string, ownerUID, uid, gid int) error {
	return chownTree(ctx, root, uid, gid, ownedByUID(ownerUID), osLchown, statDeviceOf)
}

// ownedByUID returns a filter matching entries currently owned by uid, for
// use with chownTree's filter parameter.
func ownedByUID(uid int) func(fs.FileInfo) bool {
	return func(info fs.FileInfo) bool {
		sys, ok := info.Sys().(*syscall.Stat_t)
		return ok && int(sys.Uid) == uid
	}
}

func chownTree(ctx context.Context, root string, uid, gid int, filter func(fs.FileInfo) bool, chown chownFunc, deviceOf deviceOfFunc) error {
	if err := CheckRoot(root); err != nil {
		return err
	}

	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("stat chown root %q: %w", root, err)
	}
	rootDev, hasRootDev := deviceOf(root, rootInfo)

	var errs []error
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Tolerate enumeration failures (e.g. permission denied on one
			// subdirectory) and keep going, matching chown -R's behavior.
			errs = append(errs, err)
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			errs = append(errs, fmt.Errorf("stat %q: %w", path, infoErr))
			return nil
		}
		if hasRootDev {
			if dev, ok := deviceOf(path, info); ok && dev != rootDev {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if filter != nil && !filter(info) {
			return nil
		}
		if chErr := chown(path, uid, gid); chErr != nil {
			errs = append(errs, fmt.Errorf("chown %q: %w", path, chErr))
		}
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	return errors.Join(errs...)
}
