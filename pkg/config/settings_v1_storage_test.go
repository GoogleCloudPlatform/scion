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
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- N0-1: Workspace storage config tests ---

func TestWorkspaceStorageConfig_YAMLRoundTrip(t *testing.T) {
	yamlInput := `
schema_version: "1"
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/nfs
      mount_options: "vers=4.1,hard,nconnect=8"
      uid: 2000
      gid: 2000
      subpath_root: workspaces
      storage_class: filestore-sc
      shares:
        - id: share-1
          server: "10.0.0.2"
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
`
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(yamlInput), 0644))

	projectDir := filepath.Join(tmpDir, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.WorkspaceStorage)

	ws := vs.Server.WorkspaceStorage
	assert.Equal(t, "nfs", ws.Backend)
	require.NotNil(t, ws.NFS)
	assert.Equal(t, "/mnt/nfs", ws.NFS.MountRoot)
	assert.Equal(t, "vers=4.1,hard,nconnect=8", ws.NFS.MountOptions)
	assert.Equal(t, 2000, ws.NFS.UID)
	assert.Equal(t, 2000, ws.NFS.GID)
	assert.Equal(t, "workspaces", ws.NFS.SubPathRoot)
	assert.Equal(t, "filestore-sc", ws.NFS.StorageClass)
	require.Len(t, ws.NFS.Shares, 1)
	assert.Equal(t, "share-1", ws.NFS.Shares[0].ID)
	assert.Equal(t, "10.0.0.2", ws.NFS.Shares[0].Server)
	assert.Equal(t, "/scion-workspaces", ws.NFS.Shares[0].Export)
	assert.Equal(t, "scion-workspaces-pv", ws.NFS.Shares[0].PVName)
}

func TestWorkspaceStorageConfig_JSONRoundTrip(t *testing.T) {
	ws := &V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &V1NFSConfig{
			MountRoot:    "/mnt/nfs",
			MountOptions: "vers=4.1,hard,nconnect=4,_netdev",
			UID:          1000,
			GID:          1000,
			SubPathRoot:  "projects",
			Shares: []V1NFSShare{
				{ID: "main", Server: "10.0.0.2", Export: "/scion-workspaces", PVName: "scion-ws-pv"},
			},
		},
	}

	data, err := json.Marshal(ws)
	require.NoError(t, err)

	var roundTripped V1WorkspaceStorageConfig
	require.NoError(t, json.Unmarshal(data, &roundTripped))

	assert.Equal(t, ws.Backend, roundTripped.Backend)
	require.NotNil(t, roundTripped.NFS)
	assert.Equal(t, ws.NFS.MountRoot, roundTripped.NFS.MountRoot)
	assert.Equal(t, ws.NFS.MountOptions, roundTripped.NFS.MountOptions)
	assert.Equal(t, ws.NFS.UID, roundTripped.NFS.UID)
	assert.Equal(t, ws.NFS.GID, roundTripped.NFS.GID)
	assert.Equal(t, ws.NFS.SubPathRoot, roundTripped.NFS.SubPathRoot)
	require.Len(t, roundTripped.NFS.Shares, 1)
	assert.Equal(t, ws.NFS.Shares[0].ID, roundTripped.NFS.Shares[0].ID)
	assert.Equal(t, ws.NFS.Shares[0].Server, roundTripped.NFS.Shares[0].Server)
}

func TestWorkspaceStorageConfig_NFSDefaults(t *testing.T) {
	t.Run("nfs backend applies defaults to empty sub-fields", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "nfs"}
		ws.ApplyNFSDefaults()

		require.NotNil(t, ws.NFS)
		assert.Equal(t, "vers=3,hard,nconnect=4,_netdev", ws.NFS.MountOptions)
		assert.Equal(t, 1000, ws.NFS.UID)
		assert.Equal(t, 1000, ws.NFS.GID)
		assert.Equal(t, "projects", ws.NFS.SubPathRoot)
	})

	t.Run("nfs backend preserves explicit values", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountOptions: "custom-opts",
				UID:          5000,
				GID:          5000,
				SubPathRoot:  "custom-root",
			},
		}
		ws.ApplyNFSDefaults()

		assert.Equal(t, "custom-opts", ws.NFS.MountOptions)
		assert.Equal(t, 5000, ws.NFS.UID)
		assert.Equal(t, 5000, ws.NFS.GID)
		assert.Equal(t, "custom-root", ws.NFS.SubPathRoot)
	})

	t.Run("local backend does not materialize NFS block", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "local"}
		ws.ApplyNFSDefaults()
		assert.Nil(t, ws.NFS)
	})

	t.Run("empty backend does not materialize NFS block", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{}
		ws.ApplyNFSDefaults()
		assert.Nil(t, ws.NFS)
	})

	t.Run("backend name is case-sensitive", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "NFS"}
		ws.ApplyNFSDefaults()
		assert.Nil(t, ws.NFS, "only the exact name \"nfs\" selects the NFS backend")
		assert.NoError(t, ws.ValidateNFS(), "ValidateNFS ignores non-nfs backends; ValidateWorkspaceStorage rejects the name")
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var ws *V1WorkspaceStorageConfig
		ws.ApplyNFSDefaults() // should not panic
	})
}

