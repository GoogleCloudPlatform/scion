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

package privatefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ReplaceFollowing atomically replaces the file at path with data through a
// temporary file in the same directory and a rename, so readers see the old
// or the new file and never a partial one. When path is a symlink, the file
// it points to is replaced and the link is kept (dotfile managers link
// ~/.ssh files; see TreeOptions.FollowLinks). An existing file keeps its
// mode; a new file gets newMode. tempPrefix names the temporary file
// (tempPrefix + leaf + "-<random>").
//
// This is the POSIX sequence scion's ssh-config writer used before this
// package, with one tightening (the one-name rule): an
// existing regular target with more than one name (Nlink > 1, read on the
// opened file) is refused with ErrHardLinked, never replaced. A target that
// is replaced or removed while it is being checked (Nlink == 0) is opened
// again a few times, then refused with ErrReplacedConcurrently.
func ReplaceFollowing(path string, data []byte, newMode fs.FileMode, tempPrefix string) error {
	target, mode, err := replaceTarget(path, newMode)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), tempPrefix+filepath.Base(target)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	ok = true
	return nil
}

// CheckReplaceTarget runs the checks ReplaceFollowing makes before it
// writes, and writes nothing: the leaf is resolved if it is a symlink, and
// an existing regular target must have one name (the one-name rule). Callers
// writing several files in one operation run it on each first, so a
// refusal leaves no side effect. A target whose directory does not exist
// yet passes (nothing to check).
func CheckReplaceTarget(path string) error {
	_, _, err := replaceTarget(path, 0)
	return err
}

// replaceTarget resolves path (following a symlinked leaf) and returns the
// file to replace and the mode the new file gets: the existing file's mode,
// or newMode for a new file. An existing regular target with more than one
// name is refused. A target that has no name by the time its link count is
// read was replaced or removed after it was opened: path is resolved and
// opened again (retryReplaced), and ErrReplacedConcurrently is returned if
// it keeps changing.
func replaceTarget(path string, newMode fs.FileMode) (target string, mode fs.FileMode, err error) {
	err = retryReplaced(path, func() error {
		target, mode, err = replaceTargetOnce(path, newMode)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	return target, mode, nil
}

// replaceTargetOnce is one attempt of replaceTarget. It fails with
// errNoNames when the opened target has no name left.
func replaceTargetOnce(path string, newMode fs.FileMode) (string, fs.FileMode, error) {
	target := path
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", 0, err
		}
		target = resolved
	}
	mode := newMode
	if f, err := os.OpenFile(target, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err == nil {
		afterLinkCheckOpen(target)
		fi, serr := f.Stat()
		if serr == nil {
			serr = checkSingleLinkInfo(target, fi)
		}
		_ = f.Close()
		if serr != nil {
			return "", 0, serr
		}
		mode = fi.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", 0, linkErr(target, err)
	}
	return target, mode, nil
}

// checkSingleLinkInfo refuses a regular file with more than one name
// (the one-name rule). Other file types are left to the caller (a directory has
// several links by nature). A link count that cannot be read is refused.
// A count of zero means the file was unlinked after it was opened (a stale
// handle, not a hard link). It is refused with errNoNames, so the caller
// can open the path again (retryReplaced).
func checkSingleLinkInfo(name string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("refusing %s: cannot read its link count: %w", name, ErrHardLinked)
	}
	if st.Nlink == 0 {
		return fmt.Errorf("refusing %s: %w", name, errNoNames)
	}
	if st.Nlink != 1 {
		return fmt.Errorf("refusing %s: it has %d names: %w", name, st.Nlink, ErrHardLinked)
	}
	return nil
}
