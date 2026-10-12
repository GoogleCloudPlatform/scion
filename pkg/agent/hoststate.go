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
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/agentinfolock"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// HostStateFile is the name of the host-owned per-agent state file
// (ptone/scion#4767). It lives directly in the per-agent directory
// (agentDir), as a sibling of the agent home, never inside the home.
//
// Some runtimes do not let the host write files in the container, so state
// that only the host needs has to live in a file the host owns rather than
// in agent-info.json inside the agent home. This file is that place.
//
// Callers name the file by a trusted root and an agent name, never by a
// path: agentDir is <root>/.scion/agents/<name>, built here with the
// config agent-dir layout helpers. The root is the broker-side directory
// that holds the project's .scion directory: a linked project's directory,
// a hub-native project's directory under ~/.scion/projects, or the external
// project-configs directory of a shared-workspace project. It must come from
// broker configuration, never from anything stored under agentDir or in
// the project's files. Symlinks in the root's own path are resolved once
// (EvalSymlinks) and trusted; below the root, every component (.scion,
// agents, the agent name) is opened without following a symlink on unix.
//
// The file is host-owned in the sense that the agent home and workspace,
// which are mounted into the container, never contain it. That is not an
// absolute guarantee: agentDir can itself sit inside a read-write mount in
// some local setups (see the caveat on provisionedWorktreeStateFile in
// provision.go, e.g. an explicit --workspace pointed at the project root).
// Readers must therefore treat the content as data to validate, not as a
// trusted input. ReadHostState and UpdateHostState refuse a symlinked file
// and cap reads at 64 KiB. A host-state.json with more than one hard link is
// accepted: the write replaces the name, so another link to the old file is
// left as it was. Serialization of concurrent updates is best effort (see
// UpdateHostState).
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

// maxHostStateSize caps how much of HostStateFile is read. A larger file
// is an error, the same as a corrupt one.
const maxHostStateSize = 64 << 10

var (
	// errHostStateSymlink is returned when HostStateFile is a symlink.
	errHostStateSymlink = errors.New("host state file is a symlink")
	// errHostStateTooLarge is returned when HostStateFile is larger than
	// maxHostStateSize.
	errHostStateTooLarge = errors.New("host state file is too large")
	// errHostStateAgentName is returned for an agent name that is not a
	// single path element.
	errHostStateAgentName = errors.New("invalid agent name for host state")
)

// hostStateLayout is where HostStateFile lives for one agent: the trusted
// root with symlinks resolved, and the path components of agentDir below it.
type hostStateLayout struct {
	root string   // trusted root, absolute, symlinks resolved
	rel  []string // components of agentDir below root
}

// agentDir returns the agentDir path below the resolved root.
func (l hostStateLayout) agentDir() string {
	return filepath.Join(append([]string{l.root}, l.rel...)...)
}

// validateHostStateAgentName refuses an agent name that is empty, "." or
// "..", or contains a path separator or NUL.
func validateHostStateAgentName(agentName string) error {
	if agentName == "" || agentName == "." || agentName == ".." ||
		strings.ContainsAny(agentName, "/\\\x00") || strings.ContainsRune(agentName, filepath.Separator) {
		return fmt.Errorf("%w: %q", errHostStateAgentName, agentName)
	}
	return nil
}

// hostStateAgentRel returns agentDir's path relative to trustedRoot, as the
// config layout helpers place it (<root>/.scion/agents/<name>).
func hostStateAgentRel(trustedRoot, agentName string) (string, error) {
	if err := validateHostStateAgentName(agentName); err != nil {
		return "", err
	}
	agentDir := config.GetAgentDir(filepath.Join(trustedRoot, config.DotScion), agentName, false)
	rel, err := filepath.Rel(trustedRoot, agentDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("agent dir %s is not below %s", agentDir, trustedRoot)
	}
	return rel, nil
}

// HostStatePath returns the path of HostStateFile for agentName below
// trustedRoot, without resolving symlinks. It is for messages and tests;
// read and write the file only with ReadHostState and UpdateHostState.
func HostStatePath(trustedRoot, agentName string) (string, error) {
	rel, err := hostStateAgentRel(trustedRoot, agentName)
	if err != nil {
		return "", err
	}
	return filepath.Join(trustedRoot, rel, HostStateFile), nil
}

