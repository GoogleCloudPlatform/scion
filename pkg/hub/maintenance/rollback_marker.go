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

package maintenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Rollback marker of the boot-failure guard (design D1, contract agreed
// with deploy.sh): <install_root>/rollback.marker, one line of JSON
// {"at":"<RFC 3339 UTC>","from":"<failed tag>","to":"<restored tag>"},
// written by the install owner with an atomic rename and rewritten on each
// rollback. The hub only reads it: it never deletes it and never follows it
// if it is a symlink.
const (
	RollbackMarkerName    = "rollback.marker"
	MaxRollbackMarkerSize = 4096
)

// RollbackMarker is the content of the rollback marker file.
type RollbackMarker struct {
	At   string `json:"at"`
	From string `json:"from"` // the release that failed to start
	To   string `json:"to"`   // the release the guard restored
}

// ReadRollbackMarker reads the rollback marker under installRoot. It
// returns nil, nil when there is none. A marker that is not a regular file
// (a symlink, a directory), larger than MaxRollbackMarkerSize, or not the
// agreed JSON is an error.
func ReadRollbackMarker(installRoot string) (*RollbackMarker, error) {
	if installRoot == "" {
		return nil, nil
	}
	path := filepath.Join(installRoot, RollbackMarkerName)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, before.Mode().Type())
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // removed or being replaced; the next boot reads it
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// The path may have been swapped for a symlink between the Lstat and
	// the Open: only read the file that was checked.
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%s changed while it was read", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxRollbackMarkerSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRollbackMarkerSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, MaxRollbackMarkerSize)
	}
	var m RollbackMarker
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if _, err := time.Parse(time.RFC3339, m.At); err != nil {
		return nil, fmt.Errorf("%s: invalid \"at\" %q", path, m.At)
	}
	if m.From == "" || m.To == "" {
		return nil, fmt.Errorf("%s: \"from\" and \"to\" are required", path)
	}
	return &m, nil
}
