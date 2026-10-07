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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSharedDirBackendChanges(t *testing.T) {
	gs := perDirSettings("/mnt", config.V1ProfileConfig{}, config.V1RuntimeConfig{})
	noBlock := perDirSettings("", config.V1ProfileConfig{}, config.V1RuntimeConfig{})
	dirs := notesAndCache()
	tests := []struct {
		name       string
		changes    map[string]string
		allowEmpty bool
		gs         *config.VersionedSettings
		dirs       []api.SharedDir
		wantErr    string
	}{
		{name: "valid", changes: map[string]string{"notes": "nfs"}, gs: gs, dirs: dirs},
		{name: "nothing", gs: gs, dirs: dirs},
		{name: "allow empty alone", allowEmpty: true, gs: gs, dirs: dirs, wantErr: "--allow-empty-shared-dir needs a shared dir backend change"},
		{name: "unknown dir", changes: map[string]string{"other": "nfs"}, gs: gs, dirs: dirs, wantErr: `shared dir "other" is not one of the agent's shared dirs (notes, gocache)`},
		{name: "no dirs", changes: map[string]string{"notes": "nfs"}, gs: gs, wantErr: "it has none"},
		{name: "to local", changes: map[string]string{"notes": "local"}, gs: gs, dirs: dirs, wantErr: "only a change to the nfs backend is supported"},
		{name: "invalid name", changes: map[string]string{"../x": "nfs"}, gs: gs, dirs: dirs, wantErr: "invalid shared dir name"},
		{name: "no nfs block", changes: map[string]string{"notes": "nfs"}, gs: noBlock, dirs: dirs, wantErr: "needs a complete server.shared_dir_storage.nfs block"},
		{name: "no settings", changes: map[string]string{"notes": "nfs"}, dirs: dirs, wantErr: "needs a complete server.shared_dir_storage.nfs block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSharedDirBackendChanges(tt.changes, tt.allowEmpty, tt.dirs, tt.gs)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestChangeSharedDirBackends(t *testing.T) {
	notes := map[string]string{"notes": "nfs"}
	tests := []struct {
		name       string
		rec        sharedDirStorageRecord
		allowEmpty bool
		want       sharedDirStorageRecord
	}{
		{
			name: "old local record",
			rec:  sharedDirStorageRecord{Backend: "local"},
			want: sharedDirStorageRecord{Backend: "local", Dirs: notes, Previous: map[string]string{"notes": "local"}},
		},
		{
			name:       "old local record, empty allowed",
			rec:        sharedDirStorageRecord{Backend: "local"},
			allowEmpty: true,
			want:       sharedDirStorageRecord{Backend: "local", Dirs: notes},
		},
		{
			name: "already nfs by default",
			rec:  sharedDirStorageRecord{Backend: "nfs"},
			want: sharedDirStorageRecord{Backend: "nfs"},
		},
		{
			name: "already nfs by entry",
			rec:  sharedDirStorageRecord{Backend: "local", Dirs: notes},
			want: sharedDirStorageRecord{Backend: "local", Dirs: notes},
		},
		{
			name: "local entry on an nfs default",
			rec:  sharedDirStorageRecord{Backend: "nfs", Dirs: map[string]string{"notes": "local", "gocache": "local"}},
			want: sharedDirStorageRecord{Backend: "nfs", Dirs: map[string]string{"gocache": "local"}, Previous: map[string]string{"notes": "local"}},
		},
		{
			name:       "empty allowed clears a pending check",
			rec:        sharedDirStorageRecord{Backend: "local", Dirs: notes, Previous: map[string]string{"notes": "local", "other": "local"}},
			allowEmpty: true,
			want:       sharedDirStorageRecord{Backend: "local", Dirs: notes, Previous: map[string]string{"other": "local"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.rec
			before := sharedDirStorageRecord{Backend: in.Backend, Dirs: cloneMap(in.Dirs), Previous: cloneMap(in.Previous)}
			got := changeSharedDirBackends(&in, notes, tt.allowEmpty)
			assert.Equal(t, tt.want, *got)
			assert.Equal(t, before, in, "the input record is not modified")
		})
	}
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestLoadSharedDirStorageRecord_Previous(t *testing.T) {
	dir := t.TempDir()
	writeRawSharedDirRecord(t, dir, `{"backend":"local","dirs":{"notes":"nfs"},"previous":{"notes":"local"}}`)
	rec, err := loadSharedDirStorageRecord(dir)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"notes": "local"}, rec.Previous)

	for _, content := range []string{
		`{"backend":"local","previous":{"notes":"nfs"}}`,
		`{"backend":"local","previous":{"../x":"local"}}`,
	} {
		writeRawSharedDirRecord(t, dir, content)
		_, err := loadSharedDirStorageRecord(dir)
		assert.Error(t, err, content)
	}
}

// checkFixture lays out a project dir with a local notes leaf and an nfs
// notes leaf, and returns what checkChangedSharedDirs needs.
type checkFixture struct {
	projectDir string
	localLeaf  string
	nfsLeaf    string
	rec        *sharedDirStorageRecord
	dirs       []api.SharedDir
	res        *runtime.SharedDirRealization
	volumes    map[string]api.VolumeMount
}

func newCheckFixture(t *testing.T) checkFixture {
	t.Helper()
	projectDir := newTestProjectDir(t)
	localLeaf, err := config.GetSharedDirPath(projectDir, "notes")
	require.NoError(t, err)
	nfsLeaf := filepath.Join(t.TempDir(), "notes")
	require.NoError(t, os.MkdirAll(nfsLeaf, 0o775))
	return checkFixture{
		projectDir: projectDir,
		localLeaf:  localLeaf,
		nfsLeaf:    nfsLeaf,
		rec:        &sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs"}, Previous: map[string]string{"notes": "local"}},
		dirs:       notesAndCache(),
		res: &runtime.SharedDirRealization{Backend: "nfs", PVClaimName: "pv",
			SubPaths: map[string]string{"notes": "x"}, LocalDirs: map[string]bool{"gocache": true}},
		volumes: map[string]api.VolumeMount{
			"notes":   {Source: nfsLeaf, Target: "/scion-volumes/notes"},
			"gocache": {Source: "/local/gocache", Target: "/scion-volumes/gocache"},
		},
	}
}

func writeFileIn(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o644))
}

