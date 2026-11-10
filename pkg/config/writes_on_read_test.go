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

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
)

// treeSnapshot lists every entry under root (symlinks not followed) with
// its mode, its link target or its content hash, and its mtime (for
// directories too), so a before/after comparison catches any created,
// removed, renamed or rewritten entry, a lock file or a backup included,
// and also a transient one (created and removed again: the directory's
// mtime moves).
func treeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %s", rel, info.Mode())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			line += " -> " + target
		case info.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			line += fmt.Sprintf(" %s %d", hex.EncodeToString(sum[:]), info.ModTime().UnixNano())
		case info.IsDir():
			line += fmt.Sprintf(" %d", info.ModTime().UnixNano())
		}
		out = append(out, line)
		return nil
	})
	require.NoError(t, err)
	sort.Strings(out)
	return out
}

func TestWritesOnReadDisabled_SettingsKeyMigration(t *testing.T) {
	cases := []struct {
		name, content, wantInMemory string
	}{
		// The legacy key alone: its value is used in memory.
		{"legacy only", legacyHubProjectIDYAMLForTest("legacy"), "legacy"},
		// Both keys (a conflict, which normally writes a backup): the
		// canonical key is kept and nothing is overridden.
		{"conflict", legacyHubProjectIDYAMLForTest("legacy") + "  project_id: \"other\"\n", ""},
	}
	for _, c := range cases {
		for _, disabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/disabled=%v", c.name, disabled), func(t *testing.T) {
				writesonread.IsolateForTest(t)
				dir := t.TempDir()
				path := filepath.Join(dir, "settings.yaml")
				require.NoError(t, os.WriteFile(path, []byte(c.content), 0o644))
				before := treeSnapshot(t, dir)
				if disabled {
					writesonread.Set()
				}

				r := &fakeReporter{}
				overrides := migrateLegacyHubProjectIDForTest(path, r)

				if !disabled {
					// Control: without the switch the file is rewritten.
					assert.Equal(t, 1, r.migrated+r.conflicts, "the key is migrated and reported")
					assert.NotEqual(t, before, treeSnapshot(t, dir), "the file is rewritten without the switch")
					return
				}
				assert.Equal(t, c.wantInMemory, overrides["project_id"], "the in-memory value")
				assert.Equal(t, before, treeSnapshot(t, dir),
					"with the no-write switch on, the settings directory must be unchanged (no rewrite, .settings.lock or backup)")
				assert.Zero(t, r.migrated+r.conflicts+r.skipped, "nothing is reported under the switch")
			})
		}
	}
}

func TestWritesOnReadDisabled_MarkerMigration(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("disabled=%v", disabled), func(t *testing.T) {
			writesonread.IsolateForTest(t)
			dir := t.TempDir()
			path := filepath.Join(dir, ".scion")
			require.NoError(t, os.WriteFile(path, []byte(legacyMarkerYAMLForTest("abc-123", "My Project", "my-project")), 0o644))
			before := treeSnapshot(t, dir)
			if disabled {
				writesonread.Set()
			}

			marker, err := ReadProjectMarker(path)
			require.NoError(t, err)

			assert.Equal(t, "abc-123", marker.ProjectID)
			assert.Equal(t, "my-project", marker.ProjectSlug)
			if !disabled {
				assert.NotEqual(t, before, treeSnapshot(t, dir), "control: without the switch the marker is migrated on disk")
				return
			}
			assert.Equal(t, before, treeSnapshot(t, dir),
				"with the no-write switch on, the marker's directory must be unchanged")
		})
	}
}

// legacyLayoutHome is a home with a legacy global layout: a real project
// directory and a leftover dangling back-link of an earlier migration.
func legacyLayoutHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	writeLegacyGlobalLayoutForTest(t, home)
	return home
}

func TestWritesOnReadDisabled_GlobalLayout(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("disabled=%v", disabled), func(t *testing.T) {
			writesonread.IsolateForTest(t)
			home := legacyLayoutHome(t)
			before := treeSnapshot(t, home)
			if disabled {
				writesonread.Set()
			}

			r := &globalLayoutRecorder{}
			MigrateLegacyGlobalLayout(home, r)

			if !disabled {
				assert.NotEqual(t, before, treeSnapshot(t, home), "control: without the switch the layout is migrated")
				assert.NotEmpty(t, r.migrated)
				return
			}
			assert.Equal(t, before, treeSnapshot(t, home),
				"with the no-write switch on, the legacy global layout must be left as it is")
			assert.Empty(t, r.migrated, "nothing is migrated under the switch")
			assert.Empty(t, r.conflicts, "nothing is reported under the switch")
			assert.Empty(t, r.skipped, "nothing is reported under the switch")
		})
	}
}

// TestWritesOnReadDisabled_LayoutOnceNotUsedUp checks that, under the
// switch, MigrateLegacyGlobalLayoutOnce does not use up its Once: a call
// after the switch is off again still migrates.
func TestWritesOnReadDisabled_LayoutOnceNotUsedUp(t *testing.T) {
	migrateLegacyGlobalLayoutOnce = sync.Once{}
	t.Cleanup(func() { migrateLegacyGlobalLayoutOnce = sync.Once{} })
	home := legacyLayoutHome(t)
	before := treeSnapshot(t, home)

	t.Run("switch on", func(t *testing.T) {
		writesonread.IsolateForTest(t)
		writesonread.Set()
		MigrateLegacyGlobalLayoutOnce(home, &globalLayoutRecorder{})
		assert.Equal(t, before, treeSnapshot(t, home), "nothing is migrated under the switch")
	})
	require.False(t, writesonread.Disabled())
	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayoutOnce(home, r)
	assert.NotEmpty(t, r.migrated, "the Once was used up while the switch was on")
}

func TestWritesOnReadDisabled_NoGitWorkspaceHeal(t *testing.T) {
	writesonread.IsolateForTest(t)
	f := newGitEvidenceFixture(t, "schema_version: \"1\"\n", 0)
	healedGitWorkspaces.Delete(f.inRepo)
	t.Cleanup(func() { healedGitWorkspaces.Delete(f.inRepo) })
	t.Chdir(f.repo)
	home := os.Getenv("HOME")
	before := treeSnapshot(t, home)
	writesonread.Set()

	root, ok := FindProjectRoot()
	require.True(t, ok)
	assert.Equal(t, f.inRepo, root)

	assert.Equal(t, before, treeSnapshot(t, home),
		"with the no-write switch on, the project config must not be healed")
	_, healed := healedGitWorkspaces.Load(f.inRepo)
	assert.False(t, healed, "the heal is skipped, not recorded as done")
}
