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

package config

import (
	"errors"
	"os"
)

// createTempFile is os.CreateTemp. It is a variable so tests can simulate a
// directory that refuses new files.
var createTempFile = os.CreateTemp

// beforeAtomicRename, when set by a test, runs after the temporary file is
// written, synced and closed and before it is renamed over the target. An
// error from it fails the write as a failed rename would.
var beforeAtomicRename func(tmpName, targetPath string) error

// errAtomicTempCreate marks a writeFileAtomic failure to create its
// temporary file: nothing has been written and the target is untouched.
var errAtomicTempCreate = errors.New("cannot create temporary file")

// errAtomicNotDurable marks a writeFileAtomic failure after the rename: the
// new content is in place, but the directory fsync failed, so the rename
// may not survive a crash.
var errAtomicNotDurable = errors.New("file replaced but the change may not be durable")

// writeFileAtomic writes data to targetPath through a temporary file in the
// same directory followed by a rename, so readers never see a partial file.
// On unix it preserves the mode of an existing regular file and uses 0644
// otherwise. On Windows mode bits mean nothing (an existing file's Perm
// reads 0666), so the new file gets the directory's inherited ACL (the
// protection of mode 0644) unless the existing file passes the owner-only
// check on its real ACL, when it stays private (see atomicProtection in
// atomic_write_windows.go).
// The rename does not keep the owner: the new file belongs to the writing
// user (for example root, under sudo).
//
// It needs write permission on the directory. A temp-file creation failure
// wraps errAtomicTempCreate so callers can choose an in-place fallback. An
// error wrapping errAtomicNotDurable means the content was replaced but may
// not be durable; repeating the write is safe.
//
// It does not serialise concurrent read-modify-write cycles by itself:
// settings writers hold the settings lock (LockSettingsDir) from their read
// to this write.
func writeFileAtomic(targetPath string, data []byte) error {
	return writeFileAtomicMode(targetPath, data, preservedAtomicMode(targetPath))
}