func (c checkFixture) check(runtimeName string) ([]string, error) {
	return checkChangedSharedDirs(c.rec, c.dirs, c.res, c.volumes, c.projectDir, runtimeName)
}

func TestCheckChangedSharedDirs_EmptyNFSWithLocalDataRefused(t *testing.T) {
	c := newCheckFixture(t)
	writeFileIn(t, c.localLeaf, "note.md")
	_, err := c.check("docker")
	require.Error(t, err)
	assert.Contains(t, err.Error(), c.nfsLeaf)
	assert.Contains(t, err.Error(), c.localLeaf)
	assert.Contains(t, err.Error(), "--shared-dir-backend notes=nfs --allow-empty-shared-dir")
	_, statErr := os.Stat(filepath.Join(c.localLeaf, "note.md"))
	assert.NoError(t, statErr, "the local directory is left in place")
}

func TestCheckChangedSharedDirs_Passes(t *testing.T) {
	t.Run("nfs has data", func(t *testing.T) {
		c := newCheckFixture(t)
		writeFileIn(t, c.localLeaf, "note.md")
		writeFileIn(t, c.nfsLeaf, "note.md")
		passed, err := c.check("docker")
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
	})
	t.Run("no local directory", func(t *testing.T) {
		c := newCheckFixture(t)
		passed, err := c.check("docker")
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
	})
	t.Run("empty local directory", func(t *testing.T) {
		c := newCheckFixture(t)
		require.NoError(t, os.MkdirAll(c.localLeaf, 0o755))
		passed, err := c.check("docker")
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
	})
	t.Run("nfs has data on kubernetes", func(t *testing.T) {
		c := newCheckFixture(t)
		writeFileIn(t, c.nfsLeaf, "note.md")
		passed, err := c.check("kubernetes")
		require.NoError(t, err)
		assert.Equal(t, []string{"notes"}, passed)
	})
}

