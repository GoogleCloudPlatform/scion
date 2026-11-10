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

package hub

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchEnvTestDevToken is a made-up dev token for the server built by
// newDispatchEnvFixture. It is never a real credential.
const dispatchEnvTestDevToken = "scion_dev_made_up_dispatch_env_token_0000000000"

// dispatchEnvFixture is a server whose dispatcher comes from
// CreateAuthenticatedDispatcher (the factory the running hub uses), with a
// recording broker client and one stored agent.
type dispatchEnvFixture struct {
	srv    *Server
	client *mockRuntimeBrokerClient
	d      *HTTPAgentDispatcher
	agent  *store.Agent
}

// newDispatchEnvFixture builds the fixture with the given dev auth token
// ("" turns dev auth off).
func newDispatchEnvFixture(t *testing.T, devToken string) *dispatchEnvFixture {
	t.Helper()
	ctx := context.Background()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	// See testServerOnMigratedStore: the agent below is written straight
	// to the store, with no delegation edge.
	_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")
	cfg := testServerConfig()
	cfg.DevAuthToken = devToken
	srv, _ := testServerWithStoreConfig(t, s, cfg)

	project := setupProjectWithBroker(t, s, "dispenv", "dispenv")
	userID := tid("dispenv-user")
	createDCUser(t, s, userID, "dispenv-user@test.com", project.ID, store.ProjectRoleOwner)
	agent := &store.Agent{
		ID: tid("dispenv-agent"), Slug: "dispenv-agent", Name: "dispenv-agent",
		ProjectID: project.ID, OwnerID: userID, RuntimeBrokerID: tid("broker-dispenv"),
		StateVersion: 1, Ancestry: []string{userID},
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Env:           map[string]string{"EXISTING_VAR": "existing-value"},
		},
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	d := srv.CreateAuthenticatedDispatcher()
	client := &mockRuntimeBrokerClient{}
	d.client = client
	return &dispatchEnvFixture{srv: srv, client: client, d: d, agent: agent}
}

// dispatchEnvPaths are the dispatches that build their request with
// buildCreateRequestFor and send it as a create request.
var dispatchEnvPaths = []struct {
	name     string
	dispatch func(ctx context.Context, f *dispatchEnvFixture) error
}{
	{"create", func(ctx context.Context, f *dispatchEnvFixture) error {
		_, err := f.d.DispatchAgentCreate(ctx, f.agent)
		return err
	}},
	{"provision", func(ctx context.Context, f *dispatchEnvFixture) error {
		return f.d.DispatchAgentProvision(ctx, f.agent)
	}},
	{"create with env gather", func(ctx context.Context, f *dispatchEnvFixture) error {
		_, err := f.d.DispatchAgentCreateWithGather(ctx, f.agent)
		return err
	}},
	{"finalize env", func(ctx context.Context, f *dispatchEnvFixture) error {
		_, err := f.d.DispatchFinalizeEnv(ctx, f.agent, map[string]string{"CLI_VAR": "cli-value"})
		return err
	}},
}

// dispatchedEnv runs one dispatch on a fresh fixture and returns the
// request the broker received.
func dispatchedEnv(t *testing.T, devToken string, dispatch func(context.Context, *dispatchEnvFixture) error) (*dispatchEnvFixture, *RemoteCreateAgentRequest) {
	t.Helper()
	f := newDispatchEnvFixture(t, devToken)
	require.NoError(t, dispatch(context.Background(), f))
	require.True(t, f.client.createCalled, "the broker received a create request")
	require.NotNil(t, f.client.lastCreateReq)
	return f, f.client.lastCreateReq
}

// TestDispatchedAgentEnvCarriesNoDevToken: with dev auth on, every
// create-shaped dispatch sends the agent its minted agent token and none of
// the hub's dev token, and the resolved env has exactly the keys the same
// dispatch sends with dev auth off.
func TestDispatchedAgentEnvCarriesNoDevToken(t *testing.T) {
	for _, path := range dispatchEnvPaths {
		t.Run(path.name, func(t *testing.T) {
			devF, devReq := dispatchedEnv(t, dispatchEnvTestDevToken, path.dispatch)
			offF, offReq := dispatchedEnv(t, "", path.dispatch)

			assert.Equal(t, slices.Sorted(maps.Keys(offReq.ResolvedEnv)), slices.Sorted(maps.Keys(devReq.ResolvedEnv)),
				"dev auth adds no resolved env key")
			assert.Equal(t, slices.Sorted(maps.Keys(offReq.EnvClassifications)), slices.Sorted(maps.Keys(devReq.EnvClassifications)),
				"dev auth adds no env classification")
			assert.NotContains(t, devReq.ResolvedEnv, "SCION_DEV_TOKEN")
			assert.NotContains(t, devReq.EnvClassifications, "SCION_DEV_TOKEN")
			assert.Equal(t, "existing-value", devReq.ResolvedEnv["EXISTING_VAR"], "the agent's own env is still sent")
			for k, v := range devReq.ResolvedEnv {
				assert.NotContains(t, v, dispatchEnvTestDevToken, "resolved env %s carries the dev token", k)
			}
			for _, rs := range devReq.ResolvedSecrets {
				assert.NotContains(t, rs.Value, dispatchEnvTestDevToken, "resolved secret %s carries the dev token", rs.Name)
			}

			// The agent authenticates with its minted token. With dev auth on
			// the mint raises the agent's role to full (mintDevAuthOverride),
			// so the token can create agents; with dev auth off the fixture
			// agent has no role and gets the default status scope only.
			require.NotEmpty(t, devReq.AgentToken, "the minted agent token is sent")
			assert.False(t, strings.Contains(devReq.AgentToken, dispatchEnvTestDevToken), "the agent token is not the dev token")
			devClaims, err := devF.srv.agentTokenService.ValidateAgentToken(devReq.AgentToken)
			require.NoError(t, err, "the agent token is one the hub minted")
			assert.Contains(t, devClaims.Scopes, ScopeAgentCreate, "dev auth mints the full role")
			require.NotEmpty(t, offReq.AgentToken, "the minted agent token is sent with dev auth off")
			offClaims, err := offF.srv.agentTokenService.ValidateAgentToken(offReq.AgentToken)
			require.NoError(t, err, "the agent token is one the hub minted with dev auth off")
			assert.Equal(t, []AgentTokenScope{ScopeAgentStatusUpdate}, offClaims.Scopes, "without dev auth the role-less agent gets the default scope only")
		})
	}
}
