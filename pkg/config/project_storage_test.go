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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func psNFS(mountRoot, shareID, server, export, subPathRoot string) *V1NFSConfig {
	return &V1NFSConfig{
		MountRoot:   mountRoot,
		SubPathRoot: subPathRoot,
		Shares:      []V1NFSShare{{ID: shareID, Server: server, Export: export}},
	}
}

// TestSynthesizeProjectStorage pins design M1 and acceptance criterion A5: exactly one
// storage block validates, the deprecated blocks synthesize the one block
// with a warning, and a synthesis that would hide data on a shared export
// is refused.
func TestSynthesizeProjectStorage(t *testing.T) {
	nfsA := func() *V1NFSConfig { return psNFS("/mnt/nfs", "a", "10.0.0.2", "/share/a", "") }
	tests := []struct {
		name        string
		ps          *V1ProjectStorageConfig
		ws          *V1WorkspaceStorageConfig
		sd          *V1SharedDirStorageConfig
		hs          *V1HomeStorageConfig
		wantNil     bool
		wantBackend string
		wantHomes   string
		wantWarn    []string // substrings, each must appear in some warning
		wantNoWarn  bool
		wantSplit   bool // shared dirs kept on shared_dir_storage's export
		wantLocal   bool // shared dirs kept where they are (volume backends without shared_dir_storage)
		wantErr     string
	}{
		{name: "nothing configured", wantNil: true, wantNoWarn: true},
		{name: "project_storage nfs", ps: &V1ProjectStorageConfig{Backend: "nfs", Homes: "nfs", NFS: nfsA()}, wantBackend: "nfs", wantHomes: "nfs", wantNoWarn: true},
		{name: "project_storage local", ps: &V1ProjectStorageConfig{Backend: "local"}, wantBackend: "local", wantNoWarn: true},
		{name: "project_storage with workspace_storage", ps: &V1ProjectStorageConfig{Backend: "nfs", NFS: nfsA()}, ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, wantErr: "exactly one storage block"},
		{name: "project_storage with shared_dir_storage", ps: &V1ProjectStorageConfig{}, sd: &V1SharedDirStorageConfig{Backend: "local"}, wantErr: "remove server.shared_dir_storage"},
		{name: "project_storage with home_storage", ps: &V1ProjectStorageConfig{}, hs: &V1HomeStorageConfig{StopGraceSeconds: 5}, wantErr: "remove server.home_storage"},
		{name: "project_storage nfs without shares", ps: &V1ProjectStorageConfig{Backend: "nfs"}, wantErr: "server.project_storage.backend is \"nfs\" but no NFS shares"},
		{name: "project_storage unknown backend", ps: &V1ProjectStorageConfig{Backend: "s3"}, wantErr: "server.project_storage.backend \"s3\" is not supported"},
		{name: "project_storage homes nfs on local", ps: &V1ProjectStorageConfig{Homes: "nfs"}, wantErr: "server.project_storage.homes is \"nfs\" but server.project_storage.backend is \"local\""},
		{name: "project_storage unknown homes", ps: &V1ProjectStorageConfig{Homes: "s3"}, wantErr: "server.project_storage.homes must be"},
		{name: "project_storage negative homes option", ps: &V1ProjectStorageConfig{HomesOptions: &V1HomesOptionsConfig{StopGraceSeconds: -1}}, wantErr: "server.project_storage.homes_options.stop_grace_seconds"},
		{name: "project_storage bad uid", ps: &V1ProjectStorageConfig{Backend: "nfs", NFS: func() *V1NFSConfig { n := nfsA(); n.UID = -2; return n }()}, wantErr: "server.project_storage.nfs.uid"},
		{name: "project_storage gke without volume", ps: &V1ProjectStorageConfig{Backend: "gke-shared-volume"}, wantErr: "server.project_storage.gke_shared_volume.volume_name"},

		{name: "workspace_storage nfs synthesizes", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, wantBackend: "nfs", wantHomes: "", wantWarn: []string{"legacy storage blocks in use (server.workspace_storage)", "write server.project_storage instead", "project_storage:\n        backend: nfs", "shared dirs now follow the workspace onto the nfs export"}},
		{name: "workspace_storage cloudrun-volume synthesizes, shared dirs stay", ws: &V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: "v"}}, wantBackend: "cloudrun-volume", wantLocal: true, wantWarn: []string{"legacy storage blocks in use", "shared dirs stay where they are today", "NOT moved automatically"}},
		{name: "workspace_storage gke-shared-volume synthesizes, shared dirs stay", ws: &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "v", PVClaimName: "c"}}, wantBackend: "gke-shared-volume", wantLocal: true, wantWarn: []string{"per-dir claims on Kubernetes"}},
		{name: "gke-shared-volume with explicit local shared_dir_storage keeps shared dirs", ws: &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "v", PVClaimName: "c"}}, sd: &V1SharedDirStorageConfig{Backend: "local"}, wantBackend: "gke-shared-volume", wantLocal: true, wantWarn: []string{"shared dirs stay where they are today", "NOT moved automatically"}},
		{name: "cloudrun-volume with empty shared_dir_storage backend keeps shared dirs", ws: &V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: "v"}}, sd: &V1SharedDirStorageConfig{}, wantBackend: "cloudrun-volume", wantLocal: true},
		{name: "project_storage gke-shared-volume puts shared dirs on the volume", ps: &V1ProjectStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "v"}}, wantBackend: "gke-shared-volume", wantNoWarn: true},
		{name: "workspace_storage invalid", ws: &V1WorkspaceStorageConfig{Backend: "nfs"}, wantErr: "server.workspace_storage: workspace_storage.backend is \"nfs\" but no NFS shares"},
		{name: "home_storage nfs with workspace nfs", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfsA()}, hs: &V1HomeStorageConfig{Backend: "nfs", Leaf: "broker", StopGraceSeconds: 40}, wantBackend: "nfs", wantHomes: "nfs", wantWarn: []string{"legacy storage blocks in use (server.workspace_storage, server.shared_dir_storage, server.home_storage)"}},
		{name: "home_storage nfs without any nfs share", hs: &V1HomeStorageConfig{Backend: "nfs"}, wantErr: "no nfs share is configured"},
		{name: "home_storage local options only", hs: &V1HomeStorageConfig{SkeletonMaxBytes: 1 << 20}, wantBackend: "local", wantHomes: "", wantWarn: []string{"legacy storage blocks in use (server.home_storage)"}},
		{name: "home_storage invalid", hs: &V1HomeStorageConfig{Leaf: "x"}, wantErr: "server.home_storage.leaf"},
		{name: "shared_dir_storage nfs, workspace local: split", ws: &V1WorkspaceStorageConfig{Backend: "local"}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfsA()}, wantBackend: "local", wantSplit: true, wantWarn: []string{"shared dirs stay on the shared_dir_storage export", "project-storage-migrate maintenance migration"}},
		{name: "shared_dir_storage nfs alone (T3 hybrid): split", sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfsA()}, wantBackend: "local", wantSplit: true, wantWarn: []string{"shared dirs stay on the shared_dir_storage export"}},
		{name: "shared_dir_storage nfs with nfs homes: split keeps homes on the share", sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfsA()}, hs: &V1HomeStorageConfig{Backend: "nfs"}, wantBackend: "local", wantHomes: "nfs", wantSplit: true},
		{name: "shared_dir_storage nfs, workspace cloudrun-volume: split", ws: &V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: "v"}}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfsA()}, wantBackend: "cloudrun-volume", wantSplit: true},
		{name: "both nfs, other share", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: psNFS("/mnt/nfs", "b", "10.0.0.3", "/share/b", "")}, wantErr: "both nfs but name different trees"},
		{name: "both nfs, other subpath_root", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: psNFS("/mnt/nfs", "a", "10.0.0.2", "/share/a", "team")}, wantErr: "both nfs but name different trees"},
		{name: "both nfs, same tree, other share id and mount_root", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: psNFS("/srv/other", "sd-id", "10.0.0.2", "/share/a/", "projects")}, wantBackend: "nfs", wantWarn: []string{"legacy storage blocks in use", "mount it differently on this host", `"/mnt/nfs/a"`}},
		{name: "both nfs, same tree, workspace mount_root empty", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: psNFS("", "a", "10.0.0.2", "/share/a", "")}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: psNFS("/srv/sd", "a", "10.0.0.2", "/share/a", "")}, wantErr: "server.workspace_storage.nfs.mount_root: must be set to an absolute path"},
		{name: "both nfs, other export", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: psNFS("/mnt/nfs", "a", "10.0.0.2", "/share/b", "")}, wantErr: "both nfs but name different trees"},
		{name: "both nfs, same tree", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: psNFS("/mnt/nfs", "a", "10.0.0.2", "/share/a", "projects")}, wantBackend: "nfs", wantWarn: []string{"legacy storage blocks in use"}},
		{name: "shared_dir_storage local, workspace nfs", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, sd: &V1SharedDirStorageConfig{Backend: "local"}, wantBackend: "nfs", wantWarn: []string{"shared dirs are now placed on the nfs export"}},
		{name: "shared_dir_storage invalid", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfsA()}, sd: &V1SharedDirStorageConfig{Backend: "NFS"}, wantErr: "server.shared_dir_storage.backend must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings, err := SynthesizeProjectStorage(tt.ps, tt.ws, tt.sd, tt.hs)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantBackend, got.EffectiveBackend())
				assert.Equal(t, tt.wantHomes, got.Homes)
				assert.Equal(t, tt.wantSplit, got.LegacySharedDirsNFS != nil, "split")
				assert.Equal(t, tt.wantLocal, got.LegacySharedDirsLocal, "local split")
			}
			if tt.wantNoWarn {
				assert.Empty(t, warnings)
			}
			for _, w := range tt.wantWarn {
				found := false
				for _, have := range warnings {
					if strings.Contains(have, w) {
						found = true
					}
				}
				assert.True(t, found, "no warning contains %q in %q", w, warnings)
			}
		})
	}
}