// On Kubernetes the previous local storage is a volume the broker cannot
// read, so an empty nfs directory is refused even with no local data here.
func TestCheckChangedSharedDirs_KubernetesEmptyNFSRefused(t *testing.T) {
	c := newCheckFixture(t)
	_, err := c.check("kubernetes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Kubernetes volume that this broker cannot read")
	assert.Contains(t, err.Error(), "--allow-empty-shared-dir")
}

// A symlink at the local directory is not followed and counts as data.
func TestCheckChangedSharedDirs_LocalSymlinkCountsAsData(t *testing.T) {
	c := newCheckFixture(t)
	empty := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(c.localLeaf), 0o755))
	require.NoError(t, os.Symlink(empty, c.localLeaf))
	_, err := c.check("docker")
	require.Error(t, err)
}

func TestCheckChangedSharedDirs_Skips(t *testing.T) {
	t.Run("no previous entries", func(t *testing.T) {
		c := newCheckFixture(t)
		c.rec.Previous = nil
		passed, err := c.check("kubernetes")
		require.NoError(t, err)
		assert.Empty(t, passed)
	})
	t.Run("nil record", func(t *testing.T) {
		c := newCheckFixture(t)
		c.rec = nil
		passed, err := c.check("kubernetes")
		require.NoError(t, err)
		assert.Empty(t, passed)
	})
	t.Run("dir not mounted from nfs this start", func(t *testing.T) {
		c := newCheckFixture(t)
		c.res = nil
		passed, err := c.check("kubernetes")
		require.NoError(t, err)
		assert.Empty(t, passed, "the entry is kept for a later start")
	})
	t.Run("dir no longer in the project", func(t *testing.T) {
		c := newCheckFixture(t)
		c.dirs = []api.SharedDir{{Name: "gocache"}}
		passed, err := c.check("kubernetes")
		require.NoError(t, err)
		assert.Empty(t, passed)
	})
}

// End to end on docker: reincarnate with a change of notes to nfs, the
// start refuses while the nfs directory is empty, and after the operator
// copies the data it starts on nfs and clears the check. The local
// directory is never removed.
func TestSharedDirBackendChange_DockerEndToEnd(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, ""))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	opts := perDirStartOpts(f, "agent", "local")
	opts.Workspace = workspace
	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &first)).Start(context.Background(), opts)
	require.NoError(t, err)
	localNotes := volumeSource(first.cfg, "/scion-volumes/notes")
	require.NotEmpty(t, localNotes)
	writeFileIn(t, localNotes, "note.md")

	reprov := api.StartOptions{
		Name: "agent", ProjectPath: f.projectScionDir, Profile: "local", Workspace: workspace,
		SharedDirs:              opts.SharedDirs,
		SharedDirBackendChanges: map[string]string{"notes": "nfs"},
	}
	_, err = NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), reprov)
	require.NoError(t, err)
	rec := perDirRecord(t, f, "agent")
	assert.Equal(t, &sharedDirStorageRecord{Backend: "local", Dirs: map[string]string{"notes": "nfs"}, Previous: map[string]string{"notes": "local"}}, rec)

	var refused sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &refused)).Start(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--allow-empty-shared-dir")
	assert.Equal(t, 0, refused.ran)

	nfsNotes := filepath.Join(mountRoot, sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "notes")
	writeFileIn(t, nfsNotes, "note.md")
	var started sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &started)).Start(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, started.ran)
	assert.Contains(t, volumeSource(started.cfg, "/scion-volumes/notes"), filepath.Join("shared-dirs", "notes"))
	assert.Equal(t, localNotes[:len(localNotes)-len("notes")]+"gocache", volumeSource(started.cfg, "/scion-volumes/gocache"))
	assert.Nil(t, perDirRecord(t, f, "agent").Previous, "the check passed and is not repeated")

	_, statErr := os.Stat(filepath.Join(localNotes, "note.md"))
	assert.NoError(t, statErr, "the local directory is never removed")
}

