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

//go:build !windows

package relay

import (
	"fmt"
	"io"
	"os"
)

// readPeerKeyFile reads a relay-peer key file, refusing one whose mode
// lets group or other access it.
func readPeerKeyFile(path string) ([]byte, error) {
	// The mode is checked on the open file and the key read from the same
	// descriptor, so both apply to one inode.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("conduit relay peer key %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("conduit relay peer key %s: %w", path, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("conduit relay peer key %s: mode %04o: group or other has access to it; chmod 600", path, perm)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("conduit relay peer key %s: %w", path, err)
	}
	return b, nil
}