func TestWorkspaceStorageConfig_ValidateNFS(t *testing.T) {
	t.Run("nfs backend with no shares returns error", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "nfs"}
		ws.ApplyNFSDefaults()
		err := ws.ValidateNFS()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no NFS shares are defined")
	})

	t.Run("nfs backend with shares passes", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				Shares: []V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/data"}},
			},
		}
		ws.ApplyNFSDefaults()
		err := ws.ValidateNFS()
		require.NoError(t, err)
	})

	// uid/gid are checked after ApplyNFSDefaults, so an unset (0) value is
	// already the default 1000 and passes; negative values (including -1,
	// which chown reads as "leave unchanged") and values above
	// 4294967294 are rejected with the field named.
	t.Run("uid and gid range", func(t *testing.T) {
		tests := []struct {
			name      string
			uid, gid  int64
			wantField string
		}{
			{"unset uses defaults", 0, 0, ""},
			{"explicit", 2000, 3000, ""},
			{"maximum", 4294967294, 4294967294, ""},
			{"negative one uid", -1, 1000, "server.workspace_storage.nfs.uid"},
			{"negative one gid", 1000, -1, "server.workspace_storage.nfs.gid"},
			{"negative uid", -7, 1000, "server.workspace_storage.nfs.uid"},
			{"unsigned sentinel gid", 1000, 4294967295, "server.workspace_storage.nfs.gid"},
			{"out of range uid", 1 << 33, 1000, "server.workspace_storage.nfs.uid"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// A 32-bit int cannot hold ids above math.MaxInt32, so the
				// int conversions below would wrap and test a different value.
				if strconv.IntSize < 64 && max(tt.uid, tt.gid) > math.MaxInt32 {
					t.Skipf("%d/%d does not fit in a %d-bit int", tt.uid, tt.gid, strconv.IntSize)
				}
				ws := &V1WorkspaceStorageConfig{
					Backend: "nfs",
					NFS: &V1NFSConfig{
						Shares: []V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/data"}},
						UID:    int(tt.uid),
						GID:    int(tt.gid),
					},
				}
				ws.ApplyNFSDefaults()
				err := ws.ValidateNFS()
				if tt.wantField == "" {
					require.NoError(t, err)
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantField)
				// ValidateWorkspaceStorage (hub startup) reports it too.
				require.Error(t, ws.ValidateWorkspaceStorage())
			})
		}
	})

	t.Run("local backend skips validation", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "local"}
		err := ws.ValidateNFS()
		require.NoError(t, err)
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var ws *V1WorkspaceStorageConfig
		err := ws.ValidateNFS()
		require.NoError(t, err)
	})
}

