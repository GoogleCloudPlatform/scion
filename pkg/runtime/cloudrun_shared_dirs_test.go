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

package runtime

import (
	"context"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCloudRunSharedDirPID = "11111111-2222-3333-4444-555555555555"

func testCloudRunSharedDirRunConfig() RunConfig {
	return RunConfig{
		Name:                 "test-agent",
		Image:                "docker.io/scion/agent:latest",
		Labels:               map[string]string{"agent_id": "agent-1", "scion.name": "test-agent"},
		WorkspaceBackendName: "nfs",
		ProjectID:            testCloudRunSharedDirPID,
		NFSShareServer:       "10.128.15.241",
		NFSShareExport:       "/srv/scion-shared",
		NFSSubPathRoot:       "projects",
		SharedDirs: []api.SharedDir{
			{Name: "scratchpad"},
			{Name: "design-docs", ReadOnly: true, InWorkspace: true},
		},
		SharedDirStorage: &SharedDirRealization{
			Backend:     "nfs",
			PVClaimName: "scion-shared", // hybrid hub may also set pv_name for GKE
			Server:      "10.128.15.241",
			Export:      "/srv/scion-shared",
			SubPathRoot: "projects",
			ProjectID:   testCloudRunSharedDirPID,
			SubPaths: map[string]string{
				"scratchpad":  "projects/" + testCloudRunSharedDirPID + "/shared-dirs/scratchpad",
				"design-docs": "projects/" + testCloudRunSharedDirPID + "/shared-dirs/design-docs",
			},
		},
	}
}

func testCloudRunSharedDirNFSPaths() *cloudRunNFSProvisionPaths {
	return &cloudRunNFSProvisionPaths{
		server:              "10.128.15.241",
		workspaceExportPath: "/srv/scion-shared/projects/" + testCloudRunSharedDirPID + "/workspace",
		homeExportPath:      "/srv/scion-shared/projects/" + testCloudRunSharedDirPID + "/agents/test-agent/home-agent-1",
	}
}

// TestBuildCloudRunInstance_SharedDirs_MountsNFSLeaves pins the generated
// Cloud Run Instance Volumes and Containers[0].VolumeMounts for project shared
// directories on NFS (ptone/scion#5926), including read-only and in-workspace
// targets, custom ContainerWorkspace, hybrid shares carrying PVClaimName, and
// flat Cloud Run runtimes where NFSServer/NFSExport come from the share.
func TestBuildCloudRunInstance_SharedDirs_MountsNFSLeaves(t *testing.T) {
	for _, tc := range []struct {
		name               string
		rt                 func(t *testing.T) *CloudRunRuntime
		containerWorkspace string
		wantInWsMountPath  string
	}{
		{
			name: "legacy cloudrun default workspace",
			rt: func(t *testing.T) *CloudRunRuntime {
				rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
					ProjectID: "test-project",
					Location:  "us-central1",
					NFSServer: "10.128.15.241",
					NFSExport: "/srv/scion-shared",
				})
				require.NoError(t, err)
				return rt
			},
			wantInWsMountPath: "/workspace/.scion-volumes/design-docs",
		},
		{
			name: "flat cloudrun custom container workspace",
			rt: func(t *testing.T) *CloudRunRuntime {
				rt, err := NewFlatCloudRunRuntime(config.CloudRunConfig{
					ProjectID: "test-project",
					Location:  "us-central1",
				}, flatTestCredentials())
				require.NoError(t, err)
				return rt
			},
			containerWorkspace: "/repo-root/sub",
			wantInWsMountPath:  "/repo-root/sub/.scion-volumes/design-docs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := tc.rt(t)
			cfg := testCloudRunSharedDirRunConfig()
			cfg.ContainerWorkspace = tc.containerWorkspace

			// Early env-only call (nfsPaths == nil) emits no volumes/mounts.
			envOnly, err := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
			require.NoError(t, err)
			assert.Empty(t, envOnly.GetVolumes())
			assert.Empty(t, envOnly.GetContainers()[0].GetVolumeMounts())

			inst, err := rt.buildCloudRunInstance(cfg, 1000, 1000, testCloudRunSharedDirNFSPaths())
			require.NoError(t, err)
			vols := inst.GetVolumes()
			mounts := inst.GetContainers()[0].GetVolumeMounts()
			require.Len(t, vols, 5, "workspace, home, scion-mem + 2 shared dirs")
			require.Len(t, mounts, 5)

			volByName := make(map[string]*runpb.Volume, len(vols))
			for _, v := range vols {
				volByName[v.GetName()] = v
			}
			mountByName := make(map[string]*runpb.VolumeMount, len(mounts))
			for _, m := range mounts {
				mountByName[m.GetName()] = m
			}

			sd0 := volByName["shared-dir-0"]
			require.NotNil(t, sd0, "shared-dir-0 volume must be present")
			require.NotNil(t, sd0.GetNfs())
			assert.Equal(t, "10.128.15.241", sd0.GetNfs().GetServer())
			assert.Equal(t, "/srv/scion-shared/projects/"+testCloudRunSharedDirPID+"/shared-dirs/scratchpad", sd0.GetNfs().GetPath())
			assert.False(t, sd0.GetNfs().GetReadOnly())
			require.NotNil(t, mountByName["shared-dir-0"])
			assert.Equal(t, "/scion-volumes/scratchpad", mountByName["shared-dir-0"].GetMountPath())
			assert.Empty(t, mountByName["shared-dir-0"].GetSubPath())

			sd1 := volByName["shared-dir-1"]
			require.NotNil(t, sd1, "shared-dir-1 volume must be present")
			require.NotNil(t, sd1.GetNfs())
			assert.Equal(t, "10.128.15.241", sd1.GetNfs().GetServer())
			assert.Equal(t, "/srv/scion-shared/projects/"+testCloudRunSharedDirPID+"/shared-dirs/design-docs", sd1.GetNfs().GetPath())
			assert.True(t, sd1.GetNfs().GetReadOnly())
			require.NotNil(t, mountByName["shared-dir-1"])
			assert.Equal(t, tc.wantInWsMountPath, mountByName["shared-dir-1"].GetMountPath())
			assert.Empty(t, mountByName["shared-dir-1"].GetSubPath())
		})
	}
}