// resolveHostStateLayout checks agentName, resolves the symlinks in
// trustedRoot (which must be absolute and exist), and splits agentDir below
// it into components.
func resolveHostStateLayout(trustedRoot, agentName string) (hostStateLayout, error) {
	if !filepath.IsAbs(trustedRoot) {
		return hostStateLayout{}, fmt.Errorf("host state root %q is not absolute", trustedRoot)
	}
	rel, err := hostStateAgentRel(trustedRoot, agentName)
	if err != nil {
		return hostStateLayout{}, err
	}
	root, err := filepath.EvalSymlinks(trustedRoot)
	if err != nil {
		return hostStateLayout{}, fmt.Errorf("host state root: %w", err)
	}
	return hostStateLayout{root: root, rel: strings.Split(rel, string(filepath.Separator))}, nil
}

// ReadHostState reads HostStateFile for agentName below trustedRoot (see
// HostStateFile for the layout and what the root must be). On unix every
// component below the resolved root is opened without following a symlink,
// and so is the file; on other platforms the same is checked with Lstat,
// which is not atomic. Only a missing file, or a missing directory below
// the root, gives the zero HostState and a nil error. A bad agent name, a
// missing root, a symlink below the root, or a symlinked, non-regular,
// unreadable, corrupt or too large (over 64 KiB) file returns an error.
func ReadHostState(trustedRoot, agentName string) (HostState, error) {
	l, err := resolveHostStateLayout(trustedRoot, agentName)
	if err != nil {
		return HostState{}, err
	}
	d, err := openHostStateDir(l)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return HostState{}, nil
		}
		return HostState{}, err
	}
	defer d.close()
	return readHostStateIn(d)
}

// readHostStateIn reads and decodes HostStateFile in d. A missing file
// gives the zero HostState.
func readHostStateIn(d *hostStateDir) (HostState, error) {
	data, err := readHostStateBytes(d)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return HostState{}, nil
		}
		return HostState{}, err
	}
	var hs HostState
	if err := hs.decode(data); err != nil {
		return HostState{}, fmt.Errorf("host state file %s: %w", filepath.Join(d.path, HostStateFile), err)
	}
	return hs, nil
}

// readHostStateBytes returns the content of HostStateFile in d, which must
// be a regular file of at most maxHostStateSize bytes.
func readHostStateBytes(d *hostStateDir) ([]byte, error) {
	path := filepath.Join(d.path, HostStateFile)
	f, err := d.openFile()
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
	data, err := io.ReadAll(io.LimitReader(f, maxHostStateSize+1))
	if err != nil {
		return nil, fmt.Errorf("host state file %s: %w", path, err)
	}
	if len(data) > maxHostStateSize {
		return nil, fmt.Errorf("%w: %s", errHostStateTooLarge, path)
	}
	return data, nil
}

// UpdateHostState is the only writer of HostStateFile for agentName below
// trustedRoot (see HostStateFile for the layout and what the root must
// be). It runs a read-modify-write through agentinfolock.Update on the
// file: it reads the current state (as ReadHostState does), calls mutate,
// and writes the result through a temp file in agentDir and a rename, with
// mode 0600. agentDir must already exist.
//
// On unix, agentDir is opened once by walking each component below the
// resolved root without following a symlink, and the read, the temp create
// and the rename all go through that directory fd (openat, renameat). The
// lock file is opened from the resolved root path with NoFollowParent. A
// symlink below the root is refused and nothing is written. When mutate
// returns an error, nothing is written and the error is returned. A
// symlinked, unreadable, corrupt or too large file is refused and left as
// it is.
//
// The serialization is best effort, as agentinfolock documents: on unix
// the update runs unlocked when the lock file is unusable or another holder
// keeps the lock past the wait, and on other platforms there is no lock.
// Two concurrent updates can then lose one of their writes. Each write is
// still atomic (temp file and rename), so the file is never torn.
func UpdateHostState(trustedRoot, agentName string, mutate func(*HostState) error) error {
	l, err := resolveHostStateLayout(trustedRoot, agentName)
	if err != nil {
		return err
	}
	lockTarget := filepath.Join(l.agentDir(), HostStateFile)
	opts := agentinfolock.Options{NoFollowParent: hostStateNoFollowParent}
	return agentinfolock.Update(lockTarget, opts, func() error {
		d, err := openHostStateDir(l)
		if err != nil {
			return err
		}
		defer d.close()
		hs, err := readHostStateIn(d)
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
		return d.writeFile(data)
	})
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
