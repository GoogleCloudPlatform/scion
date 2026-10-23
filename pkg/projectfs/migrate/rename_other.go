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

//go:build unix && !linux

package migrate

import "golang.org/x/sys/unix"

// renameNoReplace has no atomic no-replace rename to use where renameat2
// does not exist.
func renameNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	return errNoAtomicRename
}

// syncFS flushes what it can where syncfs(2) does not exist: the
// directory itself.
func syncFS(fd int) error {
	return unix.Fsync(fd)
}
