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

package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testOrphaned = &api.AgentAuthority{
		State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorDeleted,
		Principal: &api.AgentAuthorityPrincipal{Kind: "agent", ID: "0d9f6c1e-1111-4222-8333-444455556666", Deleted: true},
	}
	testRevoked = &api.AgentAuthority{
		State: api.AgentAuthorityRevoked, Cause: "user_delete_cascade",
		Principal: &api.AgentAuthorityPrincipal{Kind: "user", Deleted: true},
	}
)

func TestCLIShowsAgentAuthority_HumanModeOnly(t *testing.T) {
	assert.True(t, cliShowsAgentAuthority(ModeHuman))
	assert.False(t, cliShowsAgentAuthority(ModeAgent))
}

func TestAuthorityPhaseLabel(t *testing.T) {
	assert.Equal(t, "running (ORPHANED)", authorityPhaseLabel("running", testOrphaned))
	assert.Equal(t, "stopped (REVOKED)", authorityPhaseLabel("stopped", testRevoked))
	assert.Equal(t, "running", authorityPhaseLabel("running", &api.AgentAuthority{State: api.AgentAuthorityLive}))
	assert.Equal(t, "running", authorityPhaseLabel("running", &api.AgentAuthority{State: api.AgentAuthorityUnrecorded}))
	assert.Equal(t, "running", authorityPhaseLabel("running", nil))
}

func TestAuthorityCauseLine(t *testing.T) {
	assert.Equal(t,
		"Authority: ORPHANED (delegator_deleted, agent 0d9f6c1e-1111-4222-8333-444455556666 deleted); requests that need its delegation are denied. Stop or delete this agent.",
		authorityCauseLine(testOrphaned))
	assert.Equal(t,
		"Authority: REVOKED (user_delete_cascade, user deleted); requests that need its delegation are denied. Stop or delete this agent.",
		authorityCauseLine(testRevoked))
	assert.Equal(t,
		"Authority: ORPHANED (delegator_inactive, user); requests that need its delegation are denied. Stop or delete this agent.",
		authorityCauseLine(&api.AgentAuthority{State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorInactive,
			Principal: &api.AgentAuthorityPrincipal{Kind: "user"}}))
	assert.Empty(t, authorityCauseLine(&api.AgentAuthority{State: api.AgentAuthorityLive}))
	assert.Empty(t, authorityCauseLine(nil))

	// Hub strings outside the known set never reach the terminal.
	odd := authorityCauseLine(&api.AgentAuthority{State: api.AgentAuthorityRevoked, Cause: "evil\x1b[2J",
		Principal: &api.AgentAuthorityPrincipal{Kind: "user", ID: "x\x1b[2J"}})
	assert.Equal(t, "Authority: REVOKED (unknown, user); requests that need its delegation are denied. Stop or delete this agent.", odd)
	assert.Empty(t, authorityPrincipalText(&api.AgentAuthorityPrincipal{Kind: "group\x1b"}))
}

// The scion list table marks orphaned and revoked agents in the PHASE
// column (golden output).
func TestDisplayAgents_AuthorityMarkerGolden(t *testing.T) {
	prev := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prev })

	row := func(name, phase, container string, auth *api.AgentAuthority) api.AgentInfo {
		return hubAgentToAgentInfo(hubclient.Agent{
			Slug: name, Template: "default", HarnessConfig: "claude", Runtime: "docker", Project: "proj",
			RuntimeBrokerName: "b1", Phase: phase, ContainerStatus: container, Authority: auth,
		})
	}
	agents := []api.AgentInfo{
		row("live-agent", "running", "Up", &api.AgentAuthority{State: api.AgentAuthorityLive}),
		row("orphan-agent", "running", "Up", testOrphaned),
		row("revoked-agent", "stopped", "Exited", testRevoked),
	}
	assert.Equal(t, testOrphaned, agents[1].Authority, "hubAgentToAgentInfo carries the authority")

	var err error
	out := captureStdout(t, func() { err = displayAgents(agents, false) })
	require.NoError(t, err)
	const want = "" +
		"NAME           TEMPLATE  HARNESS-CFG  RUNTIME  PROJECT  BROKER  PHASE               CONTAINER  LAST ACTIVITY\n" +
		"live-agent     default   claude       docker   proj     b1      running             Up         -\n" +
		"orphan-agent   default   claude       docker   proj     b1      running (ORPHANED)  Up         -\n" +
		"revoked-agent  default   claude       docker   proj     b1      stopped (REVOKED)   Exited     -\n"
	assert.Equal(t, want, out)
}

// scion list asks for authority states in human mode and not in agent
// mode.
func TestListAgentsViaHub_IncludeAuthorityByMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"human", "authority"},
		{"agent", ""},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("SCION_CLI_MODE", tc.mode)
			var mu sync.Mutex
			var include []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/agents" {
					mu.Lock()
					include = append(include, r.URL.Query().Get("include"))
					mu.Unlock()
				}
				_, _ = w.Write([]byte(`{"agents": [{"id": "a1", "slug": "orphan", "projectId": "p1", "phase": "running",
					"authority": {"state": "orphaned", "cause": "delegator_deleted", "principal": {"kind": "user", "deleted": true}}}]}`))
			}))
			defer server.Close()
			client, err := hubclient.New(server.URL)
			require.NoError(t, err)

			oldListAll, oldOutputFormat := listAll, outputFormat
			listAll, outputFormat = true, ""
			defer func() { listAll, outputFormat = oldListAll, oldOutputFormat }()

			stdout, _ := captureStdoutStderr(t, func() {
				require.NoError(t, listAgentsViaHub(&HubContext{Client: client, Endpoint: server.URL}))
			})
			mu.Lock()
			defer mu.Unlock()
			require.NotEmpty(t, include)
			assert.Equal(t, tc.want, include[0])
			assert.Contains(t, stdout, "running (ORPHANED)", "a Hub answer that carries the state is shown")
		})
	}
}

// scion look adds the cause line after the identity header when asked to,
// and prints it alone when no identity is recorded.
func TestFetchLookIdentityHeader_AuthorityLine(t *testing.T) {
	const orphanJSON = `{"id": "a1", "slug": "worker", "appliedConfig": {"profile": "gke", "gcpIdentity": {"metadataMode": "block"}},
		"authority": {"state": "revoked", "cause": "user_delete_cascade", "principal": {"kind": "user", "deleted": true}}}`
	const noIdentityJSON = `{"id": "a1", "slug": "worker",
		"authority": {"state": "revoked", "cause": "user_delete_cascade", "principal": {"kind": "user", "deleted": true}}}`
	line := "Authority: REVOKED (user_delete_cascade, user deleted); requests that need its delegation are denied. Stop or delete this agent."

	for _, tc := range []struct {
		name      string
		agentJSON string
		show      bool
		want      string
	}{
		{"identity and cause", orphanJSON, true, "GCP identity: block (profile: gke)\n" + line},
		{"cause only", noIdentityJSON, true, line},
		{"not shown", orphanJSON, false, "GCP identity: block (profile: gke)"},
		{"not shown, no identity", noIdentityJSON, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := lookIdentityServer(t, tc.agentJSON, http.StatusOK)
			defer server.Close()
			client, err := hubclient.New(server.URL)
			require.NoError(t, err)
			got := fetchLookIdentityHeader(context.Background(), client, client.ProjectAgents("p1"), "p1", "worker", tc.show)
			assert.Equal(t, tc.want, got)
		})
	}
}
