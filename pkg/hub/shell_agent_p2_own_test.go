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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type p2OwnShellTestDispatcher struct {
	mockDispatcher
	deletedFiles map[string]bool
}

func (d *p2OwnShellTestDispatcher) DispatchAgentDelete(_ context.Context, agent *store.Agent, deleteFiles, _, _ bool, _ time.Time) error {
	if d.deletedFiles == nil {
		d.deletedFiles = make(map[string]bool)
	}
	d.deletedFiles[agent.ID] = deleteFiles
	return nil
}

func (p2OwnShellTestDispatcher) DispatchAgentLogs(_ context.Context, agent *store.Agent, _ int) (string, error) {
	return "logs-for-" + agent.Slug, nil
}

func p2OwnSeedProjectAndBroker(t *testing.T, s store.Store) (*store.Project, *store.RuntimeBroker) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "shell-p2-own-broker",
		Slug:   "shell-p2-own-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:                     api.NewUUID(),
		Name:                   "Shell P2 Own Project",
		Slug:                   "shell-p2-own-project",
		DefaultRuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  t.TempDir(),
		Status:     store.BrokerStatusOnline,
	}))
	return project, broker
}

func p2OwnSeedUser(t *testing.T, s store.Store, email, displayName string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          api.NewUUID(),
		Email:       email,
		DisplayName: displayName,
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

func p2OwnBindProjectRole(t *testing.T, s store.Store, projectID, userID, roleName string) {
	t.Helper()
	ctx := context.Background()
	role, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	require.NoError(t, err)
}

func p2OwnSeedAgent(t *testing.T, s store.Store, projectID, brokerID, slug, ownerID, harnessConfig string, phase state.Phase, ancestry []string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            slug,
		Name:            slug,
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		OwnerID:         ownerID,
		CreatedBy:       ownerID,
		Ancestry:        ancestry,
		Phase:           string(phase),
		Activity:        string(state.ActivityIdle),
		MessageMode:     store.MessageModeProject,
		HarnessConfig:   harnessConfig,
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: harnessConfig,
		},
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

// TestShellAgentOwnershipChangeRefused_S8 verifies design §3.4 S8:
// changing or clearing owner_id of an execution container is refused across
// PATCH endpoints (both direct and project-scoped) and at the store layer,
// regardless of whether hub.exec_containers is enabled.
func TestShellAgentOwnershipChangeRefused_S8(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	project, broker := p2OwnSeedProjectAndBroker(t, s)

	owner := p2OwnSeedUser(t, s, "s8-owner@test.com", "S8 Owner")
	projAdmin := p2OwnSeedUser(t, s, "s8-admin@test.com", "S8 Admin")
	otherMember := p2OwnSeedUser(t, s, "s8-member@test.com", "S8 Member")
	p2OwnBindProjectRole(t, s, project.ID, owner.ID, "project-member")
	p2OwnBindProjectRole(t, s, project.ID, projAdmin.ID, "project-admin")
	p2OwnBindProjectRole(t, s, project.ID, otherMember.ID, "project-member")

	shellAgent := p2OwnSeedAgent(t, s, project.ID, broker.ID, "shell-s8", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID})

	payloads := []struct {
		name string
		body map[string]any
	}{
		{"camelCase ownerId", map[string]any{"ownerId": projAdmin.ID}},
		{"snake_case owner_id", map[string]any{"owner_id": projAdmin.ID}},
		{"mixed-case OWNERID", map[string]any{"OWNERID": projAdmin.ID}},
		{"empty ownerId", map[string]any{"ownerId": ""}},
		{"null ownerId", map[string]any{"ownerId": nil}},
	}

	routes := []struct {
		name string
		path string
	}{
		{"direct patch", "/api/v1/agents/" + shellAgent.ID},
		{"project patch", "/api/v1/projects/" + project.ID + "/agents/" + shellAgent.ID},
	}

	for _, rt := range routes {
		for _, p := range payloads {
			t.Run(rt.name+" "+p.name+" as owner", func(t *testing.T) {
				rec := doRequestAsUser(t, srv, owner, http.MethodPatch, rt.path, p.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "ownership cannot be changed")
			})

			t.Run(rt.name+" "+p.name+" as project admin", func(t *testing.T) {
				rec := doRequestAsUser(t, srv, projAdmin, http.MethodPatch, rt.path, p.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "ownership cannot be changed")
			})
		}

		t.Run(rt.name+" unauthorized member gets standard 403 without shell leak", func(t *testing.T) {
			rec := doRequestAsUser(t, srv, otherMember, http.MethodPatch, rt.path, map[string]any{"ownerId": otherMember.ID})
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "ownership cannot be changed")
		})
	}

	t.Run("non-ownership metadata update by owner still succeeds", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+shellAgent.ID, map[string]any{"name": "shell-s8-renamed"})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("store UpdateAgent choke point refuses owner change", func(t *testing.T) {
		got, err := s.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		got.OwnerID = projAdmin.ID
		err = s.UpdateAgent(ctx, got)
		require.ErrorIs(t, err, store.ErrShellAgentOwnerImmutable)

		after, err := s.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, owner.ID, after.OwnerID)
	})
}

