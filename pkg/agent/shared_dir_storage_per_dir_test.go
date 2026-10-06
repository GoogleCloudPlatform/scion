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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeRawSharedDirRecord(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, sharedDirStorageRecordFile), []byte(content), 0o644))
}

// A record written before per-dir backends existed has no dirs, so its
// backend applies to every shared dir, named or not.
func TestLoadSharedDirStorageRecord_OldRecordWithoutDirs(t *testing.T) {
	for _, backend := range []string{"local", "nfs"} {
		dir := t.TempDir()
		writeRawSharedDirRecord(t, dir, `{"backend":"`+backend+`"}`+"\n")
		rec, err := loadSharedDirStorageRecord(dir)
		require.NoError(t, err)
		require.NotNil(t, rec)
		assert.Equal(t, backend, rec.Backend)
		assert.Empty(t, rec.Dirs)
		for _, name := range []string{"notes", "gocache", "never-seen"} {
			assert.Equal(t, backend, rec.backendFor(name))
		}
		got, err := readSharedDirStorageRecord(dir)
		require.NoError(t, err)
		assert.Equal(t, backend, got)
	}
}

// A record with dirs gives a named dir its entry and any other dir,
// including one the project does not have yet, the record's backend.
func TestLoadSharedDirStorageRecord_WithDirs(t *testing.T) {
	dir := t.TempDir()
	writeRawSharedDirRecord(t, dir, `{"backend":"local","dirs":{"notes":"nfs"}}`)
	rec, err := loadSharedDirStorageRecord(dir)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "nfs", rec.backendFor("notes"))
	assert.Equal(t, "local", rec.backendFor("gocache"))
	assert.Equal(t, "local", rec.backendFor("unknown-dir"))
}

