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
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to targetPath through a temporary file in the
// same directory followed by a rename, so readers never see a partial file.
// It preserves the mode of an existing regular file and uses 0644 otherwise.
func writeFileAtomic(targetPath string, data []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Lstat(targetPath); err == nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", targetPath, err)
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
	return nil
}
