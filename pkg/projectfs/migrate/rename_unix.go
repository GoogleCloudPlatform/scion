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

package migrate

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// errNoAtomicRename: the filesystem has no atomic no-replace rename.
var errNoAtomicRename = errors.New("no atomic no-replace rename on this filesystem")

func noAtomicRename(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

// renameDirNoReplace renames a directory only if the target does not
// exist, atomically. Without an atomic no-replace rename it returns
// errNoAtomicRename and renames nothing: the caller copies instead, because
// a plain rename(2) would replace an empty directory created concurrently.
func renameDirNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	return renameNoReplace(oldDir, oldName, newDir, newName)
}

// linkat is unix.Linkat; a test seam.
var linkat = unix.Linkat

// renameFileNoReplace places a regular file at the target only if the
// target does not exist, atomically: renameat2(RENAME_NOREPLACE), or where
// that is unavailable (NFS) linkat, which fails with EEXIST on an existing
// target, then unlinkat of the source name. A failed unlink after a
// successful link leaves both names (same inode) and is returned as an
// error; a re-run finds the target identical and keeps the source aside.
func renameFileNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	err := renameNoReplace(oldDir, oldName, newDir, newName)
	if !errors.Is(err, errNoAtomicRename) {
		return err
	}
	if err := linkat(oldDir, oldName, newDir, newName, 0); err != nil {
		return err
	}
	if err := unix.Unlinkat(oldDir, oldName, 0); err != nil {
		return fmt.Errorf("file placed in target, but removing the source name failed: %w", err)
	}
	return nil
}

// renameBackup renames a source aside to its .migrated name, which the
// caller has checked is free. Only this migration creates .migrated names,
// so where no atomic no-replace rename exists a plain rename is used.
func renameBackup(dir int, name, backup string) error {
	err := renameNoReplace(dir, name, dir, backup)
	if errors.Is(err, errNoAtomicRename) {
		return unix.Renameat(dir, name, dir, backup)
	}
	return err
}
