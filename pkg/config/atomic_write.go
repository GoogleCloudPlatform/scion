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
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// createTempFile is os.CreateTemp. It is a variable so tests can simulate a
// directory that refuses new files.
var createTempFile = os.CreateTemp

// errAtomicTempCreate marks a writeFileAtomic failure to create its
// temporary file: nothing has been written and the target is untouched.
var errAtomicTempCreate = errors.New("cannot create temporary file")

// writeFileAtomic writes data to targetPath through a temporary file in the
// same directory followed by a rename, so readers never see a partial file.
// It preserves the mode of an existing regular file and uses 0644 otherwise.
// The rename does not keep the owner: the new file belongs to the writing
// user (for example root, under sudo).
//
// It needs write permission on the directory. A temp-file creation failure
// wraps errAtomicTempCreate so callers can choose an in-place fallback.
//
// It does not serialise concurrent read-modify-write cycles: with two
// writers the last rename wins and the other update is lost (no torn file,
// though). That predates the atomic write and is tracked separately.
func writeFileAtomic(targetPath string, data []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Lstat(targetPath); err == nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(targetPath)
	tmp, err := createTempFile(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w: %w", targetPath, errAtomicTempCreate, err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		// Close is idempotent here; the error from a second Close is ignored.
		_ = tmp.Close()
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file for %s: %w", targetPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file for %s: %w", targetPath, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temp file for %s: %w", targetPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", targetPath, err)
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		return fmt.Errorf("replace %s: %w", targetPath, err)
	}
	renamed = true
	return syncDir(dir)
}

// syncDir fsyncs dir so a completed rename survives a crash. Directories
// that cannot be opened or do not support fsync are skipped: the new
// content is already in place.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, errors.ErrUnsupported) {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}
