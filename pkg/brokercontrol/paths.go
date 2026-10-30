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

// Package brokercontrol defines the Runtime Broker host's control paths
// (credentials, local settings, host identity and state, co-located Hub
// state) and the check that keeps every agent mount source clear of them.
//
// It is the single source of truth for that set. Two layers protect it:
//
//   - Named entries (namedEntries, plus process-registered extras for paths
//     that can be configured outside the Scion global directory). A mount
//     source may not equal one, be an ancestor of one, or (for a directory
//     entry) lie inside one.
//   - A floor over the Scion global directory itself (~/.scion): a source
//     that equals or is an ancestor of it is refused, and a source inside it
//     must be on an explicit allow list supplied by the caller. A control
//     file added to the global directory later is therefore protected
//     without being registered here; completeness_test.go additionally
//     requires every name joined onto the global directory in the tree to be
//     classified.
//
// Both sides are resolved through symlinks before comparison, and any path
// that cannot be resolved is refused (fail closed).
package brokercontrol

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Kind says whether an entry protects one file or a whole directory tree.
type Kind int

const (
	// File protects exactly one path: a source may not equal it or be an
	// ancestor of it.
	File Kind = iota
	// Dir protects a directory and everything below it: additionally, a
	// source may not lie inside it.
	Dir
)

// Class names what an entry holds. It is the only detail of a match that is
// shown to the requester of a refused mount.
type Class string

const (
	ClassCredentials   Class = "credentials"
	ClassLocalSettings Class = "local settings"
	ClassHostState     Class = "host identity and state"
	ClassHubState      Class = "Hub state"
	ClassHostPlugins   Class = "host plugins"
	// ClassGlobalDir is reported for the global-directory floor.
	ClassGlobalDir Class = "global directory"
)

// Entry is one protected path.
type Entry struct {
	Path  string
	Kind  Kind
	Class Class
}

// namedEntry is an entry relative to the Scion global directory.
type namedEntry struct {
	rel   string
	kind  Kind
	class Class
}

// namedEntries are the control paths that live in the Scion global
// directory. Every one is a direct child of it, so none can fall inside an
// allow-listed agent-data subtree (TestNamedEntriesDisjointFromAllowLists).
var namedEntries = []namedEntry{
	// Runtime Broker and Hub credentials.
	{brokercredentials.DefaultFileName, File, ClassCredentials},
	{brokercredentials.DefaultMultiDirName, Dir, ClassCredentials},
	{"dev-token", File, ClassCredentials},
	{"hub-profiles", Dir, ClassCredentials},
	{"transport-token", File, ClassCredentials},
	{"scion-token", File, ClassCredentials},
	// The host's local settings. settings.* also carries the host pin
	// (server.broker.managed_config.<block>: file). All spellings are
	// protected whether or not they exist.
	{"settings.yaml", File, ClassLocalSettings},
	{"settings.yml", File, ClassLocalSettings},
	{"settings.json", File, ClassLocalSettings},
	{".settings.lock", File, ClassLocalSettings},
	{"server.yaml", File, ClassLocalSettings},
	// Host identity and state.
	{brokeridentity.RuntimeBrokersDirName, Dir, ClassHostState},
	{"runtime-broker-state", Dir, ClassHostState},
	{config.BrokerWorkspacesDir, Dir, ClassHostState},
	{"hub-workspaces", Dir, ClassHostState},
	{"workspace-config-records", Dir, ClassHostState},
	{"server.pid", File, ClassHostState},
	// The global project's identity marker.
	{".scion", File, ClassHostState},
	// Executables the host runs.
	{"plugins", Dir, ClassHostPlugins},
	// The co-located Hub's default database and its SQLite side files.
	{"hub-id", File, ClassHubState},
	{"hub.db", File, ClassHubState},
	{"hub.db-wal", File, ClassHubState},
	{"hub.db-shm", File, ClassHubState},
	{"hub.db-journal", File, ClassHubState},
}

var (
	extraMu sync.RWMutex
	extras  = map[string][]Entry{}
)

// SetExtraPaths registers, under owner, control paths that are configured
// outside the Scion global directory (a broker state directory, a credentials
// file path, a Hub database path). A later call with the same owner replaces
// that owner's entries; nil clears them. A relative path is kept and makes
// every later Resolve fail, so a misregistration fails closed.
func SetExtraPaths(owner string, entries []Entry) {
	extraMu.Lock()
	defer extraMu.Unlock()
	if len(entries) == 0 {
		delete(extras, owner)
		return
	}
	extras[owner] = append([]Entry(nil), entries...)
}

func extraEntries() []Entry {
	extraMu.RLock()
	defer extraMu.RUnlock()
	owners := make([]string, 0, len(extras))
	for o := range extras {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	var out []Entry
	for _, o := range owners {
		out = append(out, extras[o]...)
	}
	return out
}

// Set is the resolved set of control paths for this host.
type Set struct {
	// GlobalDir is the resolved Scion global directory.
	GlobalDir string
	// Entries are the resolved named and extra entries.
	Entries []Entry
}

// errSetUnresolved is wrapped by every Resolve failure.
var errSetUnresolved = errors.New("the Runtime Broker control paths could not be resolved")

// globalDirFunc returns the unresolved Scion global directory. A variable so
// tests can point it elsewhere.
var globalDirFunc = config.GetGlobalDir

// Resolve returns the resolved set. It never returns a partial set: any
// failure (no home directory, a dangling symlink, a permission error, a
// relative extra) is an error, and callers refuse the mount.
func Resolve() (*Set, error) {
	g, err := globalDirFunc()
	if err != nil || g == "" || !filepath.IsAbs(g) {
		return nil, fmt.Errorf("%w: the global directory is unknown", errSetUnresolved)
	}
	rg, err := resolvePhysical(g)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSetUnresolved, err)
	}
	s := &Set{GlobalDir: rg}
	for _, n := range namedEntries {
		// Joined onto the resolved global directory: the entries are
		// plain names, so this is the same as resolving the full path
		// except where an entry is itself a symlink, which is resolved
		// below.
		p, err := resolvePhysical(filepath.Join(rg, n.rel))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errSetUnresolved, err)
		}
		s.Entries = append(s.Entries, Entry{Path: p, Kind: n.kind, Class: n.class})
	}
	for _, e := range extraEntries() {
		if e.Path == "" || !filepath.IsAbs(e.Path) {
			return nil, fmt.Errorf("%w: a registered control path is not absolute", errSetUnresolved)
		}
		p, err := resolvePhysical(e.Path)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errSetUnresolved, err)
		}
		s.Entries = append(s.Entries, Entry{Path: p, Kind: e.Kind, Class: e.Class})
	}
	return s, nil
}

// Fingerprint identifies the resolved set; it changes when any entry or the
// global directory resolves somewhere else (for example a re-pointed
// symlink), so a caller can re-check running agents.
func (s *Set) Fingerprint() string {
	parts := make([]string, 0, len(s.Entries)+1)
	parts = append(parts, "G="+s.GlobalDir)
	for _, e := range s.Entries {
		parts = append(parts, fmt.Sprintf("%d:%s", e.Kind, e.Path))
	}
	sort.Strings(parts[1:])
	return strings.Join(parts, "\n")
}