func TestLoadSharedDirStorageRecord_Missing(t *testing.T) {
	rec, err := loadSharedDirStorageRecord(t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, rec)
	rec, err = loadSharedDirStorageRecord("")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestLoadSharedDirStorageRecord_DamagedDirsAreErrors(t *testing.T) {
	for name, content := range map[string]string{
		"invalid dir name":  `{"backend":"local","dirs":{"../x":"nfs"}}`,
		"unknown backend":   `{"backend":"local","dirs":{"notes":"disk"}}`,
		"empty dir backend": `{"backend":"local","dirs":{"notes":""}}`,
		"no backend":        `{"dirs":{"notes":"nfs"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeRawSharedDirRecord(t, dir, content)
			_, err := loadSharedDirStorageRecord(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), sharedDirStorageRecordFile)
		})
	}
}

// A record for an agent with one backend for every dir is written exactly
// as before per-dir backends, with no dirs key.
func TestNewSharedDirStorageRecord_UniformHasNoDirs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, saveSharedDirStorageRecord(dir, newSharedDirStorageRecord(nil, nil)))
	data, err := os.ReadFile(filepath.Join(dir, sharedDirStorageRecordFile))
	require.NoError(t, err)
	assert.Equal(t, `{"backend":"local"}`+"\n", string(data))

	require.NoError(t, saveSharedDirStorageRecord(dir, newSharedDirStorageRecord(
		&config.V1SharedDirStorageConfig{Backend: "local"},
		map[string]*config.V1SharedDirStorageConfig{"notes": {Backend: "nfs"}})))
	data, err = os.ReadFile(filepath.Join(dir, sharedDirStorageRecordFile))
	require.NoError(t, err)
	assert.Equal(t, `{"backend":"local","dirs":{"notes":"nfs"}}`+"\n", string(data))
}

// perDirSettings returns global settings with a "p" profile on runtime
// "rt"; the nfs block is complete unless mountRoot is "".
func perDirSettings(mountRoot string, profile config.V1ProfileConfig, rt config.V1RuntimeConfig) *config.VersionedSettings {
	profile.Runtime = "rt"
	gs := &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{"p": profile},
		Runtimes: map[string]config.V1RuntimeConfig{"rt": rt},
	}
	if mountRoot != "" {
		cfg := nfsSharedDirStorageCfg(mountRoot)
		cfg.Backend = "local"
		gs.Server = &config.V1ServerConfig{SharedDirStorage: cfg}
	}
	return gs
}

func notesAndCache() []api.SharedDir {
	return []api.SharedDir{{Name: "notes"}, {Name: "gocache"}}
}

func TestSelectSharedDirBackends_NoPerDirSettingsIsNil(t *testing.T) {
	gs := perDirSettings("/mnt", config.V1ProfileConfig{SharedDirStorageBackend: "nfs"}, config.V1RuntimeConfig{})
	def, err := selectSharedDirStorage(gs, "p", "", "a1")
	require.NoError(t, err)
	got, err := selectSharedDirBackends(gs, "p", nil, def, notesAndCache(), "a1")
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = selectSharedDirBackends(nil, "p", nil, def, notesAndCache(), "a1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSelectSharedDirBackends_FirstStartUsesPerDirEntry(t *testing.T) {
	gs := perDirSettings("/mnt", config.V1ProfileConfig{SharedDirStorageBackends: map[string]string{"notes": "nfs", "not-in-project": "nfs"}}, config.V1RuntimeConfig{})
	def, err := selectSharedDirStorage(gs, "p", "", "a1")
	require.NoError(t, err)
	assert.Equal(t, "local", sharedDirStorageBackendName(def))
	got, err := selectSharedDirBackends(gs, "p", nil, def, notesAndCache(), "a1")
	require.NoError(t, err)
	require.Len(t, got, 1, "only notes differs from the default; an entry for a dir the project lacks is ignored")
	require.NotNil(t, got["notes"])
	assert.Equal(t, "nfs", got["notes"].Backend)
	require.NotNil(t, got["notes"].NFS, "an nfs dir carries the global nfs block")
	assert.Equal(t, "/mnt", got["notes"].NFS.MountRoot)
}

func TestSelectSharedDirBackends_LocalEntryAgainstNFSDefault(t *testing.T) {
	gs := perDirSettings("/mnt", config.V1ProfileConfig{SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"gocache": "local"}}, config.V1RuntimeConfig{})
	def, err := selectSharedDirStorage(gs, "p", "", "a1")
	require.NoError(t, err)
	got, err := selectSharedDirBackends(gs, "p", nil, def, notesAndCache(), "a1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "local", got["gocache"].Backend)
}

// The nearest level wins: a profile single value nfs overrides a runtime
// entry's gocache=local, so every dir is on nfs.
func TestSelectSharedDirBackends_ProfileSingleBeatsRuntimePerDir(t *testing.T) {
	gs := perDirSettings("/mnt", config.V1ProfileConfig{SharedDirStorageBackend: "nfs"},
		config.V1RuntimeConfig{SharedDirStorageBackends: map[string]string{"gocache": "local"}})
	def, err := selectSharedDirStorage(gs, "p", "", "a1")
	require.NoError(t, err)
	assert.Equal(t, "nfs", sharedDirStorageBackendName(def))
	got, err := selectSharedDirBackends(gs, "p", nil, def, notesAndCache(), "a1")
	require.NoError(t, err)
	assert.Nil(t, got, "gocache follows the profile single value")
}

func TestSelectSharedDirBackends_NFSEntryWithoutBlockNamesKey(t *testing.T) {
	gs := perDirSettings("", config.V1ProfileConfig{SharedDirStorageBackends: map[string]string{"notes": "nfs"}}, config.V1RuntimeConfig{})
	_, err := selectSharedDirBackends(gs, "p", nil, nil, notesAndCache(), "a1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "profiles.p.shared_dir_storage_backends.notes")
}

func TestSelectSharedDirBackends_InvalidEntryNamesKey(t *testing.T) {
	gs := perDirSettings("/mnt", config.V1ProfileConfig{}, config.V1RuntimeConfig{SharedDirStorageBackends: map[string]string{"notes": "disk"}})
	_, err := selectSharedDirBackends(gs, "p", nil, nil, notesAndCache(), "a1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtimes.rt.shared_dir_storage_backends.notes")
}

// An old record (no dirs) keeps every dir on the recorded backend, even
// when settings now name a dir with another backend.
func TestSelectSharedDirBackends_OldRecordKeepsEveryDir(t *testing.T) {
	for _, recorded := range []string{"local", "nfs"} {
		other := "nfs"
		if recorded == "nfs" {
			other = "local"
		}
		gs := perDirSettings("/mnt", config.V1ProfileConfig{SharedDirStorageBackends: map[string]string{"notes": other, "gocache": other}}, config.V1RuntimeConfig{})
		rec := &sharedDirStorageRecord{Backend: recorded}
		def, err := selectSharedDirStorage(gs, "p", recorded, "a1")
		require.NoError(t, err)
		got, err := selectSharedDirBackends(gs, "p", rec, def, notesAndCache(), "a1")
		require.NoError(t, err)
		assert.Nil(t, got, "recorded=%s: no dir moves", recorded)
	}
}

// A record with dirs keeps each named dir on its entry, gives a dir it
// does not name (added to the project later) the record's backend, and
// ignores an entry for a dir the project no longer has.
func TestSelectSharedDirBackends_RecordWithDirs(t *testing.T) {
	gs := perDirSettings("/mnt", config.V1ProfileConfig{}, config.V1RuntimeConfig{})
	rec := &sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs", "removed-dir": "nfs"}}
	def, err := selectSharedDirStorage(gs, "p", rec.Backend, "a1")
	require.NoError(t, err)
	dirs := append(notesAndCache(), api.SharedDir{Name: "added-later"})
	got, err := selectSharedDirBackends(gs, "p", rec, def, dirs, "a1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "nfs", got["notes"].Backend)
	_, ok := got["added-later"]
	assert.False(t, ok, "a dir added later uses the record's backend")
}

// A recorded per-dir nfs entry is kept even when settings now say local.
func TestSelectSharedDirBackends_RecordWinsOverPerDirSetting(t *testing.T) {
	gs := perDirSettings("/mnt", config.V1ProfileConfig{SharedDirStorageBackends: map[string]string{"notes": "local", "gocache": "nfs"}}, config.V1RuntimeConfig{})
	rec := &sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs"}}
	def, err := selectSharedDirStorage(gs, "p", rec.Backend, "a1")
	require.NoError(t, err)
	got, err := selectSharedDirBackends(gs, "p", rec, def, notesAndCache(), "a1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "nfs", got["notes"].Backend)
}

func TestSelectSharedDirBackends_RecordedNFSDirBlockGone(t *testing.T) {
	gs := perDirSettings("", config.V1ProfileConfig{}, config.V1RuntimeConfig{})
	rec := &sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs"}}
	_, err := selectSharedDirBackends(gs, "p", rec, &config.V1SharedDirStorageConfig{Backend: "local"}, notesAndCache(), "a1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `records the nfs shared-dir storage backend for shared dir "notes"`)
}

// Without overrides, resolveSharedDirsPerDir is resolveSharedDirs.
func TestResolveSharedDirsPerDir_NoOverridesMatchesResolveSharedDirs(t *testing.T) {
	projectDir := newTestProjectDir(t)
	dirs := notesAndCache()
	want, wantRes, err := resolveSharedDirs(nil, projectDir, "", "docker", dirs, "/workspace", false)
	require.NoError(t, err)
	got, gotRes, err := resolveSharedDirsPerDir(nil, nil, projectDir, "", "docker", dirs, "/workspace", false)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, wantRes, gotRes)
}

// Mixed backends on docker: the nfs dir is bind-mounted from the export,
// the local dir from the local layout, in the order of the project's dirs.
// Neither backend creates the other's leaf.
func TestResolveSharedDirsPerDir_MixedDocker(t *testing.T) {
	projectDir := newTestProjectDir(t)
	mountRoot := newResolvedTempDir(t)
	hostBase := filepath.Join(mountRoot, "scion-shared")
	require.NoError(t, os.MkdirAll(hostBase, 0o775))
	nfsCfg := nfsSharedDirStorageCfg(mountRoot)

	dirs := []api.SharedDir{{Name: "gocache"}, {Name: "notes", InWorkspace: true}, {Name: "scratch"}}
	overrides := map[string]*config.V1SharedDirStorageConfig{"notes": nfsCfg}
	vols, res, err := resolveSharedDirsPerDir(&config.V1SharedDirStorageConfig{Backend: "local"}, overrides, projectDir, "pid-1", "docker", dirs, "/workspace", false)
	require.NoError(t, err)
	require.Len(t, vols, 3)

	localBase, err := config.GetSharedDirsBasePath(projectDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(localBase, "gocache"), vols[0].Source)
	assert.Equal(t, "/scion-volumes/gocache", vols[0].Target)
	assert.Equal(t, filepath.Join(hostBase, "projects", "pid-1", "shared-dirs", "notes"), vols[1].Source)
	assert.Equal(t, "/workspace/.scion-volumes/notes", vols[1].Target)
	assert.Equal(t, filepath.Join(localBase, "scratch"), vols[2].Source)

	require.NotNil(t, res)
	assert.Equal(t, map[string]string{"notes": "projects/pid-1/shared-dirs/notes"}, res.SubPaths)
	assert.Equal(t, map[string]bool{"gocache": true, "scratch": true}, res.LocalDirs)
	assert.True(t, res.Serves("notes"))
	assert.False(t, res.Serves("gocache"))

	_, statErr := os.Stat(filepath.Join(localBase, "notes"))
	assert.True(t, os.IsNotExist(statErr), "no local leaf for the nfs dir")
	_, statErr = os.Stat(filepath.Join(hostBase, "projects", "pid-1", "shared-dirs", "gocache"))
	assert.True(t, os.IsNotExist(statErr), "no nfs leaf for a local dir")
}

// A local override against an nfs default: the realization serves the nfs
// dirs and names the local one.
func TestResolveSharedDirsPerDir_LocalOverrideOnNFSDefault(t *testing.T) {
	projectDir := newTestProjectDir(t)
	mountRoot := newResolvedTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "scion-shared"), 0o775))
	nfsCfg := nfsSharedDirStorageCfg(mountRoot)

	overrides := map[string]*config.V1SharedDirStorageConfig{"gocache": {Backend: "local"}}
	vols, res, err := resolveSharedDirsPerDir(nfsCfg, overrides, projectDir, "pid-1", "kubernetes", notesAndCache(), "/workspace", false)
	require.NoError(t, err)
	require.Len(t, vols, 2)
	require.NotNil(t, res)
	assert.Equal(t, "scion-shared", res.PVClaimName)
	assert.Equal(t, map[string]string{"notes": "projects/pid-1/shared-dirs/notes"}, res.SubPaths)
	assert.Equal(t, map[string]bool{"gocache": true}, res.LocalDirs)
}

// An nfs dir still fails closed (here: no project ID), even when the other
// dirs are local.
func TestResolveSharedDirsPerDir_NFSDirFailsClosed(t *testing.T) {
	projectDir := newTestProjectDir(t)
	overrides := map[string]*config.V1SharedDirStorageConfig{"notes": nfsSharedDirStorageCfg(t.TempDir())}
	_, _, err := resolveSharedDirsPerDir(nil, overrides, projectDir, "", "docker", notesAndCache(), "/workspace", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no hub project ID")
}

// An invalid default is still an error when every dir is overridden.
func TestResolveSharedDirsPerDir_InvalidDefaultFailsClosed(t *testing.T) {
	projectDir := newTestProjectDir(t)
	overrides := map[string]*config.V1SharedDirStorageConfig{"notes": {Backend: "local"}, "gocache": {Backend: "local"}}
	_, _, err := resolveSharedDirsPerDir(&config.V1SharedDirStorageConfig{Backend: "NFS"}, overrides, projectDir, "", "docker", notesAndCache(), "/workspace", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.shared_dir_storage")
}
