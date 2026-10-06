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
	"strings"
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
// export only when the workspace is mounted from it, otherwise local.
func TestStart_ReportsWorkspacePlacement(t *testing.T) {
	cases := []struct {
		name     string
		runtime  string
		nfs      bool
		noPVName bool
		env      map[string]string
		gitClone *api.GitCloneConfig
		want     string
	}{
		{name: "k8s clone-per-agent on nfs", runtime: "kubernetes", nfs: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}, gitClone: testGitClone, want: api.WorkspacePlacementExport},
		{name: "k8s shared-plain on nfs", runtime: "kubernetes", nfs: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: testGitClone, want: api.WorkspacePlacementExport},
		// Regression: without pv_name the pod gets an EmptyDir, not the export.
		{name: "k8s clone-per-agent on nfs without pv_name", runtime: "kubernetes", nfs: true, noPVName: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}, gitClone: testGitClone, want: api.WorkspacePlacementLocal},
		{name: "k8s shared-plain on nfs without pv_name", runtime: "kubernetes", nfs: true, noPVName: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: testGitClone, want: api.WorkspacePlacementLocal},
		{name: "docker shared-plain on nfs", runtime: "docker", nfs: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: testGitClone, want: api.WorkspacePlacementExport},
		// Fail closed: runtimes not known to mount the export report local.
		{name: "cloudrun-sandbox shared-plain on nfs", runtime: "cloudrun-sandbox", nfs: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: testGitClone, want: api.WorkspacePlacementLocal},
		{name: "substrate shared-plain on nfs", runtime: "substrate", nfs: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: testGitClone, want: api.WorkspacePlacementLocal},
		{name: "unknown runtime shared-plain on nfs", runtime: "future-runtime", nfs: true,
			env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: testGitClone, want: api.WorkspacePlacementLocal},
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
				if tc.noPVName {
					extra = strings.Replace(extra, "          pv_name: ws-pv\n", "", 1)
					require.NotContains(t, extra, "pv_name")
				}
			}
			info := startForPlacement(t, tc.runtime, extra, tc.env, tc.gitClone)
			assert.Equal(t, tc.want, info.WorkspacePlacement)
		})
	}
}

func TestWorkspacePlacementFor(t *testing.T) {
	cases := []struct {
		name string
		in   workspacePlacementInput
		want string
	}{
		{"local backend", workspacePlacementInput{}, api.WorkspacePlacementLocal},
		{"cloudrun-volume", workspacePlacementInput{Backend: "cloudrun-volume", Runtime: "kubernetes", PVClaimName: "pv"}, api.WorkspacePlacementLocal},
		{"docker nfs bind mount", workspacePlacementInput{Backend: "nfs", Runtime: "docker"}, api.WorkspacePlacementExport},
		{"docker nfs clone-per-agent", workspacePlacementInput{Backend: "nfs", Runtime: "docker", ClonePerAgent: true}, api.WorkspacePlacementExport},
		{"podman nfs", workspacePlacementInput{Backend: "nfs", Runtime: "podman"}, api.WorkspacePlacementExport},
		{"apple container nfs", workspacePlacementInput{Backend: "nfs", Runtime: "container"}, api.WorkspacePlacementExport},
		{"cloudrun nfs", workspacePlacementInput{Backend: "nfs", Runtime: "cloudrun"}, api.WorkspacePlacementExport},
		{"cloudrun-sandbox nfs (copies to local disk)", workspacePlacementInput{Backend: "nfs", Runtime: "cloudrun-sandbox"}, api.WorkspacePlacementLocal},
		{"substrate nfs (never mounts)", workspacePlacementInput{Backend: "nfs", Runtime: "substrate"}, api.WorkspacePlacementLocal},
		{"unknown runtime nfs", workspacePlacementInput{Backend: "nfs", Runtime: "future-runtime"}, api.WorkspacePlacementLocal},
		{"no runtime name nfs", workspacePlacementInput{Backend: "nfs"}, api.WorkspacePlacementLocal},
		{"k8s nfs shared with claim", workspacePlacementInput{Backend: "nfs", Runtime: "kubernetes", PVClaimName: "pv"}, api.WorkspacePlacementExport},
		{"k8s nfs without claim (EmptyDir)", workspacePlacementInput{Backend: "nfs", Runtime: "kubernetes"}, api.WorkspacePlacementLocal},
		{"k8s nfs clone-per-agent with claim and agent dir", workspacePlacementInput{Backend: "nfs", Runtime: "kubernetes", PVClaimName: "pv", ClonePerAgent: true, AgentDirName: "a"}, api.WorkspacePlacementExport},
		{"k8s nfs clone-per-agent without agent dir", workspacePlacementInput{Backend: "nfs", Runtime: "kubernetes", PVClaimName: "pv", ClonePerAgent: true}, api.WorkspacePlacementLocal},
		{"k8s nfs clone-per-agent without claim", workspacePlacementInput{Backend: "nfs", Runtime: "kubernetes", ClonePerAgent: true, AgentDirName: "a"}, api.WorkspacePlacementLocal},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, workspacePlacementFor(tc.in), tc.name)
	}
}
