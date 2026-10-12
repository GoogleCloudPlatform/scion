// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/agentinfolock"
)

// HostStateFile is the name of the host-owned per-agent state file
// (ptone/scion#4767). It lives directly in the per-agent directory
// (agentDir), as a sibling of the agent home, never inside the home.
//
// Some runtimes do not let the host write files in the container, so state
// that only the host needs has to live in a file the host owns rather than
// in agent-info.json inside the agent home. This file is that place.
//
// The file is host-owned in the sense that the agent home and workspace,
// which are mounted into the container, never contain it. That is not an
// absolute guarantee: agentDir can itself sit inside a read-write mount in
// some local setups (see the caveat on provisionedWorktreeStateFile in
// provision.go, e.g. an explicit --workspace pointed at the project root).
// Readers must therefore treat the content as data to validate, not as a
// trusted input, and ReadHostState refuses a symlinked file.
//
// UpdateHostState is the only writer.
const HostStateFile = "host-state.json"

// HostState is the content of HostStateFile. Top-level JSON keys that
// HostState has no field for are kept across UpdateHostState, so an older
// writer does not drop the keys of a newer one.
type HostState struct {
	// RunID is the agent's run ID.
	RunID string `json:"runId,omitempty"`

	// unknown holds the top-level keys read from the file that HostState
	// has no field for, with their raw values.
	unknown map[string]json.RawMessage
}

// HostStatePath returns the path of HostStateFile in agentDir.
func HostStatePath(agentDir string) string {
	return filepath.Join(agentDir, HostStateFile)
}

// errHostStateSymlink is returned when HostStateFile is a symlink.
var errHostStateSymlink = errors.New("host state file is a symlink")

// ReadHostState reads HostStateFile from agentDir without following a
// symlink at the file itself. Only a missing file gives the zero HostState
// and a nil error; a symlinked, non-regular, unreadable or corrupt file
// returns an error.
func ReadHostState(agentDir string) (HostState, error) {
	path := HostStatePath(agentDir)
	data, err := readHostStateBytes(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return HostState{}, nil
		}
		return HostState{}, err
	}
	var hs HostState
	if err := hs.decode(data); err != nil {
		return HostState{}, fmt.Errorf("host state file %s: %w", path, err)
	}
	return hs, nil
}

// readHostStateBytes returns the content of the regular file at path,
// refusing a symlink at path.
func readHostStateBytes(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("host state file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("host state file %s is not a regular file", path)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("host state file %s: %w", path, err)
	}
	return data, nil
}

// UpdateHostState is the only writer of HostStateFile. It runs a
// read-modify-write of the file in agentDir under the agentinfolock lock on
// the file: it reads the current state (as ReadHostState does), calls
// mutate, and writes the result through a temp file in agentDir and a
// rename, with mode 0600. When mutate returns an error, nothing is written
// and the error is returned. A symlinked, unreadable or corrupt file is
// refused and left as it is.
func UpdateHostState(agentDir string, mutate func(*HostState) error) error {
	path := HostStatePath(agentDir)
	return agentinfolock.Update(path, agentinfolock.Options{}, func() error {
		hs, err := ReadHostState(agentDir)
		if err != nil {
			return err
		}
		if err := mutate(&hs); err != nil {
			return err
		}
		data, err := hs.encode()
		if err != nil {
			return err
		}
		return writeHostStateFile(agentDir, path, data)
	})
}

// writeHostStateFile writes data to path through a temp file in agentDir
// and a rename. The rename replaces a symlink at path rather than writing
// through it, and UpdateHostState has already refused one.
func writeHostStateFile(agentDir, path string, data []byte) error {
	tmp, err := os.CreateTemp(agentDir, "."+HostStateFile+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		ok = true
		return err
	}
	ok = true
	return nil
}

var (
	hostStateKnownKeysOnce sync.Once
	hostStateKnownKeys     []string
)

// isKnownHostStateKey reports whether json.Unmarshal into HostState would
// read key into a field (field names match case-insensitively).
func isKnownHostStateKey(key string) bool {
	hostStateKnownKeysOnce.Do(func() {
		hostStateKnownKeys = jsonFieldNames(reflect.TypeOf(HostState{}))
	})
	for _, k := range hostStateKnownKeys {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// decode reads data, a JSON object, into hs, keeping unknown keys.
func (hs *HostState) decode(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("not a JSON object")
	}
	var known HostState
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	for k, v := range raw {
		if isKnownHostStateKey(k) {
			continue
		}
		if known.unknown == nil {
			known.unknown = make(map[string]json.RawMessage)
		}
		known.unknown[k] = v
	}
	*hs = known
	return nil
}

// encode returns the JSON for hs: its fields plus the unknown keys it was
// read with. A known field that is now empty is dropped (omitempty), even
// if the file had it.
func (hs *HostState) encode() ([]byte, error) {
	knownJSON, err := json.Marshal(hs)
	if err != nil {
		return nil, err
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(knownJSON, &out); err != nil {
		return nil, err
	}
	for k, v := range hs.unknown {
		if isKnownHostStateKey(k) {
			continue
		}
		out[k] = v
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
