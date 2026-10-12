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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Token admission for agent update, stop-all, port registration and message
// mode. Every token here is minted through UserAccessTokenService and sent
// through the server handler, so the bearer gate, the selector and the
// boundary apply as in production. The field-level rules of each operation
// apply to a token as to a session.

// agentTokenFixture is a project with a running agent, a second project,
// and a user who owns both projects (so the user can mint project tokens for
// either and hub tokens carrying any project-owner selector).
type agentTokenFixture struct {
	srv     *Server
	store   store.Store
	project string
	other   string
	ownerID string
	agent   *store.Agent
}

func newAgentTokenFixture(t *testing.T, name string) agentTokenFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := agentTokenFixture{
		srv:     srv,
		store:   s,
		project: tid(name + "-project"),
		other:   tid(name + "-other"),
		ownerID: tid(name + "-owner"),
	}
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.project, Name: name + " project", Slug: name + "-project"}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.other, Name: name + " other", Slug: name + "-other"}))
	createTestUserWithProjectRole(t, s, f.ownerID, f.ownerID+"@test.com", f.project, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, f.ownerID, f.ownerID+"@test.com", f.other, store.ProjectRoleOwner)
	ensureHubMembership(ctx, s, f.ownerID)
	f.agent = f.newAgent(t, name+"-agent", string(state.PhaseRunning))
	return f
}

// newAgent creates an agent in the fixture project owned by another user.
func (f agentTokenFixture) newAgent(t *testing.T, slug, phase string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:        tid(slug),
		Slug:      slug,
		Name:      slug,
		ProjectID: f.project,
		Phase:     phase,
		OwnerID:   tid("someone-else"),
		CreatedBy: tid("someone-else"),
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), agent))
	return agent
}

// mint returns a real token of userID for the boundary and selectors.
func (f agentTokenFixture) mint(t *testing.T, userID string, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "atk-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	return key
}

func (f agentTokenFixture) agentPath(agent *store.Agent, rest string) string {
	return "/api/v1/agents/" + agent.ID + rest
}