// TestSynthesizeProjectStorage_CarriesHomeOptions: home_storage's
// operational knobs keep their meaning under homes_options, and the inputs
// are never modified.
func TestSynthesizeProjectStorage_CarriesHomeOptions(t *testing.T) {
	ws := &V1WorkspaceStorageConfig{Backend: "nfs", NFS: psNFS("/mnt/nfs", "a", "s", "/e", "")}
	hs := &V1HomeStorageConfig{Backend: "nfs", Leaf: "broker", StopGraceSeconds: 40, TerminationWaitSeconds: 9, SkeletonMaxBytes: 7, AllowIncompletePhases: true}
	got, _, err := SynthesizeProjectStorage(nil, ws, &V1SharedDirStorageConfig{Backend: "nfs", NFS: ws.NFS}, hs)
	require.NoError(t, err)
	assert.Equal(t, &V1HomesOptionsConfig{Leaf: "broker", StopGraceSeconds: 40, TerminationWaitSeconds: 9, SkeletonMaxBytes: 7, AllowIncompletePhases: true}, got.HomesOptions)
	assert.Equal(t, &V1HomeStorageConfig{Backend: "nfs", Leaf: "broker", StopGraceSeconds: 40, TerminationWaitSeconds: 9, SkeletonMaxBytes: 7, AllowIncompletePhases: true}, got.HomeStorage())
	assert.Equal(t, "", ws.NFS.SubPathRoot, "input block modified")
	assert.Equal(t, "projects", got.NFS.SubPathRoot)
	got.NFS.Shares[0].ID = "changed"
	assert.Equal(t, "a", ws.NFS.Shares[0].ID, "result shares memory with the input")
}

