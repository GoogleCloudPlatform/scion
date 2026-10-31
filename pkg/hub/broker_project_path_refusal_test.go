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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// projectPathLocalPath is the provider's stored LocalPath in these tests.
	projectPathLocalPath = "/srv/provider-local-path"
	// projectPathResolvedTarget stands for a path the broker resolved (a
	// symlink or marker target). It appears only in the broker's own
	// message, and must never reach a client.
	projectPathResolvedTarget = "/resolved/symlink-target"
)

// brokerProjectPathRefusal is the broker's 409 project_path_not_initialized
// as it reaches the hub, its message naming host paths.
func brokerProjectPathRefusal() error {
	return brokerHarnessConfigErr(http.StatusConflict, projectPathNotInitializedErrorCode,
		"project path "+projectPathLocalPath+" has no .scion directory or marker (resolved "+projectPathResolvedTarget+")")
}

// requireProjectPathRefusal checks w is 409 project_path_not_initialized and
// that the message names no broker-resolved path. It returns the message.
func requireProjectPathRefusal(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, `"`+projectPathNotInitializedErrorCode+`"`)
	assert.NotContains(t, body, projectPathResolvedTarget, "a broker-resolved path must never reach the client")
	return body
}

// TestRelayBrokerRefusal_ProjectPathNotInitialized: the generic relay (used
// where the hub does not hold the agent) answers 409 with the code and the
// hub's own path-free text, never the broker's message.
func TestRelayBrokerRefusal_ProjectPathNotInitialized(t *testing.T) {
	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, brokerProjectPathRefusal(), "")
	body := requireProjectPathRefusal(t, w)
	assert.Contains(t, body, projectPathRefusalGeneric)
	assert.NotContains(t, body, projectPathLocalPath, "the generic relay names no path")
}

func TestProjectPathRefusalText(t *testing.T) {
	withPath := projectPathRefusalText("edge-1", projectPathLocalPath)
	assert.Contains(t, withPath, "Runtime Broker edge-1")
	assert.Contains(t, withPath, projectPathLocalPath+"/.scion")
	assert.Contains(t, withPath, "not a symlink to another location")
	suffixed := projectPathRefusalText("edge-1", projectPathLocalPath+"/.scion")
	assert.Contains(t, suffixed, projectPathLocalPath+"/.scion as a directory")
	assert.NotContains(t, suffixed, "/.scion/.scion")
	without := projectPathRefusalText("edge-1", "")
	assert.Contains(t, without, "Runtime Broker edge-1")
	assert.Contains(t, without, "not a symlink to another location")
	assert.NotContains(t, without, "/")
}

// TestRelayProjectPathRefusal_ProviderReaders: the agent-aware relay names
// the provider's stored LocalPath to a caller who may read the project's
// provider records, and leaves it out for anyone else. Neither sees a path
// the broker resolved.
func TestRelayProjectPathRefusal_ProviderReaders(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	ctx := context.Background()
	brokerID := tid("pp-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "pp-broker", Slug: "pp-broker", Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: brokerID, BrokerName: "pp-broker", LocalPath: projectPathLocalPath,
	}))
	agent := &store.Agent{ID: tid("pp-agent"), ProjectID: project.ID, RuntimeBrokerID: brokerID}

	userCtx := func(u *store.User) context.Context {
		return contextWithIdentity(ctx, NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web"))
	}
	require.True(t, srv.callerMayReadProviders(userCtx(alice), project.ID), "fixture: alice reads the project")
	require.False(t, srv.callerMayReadProviders(userCtx(bob), project.ID), "fixture: bob does not")

	t.Run("provider reader sees the stored LocalPath", func(t *testing.T) {
		w := httptest.NewRecorder()
		require.True(t, srv.relayProjectPathRefusal(userCtx(alice), w, agent, brokerProjectPathRefusal()))
		body := requireProjectPathRefusal(t, w)
		assert.Contains(t, body, projectPathLocalPath+"/.scion")
		assert.Contains(t, body, "pp-broker")
	})
	t.Run("other caller gets the code without the path", func(t *testing.T) {
		w := httptest.NewRecorder()
		require.True(t, srv.relayProjectPathRefusal(userCtx(bob), w, agent, brokerProjectPathRefusal()))
		body := requireProjectPathRefusal(t, w)
		assert.NotContains(t, body, projectPathLocalPath)
	})
	t.Run("no identity gets the code without the path", func(t *testing.T) {
		w := httptest.NewRecorder()
		require.True(t, srv.relayProjectPathRefusal(ctx, w, agent, brokerProjectPathRefusal()))
		body := requireProjectPathRefusal(t, w)
		assert.NotContains(t, body, projectPathLocalPath)
	})
	t.Run("other broker errors are not taken", func(t *testing.T) {
		w := httptest.NewRecorder()
		other := brokerHarnessConfigErr(http.StatusConflict, "conflict", "name in use")
		assert.False(t, srv.relayProjectPathRefusal(userCtx(alice), w, agent, other))
		assert.False(t, strings.Contains(w.Body.String(), projectPathNotInitializedErrorCode))
	})
}

