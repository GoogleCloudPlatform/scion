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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedShellTestProjectAndBroker(t *testing.T, s store.Store) (*store.Project, *store.RuntimeBroker) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "shell-test-broker",
		Slug:   "shell-test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:                     api.NewUUID(),
		Name:                   "Shell Test Project",
		Slug:                   "shell-test-project",
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

func seedShellTestUser(t *testing.T, s store.Store, email, displayName string) *store.User {
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

func bindProjectRole(t *testing.T, s store.Store, projectID, userID, roleName string) {
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

func seedShellTestAgent(t *testing.T, s store.Store, projectID, brokerID, slug, ownerID, harnessConfig string, phase state.Phase, ancestry []string) *store.Agent {
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
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: harnessConfig,
		},
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

// AC 3 & AC 9: Shell agent creation admission — experiment gate, S6 human-only,
// and initial task rejection.
func TestShellAgentCreateAdmission(t *testing.T) {
	srv, s := testServer(t)
	project, _ := seedShellTestProjectAndBroker(t, s)

	owner := seedShellTestUser(t, s, "owner-shell@test.com", "Owner Shell")
	bindProjectRole(t, s, project.ID, owner.ID, "project-owner")

	parentAgent := seedShellTestAgent(t, s, project.ID, project.DefaultRuntimeBrokerID, "parent-llm", owner.ID, "claude", state.PhaseRunning, []string{owner.ID})
	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: parentAgent.ID},
		ProjectID: project.ID,
		Ancestry:  []string{owner.ID, parentAgent.ID},
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentCreate, ScopeAgentLifecycle},
	}}

	// 1. Flag OFF + human caller -> 400 experiment_disabled.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "my-shell-off",
		HarnessConfig: config.ShellHarnessConfigName,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeExperimentDisabled, errResp.Error.Code)

	// 2. S6: Agent principal creating a shell agent -> 403 even with flag OFF.
	rec = doRequestAsIdentity(t, srv, agentIdent, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "agent-shell-off",
		HarnessConfig: config.ShellHarnessConfigName,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "shell agent: human principals only")

	// Turn hub.exec_containers ON.
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})

	// 3. S6: Agent principal creating a shell agent -> 403 with flag ON.
	rec = doRequestAsIdentity(t, srv, agentIdent, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "agent-shell-on",
		HarnessConfig: config.ShellHarnessConfigName,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "shell agent: human principals only")

	// 4. Initial task rejected -> 400 task_not_supported.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "my-shell-task",
		HarnessConfig: config.ShellHarnessConfigName,
		Task:          "rm -rf /",
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	errResp = ErrorResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeTaskNotSupported, errResp.Error.Code)

	// 5. Human caller with flag ON and no task -> succeeds (201).
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "my-shell-ok",
		HarnessConfig: config.ShellHarnessConfigName,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// AC 7 (S1/S2/S3): authorizeAgentTargetAction choke point + exec schedule
