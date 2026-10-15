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

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
)

// purgeGuardFixture is a scion home with the three kinds of broker-local
// state present, under a temporary directory that is not HOME. HOME is a
// separate temporary directory (os.UserHomeDir reads it).
type purgeGuardFixture struct {
	home      string // HOME
	scionHome string
	stateRoot string
	entries   []brokerLocalStateEntry
}

func newPurgeGuardFixture(t *testing.T) purgeGuardFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionHome := filepath.Join(t.TempDir(), ".scion")
	stateRoot := filepath.Join(scionHome, "runtime-broker-state")
	require.NoError(t, os.MkdirAll(filepath.Join(stateRoot, "b-1"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(scionHome, "cache", "templates"), 0o755))
	require.NoError(t, os.WriteFile(daemon.GetLogPath(scionHome), []byte("log"), 0o644))
	return purgeGuardFixture{
		home: home, scionHome: scionHome, stateRoot: stateRoot,
		entries: brokerLocalStatePaths(scionHome, scionHome, stateRoot, "b-1"),
	}
}

// assertAllPresent fails unless every fixture entry still exists.
func (f purgeGuardFixture) assertAllPresent(t *testing.T) {
	t.Helper()
	for _, e := range f.entries {
		_, err := os.Lstat(e.Path)
		assert.NoError(t, err, "%s was removed although the purge was refused", e.Path)
	}
}

// TestPurgeBrokerLocalState_RemovesExpectedEntries: entries at their
// expected locations are all removed.
func TestPurgeBrokerLocalState_RemovesExpectedEntries(t *testing.T) {
	f := newPurgeGuardFixture(t)
	var out bytes.Buffer
	require.NoError(t, purgeBrokerLocalState(f.entries, &out))
	for _, e := range f.entries {
		assert.NoFileExists(t, e.Path)
		assert.NoDirExists(t, e.Path)
		assert.Contains(t, out.String(), "Removed "+e.Path)
	}
}

// TestPurgeBrokerLocalState_RefusesUnexpectedEntries: one entry that is not
// at the location its kind expects refuses the whole purge: nothing is
// removed, the entry is named with its reason, and an error is returned.
func TestPurgeBrokerLocalState_RefusesUnexpectedEntries(t *testing.T) {
	cases := map[string]struct {
		// mutate returns the entry list with one entry replaced.
		mutate func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int)
		reason string
	}{
		"daemon log is a directory": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			e := f.entries[0]
			require.NoError(t, os.Remove(e.Path))
			require.NoError(t, os.MkdirAll(e.Path, 0o755))
			return e, 0
		}, "is a directory"},
		"daemon log elsewhere": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			e := f.entries[0]
			e.Path = filepath.Join(f.scionHome, "other.log")
			return e, 0
		}, "not the broker daemon log"},
		"state root is the filesystem root": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			return brokerLocalStateEntry{Kind: stateEntryStateDir, Root: "/", Path: "/b-1", BrokerID: "b-1"}, 1
		}, "state root is the filesystem root"},
		"state root is the home directory": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			return brokerLocalStateEntry{Kind: stateEntryStateDir, Root: f.home, Path: filepath.Join(f.home, "b-1"), BrokerID: "b-1"}, 1
		}, "state root is the home directory"},
		"state root resolves to the home directory": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			link := filepath.Join(t.TempDir(), "state-link")
			require.NoError(t, os.Symlink(f.home, link))
			return brokerLocalStateEntry{Kind: stateEntryStateDir, Root: link, Path: filepath.Join(link, "b-1"), BrokerID: "b-1"}, 1
		}, "state root is the home directory"},
		"state directory name is not a broker ID": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			return brokerLocalStateEntry{Kind: stateEntryStateDir, Root: f.stateRoot, Path: filepath.Join(f.stateRoot, "a..b"), BrokerID: "a..b"}, 1
		}, "not a single path element"},
		"state directory is not root-broker-id": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			return brokerLocalStateEntry{Kind: stateEntryStateDir, Root: f.stateRoot, Path: filepath.Join(f.stateRoot, "other"), BrokerID: "b-1"}, 1
		}, "not <state root>/<broker-id>"},
		"state directory path is relative": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			return brokerLocalStateEntry{Kind: stateEntryStateDir, Root: "rel", Path: filepath.Join("rel", "b-1"), BrokerID: "b-1"}, 1
		}, "not an absolute, clean path"},
		"template cache root is the filesystem root": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			return brokerLocalStateEntry{Kind: stateEntryTemplateCache, Root: "/", Path: "/cache/templates"}, 2
		}, "scion home is the filesystem root"},
		"template cache root is the home directory": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			return brokerLocalStateEntry{Kind: stateEntryTemplateCache, Root: f.home, Path: filepath.Join(f.home, "cache", "templates")}, 2
		}, "scion home is the home directory"},
		"template cache is not cache-templates": {func(t *testing.T, f purgeGuardFixture) (brokerLocalStateEntry, int) {
			e := f.entries[2]
			e.Path = filepath.Join(f.scionHome, "cache", "other")
			return e, 2
		}, "not <scion home>/cache/templates"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPurgeGuardFixture(t)
			bad, i := tc.mutate(t, f)
			entries := append([]brokerLocalStateEntry(nil), f.entries...)
			entries[i] = bad
			var out bytes.Buffer
			err := purgeBrokerLocalState(entries, &out)
			require.Error(t, err, "purge ran with %s at %s", bad.Kind, bad.Path)
			assert.Contains(t, err.Error(), "nothing was removed")
			assert.Contains(t, out.String(), bad.Path, "refused entry not named")
			assert.Contains(t, out.String(), tc.reason, "refusal reason not given")
			f.assertAllPresent(t)
		})
	}
}
