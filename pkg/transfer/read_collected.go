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

package transfer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ReadCollectedFile returns the content of fi, an entry returned by
// CollectFiles, read through root, which must be opened at the directory
// that was collected. Reading through os.Root refuses paths and links that
// leave that directory; links that stay inside it are resolved. The entry
// itself must still be a regular file (not a link), and the content read
// must match the recorded size and hash, so the bytes returned are exactly
// the ones described by the collected manifest entry. Any mismatch is an
// error and no content is returned.
func ReadCollectedFile(root *os.Root, fi FileInfo) ([]byte, error) {
	rel := filepath.FromSlash(fi.Path)

	// The entry itself must be a regular file. Checking before opening also
	// avoids blocking on a FIFO.
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", fi.Path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is no longer a regular file", fi.Path)
	}

	f, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", fi.Path, err)
	}
	defer func() { _ = f.Close() }()

	info, err = f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s: %w", fi.Path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is no longer a regular file", fi.Path)
	}
	if info.Size() != fi.Size {
		return nil, fmt.Errorf("%s changed since it was collected (size %d, collected %d)", fi.Path, info.Size(), fi.Size)
	}

	// Read at most one byte more than recorded to detect growth.
	data, err := io.ReadAll(io.LimitReader(f, fi.Size+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", fi.Path, err)
	}
	if int64(len(data)) != fi.Size {
		return nil, fmt.Errorf("%s changed since it was collected (read %d bytes, collected %d)", fi.Path, len(data), fi.Size)
	}
	if HashBytes(data) != fi.Hash {
		return nil, fmt.Errorf("%s changed since it was collected (content hash differs)", fi.Path)
	}
	return data, nil
}