// TestDispatchFailureText_ProjectPathNotInitialized: the recorded text of a
// provision-only create warning or a reincarnation state carries the code and
// the hub's path-free text, never the broker's message.
func TestDispatchFailureText_ProjectPathNotInitialized(t *testing.T) {
	got := dispatchFailureText(brokerProjectPathRefusal())
	assert.Contains(t, got, projectPathNotInitializedErrorCode)
	assert.Contains(t, got, projectPathRefusalGeneric)
	assert.NotContains(t, got, projectPathLocalPath)
	assert.NotContains(t, got, projectPathResolvedTarget)
}

// TestCallerMayReadProviders_AgentCallers: an agent caller passes the
// reader gate only as handleProjectProviders admits it, so an agent of
// another project, or one whose standing fails, never sees the path.
func TestCallerMayReadProviders_AgentCallers(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	ctx := context.Background()
	other := contextWithIdentity(context.Background(), newTestAgentIdentity(tid("pp-other-agent"), tid("pp-other-project"), nil))
	assert.False(t, srv.callerMayReadProviders(other, project.ID), "an agent of another project")
	missing := contextWithIdentity(context.Background(), newTestAgentIdentity(tid("pp-missing-agent"), project.ID, nil))
	assert.False(t, srv.callerMayReadProviders(missing, project.ID), "an agent whose standing cannot be established")

	// A same-project agent out of standing: held. The hold is checked before
	// the root user's admission, so the fixture pins the hold as the cause.
	heldID := tid("pp-held-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: heldID, Slug: "pp-held-agent", Name: "pp-held-agent",
		ProjectID: project.ID, OwnerID: alice.ID, Created: time.Now(), Updated: time.Now(),
	}))
	_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{{
		AgentID: heldID, ProjectID: project.ID, Cause: store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: alice.ID,
		Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "test",
	}})
	require.NoError(t, err)
	require.Equal(t, standingReasonAgentHeld, standingReason(srv.agentStanding(ctx, heldID)), "fixture: the agent is held")
	brokerID := tid("pp-held-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "pp-held-broker", Slug: "pp-held-broker", Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: brokerID, BrokerName: "pp-held-broker", LocalPath: projectPathLocalPath,
	}))
	held := contextWithIdentity(ctx, newTestAgentIdentity(heldID, project.ID, nil))
	assert.False(t, srv.callerMayReadProviders(held, project.ID), "a same-project agent out of standing")
	w := httptest.NewRecorder()
	require.True(t, srv.relayProjectPathRefusal(held, w, &store.Agent{ID: heldID, ProjectID: project.ID, RuntimeBrokerID: brokerID}, brokerProjectPathRefusal()))
	assert.NotContains(t, requireProjectPathRefusal(t, w), projectPathLocalPath, "a held agent gets no path")
}

// TestAgentLifecycle_StartRelaysProjectPathRefusal pins the call site: a
// start or restart whose broker answers 409 project_path_not_initialized
// reaches the client as 409 with the code through relayProjectPathRefusal
// (the provider path appears only through it), not as a 502, and without
// the broker's own text.
func TestAgentLifecycle_StartRelaysProjectPathRefusal(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			disp := &skillFailDispatcher{startErr: brokerProjectPathRefusal()}
			srv, s, project := setupCreateAgentServer(t, disp)
			agent := createLifecycleTestAgent(t, s, project, "pp-lc-"+action, state.PhaseStopped)
			provider, err := s.GetProjectProvider(context.Background(), project.ID, agent.RuntimeBrokerID)
			require.NoError(t, err)
			require.NotEmpty(t, provider.LocalPath)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)

			body := requireProjectPathRefusal(t, rec)
			assert.Contains(t, body, provider.LocalPath, "the provider reader sees the stored LocalPath")
			assert.NotContains(t, body, projectPathLocalPath, "the broker's own text is not relayed")
		})
	}
}