// With AllowEmptySharedDir the agent starts on an empty nfs directory.
func TestSharedDirBackendChange_AllowEmpty(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, ""))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "gke")
	opts.Workspace = workspace
	_, err := NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Start(context.Background(), opts)
	require.NoError(t, err)

	reprov := api.StartOptions{
		Name: "agent", ProjectPath: f.projectScionDir, Profile: "gke", Workspace: workspace,
		SharedDirs:              opts.SharedDirs,
		SharedDirBackendChanges: map[string]string{"notes": "nfs"},
	}
	_, err = NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Reprovision(context.Background(), reprov)
	require.NoError(t, err)
	var refused sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &refused)).Start(context.Background(), opts)
	require.Error(t, err, "kubernetes refuses an empty nfs directory")
	assert.Contains(t, err.Error(), "Kubernetes volume")

	reprov.AllowEmptySharedDir = true
	_, err = NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Reprovision(context.Background(), reprov)
	require.NoError(t, err)
	assert.Nil(t, perDirRecord(t, f, "agent").Previous)
	var started sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &started)).Start(context.Background(), opts)
	require.NoError(t, err)
	require.NotNil(t, started.cfg.SharedDirStorage)
	assert.True(t, started.cfg.SharedDirStorage.Serves("notes"))
	assert.False(t, started.cfg.SharedDirStorage.Serves("gocache"))
}

// A refused change leaves the record as it was, before anything is
// provisioned.
func TestSharedDirBackendChange_RefusalsLeaveRecord(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, ""))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "local")
	opts.Workspace = workspace
	_, err := NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Start(context.Background(), opts)
	require.NoError(t, err)

	for name, change := range map[string]map[string]string{
		"unknown dir": {"other": "nfs"},
		"to local":    {"notes": "local"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), api.StartOptions{
				Name: "agent", ProjectPath: f.projectScionDir, Profile: "local", Workspace: workspace,
				SharedDirs: opts.SharedDirs, SharedDirBackendChanges: change,
			})
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrReprovisionRefused), "%v", err)
			assert.Equal(t, &sharedDirStorageRecord{Backend: "local"}, perDirRecord(t, f, "agent"))
		})
	}

	t.Run("no nfs block", func(t *testing.T) {
		f.writeRawGlobalSettings(t, `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`)
		_, err := NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), api.StartOptions{
			Name: "agent", ProjectPath: f.projectScionDir, Profile: "local", Workspace: workspace,
			SharedDirs: opts.SharedDirs, SharedDirBackendChanges: map[string]string{"notes": "nfs"},
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrReprovisionRefused), "%v", err)
		assert.Equal(t, &sharedDirStorageRecord{Backend: "local"}, perDirRecord(t, f, "agent"))
	})
}

// An agent without a record gets the record its next start would have
// written, with the change applied.
func TestSharedDirBackendChange_AgentWithoutRecord(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, "    shared_dir_storage_backends:\n      gocache: nfs\n"))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "local")
	opts.Workspace = workspace
	_, err := NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Start(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(config.ResolveAgentDir(f.projectScionDir, "agent"), sharedDirStorageRecordFile)))

	_, err = NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), api.StartOptions{
		Name: "agent", ProjectPath: f.projectScionDir, Profile: "local", Workspace: workspace,
		SharedDirs: opts.SharedDirs, SharedDirBackendChanges: map[string]string{"notes": "nfs"},
	})
	require.NoError(t, err)
	assert.Equal(t, &sharedDirStorageRecord{
		Backend:  "local",
		Dirs:     map[string]string{"notes": "nfs", "gocache": "nfs"},
		Previous: map[string]string{"notes": "local"},
	}, perDirRecord(t, f, "agent"))
}