// TestBuildCloudRunInstance_SharedDirs_PropagatesError verifies that
// buildCloudRunInstance propagates shared-dir validation errors rather than
// swallowing them and returning a nil *runpb.Instance without an error.
func TestBuildCloudRunInstance_SharedDirs_PropagatesError(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "test-project",
		Location:  "us-central1",
		NFSServer: "10.128.15.241",
		NFSExport: "/srv/scion-shared",
	})
	require.NoError(t, err)

	badCfg := testCloudRunSharedDirRunConfig()
	badCfg.SharedDirStorage = nil

	inst, err := rt.buildCloudRunInstance(badCfg, 1000, 1000, testCloudRunSharedDirNFSPaths())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require NFS project storage")
	assert.Nil(t, inst)
}

// TestCloudRunSharedDirs_ParityWithBindAndKubernetes verifies that for one
// projectfs.Layout, Docker bind realization, Kubernetes inline NFS realization,
// and Cloud Run volume realization agree on every shared-dir leaf's server
// path, container mount path, and ReadOnly flag.
func TestCloudRunSharedDirs_ParityWithBindAndKubernetes(t *testing.T) {
	layout := projectfs.Layout{
		Backend:     projectfs.BackendNFS,
		HostBase:    "/mnt/nfs/share",
		SubPathRoot: "team/projects",
		Share: projectfs.Share{
			ID:     "share",
			Server: "10.0.0.20",
			Export: "/exports/scion",
		},
	}
	dirs := []api.SharedDir{
		{Name: "scratchpad"},
		{Name: "readonly-artifacts", ReadOnly: true, InWorkspace: true},
	}
	subPaths := make(map[string]string, len(dirs))
	resolved := make([]projectfs.Resolved, len(dirs))
	for i, d := range dirs {
		r, err := layout.Resolve(projectfs.Project{ID: testCloudRunSharedDirPID}, "", projectfs.SharedDirMount(d, "/workspace"))
		require.NoError(t, err)
		resolved[i] = r
		subPaths[d.Name] = r.SubPath
	}

	realization := &SharedDirRealization{
		Backend:     string(layout.Backend),
		Server:      layout.Share.Server,
		Export:      layout.Share.Export,
		SubPathRoot: layout.SubPathRoot,
		ProjectID:   testCloudRunSharedDirPID,
		SubPaths:    subPaths,
	}
	rt, err := NewFlatCloudRunRuntime(config.CloudRunConfig{
		ProjectID: "test-project",
		Location:  "us-central1",
	}, flatTestCredentials())
	require.NoError(t, err)
	cfg := RunConfig{
		Name:                 "agent-a",
		Image:                "img",
		WorkspaceBackendName: "nfs",
		ProjectID:            testCloudRunSharedDirPID,
		NFSShareServer:       layout.Share.Server,
		NFSShareExport:       layout.Share.Export,
		NFSSubPathRoot:       layout.SubPathRoot,
		SharedDirs:           dirs,
		SharedDirStorage:     realization,
	}

	crVols, crMounts, err := rt.cloudRunSharedDirVolumes(cfg)
	require.NoError(t, err)
	require.Len(t, crVols, len(dirs))
	require.Len(t, crMounts, len(dirs))

	for i, d := range dirs {
		bind, err := layout.Realize(resolved[i], projectfs.RuntimeBind)
		require.NoError(t, err)
		k8sVol, k8sSubPath, err := sharedDirPodVolume(realization, i, d.Name, d.ReadOnly, resolved[i].Target)
		require.NoError(t, err)
		assert.Empty(t, k8sSubPath)
		require.NotNil(t, k8sVol.NFS)

		assert.Equal(t, bind.Target, crMounts[i].GetMountPath())
		assert.Equal(t, bind.ReadOnly, crVols[i].GetNfs().GetReadOnly())
		assert.Equal(t, k8sVol.NFS.Server, crVols[i].GetNfs().GetServer())
		assert.Equal(t, k8sVol.NFS.Path, crVols[i].GetNfs().GetPath())
		assert.Equal(t, k8sVol.NFS.ReadOnly, crVols[i].GetNfs().GetReadOnly())
	}
}

