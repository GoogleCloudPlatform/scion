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

package agent

import (
	"fmt"
	"os"
)

// tightenCacheDir sets the cache directory's mode to resolutionCacheDirMode
// if it grants more: MkdirAll leaves an existing directory as it is, and a
// new one is subject to the umask.
func tightenCacheDir(dir string) error {
	if err := tightenMode(dir, resolutionCacheDirMode); err != nil {
		return fmt.Errorf("cannot set mode of resolution cache directory: %w", err)
	}
	return nil
}

// tightenCacheFile sets the cache file's mode to resolutionCacheFileMode if
// it grants more. A missing file is not an error.
func tightenCacheFile(path string) error {
	if err := tightenMode(path, resolutionCacheFileMode); err != nil {
		return fmt.Errorf("cannot set mode of resolution cache file: %w", err)
	}
	return nil
}

// tightenMode sets path's permission bits to mode if it grants anything
// more than mode does. A missing path is not an error.
func tightenMode(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode().Perm()&^mode == 0 {
		return nil
	}
	return chmod(path, mode)
}

// chmod is os.Chmod, replaceable by tests to simulate a path the process
// cannot change.
var chmod = os.Chmod