func TestProjectStorage_Views(t *testing.T) {
	var nilPS *V1ProjectStorageConfig
	assert.Nil(t, nilPS.WorkspaceStorage())
	assert.Nil(t, nilPS.HomeStorage())
	assert.Equal(t, "local", nilPS.EffectiveBackend())
	ps := &V1ProjectStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "v"}}
	assert.Equal(t, &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: ps.GKESharedVolume}, ps.WorkspaceStorage())
	assert.Nil(t, ps.HomeStorage())
}

func TestProjectStorageLayout(t *testing.T) {
	t.Run("nil is local", func(t *testing.T) {
		l, err := ProjectStorageLayout(nil, "")
		require.NoError(t, err)
		assert.Equal(t, projectfs.Layout{Backend: projectfs.BackendLocal, Homes: projectfs.BackendLocal}, l)
	})
	t.Run("nfs", func(t *testing.T) {
		ps := &V1ProjectStorageConfig{Backend: "nfs", Homes: "nfs", NFS: psNFS("/mnt/nfs", "a", "10.0.0.2", "/share/a", "")}
		ps.ApplyDefaults()
		l, err := ProjectStorageLayout(ps, "")
		require.NoError(t, err)
		assert.Equal(t, projectfs.BackendNFS, l.Backend)
		assert.Equal(t, "/mnt/nfs/a", l.HostBase)
		assert.Equal(t, "projects", l.SubPathRoot)
		assert.Equal(t, projectfs.BackendNFS, l.Homes)
		assert.Equal(t, 1000, l.UID)
	})
	t.Run("legacy split keeps shared dirs and homes on the shared_dir_storage export", func(t *testing.T) {
		ps, _, err := SynthesizeProjectStorage(nil, nil,
			&V1SharedDirStorageConfig{Backend: "nfs", NFS: psNFS("/mnt/sd", "s", "10.0.0.9", "/sd", "")},
			&V1HomeStorageConfig{Backend: "nfs"})
		require.NoError(t, err)
		l, err := ProjectStorageLayout(ps, "")
		require.NoError(t, err)
		assert.Equal(t, projectfs.BackendLocal, l.Backend, "workspaces stay local")
		require.NotNil(t, l.SharedDirs)
		assert.Equal(t, projectfs.BackendNFS, l.SharedDirLayout().Backend)
		assert.Equal(t, "/mnt/sd/s", l.SharedDirLayout().HostBase)
		assert.Equal(t, projectfs.BackendNFS, l.Homes)
		r, err := l.Resolve(projectfs.Project{ID: "0123abcd-0000-0000-0000-000000000000"}, "", projectfs.SharedDirMount(api.SharedDir{Name: "scratchpad"}, ""))
		require.NoError(t, err)
		assert.Equal(t, "/mnt/sd/s/projects/0123abcd-0000-0000-0000-000000000000/shared-dirs/scratchpad", r.HostPath)
		assert.Contains(t, SuggestedProjectStorageYAML(ps), "backend: nfs")
		assert.Contains(t, SuggestedProjectStorageYAML(ps), "mount_root: /mnt/sd")
		assert.NotContains(t, SuggestedProjectStorageYAML(ps), "Legacy")
	})
	t.Run("legacy volume split keeps shared dirs local", func(t *testing.T) {
		ps, _, err := SynthesizeProjectStorage(nil, &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "v", PVClaimName: "c"}}, nil, nil)
		require.NoError(t, err)
		l, err := ProjectStorageLayout(ps, "/base")
		require.NoError(t, err)
		assert.Equal(t, projectfs.BackendGKESharedVolume, l.Backend, "workspaces on the volume")
		assert.Equal(t, projectfs.BackendLocal, l.SharedDirLayout().Backend, "shared dirs stay where they are")
		assert.False(t, l.SharedDirLayout().Backend.Durable())
		assert.NotContains(t, SuggestedProjectStorageYAML(ps), "Legacy")
	})
	t.Run("cloudrun-volume", func(t *testing.T) {
		ps := &V1ProjectStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: "ws"}}
		l, err := ProjectStorageLayout(ps, "/base")
		require.NoError(t, err)
		assert.Equal(t, projectfs.Layout{Backend: projectfs.BackendCloudRunVolume, HostBase: "/base/ws", SubPathRoot: "projects", Share: projectfs.Share{ID: "ws"}, Homes: projectfs.BackendLocal}, l)
		r, err := l.Resolve(projectfs.Project{ID: "0123abcd-0000-0000-0000-000000000000"}, "", projectfs.SharedDirMount(api.SharedDir{Name: "scratchpad"}, ""))
		require.NoError(t, err)
		assert.Equal(t, "/base/ws/projects/0123abcd-0000-0000-0000-000000000000/shared-dirs/scratchpad", r.HostPath)
		assert.True(t, r.Backend.Durable())
	})
	t.Run("gke-shared-volume default base", func(t *testing.T) {
		ps := &V1ProjectStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "ws", PVClaimName: "claim", SubPathRoot: "team/p"}}
		l, err := ProjectStorageLayout(ps, "")
		require.NoError(t, err)
		assert.Equal(t, projectfs.Layout{Backend: projectfs.BackendGKESharedVolume, HostBase: "/mnt/ws", SubPathRoot: "team/p", Share: projectfs.Share{ID: "ws", PVName: "claim"}, Homes: projectfs.BackendLocal}, l)
	})
	for name, ps := range map[string]*V1ProjectStorageConfig{
		"volume name with slash":  {Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: "../x"}},
		"volume name dotdot":      {Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: ".."}},
		"absolute subpath_root":   {Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "v", SubPathRoot: "/p"}},
		"missing volume block":    {Backend: "cloudrun-volume"},
		"nfs without shares":      {Backend: "nfs", NFS: &V1NFSConfig{MountRoot: "/mnt"}},
		"unknown backend":         {Backend: "s3"},
		"nfs with escaping root":  {Backend: "nfs", NFS: psNFS("/mnt", "a", "s", "/e", "../x")},
		"cloudrun without volume": {Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ProjectStorageLayout(ps, "")
			assert.Error(t, err)
		})
	}
}