// authoring and firing against shell agents.
func TestShellAgentAuthzChokePointAndExecSchedules(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, broker := seedShellTestProjectAndBroker(t, s)

	owner := seedShellTestUser(t, s, "owner-authz@test.com", "Owner Authz")
	admin := seedShellTestUser(t, s, "admin-authz@test.com", "Admin Authz")
	bindProjectRole(t, s, project.ID, owner.ID, "project-owner")
	bindProjectRole(t, s, project.ID, admin.ID, "project-admin")

	// Seed an ancestor agent and a sibling same-project agent with ScopeAgentLifecycle.
	ancestorAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "ancestor-llm", owner.ID, "claude", state.PhaseRunning, []string{owner.ID})
	siblingAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "sibling-llm", owner.ID, "claude", state.PhaseRunning, []string{owner.ID})

	// Shell agent with ancestorAgent in its ancestry (worst-case test for S1) and experiment OFF (protections are ungated).
	shellAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "ops-shell", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID, ancestorAgent.ID})

	ownerIdent := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "session")
	adminIdent := NewAuthenticatedUser(admin.ID, admin.Email, admin.DisplayName, admin.Role, "session")
	ancestorIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ancestorAgent.ID},
		ProjectID: project.ID,
		Ancestry:  []string{owner.ID, ancestorAgent.ID},
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentLifecycle},
	}}
	siblingIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: siblingAgent.ID},
		ProjectID: project.ID,
		Ancestry:  []string{owner.ID, siblingAgent.ID},
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentLifecycle},
	}}

	// 1. Owner human -> ActionAttach allowed.
	assert.Nil(t, srv.authorizeAgentTargetAction(ctx, ownerIdent, shellAgent, ActionAttach))

	// 2. Project admin without attach -> ActionAttach denied (403), ActionLifecycle allowed.
	denial := srv.authorizeAgentTargetAction(ctx, adminIdent, shellAgent, ActionAttach)
	require.NotNil(t, denial)
	assert.Equal(t, http.StatusForbidden, denial.status)
	assert.Nil(t, srv.authorizeAgentTargetAction(ctx, adminIdent, shellAgent, ActionLifecycle))

	// 3. Ancestor agent with ScopeAgentLifecycle -> ActionAttach (and ActionStop) denied with 403 "shell agent: human principals only".
	denial = srv.authorizeAgentTargetAction(ctx, ancestorIdent, shellAgent, ActionAttach)
	require.NotNil(t, denial)
	assert.Equal(t, http.StatusForbidden, denial.status)
	assert.Equal(t, "shell agent: human principals only", denial.reason)

	// 4. Same-project agent with ScopeAgentLifecycle -> ActionAttach denied with 403 "shell agent: human principals only".
	denial = srv.authorizeAgentTargetAction(ctx, siblingIdent, shellAgent, ActionAttach)
	require.NotNil(t, denial)
	assert.Equal(t, http.StatusForbidden, denial.status)
	assert.Equal(t, "shell agent: human principals only", denial.reason)

	// 5. Exec schedule authoring (S3): agent principal -> 403; owner human -> allowed.
	payload := &scheduledExecPayload{AgentID: shellAgent.ID, Command: []string{"echo", "hi"}}
	reqAgent := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/scheduled-events", nil)
	reqAgent = reqAgent.WithContext(contextWithIdentity(ctx, ancestorIdent))
	rec := httptest.NewRecorder()
	_, ok := srv.authorizeScheduledExecAuthoring(rec, reqAgent, project.ID, payload)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	reqOwner := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/scheduled-events", nil)
	reqOwner = reqOwner.WithContext(contextWithIdentity(ctx, ownerIdent))
	rec = httptest.NewRecorder()
	gotTarget, ok := srv.authorizeScheduledExecAuthoring(rec, reqOwner, project.ID, payload)
	assert.True(t, ok, rec.Body.String())
	require.NotNil(t, gotTarget)
	assert.Equal(t, shellAgent.ID, gotTarget.ID)

	// 6. Exec schedule firing (S3): agent principal -> refused; owner human -> allowed (and validateAgentRunning passes).
	fullCeiling := store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	_, err := srv.authorizeExecFire(ctx, project.ID, FireAuthority{Ceiling: fullCeiling, PrincipalKind: "agent", PrincipalID: ancestorAgent.ID}, ancestorIdent, payload)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell agent: human principals only")

	fireTarget, err := srv.authorizeExecFire(ctx, project.ID, FireAuthority{Ceiling: fullCeiling, PrincipalKind: "user", PrincipalID: owner.ID}, ownerIdent, payload)
	require.NoError(t, err)
	assert.Equal(t, shellAgent.ID, fireTarget.ID)
	assert.Nil(t, validateAgentRunning(fireTarget), "running shell agent must pass exec-action running preflight")
}

// AC 3 & AC 8 (S5): All message delivery paths refuse or skip shell agents,
// regardless of whether hub.exec_containers is on or off.
func TestShellAgentMessageDeliveryRefusedAndSkipped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, broker := seedShellTestProjectAndBroker(t, s)

	owner := seedShellTestUser(t, s, "owner-msg@test.com", "Owner Msg")
	bindProjectRole(t, s, project.ID, owner.ID, "project-owner")

	normalAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "normal-llm", owner.ID, "claude", state.PhaseRunning, []string{owner.ID})
	shellAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "ops-shell", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID})
	suspendedShell := seedShellTestAgent(t, s, project.ID, broker.ID, "ops-shell-susp", owner.ID, config.ShellHarnessConfigName, state.PhaseSuspended, []string{owner.ID})

	// 1. validateAgentDeliverable refuses shell agents (running or suspended) with ErrCodeAgentNotMessageable.
	dmErr := validateAgentDeliverable(shellAgent)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotMessageable, dmErr.Code)
	dmErr = validateAgentDeliverable(suspendedShell)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotMessageable, dmErr.Code)

	// 2. Human owner DM to shell agent (with and without --wake) -> 422 agent_not_messageable.
	for _, tc := range []struct {
		name   string
		target *store.Agent
		wake   bool
	}{
		{"running no wake", shellAgent, false},
		{"suspended with wake", suspendedShell, true},
	} {
		t.Run("DM_"+tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+tc.target.ID+"/message", map[string]interface{}{
				"message": "hello shell",
				"wake":    tc.wake,
			})
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeAgentNotMessageable, errResp.Error.Code)
		})
	}

	// 3. Agent-to-agent ExecuteAgentDM -> ErrCodeAgentNotMessageable.
	recDisp := &recordingBroadcastDispatcher{}
	srv.SetDispatcher(recDisp)
	senderIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: normalAgent.ID},
		ProjectID: project.ID,
		Ancestry:  []string{owner.ID, normalAgent.ID},
		Scopes:    []AgentTokenScope{ScopeProjectRead},
	}}
	_, dmErr = srv.ExecuteAgentDM(ctx, &AgentDMInput{
		ProjectID:      project.ID,
		SenderAgent:    normalAgent,
		SenderIdentity: senderIdent,
		TargetAgent:    shellAgent,
		Msg:            "agent dm to shell",
		Type:           messages.TypeInstruction,
	})
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotMessageable, dmErr.Code)

	// 4. Broadcast silently skips shell agent and delivers to normal agent.
	bcastMsg := messages.NewInstruction("user:"+owner.Email, "project:all", "hello everyone")
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast", map[string]interface{}{
		"structured_message": bcastMsg,
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Contains(t, recDisp.deliveredSlugs, normalAgent.Slug)
	assert.NotContains(t, recDisp.deliveredSlugs, shellAgent.Slug)

	// 5. Scheduled message and trigger message authoring targeting shell agent -> 422 agent_not_messageable.
	ownerIdent := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "session")
	rawPayload, _ := json.Marshal(MessageEventPayload{AgentID: shellAgent.ID, Message: "scheduled hi"})
	for _, caller := range []messageAuthoringCaller{messageAuthoringCallerSchedule, messageAuthoringCallerTrigger} {
		_, authErr := srv.validateMessageAuthoringTarget(ctx, ownerIdent, project.ID, caller, string(rawPayload), shellAgent.ID, shellAgent.Slug)
		require.Error(t, authErr)
		var maErr *messageAuthoringError
		require.ErrorAs(t, authErr, &maErr)
		assert.Equal(t, ErrCodeAgentNotMessageable, maErr.code)
	}

	// 6. Trigger/scheduled message firing (messageAction.Authorize) targeting shell agent -> refused.
	act := &messageAction{srv: srv}
	_, refusal, err := act.Authorize(ctx, hubfire.FireRequest{
		ProjectID: project.ID,
		Payload:   string(rawPayload),
		FireAt:    time.Now(),
		Authority: hubfire.RecordedAuthority{FireID: "fire-1"},
	}, FireAuthority{Ceiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, PrincipalKind: "user", PrincipalID: owner.ID}, ownerIdent)
	require.NoError(t, err)
	require.NotNil(t, refusal)
	assert.Contains(t, refusal.Reason, "cannot receive messages")
}

