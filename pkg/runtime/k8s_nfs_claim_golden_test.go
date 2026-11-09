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
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// nfsClaimGoldenConfig is a shared-plain agent on an NFS workspace claim,
// with a read-write and a read-only shared dir on the same claim and a
// .scion directory in the workspace (the read-only project-config entry).
// As in production, the start also carries the share's server and export
// and the shared dirs' server, export and subPath root.
func nfsClaimGoldenConfig(t *testing.T) (RunConfig, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := RunConfig{
		Name:                   "test-agent",
		Image:                  "test:latest",
		UnixUsername:           "scion",
		Labels:                 map[string]string{"scion.project": "myproject"},
		Env:                    []string{"SCION_WORKSPACE_GIT=false"},
		Workspace:              ws,
		WorkspaceBackendName:   "nfs",
		NFSPVClaimName:         "scion-workspaces",
		NFSShareServer:         inlineTestServer,
		NFSShareExport:         inlineTestExport,
		NFSSubPath:             "projects/proj-123/workspace",
		ProjectID:              "proj-123",
		NFSWorkspacePreCreated: true,
		SharedDirs:             []api.SharedDir{{Name: "notes"}, {Name: "cache", ReadOnly: true, InWorkspace: true}},
		SharedDirStorage: &SharedDirRealization{
			Backend: "nfs", PVClaimName: "scion-workspaces", ProjectID: "proj-123",
			Server: inlineTestServer, Export: inlineTestExport, SubPathRoot: "projects",
			SubPaths: map[string]string{
				"notes": "projects/proj-123/shared-dirs/notes",
				"cache": "projects/proj-123/shared-dirs/cache",
			},
		},
	}
	return cfg, ws
}

// TestBuildPod_NFSClaimPodGolden pins the pod spec of NFS claim-mode pods
// (a pv_name share) byte for byte: the workspace, both shared dirs, the
// project-config entry and the home, on the workspace claim and on its own
// claim. Inline volumes for a share without a claim must leave these
// unchanged. Each case runs with the share's server, export and subPath
// root set (as production sets them) and without them; both must match the
// same snapshot.
func TestBuildPod_NFSClaimPodGolden(t *testing.T) {
	checkNFSClaimPodGolden(t, nil)
}

// TestBuildPod_NFSClaimPodGolden_HostGIDIsNFSGID runs the NFS claim goldens
// as on a host whose gid is the NFS default owner gid (1000), for example a
// pod running as 1000:1000. The pod FSGroup is that same gid and must match
// the snapshot, not the host-gid placeholder.
func TestBuildPod_NFSClaimPodGolden_HostGIDIsNFSGID(t *testing.T) {
	nfsGID := provision.DefaultOwnerID(0)
	orig := hostGIDForGolden
	hostGIDForGolden = func() int { return nfsGID }
	t.Cleanup(func() { hostGIDForGolden = orig })
	checkNFSClaimPodGolden(t, func(t *testing.T, pod *corev1.Pod) {
		sc := pod.Spec.SecurityContext
		if sc == nil || sc.FSGroup == nil {
			t.Fatalf("pod has no FSGroup, want the simulated host gid %d", nfsGID)
		}
		if *sc.FSGroup != int64(nfsGID) {
			t.Fatalf("pod FSGroup = %d, want the simulated host gid %d", *sc.FSGroup, nfsGID)
		}
	})
}

// checkNFSClaimPodGolden builds every NFS claim case in both share-address
// shapes and compares it with its snapshot. A non-nil checkPod also runs on
// each built pod before normalisation.
func checkNFSClaimPodGolden(t *testing.T, checkPod func(*testing.T, *corev1.Pod)) {
	t.Helper()
	withoutShareAddress := func(c *RunConfig) {
		c.NFSShareServer, c.NFSShareExport = "", ""
		sd := *c.SharedDirStorage
		sd.Server, sd.Export, sd.SubPathRoot = "", "", ""
		c.SharedDirStorage = &sd
	}
	cases := []struct {
		name   string
		mutate func(*RunConfig)
	}{
		{"nfs-claim-shared-plain", func(*RunConfig) {}},
		{"nfs-claim-home-own-claim", func(c *RunConfig) { *c = withTestHomeStorage(*c) }},
		{"nfs-claim-home-workspace-claim", func(c *RunConfig) {
			*c = withTestHomeStorage(*c)
			c.HomeStorage.PVClaimName = "scion-workspaces"
			c.HomeStorage.ProjectID = "proj-123"
		}},
	}
	for _, tc := range cases {
		for _, shape := range []string{"with-share-address", "without-share-address"} {
			t.Run(tc.name+"/"+shape, func(t *testing.T) {
				cfg, ws := nfsClaimGoldenConfig(t)
				if shape == "without-share-address" {
					withoutShareAddress(&cfg)
				}
				tc.mutate(&cfg)
				rt, _, _ := newTestK8sRuntime()
				pod, err := rt.buildPod("default", cfg)
				if err != nil {
					t.Fatalf("buildPod: %v", err)
				}
				if checkPod != nil {
					checkPod(t, pod)
				}
				normalizeHostIDs(pod, cfg)
				got, err := json.MarshalIndent(pod, "", "  ")
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				got = append(bytes.ReplaceAll(got, []byte(ws), []byte("/WORKSPACE")), '\n')
				file := filepath.Join("testdata", "k8s_pod_golden", tc.name+".json")
				if *updatePodGolden {
					if shape == "with-share-address" {
						if err := os.WriteFile(file, got, 0o644); err != nil {
							t.Fatal(err)
						}
					}
					return
				}
				want, err := os.ReadFile(file)
				if err != nil {
					t.Fatalf("read golden: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("pod spec for %s (%s) differs from %s:\n%s", tc.name, shape, file, got)
				}
			})
		}
	}
}