// TestShellAgentPortExposureAndAccessRefused_S4 verifies design §3.4 S4:
// port exposure (scion expose / POST /ports, DELETE /ports, GET /ports/tunnel)
// and port access routes (proxy, portview, Conduit TCP) are refused for
// execution containers, while Conduit PTY/SSH attach remains allowed for the
// human owner and denied for agent principals.
func TestShellAgentPortExposureAndAccessRefused_S4(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.config.ConduitTCPAllowedPorts = []int{8080}
	require.NoError(t, srv.initPortViewKey(ctx))
	_, err := s.UpsertHubSetting(ctx, "experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.PortView)), "", 0, "")
	require.NoError(t, err)
	ops := NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	project, broker := p2OwnSeedProjectAndBroker(t, s)

	owner := p2OwnSeedUser(t, s, "s4-owner@test.com", "S4 Owner")
	hubAdmin := &store.User{
		ID:          api.NewUUID(),
		Email:       "s4-hub-admin@test.com",
		DisplayName: "S4 Hub Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	projAdmin := p2OwnSeedUser(t, s, "s4-proj-admin@test.com", "S4 Proj Admin")
	p2OwnBindProjectRole(t, s, project.ID, owner.ID, "project-member")
	p2OwnBindProjectRole(t, s, project.ID, projAdmin.ID, "project-admin")

	shellAgent := p2OwnSeedAgent(t, s, project.ID, broker.ID, "shell-s4", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID})
	llmAgent := p2OwnSeedAgent(t, s, project.ID, broker.ID, "llm-s4", owner.ID, "claude", state.PhaseRunning, []string{owner.ID})

	shellToken := srv.generateRecordedAgentToken(t, shellAgent.ID, project.ID,
		[]AgentTokenScope{ScopeAgentPortForward, ScopeProjectRead}, nil)
	llmToken := srv.generateRecordedAgentToken(t, llmAgent.ID, project.ID,
		[]AgentTokenScope{ScopeAgentPortForward, ScopeProjectRead}, nil)

	t.Run("POST /ports (expose) refused for shell agent", func(t *testing.T) {
		body := map[string]any{"port": 8080, "label": "web"}

		recAgent := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+shellAgent.ID+"/ports", body, shellToken)
		assert.Equal(t, http.StatusForbidden, recAgent.Code, recAgent.Body.String())
		assert.Contains(t, recAgent.Body.String(), "port exposure and port access are not allowed")

		recOwner := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+shellAgent.ID+"/ports", body)
		assert.Equal(t, http.StatusForbidden, recOwner.Code, recOwner.Body.String())

		recHubAdmin := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/agents/"+shellAgent.ID+"/ports", body)
		assert.Equal(t, http.StatusForbidden, recHubAdmin.Code, recHubAdmin.Body.String())

		// Control: LLM agent can still expose a port.
		recLLM := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+llmAgent.ID+"/ports", body, llmToken)
		assert.Equal(t, http.StatusCreated, recLLM.Code, recLLM.Body.String())
	})

	t.Run("DELETE /ports and /ports/{port} refused for shell agent", func(t *testing.T) {
		recDelOne := doRequestWithAgentToken(t, srv, http.MethodDelete, "/api/v1/agents/"+shellAgent.ID+"/ports/8080", nil, shellToken)
		assert.Equal(t, http.StatusForbidden, recDelOne.Code, recDelOne.Body.String())

		recDelAll := doRequestWithAgentToken(t, srv, http.MethodDelete, "/api/v1/agents/"+shellAgent.ID+"/ports", nil, shellToken)
		assert.Equal(t, http.StatusForbidden, recDelAll.Code, recDelAll.Body.String())

		recAdminDel := doRequestAsUser(t, srv, hubAdmin, http.MethodDelete, "/api/v1/agents/"+shellAgent.ID+"/ports", nil)
		assert.Equal(t, http.StatusForbidden, recAdminDel.Code, recAdminDel.Body.String())
	})

	t.Run("GET /ports/tunnel refused for shell agent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+shellAgent.ID+"/ports/tunnel", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Authorization", "Bearer "+shellToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("proxy, portview, and Conduit TCP refused even if a port row exists in DB", func(t *testing.T) {
		// Seed a stale exposed port directly in the store to verify defence in depth on all access paths.
		require.NoError(t, s.UpdateAgentExposedPorts(ctx, shellAgent.ID, []store.ExposedPort{
			{Port: 8080, Host: "127.0.0.1", Label: "stale", Mode: "rw", ExposedAt: time.Now().UTC()},
		}))
		refreshed, err := s.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)

		recOwnerProxy := doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/agents/"+shellAgent.ID+"/ports/8080/proxy/", nil)
		assert.Equal(t, http.StatusForbidden, recOwnerProxy.Code, recOwnerProxy.Body.String())

		recAdminProxy := doRequestAsUser(t, srv, projAdmin, http.MethodGet, "/api/v1/agents/"+shellAgent.ID+"/ports/8080/proxy/", nil)
		assert.Equal(t, http.StatusForbidden, recAdminProxy.Code, recAdminProxy.Body.String())

		recAgentProxy := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+shellAgent.ID+"/ports/8080/proxy/", nil, shellToken)
		assert.Equal(t, http.StatusForbidden, recAgentProxy.Code, recAgentProxy.Body.String())

		ownerIdent := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "web")
		assert.False(t, srv.userPortAccessAllowed(ctx, ownerIdent, refreshed, ActionPortAccess))
		_, pvOK := srv.portViewIdentity(ctx, owner.ID, refreshed)
		assert.False(t, pvOK)

		capTok, err := srv.mintPortView(shellAgent.ID, 8080, owner.ID, time.Now().Add(time.Hour))
		require.NoError(t, err)
		pvReq := httptest.NewRequest(http.MethodGet, "/api/v1/portview/"+capTok+"/", nil)
		pvRec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(pvRec, pvReq)
		assert.Equal(t, http.StatusNotFound, pvRec.Code, "portview must refuse capability links for execution containers")

		// Conduit TCP authorization and TCP target check must both refuse.
		err = srv.authorizeConduitAction(ctx, ownerIdent, refreshed, ActionPortAccess)
		require.ErrorIs(t, err, errConduitForbidden)
		assert.Contains(t, err.Error(), "execution container: port access is not allowed")

		// Unauthorized caller must be denied by Decide before learning it is an execution container.
		outsider := p2OwnSeedUser(t, s, "s4-outsider@test.com", "S4 Outsider")
		outsiderIdent := NewAuthenticatedUser(outsider.ID, outsider.Email, outsider.DisplayName, outsider.Role, "web")
		err = srv.authorizeConduitAction(ctx, outsiderIdent, refreshed, ActionPortAccess)
		require.ErrorIs(t, err, errConduitForbidden)
		assert.NotContains(t, err.Error(), "execution container")

		err = srv.authorizeConduitTCPTarget(refreshed, 8080)
		require.ErrorIs(t, err, errConduitForbidden)

		// Conduit PTY/SSH attach must still succeed for the human owner.
		err = srv.authorizeConduitAction(ctx, ownerIdent, refreshed, ActionAttach)
		assert.NoError(t, err)
	})
}

