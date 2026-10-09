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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nfsProvisionPinCases are the provisioning init container shapes of each
// NFS workspace mode: shared-plain with a git token and a pre-created
// workspace, a non-git project, worktree-per-agent with a git token,
// clone-per-agent with a pre-created workspace, and empty-per-agent.
func nfsProvisionPinCases() map[string]RunConfig {
	sp := nfsBaseConfig("pin-sp")
	sp.ResolvedSecrets = []api.ResolvedSecret{gitTokenSecret()}
	sp.NFSWorkspacePreCreated = true
	ng := nfsBaseConfig("pin-ng")
	ng.GitCloneForInit = nil
	wt := nfsWorktreeConfig("pin-wt")
	wt.ResolvedSecrets = []api.ResolvedSecret{gitTokenSecret()}
	cpa := nfsAgentDirConfig("pin-cpa")
	cpa.NFSWorkspacePreCreated = true
	epa := nfsEmptyAgentDirConfig("pin-epa")
	return map[string]RunConfig{
		"shared-plain":    sp,
		"non-git":         ng,
		"worktree":        wt,
		"clone-per-agent": cpa,
		"empty-per-agent": epa,
	}
}

// nfsProvisionPinWant is the JSON of each case's init containers: the
// command, env, mounts and security context of the provisioning init
// container, exactly as buildPod produces them.
var nfsProvisionPinWant = map[string]string{
	"shared-plain":    `[{"name":"workspace-provision","image":"test-image","command":["sciontool","provision","--depth","1","--uid","1000","--gid","1000"],"env":[{"name":"SCION_CLONE_URL","value":"https://github.com/example/repo.git"},{"name":"SCION_CLONE_BRANCH","value":"main"},{"name":"SCION_PROVISION_STATE_DIR","value":"/scion-provision"},{"name":"SCION_PROJECT_ID","value":"proj-123"},{"name":"GITHUB_TOKEN","valueFrom":{"secretKeyRef":{"name":"scion-agent-pin-sp","key":"GITHUB_TOKEN"}}},{"name":"SCION_PROVISION_CHOWN_BEST_EFFORT","value":"1"}],"resources":{},"volumeMounts":[{"name":"workspace","mountPath":"/workspace","subPath":"projects/proj-123/workspace"},{"name":"workspace","mountPath":"/scion-provision","subPath":"projects/proj-123/provision"}],"securityContext":{"capabilities":{"add":["CHOWN","FOWNER","DAC_OVERRIDE"],"drop":["ALL"]},"runAsUser":0,"runAsGroup":0,"runAsNonRoot":false,"allowPrivilegeEscalation":false}}]`,
	"non-git":         `[{"name":"workspace-provision","image":"test-image","command":["sciontool","provision","--uid","1000","--gid","1000"],"env":[{"name":"SCION_PROVISION_STATE_DIR","value":"/scion-provision"},{"name":"SCION_PROJECT_ID","value":"proj-123"}],"resources":{},"volumeMounts":[{"name":"workspace","mountPath":"/workspace","subPath":"projects/proj-123/workspace"},{"name":"workspace","mountPath":"/scion-provision","subPath":"projects/proj-123/provision"}],"securityContext":{"capabilities":{"add":["CHOWN","FOWNER","DAC_OVERRIDE"],"drop":["ALL"]},"runAsUser":0,"runAsGroup":0,"runAsNonRoot":false,"allowPrivilegeEscalation":false}}]`,
	"worktree":        `[{"name":"workspace-provision","image":"test-image","command":["sciontool","provision","--depth","1","--uid","1000","--gid","1000"],"env":[{"name":"SCION_CLONE_URL","value":"https://github.com/example/repo.git"},{"name":"SCION_CLONE_BRANCH","value":"main"},{"name":"SCION_PROVISION_STATE_DIR","value":"/scion-provision"},{"name":"SCION_PROJECT_ID","value":"proj-123"},{"name":"GITHUB_TOKEN","valueFrom":{"secretKeyRef":{"name":"scion-agent-pin-wt","key":"GITHUB_TOKEN"}}},{"name":"SCION_WORKSPACE_MODE","value":"worktree-per-agent"},{"name":"SCION_AGENT_SLUG","value":"agent-1"},{"name":"SCION_AGENT_BRANCH","value":"agent-one"}],"resources":{},"volumeMounts":[{"name":"workspace","mountPath":"/workspace","subPath":"projects/proj-123/workspace"},{"name":"workspace","mountPath":"/scion-provision","subPath":"projects/proj-123/provision"}],"securityContext":{"capabilities":{"add":["CHOWN","FOWNER","DAC_OVERRIDE"],"drop":["ALL"]},"runAsUser":0,"runAsGroup":0,"runAsNonRoot":false,"allowPrivilegeEscalation":false}}]`,
	"clone-per-agent": `[{"name":"workspace-provision","image":"test-image","command":["sciontool","provision","--uid","1000","--gid","1000"],"env":[{"name":"SCION_PROJECT_ID","value":"proj-123"},{"name":"SCION_WORKSPACE_MODE","value":"clone-per-agent"},{"name":"SCION_AGENT_SLUG","value":"agent-1"},{"name":"SCION_AGENT_BRANCH","value":"scion/agent-1"},{"name":"SCION_PROVISION_CHOWN_BEST_EFFORT","value":"1"}],"resources":{},"volumeMounts":[{"name":"workspace","mountPath":"/workspace","subPath":"projects/proj-123/agents/agent-1"}],"securityContext":{"capabilities":{"add":["CHOWN","FOWNER","DAC_OVERRIDE"],"drop":["ALL"]},"runAsUser":0,"runAsGroup":0,"runAsNonRoot":false,"allowPrivilegeEscalation":false}}]`,
	"empty-per-agent": `[{"name":"workspace-provision","image":"test-image","command":["sciontool","provision","--uid","1000","--gid","1000"],"env":[{"name":"SCION_PROJECT_ID","value":"proj-123"},{"name":"SCION_WORKSPACE_MODE","value":"empty-per-agent"},{"name":"SCION_AGENT_SLUG","value":"agent-1"}],"resources":{},"volumeMounts":[{"name":"workspace","mountPath":"/workspace","subPath":"projects/proj-123/agents/agent-1"}],"securityContext":{"capabilities":{"add":["CHOWN","FOWNER","DAC_OVERRIDE"],"drop":["ALL"]},"runAsUser":0,"runAsGroup":0,"runAsNonRoot":false,"allowPrivilegeEscalation":false}}]`,
}

// TestBuildPod_NFSProvisionInitContainer_Pinned pins the provisioning init
// container of every NFS workspace mode byte for byte, so a change to its
// command, env, mounts or security context is a deliberate one.
func TestBuildPod_NFSProvisionInitContainer_Pinned(t *testing.T) {
	cases := nfsProvisionPinCases()
	require.Len(t, nfsProvisionPinWant, len(cases))
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			want, ok := nfsProvisionPinWant[name]
			require.True(t, ok, "no pinned value for %s", name)
			pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
			require.NoError(t, err)
			got, err := json.Marshal(pod.Spec.InitContainers)
			require.NoError(t, err)
			assert.Equal(t, want, string(got))
		})
	}
}