func TestWorkspaceStorageConfig_ApplyWorkspaceStorageDefaults(t *testing.T) {
	t.Run("nfs delegates to ApplyNFSDefaults", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "nfs"}
		ws.ApplyWorkspaceStorageDefaults()
		require.NotNil(t, ws.NFS)
		assert.Equal(t, DefaultWorkspaceSubPathRoot, ws.NFS.SubPathRoot)
		assert.Equal(t, 1000, ws.NFS.UID)
	})

	t.Run("cloudrun-volume defaults subpath_root", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: "vol"}}
		ws.ApplyWorkspaceStorageDefaults()
		assert.Equal(t, DefaultWorkspaceSubPathRoot, ws.CloudRunVolume.SubPathRoot)
	})

	t.Run("gke-shared-volume defaults subpath_root", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "vol"}}
		ws.ApplyWorkspaceStorageDefaults()
		assert.Equal(t, DefaultWorkspaceSubPathRoot, ws.GKESharedVolume.SubPathRoot)
	})

	t.Run("explicit subpath_root is preserved", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "vol", SubPathRoot: "trees"}}
		ws.ApplyWorkspaceStorageDefaults()
		assert.Equal(t, "trees", ws.GKESharedVolume.SubPathRoot)
	})

	t.Run("missing volume block stays nil", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "cloudrun-volume"}
		ws.ApplyWorkspaceStorageDefaults()
		assert.Nil(t, ws.CloudRunVolume)
	})

	t.Run("local, unknown and nil are left alone", func(t *testing.T) {
		for _, backend := range []string{"", "local", "s3", "NFS"} {
			ws := &V1WorkspaceStorageConfig{Backend: backend}
			ws.ApplyWorkspaceStorageDefaults()
			assert.Equal(t, &V1WorkspaceStorageConfig{Backend: backend}, ws)
		}
		var ws *V1WorkspaceStorageConfig
		ws.ApplyWorkspaceStorageDefaults() // must not panic
	})
}

func TestWorkspaceStorageConfig_ValidateWorkspaceStorage(t *testing.T) {
	nfsShares := []V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/data"}}
	tests := []struct {
		name    string
		ws      *V1WorkspaceStorageConfig
		wantErr string // empty means valid
	}{
		{name: "nil", ws: nil},
		{name: "empty backend", ws: &V1WorkspaceStorageConfig{}},
		{name: "local", ws: &V1WorkspaceStorageConfig{Backend: "local"}},
		{name: "unknown backend", ws: &V1WorkspaceStorageConfig{Backend: "s3"}, wantErr: `workspace_storage.backend "s3" is not supported`},
		{name: "backend names are case-sensitive", ws: &V1WorkspaceStorageConfig{Backend: "NFS"}, wantErr: "is not supported"},
		{name: "nfs without shares", ws: &V1WorkspaceStorageConfig{Backend: "nfs"}, wantErr: "no NFS shares are defined"},
		{name: "nfs with shares", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: &V1NFSConfig{Shares: nfsShares}}},
		{
			name:    "nfs with absolute subpath_root",
			ws:      &V1WorkspaceStorageConfig{Backend: "nfs", NFS: &V1NFSConfig{Shares: nfsShares, SubPathRoot: "/projects"}},
			wantErr: "workspace_storage.nfs.subpath_root must be relative",
		},
		{name: "cloudrun-volume", ws: &V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: "vol"}}},
		{name: "cloudrun-volume without block", ws: &V1WorkspaceStorageConfig{Backend: "cloudrun-volume"}, wantErr: "cloudrun_volume.volume_name is not set"},
		{
			name:    "cloudrun-volume with empty volume_name",
			ws:      &V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{SubPathRoot: "projects"}},
			wantErr: "cloudrun_volume.volume_name is not set",
		},
		{
			name:    "cloudrun-volume with traversing subpath_root",
			ws:      &V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &V1CloudRunVolumeConfig{VolumeName: "vol", SubPathRoot: "../x"}},
			wantErr: "workspace_storage.cloudrun_volume.subpath_root",
		},
		{name: "gke-shared-volume", ws: &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "vol", PVClaimName: "pvc"}}},
		{name: "gke-shared-volume without block", ws: &V1WorkspaceStorageConfig{Backend: "gke-shared-volume"}, wantErr: "gke_shared_volume.volume_name is not set"},
		{
			name:    "gke-shared-volume with empty volume_name",
			ws:      &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{PVClaimName: "pvc"}},
			wantErr: "gke_shared_volume.volume_name is not set",
		},
		{
			name:    "gke-shared-volume with unclean subpath_root",
			ws:      &V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &V1GKESharedVolumeConfig{VolumeName: "vol", SubPathRoot: "projects/"}},
			wantErr: "workspace_storage.gke_shared_volume.subpath_root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.ws.ApplyWorkspaceStorageDefaults()
			err := tt.ws.ValidateWorkspaceStorage()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidateSubPathRoot_SuggestsCleanValue pins the clean-path check: it