// TestShellAgentReadPaths_DenyAgentPrincipals verifies that read paths that
// previously checked ActionAttach directly (env reveal and full message/log
// history) deny agent principals on execution containers, including ancestor
// agents and same-project agents with ScopeAgentLifecycle.
func TestShellAgentReadPaths_DenyAgentPrincipals(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	project, broker := p2OwnSeedProjectAndBroker(t, s)

	owner := p2OwnSeedUser(t, s, "read-owner@test.com", "Read Owner")
	p2OwnBindProjectRole(t, s, project.ID, owner.ID, "project-member")

	// Ancestor agent (even in the shell agent's ancestry, to prove the guard
	// holds even if ancestry were ever populated).
	ancestorAgent := p2OwnSeedAgent(t, s, project.ID, broker.ID, "ancestor-llm", owner.ID, "claude", state.PhaseRunning, []string{owner.ID})

	shellAgent := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "shell-read-target",
		Name:            "shell-read-target",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		OwnerID:         owner.ID,
		CreatedBy:       owner.ID,
		Ancestry:        []string{owner.ID, ancestorAgent.ID},
		Phase:           string(state.PhaseRunning),
		Activity:        string(state.ActivityIdle),
		MessageMode:     store.MessageModeProject,
		HarnessConfig:   config.ShellHarnessConfigName,
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: config.ShellHarnessConfigName,
			Env:           map[string]string{"MY_PRIVATE_VAR": "secret-value"},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, shellAgent))

	llmChild := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "llm-read-child",
		Name:            "llm-read-child",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		OwnerID:         ancestorAgent.ID,
		CreatedBy:       ancestorAgent.ID,
		Ancestry:        []string{owner.ID, ancestorAgent.ID},
		Phase:           string(state.PhaseRunning),
		Activity:        string(state.ActivityIdle),
		MessageMode:     store.MessageModeProject,
		HarnessConfig:   "claude",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Env:           map[string]string{"CHILD_VAR": "child-value"},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, llmChild))

	ownerIdent := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "web")
	ancestorIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ancestorAgent.ID},
		ProjectID: project.ID,
		Ancestry:  []string{owner.ID, ancestorAgent.ID},
		Scopes:    []AgentTokenScope{ScopeAgentLifecycle, ScopeProjectRead},
	}}
	ancestorToken := srv.generateRecordedAgentToken(t, ancestorAgent.ID, project.ID,
		[]AgentTokenScope{ScopeAgentLifecycle, ScopeProjectRead}, []string{owner.ID})

	t.Run("env reveal helpers deny agent principal on shell agent", func(t *testing.T) {
		allowed, indet := canIdentityViewAgentEnv(ctx, srv, ancestorIdent, shellAgent)
		assert.False(t, allowed)
		assert.False(t, indet)

		// Even with an explicit attach capability, envViewAllowed must deny agent principals on shell agents.
		capWithAttach := &Capabilities{Actions: []string{string(ActionRead), string(ActionAttach)}}
		assert.False(t, srv.envViewAllowed(ctx, ancestorIdent, shellAgent, capWithAttach))

		// Human owner is still allowed.
		ownerAllowed, ownerIndet := canIdentityViewAgentEnv(ctx, srv, ownerIdent, shellAgent)
		assert.True(t, ownerAllowed)
		assert.False(t, ownerIndet)
		assert.True(t, srv.envViewAllowed(ctx, ownerIdent, shellAgent, capWithAttach))

		// Control: ancestor agent can still view its LLM child's env.
		childAllowed, _ := canIdentityViewAgentEnv(ctx, srv, ancestorIdent, llmChild)
		assert.True(t, childAllowed)
	})

	t.Run("GET /api/v1/agents/{id} redacts env for agent principal on shell agent", func(t *testing.T) {
		recAgent := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+shellAgent.ID, nil, ancestorToken)
		require.Equal(t, http.StatusOK, recAgent.Code, recAgent.Body.String())
		assert.NotContains(t, recAgent.Body.String(), "secret-value")

		recOwner := doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/agents/"+shellAgent.ID, nil)
		require.Equal(t, http.StatusOK, recOwner.Code, recOwner.Body.String())
		assert.Contains(t, recOwner.Body.String(), "secret-value")
	})

	t.Run("agentFullHistoryDecide and message-logs deny agent principal on shell agent", func(t *testing.T) {
		decAgent := srv.agentFullHistoryDecision(ctx, ancestorIdent, shellAgent)
		assert.False(t, decAgent.Allowed)
		assert.Contains(t, decAgent.Reason, "human principals only")

		decDirect := agentFullHistoryDecide(ctx, srv.authzService, ancestorIdent, shellAgent)
		assert.False(t, decDirect.Allowed)

		// Human owner still gets full history.
		decOwner := srv.agentFullHistoryDecision(ctx, ownerIdent, shellAgent)
		assert.True(t, decOwner.Allowed)

		// Control: ancestor agent still gets full history on its LLM child.
		decChild := srv.agentFullHistoryDecision(ctx, ancestorIdent, llmChild)
		assert.True(t, decChild.Allowed)

		// HTTP message-logs endpoint refuses agent principal on shell agent.
		recMsgLogs := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+shellAgent.ID+"/message-logs", nil, ancestorToken)
		assert.Equal(t, http.StatusForbidden, recMsgLogs.Code, recMsgLogs.Body.String())
	})
}