// TestRemovedStorageOverrideErrors pins the second half of AC A5: the
// per-runtime, per-profile and per-instance overrides are rejected with a
// message naming volumes:.
func TestRemovedStorageOverrideErrors(t *testing.T) {
	errs := RemovedStorageOverrideErrors(
		map[string]V1RuntimeConfig{"k8s": {SharedDirStorageBackend: "nfs", HomeStorageLeaf: "pod"}, "docker": {}},
		map[string]V1ProfileConfig{"p": {SharedDirStorageBackends: map[string]string{"cache": "local"}, HomeStorageBackend: "nfs"}},
		map[string]V1RuntimeTargetConfig{"server.broker.instances[0].runtime_target": {HomeStorageBackend: "local"}},
	)
	var paths []string
	for _, e := range errs {
		paths = append(paths, e.Path)
		assert.Contains(t, e.Message, "volumes:")
		assert.Contains(t, e.Message, "no longer supported")
	}
	assert.Equal(t, []string{
		"profiles.p.home_storage_backend",
		"profiles.p.shared_dir_storage_backends",
		"runtimes.k8s.home_storage_leaf",
		"runtimes.k8s.shared_dir_storage_backend",
		"server.broker.instances[0].runtime_target.home_storage_backend",
	}, paths)
	assert.Empty(t, RemovedStorageOverrideErrors(map[string]V1RuntimeConfig{"docker": {}}, nil, nil))
}

