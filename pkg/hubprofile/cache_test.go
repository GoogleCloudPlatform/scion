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

package hubprofile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleEntry() *CacheEntry {
	return &CacheEntry{Alias: "owner", Run: "r1", Profile: "t1", Endpoint: "https://t1.example.com", IdentityID: "fx-1", Email: "e@scion-fixture.invalid", Kind: KindTestFixture, Role: "member", ExpiresAt: time.Now().Add(time.Hour).UTC(), TokenExpiresAt: time.Now().Add(30 * time.Minute).UTC(), Token: "FIXTURE-TOKEN-1"}
}

func TestCacheEntryCreatedExclusive0600(t *testing.T) {
	home := t.TempDir()
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	require.NoError(t, res.Fill(sampleEntry()))

	fi, err := os.Lstat(res.Path())
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	for _, d := range []string{filepath.Dir(res.Path()), filepath.Dir(filepath.Dir(res.Path()))} {
		di, err := os.Stat(d)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), di.Mode().Perm(), d)
	}
	assert.Equal(t, filepath.Join(home, ".scion", "hub-profiles", "t1", "run-r1", "owner.json"), res.Path())

	// O_EXCL: a second reservation of the same alias is refused.
	_, err = ReserveEntry(home, "t1", "r1", "owner")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEntryExists))

	got, err := ReadEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	assert.Equal(t, "FIXTURE-TOKEN-1", got.Token)
}

func TestCacheRefusesSymlinks(t *testing.T) {
	home := t.TempDir()
	dir, err := RunDir(home, "t1", "r1")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	target := filepath.Join(home, "target.json")

	// A dangling symlink at the alias path is refused (O_EXCL|O_NOFOLLOW).
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "owner.json")))
	_, err = ReserveEntry(home, "t1", "r1", "owner")
	require.Error(t, err)
	_, statErr := os.Lstat(target)
	assert.True(t, os.IsNotExist(statErr), "nothing was written through the symlink")

	// A symlink to a valid private file is refused on read.
	data := []byte(`{"alias":"owner","run":"r1","profile":"t1","identityId":"fx-1","token":"T"}`)
	require.NoError(t, os.WriteFile(target, data, 0o600))
	_, err = ReadEntry(home, "t1", "r1", "owner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	// A symlinked run directory is refused.
	home2 := t.TempDir()
	real := filepath.Join(home2, "real")
	require.NoError(t, os.MkdirAll(real, 0o700))
	pdir := filepath.Join(home2, ".scion", "hub-profiles", "t1")
	require.NoError(t, os.MkdirAll(pdir, 0o700))
	require.NoError(t, os.Symlink(real, filepath.Join(pdir, "run-r1")))
	_, err = ReserveEntry(home2, "t1", "r1", "owner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
	entries, _ := os.ReadDir(real)
	assert.Empty(t, entries)
}

func TestCacheReadRefusesOpenModes(t *testing.T) {
	home := t.TempDir()
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	require.NoError(t, res.Fill(sampleEntry()))
	require.NoError(t, os.Chmod(res.Path(), 0o644))
	_, err = ReadEntry(home, "t1", "r1", "owner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be 0600")
}

func TestCacheReplaceIsAtomicAndKeeps0600(t *testing.T) {
	home := t.TempDir()
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	e := sampleEntry()
	require.NoError(t, res.Fill(e))

	ne := *e
	ne.Token = "FIXTURE-TOKEN-2"
	require.NoError(t, ReplaceEntry(home, &ne))
	got, err := ReadEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	assert.Equal(t, "FIXTURE-TOKEN-2", got.Token)
	fi, _ := os.Lstat(res.Path())
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	entries, _ := os.ReadDir(filepath.Dir(res.Path()))
	assert.Len(t, entries, 1, "no temporary file is left behind")

	// Replacing an alias that is not cached is refused.
	other := *e
	other.Alias = "other"
	assert.Error(t, ReplaceEntry(home, &other))
}

func TestCacheReadChecksPathBinding(t *testing.T) {
	home := t.TempDir()
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	e := sampleEntry()
	e.Alias = "admin"
	require.NoError(t, res.Fill(e))
	_, err = ReadEntry(home, "t1", "r1", "owner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match its path")
}

func TestCacheNamesAreValidated(t *testing.T) {
	home := t.TempDir()
	for _, bad := range []string{"../x", "a/b", "", ".hidden", "UP"} {
		_, err := ReserveEntry(home, "t1", "r1", bad)
		assert.Error(t, err, "alias %q", bad)
		_, err = ReserveEntry(home, "t1", bad, "owner")
		assert.Error(t, err, "run %q", bad)
	}
}

func TestRunListingAndRemoval(t *testing.T) {
	home := t.TempDir()
	for _, a := range []string{"b", "a"} {
		res, err := ReserveEntry(home, "t1", "r1", a)
		require.NoError(t, err)
		e := sampleEntry()
		e.Alias = a
		require.NoError(t, res.Fill(e))
	}
	aliases, err := ListRun(home, "t1", "r1")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, aliases)
	runs, err := ListRuns(home, "t1")
	require.NoError(t, err)
	assert.Equal(t, []string{"r1"}, runs)

	require.Error(t, RemoveRunDir(home, "t1", "r1"), "a run with cached aliases is not removed")
	require.NoError(t, RemoveEntry(home, "t1", "r1", "a"))
	require.NoError(t, RemoveEntry(home, "t1", "r1", "b"))
	require.NoError(t, RemoveRunDir(home, "t1", "r1"))
	dir, _ := RunDir(home, "t1", "r1")
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}
