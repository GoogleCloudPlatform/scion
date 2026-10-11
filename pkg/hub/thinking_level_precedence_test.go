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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover Gap 1 end to end on the hub side: a thinking level set by
// the scion.io/default-thinking-level project annotation, or by the create
// request, must leave the hub as SCION_THINKING_LEVEL in the dispatch
// request's ResolvedEnv.
//
// Scope, stated plainly: these assert the **hub-side terminal hop only** — the
// value the hub hands to the runtime broker. The broker-side hop from opts.Env
// into the container is pkg/agent/run.go's existing guarded read of
// SCION_THINKING_LEVEL and cannot be exercised without a container runtime, so
// no test here is entitled to claim the value "reaches the harness".

// setupThinkingLevelDispatch wires the agent-create HTTP handler to a real
// HTTPAgentDispatcher backed by a mock RuntimeBrokerClient, so the assertion
// can be made on the RemoteCreateAgentRequest the hub actually builds — the
// stub AgentDispatcher used by most create tests short-circuits before
// buildCreateRequest and would never run the env injectors.
func setupThinkingLevelDispatch(t *testing.T) (*Server, store.Store, *store.Project, *mockRuntimeBrokerClient) {
	t.Helper()

	// This stub only satisfies setupCreateAgentServer's signature: it is
	// replaced by the SetDispatcher call below before any request is served
	// and is never invoked, so createPhase here is not load-bearing. The live
	// dispatcher is the real HTTPAgentDispatcher constructed below.
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{createPhase: string(state.PhaseRunning)})
	ctx := context.Background()

	// The real dispatcher refuses to dispatch to a broker with no endpoint.
	// The endpoint is deliberately un-dialable: .invalid is reserved by
	// RFC 2606 and can never resolve, so if a future change makes this path
	// dial for real it fails loudly here instead of quietly reaching whatever
	// happens to be listening on a plausible localhost port.
	broker, err := s.GetRuntimeBroker(ctx, project.DefaultRuntimeBrokerID)
	require.NoError(t, err)
	broker.Endpoint = "http://broker.invalid"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	mockClient := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default()))

	return srv, s, project, mockClient
}

// TestCreateAgent_ProjectThinkingLevelAnnotation_ReachesDispatch is the Gap 1A
// regression test: before injectThinkingLevelEnv existed, a project's
// scion.io/default-thinking-level annotation landed in
// AppliedConfig.ThinkingLevel and stopped there — there is no
// RemoteAgentConfig wire field for it and no argv path, so the hub dropped it
// at the dispatch boundary.
//
// The name says ReachesDispatch, not ReachesHarness, and it means it: this
// asserts only that the hub puts SCION_THINKING_LEVEL into the dispatch
// request's ResolvedEnv. Delivery from there into the container is
// pkg/agent/run.go's existing behaviour (it reads SCION_THINKING_LEVEL from
// opts.Env under an "if not already set" guard) and is not covered here — it
// would need a container runtime. Do not rename this test to imply otherwise.
func TestCreateAgent_ProjectThinkingLevelAnnotation_ReachesDispatch(t *testing.T) {
	srv, s, project, mockClient := setupThinkingLevelDispatch(t)

	setProjectAnnotations(t, s, project, map[string]string{
		projectSettingDefaultThinkingLevel: "7",
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "thinking-annotation-agent",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.True(t, mockClient.createCalled, "agent should have been dispatched to the broker")
	require.NotNil(t, mockClient.lastCreateReq)
	require.NotNil(t, mockClient.lastCreateReq.ResolvedEnv, "ResolvedEnv should be non-nil")
	assert.Equal(t, "7", mockClient.lastCreateReq.ResolvedEnv["SCION_THINKING_LEVEL"],
		"the project's default-thinking-level annotation must reach the dispatch request's env")
}

// TestCreateAgent_RequestThinkingLevelBeatsAnnotation pins the precedence:
// an explicit thinking level on the create request outranks the project
// annotation.
//
// This guards the `ac.ThinkingLevel == nil` condition in applyProjectDefaults
// (project_settings_handlers.go) against being turned into an unconditional
// write. It is not redundant with the annotation-only test above: that one
// passes whether or not the guard exists, and only this one fails if the
// project tier starts clobbering the request tier. Do not delete it as a
// duplicate.
func TestCreateAgent_RequestThinkingLevelBeatsAnnotation(t *testing.T) {
	srv, s, project, mockClient := setupThinkingLevelDispatch(t)

	setProjectAnnotations(t, s, project, map[string]string{
		projectSettingDefaultThinkingLevel: "9",
	})

	requested := 7
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "thinking-request-agent",
		ProjectID: project.ID,
		Task:      "do something",
		Config:    &api.ScionConfig{ThinkingLevel: &requested},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.True(t, mockClient.createCalled, "agent should have been dispatched to the broker")
	require.NotNil(t, mockClient.lastCreateReq)
	require.NotNil(t, mockClient.lastCreateReq.ResolvedEnv, "ResolvedEnv should be non-nil")
	assert.Equal(t, "7", mockClient.lastCreateReq.ResolvedEnv["SCION_THINKING_LEVEL"],
		"the request's thinking level must outrank the project annotation (9)")
}
