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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A shared-plain git project's workspace clone settings arrive as
// GitCloneForInit, with no worktree or agent-directory settings. Every agent
// gets the same provisioning init container: a full clone into the shared
// workspace in shared-plain mode. sciontool provision clones only when the
// workspace is not provisioned yet, so later agents reuse the clone.
func TestBuildPod_NFSSharedPlainGit_InitContainerClonesSharedWorkspace(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "shared-agent",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		ProjectID:            "proj-123",
		GitCloneForInit: &api.GitCloneConfig{
			URL:    "https://github.com/example/shared.git",
			Branch: "main",
			Depth:  intPtr(0),
		},
	}

	pod, err := r.buildPod("default", config)
	require.NoError(t, err)
	require.Len(t, pod.Spec.InitContainers, 1)
	ic := pod.Spec.InitContainers[0]

	require.GreaterOrEqual(t, len(ic.Command), 4)
	assert.Equal(t, []string{"sciontool", "provision", "--depth", "0"}, ic.Command[:4])
	assert.NotContains(t, ic.Command, "--wait-for-sentinel")

	env := map[string]string{}
	for _, e := range ic.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "https://github.com/example/shared.git", env["SCION_CLONE_URL"])
	assert.Equal(t, "main", env["SCION_CLONE_BRANCH"])
	assert.Equal(t, "proj-123", env["SCION_PROJECT_ID"])
	// No mode or agent settings: sciontool provision defaults to shared-plain
	// and provisions the shared workspace itself.
	for _, name := range []string{"SCION_WORKSPACE_MODE", "SCION_AGENT_SLUG", "SCION_AGENT_BRANCH"} {
		assert.NotContains(t, env, name)
	}

	var wsSubPath string
	for _, m := range ic.VolumeMounts {
		if m.Name == "workspace" {
			wsSubPath = m.SubPath
		}
	}
	assert.Equal(t, "projects/proj-123/workspace", wsSubPath)
}