// runs before the component checks so an unclean value names the value to
// use instead, and gives no suggestion for ".." or for a value that cleans
// to ".".
func TestValidateSubPathRoot_SuggestsCleanValue(t *testing.T) {
	suggest := map[string]string{
		"projects/":        "projects",
		"./projects":       "projects",
		"projects/.":       "projects",
		"a//b":             "a/b",
		"team/./projects/": "team/projects",
	}
	for in, want := range suggest {
		err := ValidateSubPathRoot(in)
		require.Error(t, err, in)
		assert.Contains(t, err.Error(), fmt.Sprintf("must be a clean path (got %q, use %q)", in, want))
	}

	noSuggestion := map[string]string{
		"projects/../escape": `".." path component`,
		"../projects":        `".." path component`,
		"a/..":               `".." path component`,
		"./a/../b":           `".." path component`,
		".":                  `"." path component`,
	}
	for in, want := range noSuggestion {
		err := ValidateSubPathRoot(in)
		require.Error(t, err, in)
		assert.NotContains(t, err.Error(), "use ", in)
		assert.Contains(t, err.Error(), want, in)
	}

	for _, valid := range []string{"projects", "team/projects", "a.b/c-d"} {
		assert.NoError(t, ValidateSubPathRoot(valid), valid)
	}
}

func TestResolveSubPathRoot(t *testing.T) {
	got, err := ResolveSubPathRoot("")
	require.NoError(t, err)
	assert.Equal(t, DefaultWorkspaceSubPathRoot, got)

	for _, valid := range []string{"projects", "trees", "team/trees"} {
		got, err := ResolveSubPathRoot(valid)
		require.NoError(t, err, valid)
		assert.Equal(t, valid, got)
	}

	for _, invalid := range []string{"/projects", "../projects", "a/../b", "a/./b", "./a", "a/", "a//b", "..", "."} {
		_, err := ResolveSubPathRoot(invalid)
		assert.Error(t, err, invalid)
	}
}

func TestWorkspaceStorageConfig_BackendUnset_IsLocal(t *testing.T) {
	// Backend unset => treated as "local", no NFS struct required.
	ws := &V1WorkspaceStorageConfig{}
	assert.Equal(t, "", ws.Backend, "empty backend is treated as local")
	assert.Nil(t, ws.NFS, "no NFS block when backend is local/empty")
}

// TestSharedDirStorageConfig_Validate covers design deploy-config-explore
// §3.2.1's validation rule: backend=nfs ⇒ NFS!=nil, len(Shares)>=1,
// MountRoot!="", Shares[0].ID!="". Unlike V1WorkspaceStorageConfig.ValidateNFS,
// shared_dir_storage requires MountRoot and the first share's ID, because it
// has no other source for the host mount point used by local-container bind
// mounts (test (c) / AC4).
func TestSharedDirStorageConfig_Validate(t *testing.T) {
	t.Run("nil receiver is safe", func(t *testing.T) {
		var s *V1SharedDirStorageConfig
		require.NoError(t, s.Validate())
	})

	t.Run("unset backend skips validation", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{}
		require.NoError(t, s.Validate())
	})

	t.Run("local backend skips validation", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{Backend: "local"}
		require.NoError(t, s.Validate())
	})

	t.Run("nfs backend with nil NFS block errors", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{Backend: "nfs"}
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no nfs block is configured")
	})

	t.Run("nfs backend with no shares errors", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS:     &V1NFSConfig{MountRoot: "/srv"},
		}
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no NFS shares are defined")
	})

	t.Run("nfs backend with empty mount_root errors", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				Shares: []V1NFSShare{{ID: "scion-shared"}},
			},
		}
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mount_root is empty")
	})

	t.Run("nfs backend with empty share id errors", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []V1NFSShare{{ID: ""}},
			},
		}
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shares[0].id is empty")
	})

	t.Run("fully configured nfs backend passes", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountRoot: "/srv",
				Shares: []V1NFSShare{
					{ID: "scion-shared", Server: "10.128.15.241", Export: "/srv/scion-shared", PVName: "scion-shared"},
				},
			},
		}
		require.NoError(t, s.Validate())
	})

	// Round 6 review nit #6 (hy-rev-6): an absolute or "." / ".."-containing
	// subpath_root produces a confusing low-level error from the component
	// walk instead of a clear configuration error. Validate must reject it.
	baseNFSConfig := func() *V1NFSConfig {
		return &V1NFSConfig{
			MountRoot: "/srv",
			Shares:    []V1NFSShare{{ID: "scion-shared"}},
		}
	}
	invalidSubPathRoots := []struct {
		name  string
		value string
		want  string
	}{
		{"absolute", "/projects", "must be relative"},
		{"empty component", "projects//nested", `must be a clean path (got "projects//nested", use "projects/nested")`},
		{"leading slash empty component", "/", "must be relative"},
		{"dot component", "projects/./nested", `use "projects/nested"`},
		{"dotdot component", "projects/../escape", "\"..\" path component"},
		{"bare dotdot", "..", "\"..\" path component"},
	}
	for _, tc := range invalidSubPathRoots {
		t.Run("nfs backend with invalid subpath_root "+tc.name, func(t *testing.T) {
			nfs := baseNFSConfig()
			nfs.SubPathRoot = tc.value
			s := &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfs}
			err := s.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "subpath_root")
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("nfs backend with valid non-default subpath_root passes", func(t *testing.T) {
		nfs := baseNFSConfig()
		nfs.SubPathRoot = "nested/subdir"
		s := &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfs}
		require.NoError(t, s.Validate())
	})

	// Round 1 review (r1-code.md #2 / r1-test.md #3): an unrecognized
	// backend must fail closed rather than silently taking the local-layout
	// branch. Exact match only — no trimming or case folding.
	unknownBackends := []string{"nsf", "NFS ", "Nfs", "garbage", "nfs2", " nfs"}
	for _, backend := range unknownBackends {
		t.Run("unknown backend "+strconv.Quote(backend)+" errors", func(t *testing.T) {
			s := &V1SharedDirStorageConfig{Backend: backend}
			err := s.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be")
			assert.Contains(t, err.Error(), strconv.Quote(backend))
		})
	}
}