// TestAgentUpdateToken_PassthroughGateStillApplies pins that agent:update
// admits a token on the agent PATCH, and that a passthrough GCP identity on
// that PATCH still requires broker ownership: a token of a project owner who
// does not own the agent's broker is refused by the passthrough gate before
// any actAs check.
func TestAgentUpdateToken_PassthroughGateStillApplies(t *testing.T) {
	f := newAgentTokenFixture(t, "atk-pt")
	ctx := context.Background()
	hostSAEmail := "atk-host@atk-project.iam.gserviceaccount.com"
	broker := &store.RuntimeBroker{
		ID: tid("atk-pt-broker"), Name: "ATK Broker", Slug: "atk-pt-broker", Status: store.BrokerStatusOnline,
		CreatedBy: tid("broker-owner"), GCPHostServiceAccountEmail: hostSAEmail, GCPHostProjectID: "atk-project",
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, broker))
	agent := &store.Agent{
		ID: tid("atk-pt-created"), Slug: "atk-pt-created", Name: "atk-pt-created", ProjectID: f.project,
		RuntimeBrokerID: broker.ID, Phase: string(state.PhaseCreated),
		OwnerID: tid("someone-else"), CreatedBy: tid("someone-else"),
	}
	require.NoError(t, f.store.CreateAgent(ctx, agent))
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(f.srv, checker)

	passthrough := map[string]interface{}{"gcp_identity": map[string]interface{}{"metadata_mode": "passthrough"}}
	for _, boundary := range []TokenBoundary{projectBoundary(f.project), hubBoundary()} {
		withSelector := f.mint(t, f.ownerID, boundary, "agent:update")
		rec := doRequestWithUAT(t, f.srv, withSelector, http.MethodPatch, f.agentPath(agent, ""), passthrough)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s token: %s", boundary.Kind, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "broker ownership", "%s token", boundary.Kind)

		// The same token is admitted on a PATCH the gate does not cover.
		rec = doRequestWithUAT(t, f.srv, withSelector, http.MethodPatch, f.agentPath(agent, ""), map[string]interface{}{"name": "atk-renamed-" + string(boundary.Kind)})
		assert.Equal(t, http.StatusOK, rec.Code, "%s token: %s", boundary.Kind, rec.Body.String())

		// A token without agent:update is refused before the gate.
		without := f.mint(t, f.ownerID, boundary, "agent:read")
		rec = doRequestWithUAT(t, f.srv, without, http.MethodPatch, f.agentPath(agent, ""), passthrough)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s token: %s", boundary.Kind, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "broker ownership", "%s token", boundary.Kind)
	}
	assert.Equal(t, 0, checker.CallCount(), "the ownership refusal comes before the actAs check")

	got, err := f.store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	if got.AppliedConfig != nil && got.AppliedConfig.GCPIdentity != nil {
		assert.NotEqual(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode)
	}

	// A token for another project is refused on the agent.
	otherKey := f.mint(t, f.ownerID, projectBoundary(f.other), "agent:update")
	rec := doRequestWithUAT(t, f.srv, otherKey, http.MethodPatch, f.agentPath(agent, ""), map[string]interface{}{"name": "atk-other"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestAgentUpdateToken_ServiceAccountAssignRequiresAssignSelector pins that
// assigning a service account on the agent PATCH needs the
// gcp_service_account:assign selector in addition to agent:update.
func TestAgentUpdateToken_ServiceAccountAssignRequiresAssignSelector(t *testing.T) {
	f := newAgentTokenFixture(t, "atk-sa")
	ctx := context.Background()
	sa := wiringSA(t, f.store, store.ScopeProject, f.project, "atk-sa@atk.iam.gserviceaccount.com")
	agent := f.newAgent(t, "atk-sa-created", string(state.PhaseCreated))
	assign := map[string]interface{}{"gcp_identity": map[string]interface{}{
		"metadata_mode": store.GCPMetadataModeAssign, "service_account_id": sa.ID,
	}}

	updateOnly := f.mint(t, f.ownerID, projectBoundary(f.project), "agent:update")
	rec := doRequestWithUAT(t, f.srv, updateOnly, http.MethodPatch, f.agentPath(agent, ""), assign)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	got, err := f.store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	if got.AppliedConfig != nil && got.AppliedConfig.GCPIdentity != nil {
		assert.NotEqual(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID, "a refused assignment writes nothing")
	}

	both := f.mint(t, f.ownerID, projectBoundary(f.project), "agent:update", "gcp_service_account:assign")
	rec = doRequestWithUAT(t, f.srv, both, http.MethodPatch, f.agentPath(agent, ""), assign)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = f.store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
}

// TestStopAllHubForm_RequiresStopAllSelectorAndHubBoundary pins that the
// hub form of stop-all admits only a hub token carrying agent:stop_all of a
// user who holds agent.stop_all on the hub, and that the project form admits
// a project token for that project.
func TestStopAllHubForm_RequiresStopAllSelectorAndHubBoundary(t *testing.T) {
	f := newAgentTokenFixture(t, "atk-sa-all")
	ctx := context.Background()
	adminID := tid("atk-stopall-admin")
	createTestUserWithRole(t, f.store, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	createTestUserWithProjectRole(t, f.store, adminID, adminID+"@test.com", f.project, store.ProjectRoleOwner)
	ensureHubMembership(ctx, f.store, adminID)

	phase := func(agent *store.Agent) string {
		got, err := f.store.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		return got.Phase
	}
	const hubPath = "/api/v1/agents/stop-all"
	projectPath := "/api/v1/projects/" + f.project + "/agents/stop-all"

	// A hub token without the selector is refused.
	rec := doRequestWithUAT(t, f.srv, f.mint(t, adminID, hubBoundary(), "agent:read"), http.MethodPost, hubPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// A project token with the selector is refused on the hub form: the
	// hub target is outside a project boundary.
	projectKey := f.mint(t, adminID, projectBoundary(f.project), "agent:stop_all")
	rec = doRequestWithUAT(t, f.srv, projectKey, http.MethodPost, hubPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// A project owner without hub authority is refused on the hub form even
	// with a hub token carrying the selector.
	rec = doRequestWithUAT(t, f.srv, f.mint(t, f.ownerID, hubBoundary(), "agent:stop_all"), http.MethodPost, hubPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, string(state.PhaseRunning), phase(f.agent), "no refused request stops the agent")

	// The project form admits the project token for that project.
	rec = doRequestWithUAT(t, f.srv, projectKey, http.MethodPost, projectPath, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(state.PhaseStopped), phase(f.agent))
	// and refuses a project token for another project.
	second := f.newAgent(t, "atk-sa-all-second", string(state.PhaseRunning))
	otherKey := f.mint(t, f.ownerID, projectBoundary(f.other), "agent:stop_all")
	rec = doRequestWithUAT(t, f.srv, otherKey, http.MethodPost, projectPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, string(state.PhaseRunning), phase(second))

	// The hub form admits a hub token with the selector.
	rec = doRequestWithUAT(t, f.srv, f.mint(t, adminID, hubBoundary(), "agent:stop_all"), http.MethodPost, hubPath, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(state.PhaseStopped), phase(second))
}

// TestPortRegistration_UsesAgentRecordTarget pins that registering,
// deleting and clearing an agent's exposed ports is authorized by
// agent.port_access on the agent record: a token needs agent:port_access and
// a boundary that contains the agent's project, and a session user needs
// agent.port_access on the agent. The loopback-host rule and the reserved
// ports apply to a token too.
func TestPortRegistration_UsesAgentRecordTarget(t *testing.T) {
	f := newAgentTokenFixture(t, "atk-port")
	ctx := context.Background()
	ports := f.agentPath(f.agent, "/ports")

	for _, scopes := range [][]string{{"agent:read"}, {"agent:update"}} {
		key := f.mint(t, f.ownerID, hubBoundary(), scopes...)
		rec := doRequestWithUAT(t, f.srv, key, http.MethodPost, ports, map[string]any{"port": 3000})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%v: %s", scopes, rec.Body.String())
	}
	otherKey := f.mint(t, f.ownerID, projectBoundary(f.other), "agent:port_access")
	rec := doRequestWithUAT(t, f.srv, otherKey, http.MethodPost, ports, map[string]any{"port": 3000})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A session user with no grant on the agent is refused.
	strangerID := tid("atk-port-stranger")
	createTestUserWithProjectRole(t, f.store, strangerID, strangerID+"@test.com", f.other, store.ProjectRoleOwner)
	stranger, err := f.store.GetUser(ctx, strangerID)
	require.NoError(t, err)
	rec = doRequestAsUser(t, f.srv, stranger, http.MethodPost, ports, map[string]any{"port": 3000})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	got, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExposedPorts, "no refused request registers a port")

	for _, boundary := range []TokenBoundary{projectBoundary(f.project), hubBoundary()} {
		key := f.mint(t, f.ownerID, boundary, "agent:port_access")
		rec = doRequestWithUAT(t, f.srv, key, http.MethodPost, ports, map[string]any{"port": 3000, "host": "10.0.0.1"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s token, non-loopback host: %s", boundary.Kind, rec.Body.String())
		rec = doRequestWithUAT(t, f.srv, key, http.MethodPost, ports, map[string]any{"port": 18380})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s token, reserved port: %s", boundary.Kind, rec.Body.String())

		rec = doRequestWithUAT(t, f.srv, key, http.MethodPost, ports, map[string]any{"port": 3000})
		require.Equal(t, http.StatusCreated, rec.Code, "%s token: %s", boundary.Kind, rec.Body.String())
		rec = doRequestWithUAT(t, f.srv, key, http.MethodDelete, ports+"/3000", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "%s token: %s", boundary.Kind, rec.Body.String())
		rec = doRequestWithUAT(t, f.srv, key, http.MethodPost, ports, map[string]any{"port": 3001})
		require.Equal(t, http.StatusCreated, rec.Code, "%s token: %s", boundary.Kind, rec.Body.String())
		rec = doRequestWithUAT(t, f.srv, key, http.MethodDelete, ports, nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "%s token: %s", boundary.Kind, rec.Body.String())
	}
	got, err = f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExposedPorts)
}

// TestPortTunnel_OnlyTheAgentItself pins that only the agent itself, with
// its own agent token and the port-forward scope, opens its port tunnel.
// Every user, including a hub admin session, is refused, and so is another
// agent.
func TestPortTunnel_OnlyTheAgentItself(t *testing.T) {
	srv, s := testServer(t)
	agent, ownToken := createPortForwardAgent(t, srv, s)
	path := "/api/v1/agents/" + agent.ID + "/ports/tunnel"
	upgrade := func(header, value string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		req.Header.Set(header, value)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := upgrade("Authorization", "Bearer "+testDevToken)
	assert.Equal(t, http.StatusForbidden, rec.Code, "admin session: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Only the agent can open its port tunnel")

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	other := &store.Agent{ID: tid("pf-other-agent"), Slug: "pf-other-agent", Name: "PF Other", ProjectID: agent.ProjectID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(context.Background(), other))
	otherToken, err := tokenSvc.GenerateAgentToken(other.ID, agent.ProjectID, []AgentTokenScope{ScopeAgentPortForward}, nil)
	require.NoError(t, err)
	rec = upgrade("X-Scion-Agent-Token", otherToken)
	assert.Equal(t, http.StatusForbidden, rec.Code, "another agent: %s", rec.Body.String())

	noScope, err := tokenSvc.GenerateAgentToken(agent.ID, agent.ProjectID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)
	rec = upgrade("X-Scion-Agent-Token", noScope)
	assert.Equal(t, http.StatusForbidden, rec.Code, "own agent without the port-forward scope: %s", rec.Body.String())

	// The agent itself passes authorization; the recorder cannot be
	// taken over as a connection, so the upgrade itself fails after it.
	rec = upgrade("X-Scion-Agent-Token", ownToken)
	assert.NotEqual(t, http.StatusForbidden, rec.Code, "the agent itself: %s", rec.Body.String())
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code, "the agent itself: %s", rec.Body.String())
}

// TestSetMessageModeToken_ProjectAdminDenialStillApplies pins that a token
// changes message mode only with agent:set_message_mode, and that the
// project-admin denial applies to a token of a project admin who is not a
// lineage owner, even when that token carries the selector.
func TestSetMessageModeToken_ProjectAdminDenialStillApplies(t *testing.T) {
	f := newAgentTokenFixture(t, "atk-mm")
	ctx := context.Background()
	path := f.agentPath(f.agent, "/set_message_mode")

	// The owner: refused without the selector, admitted with it.
	rec := doRequestWithUAT(t, f.srv, f.mint(t, f.ownerID, projectBoundary(f.project), "agent:read"), http.MethodPost, path, map[string]any{"mode": "lineage"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithUAT(t, f.srv, f.mint(t, f.ownerID, projectBoundary(f.other), "agent:set_message_mode"), http.MethodPost, path, map[string]any{"mode": "lineage"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "token for another project: %s", rec.Body.String())
	rec = doRequestWithUAT(t, f.srv, f.mint(t, f.ownerID, projectBoundary(f.project), "agent:set_message_mode"), http.MethodPost, path, map[string]any{"mode": "lineage"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A project admin of the agent's project who owns another project can
	// mint the selector on a hub boundary; the project-admin rule refuses it.
	adminID := tid("atk-mm-project-admin")
	createTestUserWithProjectRole(t, f.store, adminID, adminID+"@test.com", f.project, store.ProjectRoleAdmin)
	createTestUserWithProjectRole(t, f.store, adminID, adminID+"@test.com", f.other, store.ProjectRoleOwner)
	ensureHubMembership(ctx, f.store, adminID)
	adminKey := f.mint(t, adminID, hubBoundary(), "agent:set_message_mode")
	rec = doRequestWithUAT(t, f.srv, adminKey, http.MethodPost, path, map[string]any{"mode": "none"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Project admins cannot change message mode")

	got, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageModeLineage, got.MessageMode, "the refused request leaves the mode")
}

// TestSetMessageModeToken_ProjectBoundaryTokensCannotGrantHubMode pins that
// a project-boundary token cannot grant hub message mode, to the agent or to
// a descendant in a cascade, and that a hub-boundary token of the same user
// can.
func TestSetMessageModeToken_ProjectBoundaryTokensCannotGrantHubMode(t *testing.T) {
	f := newAgentTokenFixture(t, "atk-hub")
	ctx := context.Background()
	mode := func(agent *store.Agent) string {
		got, err := f.store.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		return got.MessageMode
	}
	projectKey := f.mint(t, f.ownerID, projectBoundary(f.project), "agent:set_message_mode")
	hubKey := f.mint(t, f.ownerID, hubBoundary(), "agent:set_message_mode")

	before := mode(f.agent)
	rec := doRequestWithUAT(t, f.srv, projectKey, http.MethodPost, f.agentPath(f.agent, "/set_message_mode"), map[string]any{"mode": "hub"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "hub boundary")
	assert.Equal(t, before, mode(f.agent))

	// Cascade: the root already has hub mode, so only the descendant would
	// newly receive it; a project token leaves the descendant in its mode.
	root := f.newAgent(t, "atk-hub-root", string(state.PhaseRunning))
	root, err := f.store.GetAgent(ctx, root.ID)
	require.NoError(t, err)
	root.MessageMode = store.MessageModeHub
	require.NoError(t, f.store.UpdateAgent(ctx, root))
	child := &store.Agent{
		ID: tid("atk-hub-child"), Slug: "atk-hub-child", Name: "atk-hub-child", ProjectID: f.project,
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
		Ancestry: []string{tid("someone-else"), root.ID}, OwnerID: tid("someone-else"), CreatedBy: tid("someone-else"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(ctx, child))
	cascade := map[string]any{"mode": "hub", "cascade": true}
	rec = doRequestWithUAT(t, f.srv, projectKey, http.MethodPost, f.agentPath(root, "/set_message_mode"), cascade)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.MessageModeProject, mode(child), "a project token does not grant hub mode to a descendant")

	rec = doRequestWithUAT(t, f.srv, hubKey, http.MethodPost, f.agentPath(root, "/set_message_mode"), cascade)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.MessageModeHub, mode(child), "a hub token grants hub mode to the descendant")

	rec = doRequestWithUAT(t, f.srv, hubKey, http.MethodPost, f.agentPath(f.agent, "/set_message_mode"), map[string]any{"mode": "hub"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.MessageModeHub, mode(f.agent))
}

// TestAgentCreateToken_ProjectBoundaryTokensCannotGrantHubMode pins that a
// project-boundary token cannot create an agent in hub message mode, while
// the same request is not refused by the grant rule for a hub-boundary
// token.
func TestAgentCreateToken_ProjectBoundaryTokensCannotGrantHubMode(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ownerID := tid("atk-create-owner")
	createTestUserWithProjectRole(t, s, ownerID, ownerID+"@test.com", project.ID, store.ProjectRoleOwner)
	ensureHubMembership(context.Background(), s, ownerID)
	f := agentTokenFixture{srv: srv, store: s, project: project.ID, ownerID: ownerID}

	body := func(name string) map[string]any {
		return map[string]any{"name": name, "projectId": project.ID, "agentRole": "none", "messageMode": "hub", "task": "x"}
	}
	projectKey := f.mint(t, ownerID, projectBoundary(project.ID), "agent:create")
	rec := doRequestWithUAT(t, srv, projectKey, http.MethodPost, "/api/v1/agents", body("atk-create-project"))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Cannot grant hub message mode")
	_, err := s.GetAgentBySlug(context.Background(), project.ID, "atk-create-project")
	assert.ErrorIs(t, err, store.ErrNotFound, "the refused create writes no agent")

	hubKey := f.mint(t, ownerID, hubBoundary(), "agent:create")
	rec = doRequestWithUAT(t, srv, hubKey, http.MethodPost, "/api/v1/agents", body("atk-create-hub"))
	assert.NotContains(t, rec.Body.String(), "Cannot grant hub message mode", rec.Body.String())
}
