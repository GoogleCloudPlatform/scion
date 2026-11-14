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
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// vertexGatherBroker is a broker whose reprovision gather asks for the
// Vertex AI discovery keys on the first pass and accepts the replay, like a
// broker reprovisioning an agent onto a vertex-ai harness-config whose keys
// are stored as as_needed hub env vars.
func vertexGatherBroker() *mockRuntimeBrokerClient {
	return &mockRuntimeBrokerClient{
		createWithGatherFunc: func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			if req.ResolvedEnv["GOOGLE_CLOUD_PROJECT"] == "" {
				return nil, &RemoteEnvRequirementsResponse{
					AgentID: req.ID,
					Needs:   []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"},
				}, nil
			}
			return &RemoteAgentResponse{
				Agent:         &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name},
				Created:       true,
				Reprovisioned: true,
				HomeReset:     req.ResetHome,
			}, nil, nil
		},
	}
}

func vertexAsNeededAgent(t *testing.T, ctx context.Context, memStore store.Store) *store.Agent {
	t.Helper()
	agent := setupProvisionEnvTest(t, ctx, memStore, []store.EnvVar{
		{Key: "GOOGLE_CLOUD_PROJECT", Value: "vertex-project-value"},
		{Key: "GOOGLE_CLOUD_REGION", Value: "vertex-region-value"},
	})
	agent.AppliedConfig.HarnessConfig = "claude"
	return agent
}

// TestReincarnateHomeResetStartCarriesReprovisionGatheredEnv is the
// ptone/scion#7757 regression test: after a harness switch, the start that
// follows the home-reset reprovision must carry the as_needed env the
// reprovision gather resolved for the new harness. A start resolves no
// as_needed env itself, and the broker's recorded secrets belong to the old
// harness-config, so without this the new harness starts without its Vertex
// AI project and region and its provisioner fails in the pre-start hook.
func TestReincarnateHomeResetStartCarriesReprovisionGatheredEnv(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	agent := vertexAsNeededAgent(t, ctx, memStore)
	mockClient := vertexGatherBroker()
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	wctx := withReprovisionGatheredEnv(ctx)
	require.NoError(t, dispatcher.DispatchAgentReprovision(wctx, agent, ReprovisionOptions{ResetHome: true, ResetID: "reset-1"}))
	require.NoError(t, dispatcher.DispatchAgentStart(wctx, agent, "task", false))

	require.True(t, mockClient.startCalled)
	assert.Equal(t, "vertex-project-value", mockClient.lastResolvedEnv["GOOGLE_CLOUD_PROJECT"])
	assert.Equal(t, "vertex-region-value", mockClient.lastResolvedEnv["GOOGLE_CLOUD_REGION"])
	for _, k := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		kind, ok := api.ClassifyEnvKey(mockClient.lastStartExtras.EnvClassifications, k)
		require.True(t, ok, "%s must carry a classification", k)
		assert.Equal(t, api.EnvKindSecretFetchable, kind, "%s is as_needed storage env", k)
	}
	// Nothing is persisted to the applied config.
	assert.NotContains(t, agent.AppliedConfig.Env, "GOOGLE_CLOUD_PROJECT")
	assert.NotContains(t, agent.AppliedConfig.Env, "GOOGLE_CLOUD_REGION")
}

// TestPlainReprovisionStartUnchanged pins that a reincarnation without a
// home reset (no holder in ctx) starts exactly as before: the start carries
// no as_needed env, whatever the reprovision gather resolved.
func TestPlainReprovisionStartUnchanged(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	agent := vertexAsNeededAgent(t, ctx, memStore)
	mockClient := vertexGatherBroker()
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	require.NoError(t, dispatcher.DispatchAgentReprovision(ctx, agent, ReprovisionOptions{}))
	require.NoError(t, dispatcher.DispatchAgentStart(ctx, agent, "task", false))

	require.True(t, mockClient.startCalled)
	assert.NotContains(t, mockClient.lastResolvedEnv, "GOOGLE_CLOUD_PROJECT")
	assert.NotContains(t, mockClient.lastResolvedEnv, "GOOGLE_CLOUD_REGION")
}

// TestMergeReprovisionGatheredEnvFillOnly pins that the carried env never
// replaces a value the start resolved itself, and fills an empty one.
func TestMergeReprovisionGatheredEnvFillOnly(t *testing.T) {
	ctx := withReprovisionGatheredEnv(context.Background())
	recordReprovisionGatheredEnv(ctx, map[string]string{"KEPT": "gathered", "FILLED": "gathered", "NEW": "gathered", "BLANK": ""})

	env := map[string]string{"KEPT": "start", "FILLED": ""}
	var cls map[string]api.EnvKind
	classifyEnv(&cls, "KEPT", api.EnvKindPlain)
	mergeReprovisionGatheredEnv(ctx, env, &cls)

	assert.Equal(t, map[string]string{"KEPT": "start", "FILLED": "gathered", "NEW": "gathered"}, env)
	assert.Equal(t, api.EnvKindPlain, cls["KEPT"])
	assert.Equal(t, api.EnvKindSecretFetchable, cls["FILLED"])
	assert.Equal(t, api.EnvKindSecretFetchable, cls["NEW"])

	// Without a holder nothing is recorded or merged.
	plain := context.Background()
	recordReprovisionGatheredEnv(plain, map[string]string{"NEW": "gathered"})
	env2 := map[string]string{}
	mergeReprovisionGatheredEnv(plain, env2, &cls)
	assert.Empty(t, env2)
}

// TestReincarnateWorker_GatheredEnvHolderOnlyForHomeReset pins the worker
// side: a harness switch (home reset) hands its reprovision and its start
// the same reprovisionGatheredEnv holder; a later plain reincarnation hands
// neither one a holder, so it starts as before.
func TestReincarnateWorker_GatheredEnvHolderOnlyForHomeReset(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, nil)
	rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{HarnessConfig: "codex-hc", Handoff: "HANDOFF-TEXT"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, f.s, f.agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

	f.disp.mu.Lock()
	reprov := append([]*reprovisionGatheredEnv(nil), f.disp.reprovisionGatheredEnvHolders...)
	starts := append([]*reprovisionGatheredEnv(nil), f.disp.startGatheredEnvHolders...)
	f.disp.mu.Unlock()
	require.Len(t, reprov, 1)
	require.Len(t, starts, 1)
	require.NotNil(t, reprov[0], "a home-reset reprovision gets a holder")
	assert.Same(t, reprov[0], starts[0], "the start gets the reprovision's holder")

	rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForGeneration(t, f.s, f.agent.ID, 3)

	f.disp.mu.Lock()
	defer f.disp.mu.Unlock()
	require.Len(t, f.disp.reprovisionGatheredEnvHolders, 2)
	require.Len(t, f.disp.startGatheredEnvHolders, 2)
	assert.Nil(t, f.disp.reprovisionGatheredEnvHolders[1], "a plain reincarnation gets no holder")
	assert.Nil(t, f.disp.startGatheredEnvHolders[1], "a plain reincarnation's start gets no holder")
}
