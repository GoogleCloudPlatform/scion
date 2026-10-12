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

// This file covers ptone/scion#1956's one handler-level gap: when a
// non-gather create finds the broker still reports required env vars
// missing (handlers_agents_core.go's "Broker reported missing required env
// vars" branch, mirroring TestNonGatherEnv_MissingEnvVars_Returns422 in
// envgather_test.go), DispatchAgentCreateWithGather returned that as a
// value, not an error, so its own revoke-on-error defer never ran — the
// credential it minted must still be revoked here, before the row is
// deleted.
package hub

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateAgent_NonGatherEnvMissingVars_RevokesCredential(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("project-revoke-missing"), Name: "revoke-missing-project", Slug: "revoke-missing-project"}
	require.NoError(t, st.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: tid("broker-revoke-missing"), Name: "revoke-missing-broker", Slug: "revoke-missing-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: tid("project-revoke-missing"), BrokerID: tid("broker-revoke-missing"), BrokerName: "test-broker",
		LocalPath: "/tmp/test-project",
	}))

	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"ANTHROPIC_API_KEY"},
			HubHas:   []string{},
			Needs:    []string{"ANTHROPIC_API_KEY"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	gen := &fakeMintingTokenGenerator{store: st}
	dispatcher.SetTokenGenerator(gen)
	srv.SetDispatcher(dispatcher)

	reqBody := map[string]interface{}{
		"name":      "revoke-missing-agent",
		"projectId": tid("project-revoke-missing"),
		"template":  "claude",
		// gatherEnv is NOT set, matching TestNonGatherEnv_MissingEnvVars_Returns422.
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	require.Len(t, gen.jtis, 1, "expected exactly one credential to have been minted for the failed create")
	cred := getTestAgentCredential(t, st, gen.lastJTI())
	require.NotNil(t, cred.RevokedAt, "the credential minted for a create that fails on missing env vars must be revoked")
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)

	// Unchanged behavior: the agent row is still cleaned up.
	result, err := st.ListAgents(ctx, store.AgentFilter{ProjectID: tid("project-revoke-missing")}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items, "expected the agent row to still be cleaned up")
}