// TestSharedDirStorageConfig_IgnoredNFSFields is Phase 2 item 5 (design §7
// Phase 2: "startup validation warns about ignored fields"): uid, gid,
// mount_options, and storage_class are meaningful for workspace_storage but
// never consulted by shared_dir_storage (design §3.2.1).
func TestSharedDirStorageConfig_IgnoredNFSFields(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var s *V1SharedDirStorageConfig
		assert.Nil(t, s.IgnoredNFSFields())
	})

	t.Run("nil NFS block returns nil", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{Backend: "nfs"}
		assert.Nil(t, s.IgnoredNFSFields())
	})

	t.Run("local backend never warns even if NFS fields happen to be set", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{Backend: "local", NFS: &V1NFSConfig{UID: 1000, GID: 1000}}
		assert.Nil(t, s.IgnoredNFSFields())
	})

	t.Run("nfs backend with none of the ignored fields set returns nil", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []V1NFSShare{{ID: "scion-shared"}},
			},
		}
		assert.Nil(t, s.IgnoredNFSFields())
	})

	t.Run("nfs backend with all ignored fields set", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountRoot:    "/srv",
				Shares:       []V1NFSShare{{ID: "scion-shared"}},
				UID:          1000,
				GID:          1000,
				MountOptions: "vers=3",
				StorageClass: "standard",
				AutoMount:    true,
			},
		}
		// gid is not ignored: it is the leaf-group allowlist (ptone/scion#3155).
		assert.ElementsMatch(t, []string{"uid", "mount_options", "storage_class", "auto_mount"}, s.IgnoredNFSFields())
	})

	t.Run("nfs backend with only one ignored field set", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []V1NFSShare{{ID: "scion-shared"}},
				UID:       1003,
			},
		}
		assert.Equal(t, []string{"uid"}, s.IgnoredNFSFields())
	})

	t.Run("nfs backend with gid set is not warned about", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []V1NFSShare{{ID: "scion-shared"}},
				GID:       1003,
			},
		}
		assert.Nil(t, s.IgnoredNFSFields())
	})
}