// TestLoadGlobalConfig_ProjectStorage: the hub's loader carries
// server.project_storage (file and env) and the replaced blocks (file only)
// into GlobalConfig, where ResolveGlobalProjectStorage synthesizes.
func TestLoadGlobalConfig_ProjectStorage(t *testing.T) {
	load := func(t *testing.T, settings string) *GlobalConfig {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o600))
		cfg, err := LoadGlobalConfig(dir)
		require.NoError(t, err)
		return cfg
	}
	t.Run("project_storage from file", func(t *testing.T) {
		cfg := load(t, "schema_version: \"1\"\nserver:\n  project_storage:\n    backend: nfs\n    homes: nfs\n    nfs:\n      mount_root: /mnt/nfs\n      shares:\n        - id: a\n          server: 10.0.0.2\n          export: /e\n")
		ps, warnings, err := ResolveGlobalProjectStorage(cfg)
		require.NoError(t, err)
		assert.Empty(t, warnings)
		require.NotNil(t, ps)
		assert.Equal(t, "nfs", ps.Backend)
		assert.Equal(t, "nfs", ps.Homes)
	})
	t.Run("project_storage backend from env", func(t *testing.T) {
		t.Setenv("SCION_SERVER_PROJECT_STORAGE_BACKEND", "local")
		cfg := load(t, "schema_version: \"1\"\nserver:\n  mode: hosted\n")
		ps, _, err := ResolveGlobalProjectStorage(cfg)
		require.NoError(t, err)
		require.NotNil(t, ps)
		assert.Equal(t, "local", ps.Backend)
	})
	t.Run("shared_dir_storage from file joins the synthesis", func(t *testing.T) {
		cfg := load(t, "schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs:\n      mount_root: /mnt\n      shares:\n        - id: s1\n")
		ps, _, err := ResolveGlobalProjectStorage(cfg)
		require.NoError(t, err)
		require.NotNil(t, ps.LegacySharedDirsNFS)
		assert.Equal(t, "/mnt", ps.LegacySharedDirsNFS.MountRoot)
	})
	t.Run("shared_dir_storage env is not consulted", func(t *testing.T) {
		t.Setenv("SCION_SERVER_SHARED_DIR_STORAGE_BACKEND", "nfs")
		cfg := load(t, "schema_version: \"1\"\nserver:\n  mode: hosted\n")
		assert.Nil(t, cfg.SharedDirStorage)
	})
}