// TestShellAgentProjectAdminGovernance_S10 verifies design §3.4 S10 / AC 10:
// project governance roles (project-admin holding agent.lifecycle, and
// project-owner holding agent.lifecycle + agent.delete, neither holding
// agent.attach on another member's agent) can stop/suspend (and project-owner
// can delete) another user's execution container, while remaining unable to
// attach, exec, read logs, reveal env, expose/proxy ports, or change ownership.
func TestShellAgentProjectAdminGovernance_S10(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetDispatcher(&p2OwnShellTestDispatcher{})
	project, broker := p2OwnSeedProjectAndBroker(t, s)

	owner := p2OwnSeedUser(t, s, "s10-owner@test.com", "S10 Owner")
	projAdmin := p2OwnSeedUser(t, s, "s10-admin@test.com", "S10 Admin")
	projOwner := p2OwnSeedUser(t, s, "s10-proj-owner@test.com", "S10 Project Owner")
	p2OwnBindProjectRole(t, s, project.ID, owner.ID, "project-member")
	p2OwnBindProjectRole(t, s, project.ID, projAdmin.ID, "project-admin")
	p2OwnBindProjectRole(t, s, project.ID, projOwner.ID, "project-owner")

	makeShell := func(slug string) *store.Agent {
		a := &store.Agent{
			ID:              api.NewUUID(),
			Slug:            slug,
			Name:            slug,
			ProjectID:       project.ID,
			RuntimeBrokerID: broker.ID,
			OwnerID:         owner.ID,
			CreatedBy:       owner.ID,
			Ancestry:        []string{owner.ID},
			Phase:           string(state.PhaseRunning),
			Activity:        string(state.ActivityIdle),
			MessageMode:     store.MessageModeProject,
			HarnessConfig:   config.ShellHarnessConfigName,
			AppliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: config.ShellHarnessConfigName,
				Env:           map[string]string{"OWNER_SECRET": "do-not-leak"},
			},
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}

	t.Run("project admin cannot observe, exec, expose, or re-own", func(t *testing.T) {
		a := makeShell("shell-s10-deny")

		// Env is redacted on GET.
		recGet := doRequestAsUser(t, srv, projAdmin, http.MethodGet, "/api/v1/agents/"+a.ID, nil)
		require.Equal(t, http.StatusOK, recGet.Code, recGet.Body.String())
		assert.NotContains(t, recGet.Body.String(), "do-not-leak")

		// Exec is denied (403).
		recExec := doRequestAsUser(t, srv, projAdmin, http.MethodPost, "/api/v1/agents/"+a.ID+"/exec", map[string]any{"command": []string{"id"}})
		assert.Equal(t, http.StatusForbidden, recExec.Code, recExec.Body.String())

		// Logs are denied (403).
		recLogs := doRequestAsUser(t, srv, projAdmin, http.MethodGet, "/api/v1/agents/"+a.ID+"/logs", nil)
		assert.Equal(t, http.StatusForbidden, recLogs.Code, recLogs.Body.String())

		// Ownership change is denied (403).
		recOwner := doRequestAsUser(t, srv, projAdmin, http.MethodPatch, "/api/v1/agents/"+a.ID, map[string]any{"ownerId": projAdmin.ID})
		assert.Equal(t, http.StatusForbidden, recOwner.Code, recOwner.Body.String())

		// Port expose is denied (403).
		recExpose := doRequestAsUser(t, srv, projAdmin, http.MethodPost, "/api/v1/agents/"+a.ID+"/ports", map[string]any{"port": 8080})
		assert.Equal(t, http.StatusForbidden, recExpose.Code, recExpose.Body.String())
	})

	t.Run("project admin can suspend execution container", func(t *testing.T) {
		a := makeShell("shell-s10-suspend")
		rec := doRequestAsUser(t, srv, projAdmin, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
		assert.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted,
			"expected 200/202 for project-admin suspend, got %d: %s", rec.Code, rec.Body.String())
	})

	t.Run("project admin can stop execution container", func(t *testing.T) {
		a := makeShell("shell-s10-stop")
		rec := doRequestAsUser(t, srv, projAdmin, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
		assert.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted,
			"expected 200/202 for project-admin stop, got %d: %s", rec.Code, rec.Body.String())
	})

	t.Run("project owner can delete execution container while still denied attach/re-own", func(t *testing.T) {
		a := makeShell("shell-s10-delete")

		recExec := doRequestAsUser(t, srv, projOwner, http.MethodPost, "/api/v1/agents/"+a.ID+"/exec", map[string]any{"command": []string{"id"}})
		assert.Equal(t, http.StatusForbidden, recExec.Code, recExec.Body.String())

		recOwner := doRequestAsUser(t, srv, projOwner, http.MethodPatch, "/api/v1/agents/"+a.ID, map[string]any{"ownerId": projOwner.ID})
		assert.Equal(t, http.StatusForbidden, recOwner.Code, recOwner.Body.String())

		rec := doRequestAsUser(t, srv, projOwner, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
		assert.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted || rec.Code == http.StatusNoContent,
			"expected 200/202/204 for project-owner delete, got %d: %s", rec.Code, rec.Body.String())

		// Confirm response body is valid JSON if non-empty.
		if rec.Body.Len() > 0 {
			var parsed map[string]any
			assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
		}
	})
}