// A passing start drops only the previous entries it checked: a dir with an
// entry that this start does not mount from nfs keeps it.
func TestSharedDirBackendChange_StartDropsOnlyPassedEntries(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, ""))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "local")
	opts.Workspace = workspace
	opts.SharedDirs = []api.SharedDir{{Name: "notes"}, {Name: "gocache"}, {Name: "later"}}
	_, err := NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Start(context.Background(), opts)
	require.NoError(t, err)

	agentDir := config.ResolveAgentDir(f.projectScionDir, "agent")
	writeRawSharedDirRecord(t, agentDir, `{"backend":"local","dirs":{"notes":"nfs"},"previous":{"notes":"local","later":"local"}}`)
	writeFileIn(t, filepath.Join(mountRoot, sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "notes"), "note.md")

	var started sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &started)).Start(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, started.ran)
	assert.Equal(t, map[string]string{"later": "local"}, perDirRecord(t, f, "agent").Previous)
}

// An agent without a record, reprovisioned without a profile, gets the
// record of the profile it was created under, not the active profile's.
func TestSharedDirBackendChange_AgentWithoutRecordUsesCreatedProfile(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, fmt.Sprintf(`schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
    shared_dir_storage_backends:
      gocache: nfs
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: %s
      shares:
        - id: %s
          pv_name: pv-1
`, mountRoot, sdsProfileShareID))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "gke")
	opts.Workspace = workspace
	_, err := NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Start(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(config.ResolveAgentDir(f.projectScionDir, "agent"), sharedDirStorageRecordFile)))

	_, err = NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Reprovision(context.Background(), api.StartOptions{
		Name: "agent", ProjectPath: f.projectScionDir, Workspace: workspace,
		SharedDirs: opts.SharedDirs, SharedDirBackendChanges: map[string]string{"notes": "nfs"},
	})
	require.NoError(t, err)
	assert.Equal(t, &sharedDirStorageRecord{
		Backend:  "local",
		Dirs:     map[string]string{"notes": "nfs", "gocache": "nfs"},
		Previous: map[string]string{"notes": "local"},
	}, perDirRecord(t, f, "agent"), "gocache follows the gke profile the agent was created under")
}

// A dir mounted inside the workspace is checked like any other.
func TestSharedDirBackendChange_InWorkspaceDir(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, ""))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	opts := perDirStartOpts(f, "agent", "local")
	opts.Workspace = workspace
	opts.SharedDirs = []api.SharedDir{{Name: "notes", InWorkspace: true}, {Name: "gocache"}}

	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &first)).Start(context.Background(), opts)
	require.NoError(t, err)
	var localNotes string
	for _, v := range first.cfg.Volumes {
		if filepath.Base(v.Target) == "notes" {
			localNotes = v.Source
			assert.Contains(t, v.Target, ".scion-volumes/notes")
		}
	}
	require.NotEmpty(t, localNotes)
	writeFileIn(t, localNotes, "note.md")

	_, err = NewManager(newSDSMockRuntime("docker", &sdsCapture{})).Reprovision(context.Background(), api.StartOptions{
		Name: "agent", ProjectPath: f.projectScionDir, Profile: "local", Workspace: workspace,
		SharedDirs: opts.SharedDirs, SharedDirBackendChanges: map[string]string{"notes": "nfs"},
	})
	require.NoError(t, err)

	var refused sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &refused)).Start(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--allow-empty-shared-dir")
	assert.Equal(t, 0, refused.ran)

	writeFileIn(t, filepath.Join(mountRoot, sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "notes"), "note.md")
	var started sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &started)).Start(context.Background(), opts)
	require.NoError(t, err)
	assert.Nil(t, perDirRecord(t, f, "agent").Previous)
}

// A dir with no volume in the map fails the check instead of passing it.
func TestCheckChangedSharedDirs_MissingVolumeFails(t *testing.T) {
	c := newCheckFixture(t)
	delete(c.volumes, "notes")
	_, err := c.check("docker")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot find its nfs directory")
}