// TestCheckStorageBlocksLoaded pins the fail-closed path for a legacy-format
// global file that holds a storage block the loader did not carry, without
// refusing a schema_version "1" file, a key that only contains a storage
// key's name, or a comment.
func TestCheckStorageBlocksLoaded(t *testing.T) {
	legacyBlock := []byte("server:\n  shared_dir_storage:\n    backend: nfs\n")
	err := CheckStorageBlocksLoaded(&GlobalConfig{}, true, legacyBlock)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has a shared_dir_storage block that was not loaded")
	assert.ErrorContains(t, CheckStorageBlocksLoaded(&GlobalConfig{}, true, []byte("project_storage:\n  backend: nfs\n")), "project_storage")
	assert.ErrorContains(t, CheckStorageBlocksLoaded(nil, true, []byte("workspace_storage: [unterminated\n")), "cannot be parsed")

	for name, tc := range map[string]struct {
		gc     *GlobalConfig
		legacy bool
		data   string
	}{
		"check disabled":                    {&GlobalConfig{}, false, "server:\n  shared_dir_storage:\n    backend: nfs\n"},
		"block loaded":                      {&GlobalConfig{SharedDirStorage: &V1SharedDirStorageConfig{Backend: "nfs"}}, true, string(legacyBlock)},
		"live field containing the name":    {&GlobalConfig{}, true, "runtimes:\n  k8s:\n    shared_dir_storage_class: rwx\n"},
		"leftover override key":             {&GlobalConfig{}, true, "profiles:\n  p:\n    home_storage_backend: nfs\n"},
		"comment only":                      {&GlobalConfig{}, true, "# workspace_storage: moved\nserver:\n  mode: hosted\n"},
		"no file":                           {&GlobalConfig{}, true, ""},
		"unparseable without a storage key": {&GlobalConfig{}, true, "server: [unterminated\n"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, CheckStorageBlocksLoaded(tc.gc, tc.legacy, []byte(tc.data)))
		})
	}

	t.Run("v1 global file with a comment and a live field starts", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"),
			[]byte("schema_version: \"1\"\n# project_storage: later\nruntimes:\n  k8s:\n    type: kubernetes\n    shared_dir_storage_class: rwx\nserver:\n  mode: hosted\n"), 0o600))
		cfg, err := LoadGlobalConfig(dir)
		require.NoError(t, err)
		assert.NoError(t, CheckGlobalStorageBlocksLoaded(cfg))
	})
}