// TestSharedDirStorageConfig_ResolvedLayoutSummary is the other half of
// Phase 2 item 5: "log exactly one resolved-layout line: backend, host
// base, subpath_root, pv_name."
func TestSharedDirStorageConfig_ResolvedLayoutSummary(t *testing.T) {
	t.Run("nil receiver returns empty", func(t *testing.T) {
		var s *V1SharedDirStorageConfig
		assert.Empty(t, s.ResolvedLayoutSummary())
	})

	t.Run("unset/local backend returns empty (nothing new to log)", func(t *testing.T) {
		assert.Empty(t, (&V1SharedDirStorageConfig{}).ResolvedLayoutSummary())
		assert.Empty(t, (&V1SharedDirStorageConfig{Backend: "local"}).ResolvedLayoutSummary())
	})

	t.Run("nfs backend summarizes backend, host base, subpath_root, pv_name", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []V1NFSShare{{ID: "scion-shared", PVName: "scion-shared-pv"}},
			},
		}
		summary := s.ResolvedLayoutSummary()
		assert.Contains(t, summary, "backend=nfs")
		assert.Contains(t, summary, filepath.Join("/srv", "scion-shared"))
		assert.Contains(t, summary, "subpath_root=projects") // default
		assert.Contains(t, summary, "pv_name=scion-shared-pv")
	})

	t.Run("nfs backend with a custom subpath_root", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountRoot:   "/srv",
				Shares:      []V1NFSShare{{ID: "scion-shared", PVName: "pv"}},
				SubPathRoot: "nested/subdir",
			},
		}
		assert.Contains(t, s.ResolvedLayoutSummary(), "subpath_root="+filepath.Join("nested", "subdir"))
	})

	// Distinct from "local backend, no nfs block at all" above -- an nfs
	// sub-block can still be present (e.g. left over from a prior nfs
	// configuration, or configured ahead of a planned switch) while backend
	// stays "local". No summary line must be logged either way:
	// ResolvedLayoutSummary gates strictly on Backend == "nfs", never on the
	// nfs block's mere presence.
	t.Run("local backend with an nfs block present still returns empty", func(t *testing.T) {
		s := &V1SharedDirStorageConfig{
			Backend: "local",
			NFS: &V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []V1NFSShare{{ID: "scion-shared", PVName: "pv"}},
			},
		}
		assert.Empty(t, s.ResolvedLayoutSummary())
	})
}

// TestSharedDirStorageConfig_YAMLRoundTrip replaces a prior tautological
// test that built the struct in Go and read the same fields back (r1-test.md
// #9: a koanf tag typo would have gone undetected). This writes the design
// §3.2.1 YAML to a global settings.yaml and loads it through
// LoadEffectiveSettings, pinning the koanf/yaml tags end to end.
func TestSharedDirStorageConfig_YAMLRoundTrip(t *testing.T) {
	// Round 6 addendum (hy-rev-6 finding #3): this test goes through the
	// general LoadEffectiveSettings loader, which — unlike the dedicated,
	// env-free LoadGlobalSettings added for the Start path — legitimately
	// does merge SCION_ environment variables (that's correct for the
	// general loader; it's just not what this test is exercising). An
	// ambient SCION_* variable whose mapped key collides with a
	// struct-typed field (e.g. SCION_AUTO_EXPOSE_PORTS, common in a scion
	// agent's own container) makes Unmarshal fail before this test's
	// assertions ever run, so this test is not hermetic against the shell
	// it happens to run in without clearing them first.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "SCION_") {
			t.Setenv(key, "")
			require.NoError(t, os.Unsetenv(key))
		}
	}

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	settingsYAML := `
schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /srv
      subpath_root: projects
      shares:
        - id: scion-shared
          server: 10.128.15.241
          export: /srv/scion-shared
          pv_name: scion-shared
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644))

	vs, warnings, err := LoadEffectiveSettings("")
	require.NoError(t, err)
	assert.Empty(t, warnings)

	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.SharedDirStorage)
	sd := vs.Server.SharedDirStorage
	assert.Equal(t, "nfs", sd.Backend)
	require.NotNil(t, sd.NFS)
	assert.Equal(t, "/srv", sd.NFS.MountRoot)
	assert.Equal(t, "projects", sd.NFS.SubPathRoot)
	require.Len(t, sd.NFS.Shares, 1)
	assert.Equal(t, "scion-shared", sd.NFS.Shares[0].ID)
	assert.Equal(t, "10.128.15.241", sd.NFS.Shares[0].Server)
	assert.Equal(t, "/srv/scion-shared", sd.NFS.Shares[0].Export)
	assert.Equal(t, "scion-shared", sd.NFS.Shares[0].PVName)
}
