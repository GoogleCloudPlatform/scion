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

//go:build linux

package migrate

import "golang.org/x/sys/unix"

// renameat2 is unix.Renameat2; a test seam forces the NFS behaviour
// (every rename flag rejected with EINVAL).
var renameat2 = unix.Renameat2

// renameNoReplace is renameat2(RENAME_NOREPLACE): atomic, and EEXIST when
// the target exists. errNoAtomicRename where the filesystem rejects the
// flag (the Linux NFS client rejects every rename flag).
func renameNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	err := renameat2(oldDir, oldName, newDir, newName, unix.RENAME_NOREPLACE)
	if noAtomicRename(err) {
		return errNoAtomicRename
	}
	return err
}