// TestCloudRunRun_SharedDirs_EndToEndFakeClient verifies CloudRunRuntime.Run
// sends the shared-dir NFS volumes and volumeMounts in CreateInstanceRequest
// for both legacy and flat Cloud Run runtimes.
func TestCloudRunRun_SharedDirs_EndToEndFakeClient(t *testing.T) {
	for _, flat := range []bool{false, true} {
		name := "legacy"
		if flat {
			name = "flat"
		}
		t.Run(name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			var rt *CloudRunRuntime
			if flat {
				var err error
				rt, err = NewFlatCloudRunRuntime(config.CloudRunConfig{
					ProjectID: "test-project",
					Location:  "us-central1",
				}, flatTestCredentials())
				require.NoError(t, err)
				rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil }
				rt.provisionNFS = stubCloudRunNFSProvision
				rt.readyBoundsForTest = fakeClockReadyBounds()
			} else {
				rt = newReadyTestRuntime(t, fake)
				rt.config.NFSServer = "10.128.15.241"
				rt.config.NFSExport = "/srv/scion-shared"
			}

			cfg := testCloudRunSharedDirRunConfig()
			_, err := rt.Run(context.Background(), cfg)
			require.NoError(t, err)
			require.Len(t, fake.createReqs, 1)

			created := fake.createReqs[0].GetInstance()
			require.Len(t, created.GetVolumes(), 5)
			require.Len(t, created.GetContainers()[0].GetVolumeMounts(), 5)
			assert.Equal(t, "shared-dir-0", created.GetVolumes()[3].GetName())
			assert.Equal(t, "/srv/scion-shared/projects/"+testCloudRunSharedDirPID+"/shared-dirs/scratchpad",
				created.GetVolumes()[3].GetNfs().GetPath())
			assert.Equal(t, "/scion-volumes/scratchpad",
				created.GetContainers()[0].GetVolumeMounts()[3].GetMountPath())
			assert.Equal(t, "shared-dir-1", created.GetVolumes()[4].GetName())
			assert.True(t, created.GetVolumes()[4].GetNfs().GetReadOnly())
			assert.Equal(t, "/workspace/.scion-volumes/design-docs",
				created.GetContainers()[0].GetVolumeMounts()[4].GetMountPath())
		})
	}
}

