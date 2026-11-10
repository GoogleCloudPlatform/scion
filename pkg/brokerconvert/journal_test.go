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

package brokerconvert

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func newTestJournal(t *testing.T) *Journal {
	t.Helper()
	j, err := NewJournal("digest", "legacy-id", testNow)
	require.NoError(t, err)
	return j
}

func TestJournal_SaveLoadRoundTrip(t *testing.T) {
	g := t.TempDir()
	got, err := LoadJournal(g)
	require.NoError(t, err)
	assert.Nil(t, got, "no journal yet")

	j := newTestJournal(t)
	j.Register = append(j.Register, RegisterItem{Key: "k8s", Name: "k8s", ItemStatus: ItemStatus{State: StepStarted}})
	require.NoError(t, SaveJournal(g, j, testNow))

	got, err = LoadJournal(g)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, j.RunID, got.RunID)
	assert.Equal(t, "digest", got.PlanDigest)
	assert.True(t, got.HasStarted())
	assert.Len(t, got.RunID, 32)
}

func TestJournal_OwnerOnlyModes(t *testing.T) {
	g := t.TempDir()
	require.NoError(t, SaveJournal(g, newTestJournal(t), testNow))
	name, err := WriteEvidenceCopy(g, "run1", []byte("a: 1\n"))
	require.NoError(t, err)

	for _, p := range []string{
		filepath.Join(ConvertDir(g), JournalFileName),
		filepath.Join(ConvertDir(g), name),
	} {
		fi, err := os.Lstat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), p)
	}
	fi, err := os.Lstat(ConvertDir(g))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	entries, err := os.ReadDir(ConvertDir(g))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp-", "no temporary file left behind")
	}
}

// Named mutant target: writing the journal through a link at its path.
func TestJournal_RefusesSymlinkAtJournalAndDir(t *testing.T) {
	t.Run("link at the journal path is replaced, not written through", func(t *testing.T) {
		g := t.TempDir()
		require.NoError(t, os.MkdirAll(ConvertDir(g), 0o700))
		victim := filepath.Join(t.TempDir(), "victim")
		require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
		require.NoError(t, os.Symlink(victim, filepath.Join(ConvertDir(g), JournalFileName)))

		require.NoError(t, SaveJournal(g, newTestJournal(t), testNow))

		data, err := os.ReadFile(victim)
		require.NoError(t, err)
		assert.Equal(t, "untouched", string(data), "the link target must not be written")
		fi, err := os.Lstat(filepath.Join(ConvertDir(g), JournalFileName))
		require.NoError(t, err)
		assert.True(t, fi.Mode().IsRegular(), "the link is replaced by the journal")
	})
	t.Run("link at the convert directory is refused", func(t *testing.T) {
		g := t.TempDir()
		elsewhere := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(g, "runtime-brokers"), 0o700))
		require.NoError(t, os.Symlink(elsewhere, ConvertDir(g)))

		require.Error(t, SaveJournal(g, newTestJournal(t), testNow))
		_, err := os.Lstat(filepath.Join(elsewhere, JournalFileName))
		assert.True(t, errors.Is(err, os.ErrNotExist), "nothing is written through the link")
	})
	t.Run("link at runtime-brokers is refused", func(t *testing.T) {
		g := t.TempDir()
		elsewhere := t.TempDir()
		require.NoError(t, os.Symlink(elsewhere, filepath.Join(g, "runtime-brokers")))
		require.Error(t, SaveJournal(g, newTestJournal(t), testNow))
	})
	t.Run("load refuses a link at the journal path", func(t *testing.T) {
		g := t.TempDir()
		require.NoError(t, os.MkdirAll(ConvertDir(g), 0o700))
		other := filepath.Join(t.TempDir(), "j.json")
		require.NoError(t, os.WriteFile(other, []byte(`{"schemaVersion":1}`), 0o600))
		require.NoError(t, os.Symlink(other, filepath.Join(ConvertDir(g), JournalFileName)))
		_, err := LoadJournal(g)
		assert.ErrorIs(t, err, ErrJournalUnreadable)
	})
}

func TestJournal_LoadRefusesNonPrivateAndInvalid(t *testing.T) {
	g := t.TempDir()
	require.NoError(t, os.MkdirAll(ConvertDir(g), 0o700))
	p := filepath.Join(ConvertDir(g), JournalFileName)

	require.NoError(t, os.WriteFile(p, []byte(`{"schemaVersion":1}`), 0o644))
	require.NoError(t, os.Chmod(p, 0o644))
	_, err := LoadJournal(g)
	assert.ErrorIs(t, err, ErrJournalUnreadable, "a group- or world-readable journal is refused")

	require.NoError(t, os.Chmod(p, 0o600))
	require.NoError(t, os.WriteFile(p, []byte(`not json`), 0o600))
	_, err = LoadJournal(g)
	assert.ErrorIs(t, err, ErrJournalUnreadable)

	require.NoError(t, os.WriteFile(p, []byte(`{"schemaVersion":2}`), 0o600))
	_, err = LoadJournal(g)
	assert.ErrorIs(t, err, ErrJournalUnreadable)
}

func TestWriteEvidenceCopy_KeepsFirstCopy(t *testing.T) {
	g := t.TempDir()
	name, err := WriteEvidenceCopy(g, "run1", []byte("first\n"))
	require.NoError(t, err)
	assert.Equal(t, "settings-before-run1.yaml", name)
	_, err = WriteEvidenceCopy(g, "run1", []byte("second\n"))
	require.NoError(t, err)
	data, err := ReadRunCopy(g, name)
	require.NoError(t, err)
	assert.Equal(t, "first\n", string(data))

	iname, err := WriteInstancesCopy(g, "run1", []byte("- key: a\n"))
	require.NoError(t, err)
	assert.Equal(t, "instances-written-run1.yaml", iname)
	_, err = ReadRunCopy(g, "../journal.json")
	assert.Error(t, err)
}

func TestJournal_HasStarted(t *testing.T) {
	var nilJ *Journal
	assert.False(t, nilJ.HasStarted())
	j := newTestJournal(t)
	assert.False(t, j.HasStarted())
	j.Projects = []ProjectItem{{ProjectID: "p", ItemStatus: ItemStatus{State: StepDone}}}
	assert.False(t, j.HasStarted())
	j.Rollback = &RollbackRecord{Moved: []MovedItem{{Key: "k", ItemStatus: ItemStatus{State: StepStarted}}}}
	assert.True(t, j.HasStarted())
}
