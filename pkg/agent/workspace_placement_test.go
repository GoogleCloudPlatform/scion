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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startForPlacement runs Manager.Start against a mock runtime and returns
// the info Start reports. extraServerYAML is spliced under "server:" in the
// global settings ("" for no workspace storage).
func startForPlacement(t *testing.T, runtimeName, extraServerYAML string, env map[string]string, gitClone *api.GitCloneConfig) *api.AgentInfo {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, extraServerYAML)
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	fullEnv := map[string]string{"SCION_PROJECT_ID": testNFSWorkspaceProjectID}
	for k, v := range env {
		fullEnv[k] = v
	}
	info, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         fullEnv,
		GitClone:    gitClone,
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	return info
}

// Every start that resolves the workspace reports its placement: on the
// export when the nfs workspace backend served it, otherwise local.
func TestStart_ReportsWorkspacePlacement(t *testing.T) {
	cases := []struct {
		name     string
		runtime  string
		nfs      bool
		env      map[string]string
		gitClone *api.GitCloneConfig
		want     string
	}{
		{name: "k8s clone-per-agent on nfs", runtime: "kubernetes", nfs: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}, gitClone: testGitClone, want: api.WorkspacePlacementExport},
		{name: "k8s shared-plain on nfs", runtime: "kubernetes", nfs: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: testGitClone, want: api.WorkspacePlacementExport},
		{name: "k8s clone-per-agent without workspace storage", runtime: "kubernetes",
			env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}, gitClone: testGitClone, want: api.WorkspacePlacementLocal},
		{name: "docker without workspace storage", runtime: "docker", want: api.WorkspacePlacementLocal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := ""
			if tc.nfs {
				mountRoot := filepath.Join(t.TempDir(), "nfs")
				require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
				extra = fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot)
			}
			info := startForPlacement(t, tc.runtime, extra, tc.env, tc.gitClone)
			assert.Equal(t, tc.want, info.WorkspacePlacement)
		})
	}
}

func TestWorkspacePlacementFor(t *testing.T) {
	assert.Equal(t, api.WorkspacePlacementExport, workspacePlacementFor("nfs"))
	assert.Equal(t, api.WorkspacePlacementLocal, workspacePlacementFor(""))
	assert.Equal(t, api.WorkspacePlacementLocal, workspacePlacementFor("cloudrun-volume"))
}
