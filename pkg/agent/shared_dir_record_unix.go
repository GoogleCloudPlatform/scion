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

// readSharedDirRecord reads a per-agent shared-dir backend record of an
// earlier version from the broker-local agent directory (it is not
// mounted into the agent container). It opens with O_NOFOLLOW|O_NONBLOCK,
// requires a regular file and reads at most maxSharedDirRecordSize bytes.
// A missing record returns (nil, nil); a link, a non-regular file or an
// oversized record is an error.
func readSharedDirRecord(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("shared-dir storage record %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("shared-dir storage record %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("shared-dir storage record %s is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSharedDirRecordSize+1))
	if err != nil {
		return nil, fmt.Errorf("shared-dir storage record %s: %w", path, err)
	}
	if len(raw) > maxSharedDirRecordSize {
		return nil, fmt.Errorf("shared-dir storage record %s exceeds %d bytes", path, maxSharedDirRecordSize)
	}
	return raw, nil
}