// TestCloudRunRun_SharedDirs_FailsClosedBeforeProvisionOrAPICall verifies that
// whenever a shared dir cannot be served safely from the Cloud Run instance's
// unified NFS share, CloudRunRuntime.Run refuses cleanly before provisioning
// any NFS directories or calling the Cloud Run API.
func TestCloudRunRun_SharedDirs_FailsClosedBeforeProvisionOrAPICall(t *testing.T) {
	const otherPID = "99999999-8888-7777-6666-555555555555"

	cases := []struct {
		name    string
		mutate  func(rt *CloudRunRuntime, cfg *RunConfig)
		wantErr string
	}{
		{
			name: "nil SharedDirStorage (local shared-dir layout)",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage = nil
			},
			wantErr: "require NFS project storage",
		},
		{
			name: "gke-shared-volume backend",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.Backend = "gke-shared-volume"
			},
			wantErr: "gke-shared-volume",
		},
		{
			name: "cloudrun-volume backend",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.Backend = "cloudrun-volume"
			},
			wantErr: "cloudrun-volume",
		},
		{
			name: "legacy split (missing inline server/export)",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.Server = ""
				cfg.SharedDirStorage.Export = ""
				cfg.SharedDirStorage.SubPathRoot = ""
			},
			wantErr: "workspace NFS share",
		},
		{
			name: "server mismatch with workspace share",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.Server = "10.128.99.99"
			},
			wantErr: "does not match",
		},
		{
			name: "export mismatch with workspace share",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.Export = "/srv/other-export"
			},
			wantErr: "does not match",
		},
		{
			name: "subpath_root mismatch with workspace share",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.SubPathRoot = "other-root"
			},
			wantErr: "subpath_root",
		},
		{
			name: "project ID mismatch",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.ProjectID = otherPID
			},
			wantErr: "project ID",
		},
		{
			name: "invalid shared-dir name traversal",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirs = []api.SharedDir{{Name: "../escape"}}
			},
			wantErr: "invalid name",
		},
		{
			name: "duplicate shared-dir name",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirs = []api.SharedDir{{Name: "scratchpad"}, {Name: "scratchpad"}}
			},
			wantErr: "duplicate name",
		},
		{
			name: "missing resolved subPath",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				delete(cfg.SharedDirStorage.SubPaths, "scratchpad")
			},
			wantErr: "no resolved subPath",
		},
		{
			name: "subPath points to export root",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.SubPaths["scratchpad"] = "."
			},
			wantErr: "shared-dirs/scratchpad",
		},
		{
			name: "subPath points to project root",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.SubPaths["scratchpad"] = "projects/" + testCloudRunSharedDirPID
			},
			wantErr: "shared-dirs/scratchpad",
		},
		{
			name: "subPath points to project .scion config dir",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.SubPaths["scratchpad"] = "projects/" + testCloudRunSharedDirPID + "/.scion"
			},
			wantErr: "shared-dirs/scratchpad",
		},
		{
			name: "subPath points to another project",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.SubPaths["scratchpad"] = "projects/" + otherPID + "/shared-dirs/scratchpad"
			},
			wantErr: "not under this project's tree",
		},
		{
			name: "subPath traverses out of shared-dirs",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.SubPaths["scratchpad"] = "projects/" + testCloudRunSharedDirPID + "/shared-dirs/../workspace"
			},
			wantErr: "shared-dirs/scratchpad",
		},
		{
			name: "subPath nested under extra segments",
			mutate: func(_ *CloudRunRuntime, cfg *RunConfig) {
				cfg.SharedDirStorage.SubPaths["scratchpad"] = "projects/" + testCloudRunSharedDirPID + "/agents/" + testCloudRunSharedDirPID + "/shared-dirs/scratchpad"
			},
			wantErr: "does not match",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newReadyTestRuntime(t, fake)
			provisionCalled := false
			rt.provisionNFS = func(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error) {
				provisionCalled = true
				return stubCloudRunNFSProvision(ctx, cfg, agentID, uid, gid)
			}

			cfg := testCloudRunSharedDirRunConfig()
			tc.mutate(rt, &cfg)

			_, err := rt.Run(context.Background(), cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.False(t, provisionCalled, "NFS provisioning must not run when shared-dir validation fails")
			assert.Empty(t, fake.createReqs, "CreateInstance must not be called when shared-dir validation fails")
		})
	}
}
