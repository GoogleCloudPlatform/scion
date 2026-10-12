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

//go:build !no_sqlite

// This file covers ptone/scion#1956's handleExistingAgent "Phase 2:
// env-gather re-provisioning" path (handlers_agent_create_helpers.go ~1190):
// recreating a still-provisioning agent hard-deletes the old row outside the
// main delete handler, so it must revoke the old row's credential the same
// way that handler does. It also covers the ordering condition: the revoke
// is scoped to the OLD agent's id, so the brand new agent row the
// fall-through create mints its own credential for — a different id — must
// come out of this untouched.
package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateAgent_EnvGatherRecreate_RevokesOldCredentialOnly(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("project-recreate-revoke"), Name: "recreate-revoke-project", Slug: "recreate-revoke-project"}
	require.NoError(t, st.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: tid("broker-recreate-revoke"), Name: "recreate-revoke-broker", Slug: "recreate-revoke-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: tid("project-recreate-revoke"), BrokerID: tid("broker-recreate-revoke"), BrokerName: "test-broker",
		LocalPath: "/tmp/test-project",
	}))

	// Every CreateAgentWithGather call (both the initial create and the
	// fall-through recreate below) reports the same still-missing key, so
	// both land the agent in PhaseProvisioning with a 202.
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"GEMINI_API_KEY"},
			Needs:    []string{"GEMINI_API_KEY"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	gen := &fakeMintingTokenGenerator{store: st}
	dispatcher.SetTokenGenerator(gen)
	srv.SetDispatcher(dispatcher)

	reqBody := map[string]interface{}{
		"name":      "recreate-revoke-agent",
		"projectId": tid("project-recreate-revoke"),
		"template":  "claude",
		"gatherEnv": true,
	}

	// First create: lands in PhaseProvisioning, mints credential #1.
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)
	require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
	var resp1 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	require.Equal(t, string(state.PhaseProvisioning), resp1.Agent.Phase)
	oldAgentID := resp1.Agent.ID
	require.Len(t, gen.jtis, 1)
	oldJTI := gen.lastJTI()

	// Second create with the same name: handleExistingAgent's Phase 2 branch
	// tears down the still-provisioning row and falls through to a fresh
	// create, which mints credential #2 for a new agent id.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)
	require.Equal(t, http.StatusAccepted, rec2.Code, rec2.Body.String())
	var resp2 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	newAgentID := resp2.Agent.ID
	require.NotEqual(t, oldAgentID, newAgentID, "recreate must allocate a fresh agent id, not reuse the old one")
	require.Len(t, gen.jtis, 2, "expected a second credential minted for the recreated agent")
	newJTI := gen.lastJTI()

	oldCred := getTestAgentCredential(t, st, oldJTI)
	require.NotNil(t, oldCred.RevokedAt, "the old provisioning agent's credential must be revoked on teardown-before-recreate")
	require.NotNil(t, oldCred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonDeleted, *oldCred.RevokeReason, "teardown-before-recreate hard-deletes the old row exactly like the main delete handler, so it must record the same reason")

	newCred := getTestAgentCredential(t, st, newJTI)
	assert.Nil(t, newCred.RevokedAt, "the recreated agent's own fresh credential must stay active — it must not be swept up by the old agent's revoke")
}
