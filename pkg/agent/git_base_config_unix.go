//go:build unix

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

package agent

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// readBaseGitConfig reads up to maxPrivateGitConfigSize bytes of the base
// .git/config. A missing file is not an error (returns empty); a file larger
// than the cap is.
func readBaseGitConfig(path string) ([]byte, error) {
	// Open with O_NOFOLLOW so a symlink at the base .git/config path cannot
	// redirect the broker's read elsewhere, and confirm via fstat that the opened
	// file is a regular file (not a FIFO/device). A missing file is not an error.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only; a close error cannot affect the bytes already read
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("base git config %s is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPrivateGitConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPrivateGitConfigSize {
		return nil, fmt.Errorf("base git config %s exceeds %d bytes", path, maxPrivateGitConfigSize)
	}
	return raw, nil
}