// TestProjectSettingsCannotPlaceStorage pins the Layer-0 rule for the
// storage blocks: a project's settings.yaml that sets server.project_storage
// or any deprecated storage block has no effect on the merged settings the
// broker places workspaces, shared dirs and homes from.
func TestProjectSettingsCannotPlaceStorage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(
		"schema_version: \"1\"\nserver:\n  workspace_storage:\n    backend: local\n"), 0o600))
	projectDir := filepath.Join(home, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: from-project
server:
  project_storage:
    backend: nfs
    homes: nfs
    nfs:
      mount_root: /attacker
      shares:
        - id: x
          server: 10.9.9.9
          export: /x
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: /attacker
  shared_dir_storage:
    backend: nfs
  home_storage:
    backend: nfs
`), 0o600))

	check := func(t *testing.T, vs *VersionedSettings) {
		t.Helper()
		require.Equal(t, "from-project", vs.ActiveProfile, "sanity: the project layer was loaded")
		require.NotNil(t, vs.Server)
		assert.Nil(t, vs.Server.ProjectStorage, "project_storage must come from global settings only")
		assert.Nil(t, vs.Server.SharedDirStorage)
		assert.Nil(t, vs.Server.HomeStorage)
		require.NotNil(t, vs.Server.WorkspaceStorage, "the global block is kept")
		assert.Equal(t, "local", vs.Server.WorkspaceStorage.Backend)
		assert.Nil(t, vs.Server.WorkspaceStorage.NFS, "no project key is merged into the global block")
	}
	t.Run("project found through the current directory (external layer step)", func(t *testing.T) {
		t.Chdir(filepath.Dir(projectDir))
		vs, _, err := LoadEffectiveSettings("")
		require.NoError(t, err)
		check(t, vs)
	})

	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	require.Equal(t, "from-project", vs.ActiveProfile, "sanity: the project layer was loaded")
	require.NotNil(t, vs.Server)
	assert.Nil(t, vs.Server.ProjectStorage, "project_storage must come from global settings only")
	assert.Nil(t, vs.Server.SharedDirStorage)
	assert.Nil(t, vs.Server.HomeStorage)
	require.NotNil(t, vs.Server.WorkspaceStorage, "the global block is kept")
	assert.Equal(t, "local", vs.Server.WorkspaceStorage.Backend)
	assert.Nil(t, vs.Server.WorkspaceStorage.NFS, "no project key is merged into the global block")
	assert.Equal(t, "local", vs.Server.EffectiveWorkspaceStorage().Backend)
	assert.Nil(t, vs.Server.EffectiveHomeStorage())
}

func TestValidateHubProjectStorageLayout(t *testing.T) {
	assert.NoError(t, ValidateHubProjectStorageLayout(nil))
	assert.NoError(t, ValidateHubProjectStorageLayout(&V1ProjectStorageConfig{Backend: "nfs", NFS: psNFS("/mnt/nfs", "a", "s", "/e", "")}))
	assert.NoError(t, ValidateHubProjectStorageLayout(&V1ProjectStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "v"}}))
	assert.ErrorContains(t, ValidateHubProjectStorageLayout(&V1ProjectStorageConfig{Backend: "nfs", NFS: psNFS("relative", "a", "s", "/e", "")}), "absolute path")
	split := &V1ProjectStorageConfig{LegacySharedDirsNFS: psNFS("", "a", "s", "/e", "")}
	assert.ErrorContains(t, ValidateHubProjectStorageLayout(split), "absolute path", "the shared-dir split is checked too")
}

// TestCheckGlobalStorageBlocksLoaded_LegacyTopLevelBlock: a legacy-format
// global file (no schema_version) with a storage block that does not load
// (here at the top level instead of under server:) refuses startup rather
// than resolving to the local backend.
func TestCheckGlobalStorageBlocksLoaded_LegacyTopLevelBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("shared_dir_storage:\n  backend: nfs\n"), 0o600))
	require.True(t, GlobalSettingsIsLegacyFormat(), "fixture must load as the legacy format")
	cfg, err := LoadGlobalConfig(dir)
	require.NoError(t, err)
	err = CheckGlobalStorageBlocksLoaded(cfg)
	assert.ErrorContains(t, err, "has a shared_dir_storage block that was not loaded")
}

// TestGlobalSettingsStorageKey: only parsed mapping keys count, never a
// comment or a key that only contains a storage key's name.
func TestGlobalSettingsStorageKey(t *testing.T) {
	write := func(t *testing.T, content string) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o600))
	}
	keys := []string{"project_storage", "workspace_storage", "shared_dir_storage"}
	write(t, "# shared_dir_storage: moved\nruntimes:\n  k8s:\n    shared_dir_storage_class: rwx\n")
	assert.Equal(t, "", GlobalSettingsStorageKey(keys), "a comment and a containing key name do not count")
	write(t, "shared_dir_storage:\n  backend: nfs\n")
	assert.Equal(t, "shared_dir_storage", GlobalSettingsStorageKey(keys))
	write(t, "server: [unterminated\n")
	assert.Equal(t, "", GlobalSettingsStorageKey(keys), "an unparseable file is left to the load-error path")
}

// TestCheckGlobalStorageBlocksLoaded_V1BlockNotLoaded: a schema_version "1"
// file that holds a storage block the hub's loader did not carry (gc has
// none) refuses startup, whatever the reason the block did not load.
func TestCheckGlobalStorageBlocksLoaded_V1BlockNotLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n"), 0o600))
	err := CheckGlobalStorageBlocksLoaded(&GlobalConfig{})
	assert.ErrorContains(t, err, "shared_dir_storage")
	assert.NoError(t, CheckGlobalStorageBlocksLoaded(&GlobalConfig{SharedDirStorage: &V1SharedDirStorageConfig{Backend: "nfs"}}), "a loaded block passes")
}

// nonStringParentMergeKeyYAML has a "<<" merge key with a non-scalar
// mapping key under a parent mapping that also has a non-string key. The
// yaml.v3 decoder panics on it even when the target is interface{}.
const nonStringParentMergeKeyYAML = "schema_version: \"1\"\nserver:\n  1: x\n  <<: {[a]: b}\n"

// TestStorageBlockChecks_DecoderPanicIsParseError: a global settings file
// whose YAML makes the decoder panic is an ordinary parse failure for the
// storage-block checks, as for every other settings decode:
// CheckStorageBlocksLoaded refuses a file that mentions a storage key and
// GlobalSettingsStorageKey reports none, neither panicking.
func TestStorageBlockChecks_DecoderPanicIsParseError(t *testing.T) {
	content := nonStringParentMergeKeyYAML + "  project_storage:\n    backend: nfs\n"
	var err error
	require.NotPanics(t, func() { err = CheckStorageBlocksLoaded(&GlobalConfig{}, true, []byte(content)) })
	assert.ErrorContains(t, err, "cannot be parsed and mentions project_storage")

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o600))
	var key string
	require.NotPanics(t, func() { key = GlobalSettingsStorageKey([]string{"project_storage"}) })
	assert.Equal(t, "", key, "an unparseable file is left to the load-error path")
}