// TestShellAgentDelete_ForcesDeleteFilesForNonOwner verifies H1 (ptone/scion#8818):
// a caller holding agent.delete without ActionAttach (e.g. a project owner/admin)
// cannot preserve an execution container's host files with deleteFiles=false,
// preventing a delete+recreate $HOME credential takeover. The owning human may
// still pass deleteFiles=false, and non-shell agents are unchanged.
func TestShellAgentDelete_ForcesDeleteFilesForNonOwner(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &p2OwnShellTestDispatcher{}
	srv.SetDispatcher(disp)
	project, broker := p2OwnSeedProjectAndBroker(t, s)

	owner := p2OwnSeedUser(t, s, "h1-owner@test.com", "H1 Owner")
	projOwner := p2OwnSeedUser(t, s, "h1-proj-owner@test.com", "H1 Project Owner")
	p2OwnBindProjectRole(t, s, project.ID, owner.ID, "project-member")
	p2OwnBindProjectRole(t, s, project.ID, projOwner.ID, "project-owner")

	t.Run("non-owner delete with force=true&deleteFiles=false forces deleteFiles=true", func(t *testing.T) {
		a := p2OwnSeedAgent(t, s, project.ID, broker.ID, "shell-h1-admin-del", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID})
		rec := doRequestAsUser(t, srv, projOwner, http.MethodDelete, "/api/v1/agents/"+a.ID+"?force=true&deleteFiles=false", nil)
		require.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted || rec.Code == http.StatusNoContent,
			"expected 200/202/204, got %d: %s", rec.Code, rec.Body.String())

		gotDeleteFiles, dispatched := disp.deletedFiles[a.ID]
		require.True(t, dispatched, "DispatchAgentDelete must be called")
		assert.True(t, gotDeleteFiles, "non-owner deleting execution container must force deleteFiles=true")
	})

	t.Run("owner delete with force=true&deleteFiles=false may keep files", func(t *testing.T) {
		a := p2OwnSeedAgent(t, s, project.ID, broker.ID, "shell-h1-owner-del", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID})
		rec := doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/agents/"+a.ID+"?force=true&deleteFiles=false", nil)
		require.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted || rec.Code == http.StatusNoContent,
			"expected 200/202/204, got %d: %s", rec.Code, rec.Body.String())

		gotDeleteFiles, dispatched := disp.deletedFiles[a.ID]
		require.True(t, dispatched, "DispatchAgentDelete must be called")
		assert.False(t, gotDeleteFiles, "owning human deleting own execution container may keep files")
	})

	t.Run("control: non-owner delete with deleteFiles=false on LLM agent still keeps files", func(t *testing.T) {
		a := p2OwnSeedAgent(t, s, project.ID, broker.ID, "llm-h1-admin-del", owner.ID, "claude", state.PhaseRunning, []string{owner.ID})
		rec := doRequestAsUser(t, srv, projOwner, http.MethodDelete, "/api/v1/agents/"+a.ID+"?force=true&deleteFiles=false", nil)
		require.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted || rec.Code == http.StatusNoContent,
			"expected 200/202/204, got %d: %s", rec.Code, rec.Body.String())

		gotDeleteFiles, dispatched := disp.deletedFiles[a.ID]
		require.True(t, dispatched, "DispatchAgentDelete must be called")
		assert.False(t, gotDeleteFiles, "non-shell agent deleteFiles=false behaviour must remain unchanged")
		_ = ctx
	})
}