// recordingBroadcastDispatcher is a minimal AgentDispatcher that records
// message dispatch slugs for broadcast skip assertions.
type recordingBroadcastDispatcher struct {
	mockDispatcher
	deliveredSlugs []string
}

func (d *recordingBroadcastDispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	if agent != nil {
		d.deliveredSlugs = append(d.deliveredSlugs, agent.Slug)
	}
	return nil
}

// AC 11: Hub harness-config create/update/patch/clone to slug "shell" is
// refused with 400 reserved_name at every scope (ungated), and bootstrap skips
// on-disk "shell" directories.
func TestShellHarnessConfigSlugReservedOnHub(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, _ := seedShellTestProjectAndBroker(t, s)

	// 1. Create at global, project, and user scopes (flag OFF) -> 400 reserved_name.
	for _, tc := range []struct {
		name    string
		scope   string
		scopeID string
	}{
		{"global", store.HarnessConfigScopeGlobal, ""},
		{"project", store.HarnessConfigScopeProject, project.ID},
		{"user", store.HarnessConfigScopeUser, ""},
	} {
		t.Run("create_"+tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
				Name:    "shell",
				Harness: "claude",
				Scope:   tc.scope,
				ScopeID: tc.scopeID,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeReservedName, errResp.Error.Code)
		})
	}

	// Seed an existing config to test PUT/PATCH/clone renames to "shell".
	existing := &store.HarnessConfig{
		ID:      api.NewUUID(),
		Name:    "other-hc",
		Slug:    "other-hc",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeGlobal,
		Status:  store.HarnessConfigStatusActive,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, existing))

	// 2. PUT rename to "shell" -> 400 reserved_name.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+existing.ID, UpdateHarnessConfigRequest{
		Name: "shell",
		Slug: "shell",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeReservedName)

	// 3. PATCH rename to "shell" -> 400 reserved_name.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/harness-configs/"+existing.ID, map[string]string{
		"slug": "shell",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeReservedName)

	// 4. Clone to "shell" -> 400 reserved_name.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+existing.ID+"/clone", map[string]string{
		"name":  "shell",
		"scope": store.HarnessConfigScopeGlobal,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeReservedName)

	// 5. BootstrapHarnessConfigsFromDir skips on-disk "shell" dir.
	hcRoot := t.TempDir()
	shellDir := filepath.Join(hcRoot, "shell")
	require.NoError(t, os.MkdirAll(shellDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(shellDir, "config.yaml"), []byte("harness: claude\nimage: evil:latest\nuser: scion\n"), 0644))
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, hcRoot))
	_, err := s.GetHarnessConfigBySlug(ctx, "shell", store.HarnessConfigScopeGlobal, "")
	assert.ErrorIs(t, err, store.ErrNotFound, "on-disk shell dir must not be imported")
}
