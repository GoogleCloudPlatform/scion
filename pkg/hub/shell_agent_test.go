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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
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
	assert.Contains(t, rec.Body.String(), "execution container: human principals only")

	// Turn hub.exec_containers ON.
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})

	// 3. S6: Agent principal creating a shell agent -> 403 with flag ON.
	rec = doRequestAsIdentity(t, srv, agentIdent, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "agent-shell-on",
		HarnessConfig: config.ShellHarnessConfigName,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "execution container: human principals only")

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

	// 3. Ancestor agent with ScopeAgentLifecycle -> ActionAttach (and ActionStop) denied with 403 "execution container: human principals only".
	denial = srv.authorizeAgentTargetAction(ctx, ancestorIdent, shellAgent, ActionAttach)
	require.NotNil(t, denial)
	assert.Equal(t, http.StatusForbidden, denial.status)
	assert.Equal(t, "execution container: human principals only", denial.reason)

	// 4. Same-project agent with ScopeAgentLifecycle -> ActionAttach denied with 403 "execution container: human principals only".
	denial = srv.authorizeAgentTargetAction(ctx, siblingIdent, shellAgent, ActionAttach)
	require.NotNil(t, denial)
	assert.Equal(t, http.StatusForbidden, denial.status)
	assert.Equal(t, "execution container: human principals only", denial.reason)

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
	assert.Contains(t, err.Error(), "execution container: human principals only")

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

// R2 (AC8, S5): dispatchMarkedAgentMessage / dispatchWithBrokerRetry refuses
// shell agents before DispatchAgentMessage is ever reached, with a typed
// pre-dispatch error classified by isPreDispatchDeliveryError.
func TestShellAgentDispatchChokePointNeverReachesDispatcher(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, broker := seedShellTestProjectAndBroker(t, s)
	owner := seedShellTestUser(t, s, "owner-choke@test.com", "Owner Choke")

	shellAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "choke-shell", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID})

	recDisp := &recordingBroadcastDispatcher{}
	err := dispatchWithBrokerRetry(ctx, srv.deliveryMarker(), nil, recDisp, shellAgent, "must not paste", false, nil)
	require.Error(t, err)
	assert.True(t, isPreDispatchDeliveryError(err), "shell refusal must be classified as a pre-dispatch error")
	var notMsgErr *errAgentNotMessageable
	assert.ErrorAs(t, err, &notMsgErr)
	assert.Empty(t, recDisp.deliveredSlugs, "DispatchAgentMessage must never be called for a shell agent")
}

// R2 (AC8, S5): external message-broker inbound routes (/api/v1/broker/inbound
// and /api/v1/broker/inbound/routed) refuse delivery to shell agents without
// ever invoking the dispatcher.
func TestShellAgentBrokerInboundRefused(t *testing.T) {
	srv, s := testServer(t)
	project, broker := seedShellTestProjectAndBroker(t, s)
	owner := seedShellTestUser(t, s, "owner-inbound@test.com", "Owner Inbound")
	bindProjectRole(t, s, project.ID, owner.ID, "project-owner")

	shellAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "inbound-shell", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID})
	recDisp := &recordingBroadcastDispatcher{}
	srv.SetDispatcher(recDisp)

	postBroker := func(path string, payload interface{}) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Plugin-Name", "test-plugin")
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		return rec
	}

	// 1. Direct broker inbound -> 422 agent_not_messageable.
	rec := postBroker("/api/v1/broker/inbound", inboundMessageRequest{
		Topic: "scion.project." + project.ID + ".agent." + shellAgent.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			Sender:    "user:" + owner.Email,
			Recipient: "agent:" + shellAgent.Slug,
			Msg:       "hello from slack",
			Type:      messages.TypeInstruction,
		},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeAgentNotMessageable)

	// 2. Routed broker inbound -> recipient status "not_messageable".
	rec = postBroker("/api/v1/broker/inbound/routed", routedInboundRequest{
		ProjectID:    project.ID,
		DefaultAgent: shellAgent.Slug,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			Sender:    "user:" + owner.Email,
			Msg:       "routed hello from slack",
			Type:      messages.TypeInstruction,
		},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeAgentNotMessageable)
	assert.Contains(t, rec.Body.String(), "not_messageable")
	assert.Empty(t, recDisp.deliveredSlugs, "DispatchAgentMessage must never be called for a shell agent")
}

// O2: S6 allowlists only interactive/dev human principals ("user" / "dev")
// and fails closed on nil, agent, delegated, or unknown principal kinds.
func TestIsHumanShellPrincipal_Allowlist(t *testing.T) {
	assert.True(t, isHumanShellPrincipal(NewAuthenticatedUser("u1", "u@test.com", "User", "member", "session")))
	assert.True(t, isHumanShellPrincipal(NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev"})))
	assert.False(t, isHumanShellPrincipal(nil))
	assert.False(t, isHumanShellPrincipal(&agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: "a1"}}}))
	assert.False(t, isHumanShellPrincipal(NewFederatedServiceIdentity("https://issuer.example", "svc", "svc@test.com", nil)))
}

// R2 (AC2 / §8 exit gate): an attached-terminal heartbeat on a shell agent in
// running/idle refreshes activity_since so idle-suspend waits IdleSuspendAfter
// after detach (not ~90s). Non-shell agents are unaffected.
func TestShellAgentIdleSuspend_AttachedRefreshesActivitySince(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &recordingBroadcastDispatcher{}
	srv.SetDispatcher(disp)

	idleAfter := 15 * time.Minute
	setExperimentOverrides(t, srv, map[string]bool{experiments.IdleSuspend: true})
	srv.mu.Lock()
	srv.config.AutoSuspendIdle = true
	srv.config.IdleSuspendAfter = idleAfter
	srv.mu.Unlock()

	project, broker := seedShellTestProjectAndBroker(t, s)
	owner := seedShellTestUser(t, s, "owner-idle@test.com", "Owner Idle")

	now := time.Now().UTC()
	longAgo := now.Add(-30 * time.Minute)
	freshSeen := now.Add(-30 * time.Second)

	seedIdleAgent := func(slug, hc string) *store.Agent {
		a := &store.Agent{
			ID:              api.NewUUID(),
			Slug:            slug,
			Name:            slug,
			ProjectID:       project.ID,
			RuntimeBrokerID: broker.ID,
			OwnerID:         owner.ID,
			CreatedBy:       owner.ID,
			Phase:           string(state.PhaseRunning),
			Activity:        string(state.ActivityIdle),
			ActivitySince:   &longAgo,
			LastSeen:        freshSeen,
			RunID:           "run-" + slug,
			RunIntent:       store.RunIntentRunning,
			AppliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: hc,
			},
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		// Pin activity_since to 30m ago directly (CreateAgent stamps now).
		db := s.(*entadapter.CompositeStore).DB()
		_, err := db.ExecContext(ctx, "UPDATE agents SET activity_since = ?, last_seen = ? WHERE id = ?", longAgo, freshSeen, a.ID)
		require.NoError(t, err)
		return a
	}

	shellAgent := seedIdleAgent("shell-idle-guard", config.ShellHarnessConfigName)
	llmAgent := seedIdleAgent("llm-idle-guard", "claude")

	// 1. Attached heartbeat at T=now refreshes activity_since on shell agent only.
	attachedUpdate := store.AgentStatusUpdate{
		Heartbeat: true,
		LiveWork:  &store.LiveWork{Kind: "attached", SuppressStall: false, Detail: "terminal attached"},
	}
	require.NoError(t, s.UpdateAgentStatus(ctx, shellAgent.ID, attachedUpdate))
	require.NoError(t, s.UpdateAgentStatus(ctx, llmAgent.ID, attachedUpdate))

	gotShell, err := s.GetAgent(ctx, shellAgent.ID)
	require.NoError(t, err)
	require.NotNil(t, gotShell.ActivitySince)
	assert.True(t, gotShell.ActivitySince.After(now.Add(-5*time.Second)),
		"attached heartbeat on shell agent must refresh activity_since to ~now; got %v", gotShell.ActivitySince)

	gotLLM, err := s.GetAgent(ctx, llmAgent.ID)
	require.NoError(t, err)
	require.NotNil(t, gotLLM.ActivitySince)
	assert.True(t, gotLLM.ActivitySince.Before(now.Add(-20*time.Minute)),
		"attached heartbeat on non-shell agent must not refresh activity_since; got %v", gotLLM.ActivitySince)

	// 2. Simulate T + 2 min after detach (last_live_process_at stale > 90s,
	//    activity_since = now - 2m < IdleSuspendAfter 15m): sweep must NOT suspend.
	db := s.(*entadapter.CompositeStore).DB()
	twoMinAgo := now.Add(-2 * time.Minute)
	_, err = db.ExecContext(ctx,
		"UPDATE agents SET activity_since = ?, last_live_process_at = ?, last_seen = ? WHERE id = ?",
		twoMinAgo, twoMinAgo, freshSeen, shellAgent.ID)
	require.NoError(t, err)

	srv.agentIdleSuspendHandler()(ctx)
	srv.waitAutoSuspendIdle()

	gotShell, err = s.GetAgent(ctx, shellAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotShell.Phase, "shell agent must not be suspended 2m after detach (< IdleSuspendAfter)")

	// 3. Simulate T + IdleSuspendAfter + 1m after detach: sweep MUST suspend.
	pastCutoff := now.Add(-(idleAfter + time.Minute))
	_, err = db.ExecContext(ctx,
		"UPDATE agents SET activity_since = ?, last_live_process_at = ?, last_seen = ? WHERE id = ?",
		pastCutoff, pastCutoff, freshSeen, shellAgent.ID)
	require.NoError(t, err)

	srv.agentIdleSuspendHandler()(ctx)
	srv.waitAutoSuspendIdle()

	gotShell, err = s.GetAgent(ctx, shellAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), gotShell.Phase, "shell agent must be suspended after detach + IdleSuspendAfter")
	assert.Equal(t, string(state.SuspendReasonIdle), gotShell.SuspendReason)
}

// Security audit H1, H3 & L2:
//   - H1: Shell agents refuse explicit templates (400), refuse templates that
//     declare default_harness_config: shell (400), and drop defaulted templates
//     (project "default" template and project default_template annotation).
//   - H3: Shell agents never receive project (or hub) pre-start hooks in
//     AppliedConfig.ProjectPreStartHookID / ProjectPreStartHookScript.
//   - L2: isHumanShellCreatePrincipal requires both the user/dev allowlist and
//     store.DelegationPrincipalUser.
func TestShellAgentCreate_NoTemplateAndNoPreStartHook(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})

	project, _ := seedShellTestProjectAndBroker(t, s)
	owner := seedShellTestUser(t, s, "owner-h1h3@test.com", "Owner H1H3")
	bindProjectRole(t, s, project.ID, owner.ID, "project-owner")

	// Seed a project-scoped "default" template, a project-scoped member template
	// (set as the project's default_template annotation), and an active project
	// pre-start hook.
	defaultTpl := &store.Template{
		ID:          api.NewUUID(),
		Name:        "default",
		Slug:        "default",
		Scope:       store.TemplateScopeProject,
		ScopeID:     project.ID,
		Status:      store.TemplateStatusActive,
		ContentHash: "hash-default-tpl",
		Files:       []store.TemplateFile{{Path: "home/.bashrc", Size: 16, Hash: "abc"}},
	}
	require.NoError(t, s.CreateTemplate(ctx, defaultTpl))

	memberTpl := &store.Template{
		ID:          api.NewUUID(),
		Name:        "member-tpl",
		Slug:        "member-tpl",
		Scope:       store.TemplateScopeProject,
		ScopeID:     project.ID,
		Status:      store.TemplateStatusActive,
		ContentHash: "hash-member-tpl",
		Files:       []store.TemplateFile{{Path: "home/.bashrc", Size: 16, Hash: "def"}},
	}
	require.NoError(t, s.CreateTemplate(ctx, memberTpl))

	shellDeclaringTpl := &store.Template{
		ID:                   api.NewUUID(),
		Name:                 "shell-decl-tpl",
		Slug:                 "shell-decl-tpl",
		Scope:                store.TemplateScopeProject,
		ScopeID:              project.ID,
		Status:               store.TemplateStatusActive,
		ContentHash:          "hash-shell-decl",
		DefaultHarnessConfig: config.ShellHarnessConfigName,
		Files:                []store.TemplateFile{{Path: "scion-agent.json", Size: 32, Hash: "ghi"}},
	}
	require.NoError(t, s.CreateTemplate(ctx, shellDeclaringTpl))

	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ID:        api.NewUUID(),
		Scope:     store.PreStartHookScopeProject,
		ProjectID: project.ID,
		Name:      "project-hook",
		Slug:      "project-hook",
		Script:    "#!/bin/sh\necho project-hook-ran\n",
		Status:    store.ProjectPreStartHookStatusActive,
		CreatedBy: owner.ID,
	})
	require.NoError(t, err)

	// 1. Explicit template with harness_config=shell -> 400 validation_error.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "shell-explicit-tpl",
		Template:      "member-tpl",
		HarnessConfig: config.ShellHarnessConfigName,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "explicit template on shell agent must be refused: %s", rec.Body.String())

	// 2. Shell create with no explicit template while project has a "default"
	//    template (implicit default) and an active project pre-start hook -> 201,
	//    template dropped and pre-start hook NOT stamped.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "shell-implicit-default",
		HarnessConfig: config.ShellHarnessConfigName,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "shell create with implicit default template should succeed: %s", rec.Body.String())
	var created1 struct {
		Agent store.Agent `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created1))
	got1, err := s.GetAgent(ctx, created1.Agent.ID)
	require.NoError(t, err)
	assert.Empty(t, got1.Template, "defaulted template must be dropped from shell agent")
	require.NotNil(t, got1.AppliedConfig)
	assert.Empty(t, got1.AppliedConfig.TemplateID, "TemplateID must be cleared on shell agent")
	assert.Empty(t, got1.AppliedConfig.TemplateHash, "TemplateHash must be cleared on shell agent")
	assert.Empty(t, got1.AppliedConfig.ProjectPreStartHookID, "ProjectPreStartHookID must be empty on shell agent")
	assert.Empty(t, got1.AppliedConfig.ProjectPreStartHookScript, "ProjectPreStartHookScript must be empty on shell agent")

	// 3. Set project default_template annotation to member-tpl: shell create
	//    drops the project-defaulted template and skips the pre-start hook;
	//    normal LLM create still inherits both.
	project.Annotations = map[string]string{projectSettingDefaultTemplate: "member-tpl"}
	require.NoError(t, s.UpdateProject(ctx, project))

	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "shell-project-default",
		HarnessConfig: config.ShellHarnessConfigName,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "shell create with project default_template annotation should succeed: %s", rec.Body.String())
	var created2 struct {
		Agent store.Agent `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created2))
	got2, err := s.GetAgent(ctx, created2.Agent.ID)
	require.NoError(t, err)
	assert.Empty(t, got2.Template)
	require.NotNil(t, got2.AppliedConfig)
	assert.Empty(t, got2.AppliedConfig.TemplateID)
	assert.Empty(t, got2.AppliedConfig.TemplateHash)
	assert.Empty(t, got2.AppliedConfig.ProjectPreStartHookID)
	assert.Empty(t, got2.AppliedConfig.ProjectPreStartHookScript)

	// Control: a normal LLM agent in the same project still receives both.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "llm-control",
		HarnessConfig: "claude",
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "control LLM create should succeed: %s", rec.Body.String())
	var createdLLM struct {
		Agent store.Agent `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createdLLM))
	gotLLM, err := s.GetAgent(ctx, createdLLM.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "member-tpl", gotLLM.Template)
	require.NotNil(t, gotLLM.AppliedConfig)
	assert.Equal(t, memberTpl.ID, gotLLM.AppliedConfig.TemplateID)
	assert.NotEmpty(t, gotLLM.AppliedConfig.ProjectPreStartHookScript)

	// 4. Project default_template pointing at a template that itself declares
	//    default_harness_config: shell -> 400 (fail-closed; templates cannot
	//    turn a create into a shell container).
	project.Annotations = map[string]string{projectSettingDefaultTemplate: "shell-decl-tpl"}
	require.NoError(t, s.UpdateProject(ctx, project))

	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:          "shell-from-tpl-default",
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "template supplying default_harness_config=shell must be refused: %s", rec.Body.String())
}

func TestIsHumanShellCreatePrincipal(t *testing.T) {
	user := NewAuthenticatedUser("u-1", "u@test.com", "U", "member", "user")
	dev := NewAuthenticatedUser("dev-1", "dev@test.com", "Dev", "admin", "dev")

	assert.True(t, isHumanShellCreatePrincipal(user, store.DelegationPrincipalUser))
	assert.True(t, isHumanShellCreatePrincipal(dev, store.DelegationPrincipalUser))
	assert.False(t, isHumanShellCreatePrincipal(user, store.DelegationPrincipalAgent))
	assert.False(t, isHumanShellCreatePrincipal(user, ""))
	assert.False(t, isHumanShellCreatePrincipal(nil, store.DelegationPrincipalUser))
}

func TestShellAgentEnvAndSecretIsolation_H2(t *testing.T) {
	ctx := context.Background()
	s := createTestStore(t)
	project, broker := seedShellTestProjectAndBroker(t, s)

	ownerID := api.NewUUID()
	ancestorID := api.NewUUID()
	hubID := api.NewUUID()
	shellAgentID := api.NewUUID()
	llmAgentID := api.NewUUID()

	// Seed env vars across every scope + progeny + agent scope.
	envSeeds := []*store.EnvVar{
		{ID: api.NewUUID(), Key: "PROMPT_COMMAND", Value: "evil-project-cmd", Scope: store.ScopeProject, ScopeID: project.ID, InjectionMode: store.InjectionModeAlways},
		{ID: api.NewUUID(), Key: "PROJ_AS_NEEDED", Value: "evil-proj-an", Scope: store.ScopeProject, ScopeID: project.ID, InjectionMode: store.InjectionModeAsNeeded},
		{ID: api.NewUUID(), Key: "HUB_ENV", Value: "evil-hub-val", Scope: store.ScopeHub, ScopeID: hubID, InjectionMode: store.InjectionModeAlways},
		{ID: api.NewUUID(), Key: "BROKER_ENV", Value: "evil-broker-val", Scope: store.ScopeRuntimeBroker, ScopeID: broker.ID, InjectionMode: store.InjectionModeAlways},
		{ID: api.NewUUID(), Key: "OWNER_ENV", Value: "good-owner-val", Scope: store.ScopeUser, ScopeID: ownerID, InjectionMode: store.InjectionModeAlways},
		{ID: api.NewUUID(), Key: "OWNER_AS_NEEDED", Value: "good-owner-an", Scope: store.ScopeUser, ScopeID: ownerID, InjectionMode: store.InjectionModeAsNeeded},
		{ID: api.NewUUID(), Key: "PROGENY_ENV", Value: "evil-progeny-val", Scope: store.ScopeUser, ScopeID: ancestorID, InjectionMode: store.InjectionModeAlways, AllowProgeny: true},
		{ID: api.NewUUID(), Key: "AGENT_ENV", Value: "evil-agent-val", Scope: store.ScopeAgent, ScopeID: shellAgentID, InjectionMode: store.InjectionModeAlways},
	}
	for _, ev := range envSeeds {
		require.NoError(t, s.CreateEnvVar(ctx, ev))
	}

	// Seed secrets across every scope + progeny + agent scope via LocalBackend.
	backend := secret.NewLocalBackend(s, hubID, "")
	secretSeeds := []*secret.SetSecretInput{
		{Name: "PROJECT_SECRET", Value: "proj-sec-val", SecretType: store.SecretTypeEnvironment, Target: "PROJECT_SECRET", Scope: store.ScopeProject, ScopeID: project.ID, InjectionMode: store.InjectionModeAlways},
		{Name: "PROJECT_AS_NEEDED_SEC", Value: "proj-an-sec", SecretType: store.SecretTypeEnvironment, Target: "PROJECT_AS_NEEDED_SEC", Scope: store.ScopeProject, ScopeID: project.ID, InjectionMode: store.InjectionModeAsNeeded},
		{Name: "HUB_SECRET", Value: "hub-sec-val", SecretType: store.SecretTypeEnvironment, Target: "HUB_SECRET", Scope: store.ScopeHub, ScopeID: hubID, InjectionMode: store.InjectionModeAlways},
		{Name: "BROKER_SECRET", Value: "broker-sec-val", SecretType: store.SecretTypeEnvironment, Target: "BROKER_SECRET", Scope: store.ScopeRuntimeBroker, ScopeID: broker.ID, InjectionMode: store.InjectionModeAlways},
		{Name: "OWNER_SECRET", Value: "owner-sec-val", SecretType: store.SecretTypeEnvironment, Target: "OWNER_SECRET", Scope: store.ScopeUser, ScopeID: ownerID, InjectionMode: store.InjectionModeAlways},
		{Name: "OWNER_AS_NEEDED_SEC", Value: "owner-an-sec", SecretType: store.SecretTypeEnvironment, Target: "OWNER_AS_NEEDED_SEC", Scope: store.ScopeUser, ScopeID: ownerID, InjectionMode: store.InjectionModeAsNeeded},
		{Name: "PROGENY_SECRET", Value: "progeny-sec-val", SecretType: store.SecretTypeEnvironment, Target: "PROGENY_SECRET", Scope: store.ScopeUser, ScopeID: ancestorID, InjectionMode: store.InjectionModeAlways, AllowProgeny: true},
		{Name: "AGENT_SECRET", Value: "agent-sec-val", SecretType: store.SecretTypeEnvironment, Target: "AGENT_SECRET", Scope: store.ScopeAgent, ScopeID: shellAgentID, InjectionMode: store.InjectionModeAlways},
		{Name: "AGENT_AS_NEEDED_SEC", Value: "agent-an-sec", SecretType: store.SecretTypeEnvironment, Target: "AGENT_AS_NEEDED_SEC", Scope: store.ScopeAgent, ScopeID: shellAgentID, InjectionMode: store.InjectionModeAsNeeded},
		{Name: "LLM_AGENT_SECRET", Value: "llm-agent-sec-val", SecretType: store.SecretTypeEnvironment, Target: "LLM_AGENT_SECRET", Scope: store.ScopeAgent, ScopeID: llmAgentID, InjectionMode: store.InjectionModeAlways},
	}
	for _, inp := range secretSeeds {
		_, _, err := backend.Set(ctx, inp)
		require.NoError(t, err)
	}

	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(hubID)
	d.SetSecretBackend(backend)
	d.SetAgentSecretsEnabledProvider(func() bool { return true })

	shellAgent := &store.Agent{
		ID:              shellAgentID,
		Slug:            "shell-h2",
		Name:            "shell-h2",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		OwnerID:         ownerID,
		Ancestry:        []string{ancestorID, api.NewUUID()},
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
	}
	llmAgent := &store.Agent{
		ID:              llmAgentID,
		Slug:            "llm-h2",
		Name:            "llm-h2",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		OwnerID:         ownerID,
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}

	// 1. envScopesForDispatch: shell agent gets ONLY owner's user scope (and nil if no owner).
	scopes := d.envScopesForDispatch(ctx, shellAgent)
	require.Equal(t, []store.EnvVarFilter{{Scope: store.ScopeUser, ScopeID: ownerID}}, scopes)

	ownerlessShell := *shellAgent
	ownerlessShell.OwnerID = ""
	assert.Empty(t, d.envScopesForDispatch(ctx, &ownerlessShell))
	ownerlessSecs, _, err := d.resolveSecrets(ctx, &ownerlessShell)
	require.NoError(t, err)
	assert.Empty(t, ownerlessSecs)

	// 2. Create path: resolveEnvFromStorageWithSource + resolveSecrets + resolveAsNeededForKeys.
	shellEnv, _, _, err := d.resolveEnvFromStorageWithSource(ctx, shellAgent)
	require.NoError(t, err)
	assert.Equal(t, "good-owner-val", shellEnv["OWNER_ENV"], "shell agent must receive owner user-scope env")
	assert.NotContains(t, shellEnv, "PROMPT_COMMAND", "shell agent must not receive project env")
	assert.NotContains(t, shellEnv, "HUB_ENV", "shell agent must not receive hub env")
	assert.NotContains(t, shellEnv, "BROKER_ENV", "shell agent must not receive broker env")
	assert.NotContains(t, shellEnv, "PROGENY_ENV", "shell agent must not receive progeny env")
	assert.NotContains(t, shellEnv, "AGENT_ENV", "shell agent must not receive agent-scoped env")

	shellSecs, _, err := d.resolveSecrets(ctx, shellAgent)
	require.NoError(t, err)
	shellSecMap := make(map[string]string, len(shellSecs))
	for _, rs := range shellSecs {
		shellSecMap[rs.Name] = rs.Value
	}
	assert.Equal(t, "owner-sec-val", shellSecMap["OWNER_SECRET"], "shell agent must receive owner user-scope secret")
	assert.NotContains(t, shellSecMap, "PROJECT_SECRET", "shell agent must not receive project secret")
	assert.NotContains(t, shellSecMap, "HUB_SECRET", "shell agent must not receive hub secret")
	assert.NotContains(t, shellSecMap, "BROKER_SECRET", "shell agent must not receive broker secret")
	assert.NotContains(t, shellSecMap, "PROGENY_SECRET", "shell agent must not receive progeny secret")
	assert.NotContains(t, shellSecMap, "AGENT_SECRET", "shell agent must not receive agent-scoped secret")

	asNeeded, err := d.resolveAsNeededForKeys(ctx, shellAgent, []string{
		"PROJ_AS_NEEDED", "OWNER_AS_NEEDED", "PROJECT_AS_NEEDED_SEC", "OWNER_AS_NEEDED_SEC", "AGENT_AS_NEEDED_SEC",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "good-owner-an", asNeeded["OWNER_AS_NEEDED"])
	assert.Equal(t, "owner-an-sec", asNeeded["OWNER_AS_NEEDED_SEC"])
	assert.NotContains(t, asNeeded, "PROJ_AS_NEEDED")
	assert.NotContains(t, asNeeded, "PROJECT_AS_NEEDED_SEC")
	assert.NotContains(t, asNeeded, "AGENT_AS_NEEDED_SEC")

	// 3. Start/resume/restart path: selectDeliveryEnv + selectDeliverySecretsWith.
	sel := newDeliverySelector(nil, shellAgent, PurposeRestart)
	startEnv, _, err := d.selectDeliveryEnv(ctx, sel, shellAgent, nil)
	require.NoError(t, err)
	assert.Equal(t, "good-owner-val", startEnv["OWNER_ENV"])
	assert.NotContains(t, startEnv, "PROMPT_COMMAND")
	assert.NotContains(t, startEnv, "HUB_ENV")
	assert.NotContains(t, startEnv, "BROKER_ENV")
	assert.NotContains(t, startEnv, "PROGENY_ENV")
	assert.NotContains(t, startEnv, "AGENT_ENV")

	startSecs, _, _, err := d.selectDeliverySecretsWith(ctx, sel, shellAgent, nil)
	require.NoError(t, err)
	startSecMap := make(map[string]string, len(startSecs))
	for _, rs := range startSecs {
		startSecMap[rs.Name] = rs.Value
	}
	assert.Equal(t, "owner-sec-val", startSecMap["OWNER_SECRET"])
	assert.NotContains(t, startSecMap, "PROJECT_SECRET")
	assert.NotContains(t, startSecMap, "HUB_SECRET")
	assert.NotContains(t, startSecMap, "BROKER_SECRET")
	assert.NotContains(t, startSecMap, "PROGENY_SECRET")
	assert.NotContains(t, startSecMap, "AGENT_SECRET")

	// 4. Control: an LLM agent in the same project still receives project env, project secret, and agent-scoped secret.
	llmEnv, _, _, err := d.resolveEnvFromStorageWithSource(ctx, llmAgent)
	require.NoError(t, err)
	assert.Equal(t, "evil-project-cmd", llmEnv["PROMPT_COMMAND"], "LLM control agent must still receive project env")
	assert.Equal(t, "good-owner-val", llmEnv["OWNER_ENV"], "LLM control agent must still receive owner env")

	llmSecs, _, err := d.resolveSecrets(ctx, llmAgent)
	require.NoError(t, err)
	llmSecMap := make(map[string]string, len(llmSecs))
	for _, rs := range llmSecs {
		llmSecMap[rs.Name] = rs.Value
	}
	assert.Equal(t, "proj-sec-val", llmSecMap["PROJECT_SECRET"], "LLM control agent must still receive project secret")
	assert.Equal(t, "owner-sec-val", llmSecMap["OWNER_SECRET"], "LLM control agent must still receive owner secret")
	assert.Equal(t, "llm-agent-sec-val", llmSecMap["LLM_AGENT_SECRET"], "LLM control agent must still receive agent-scoped secret when hub.agent_secrets is enabled")
}

func TestIsShellAgent_FailClosed(t *testing.T) {
	cases := []struct {
		name  string
		agent *store.Agent
		want  bool
	}{
		{name: "nil agent", agent: nil, want: false},
		{name: "empty agent", agent: &store.Agent{}, want: false},
		{
			name:  "column shell with nil applied_config (unparsable JSON)",
			agent: &store.Agent{HarnessConfig: config.ShellHarnessConfigName, AppliedConfig: nil},
			want:  true,
		},
		{
			name: "column shell disagrees with non-shell applied_config",
			agent: &store.Agent{
				HarnessConfig: config.ShellHarnessConfigName,
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
			},
			want: true,
		},
		{
			name: "applied_config shell with empty column",
			agent: &store.Agent{
				HarnessConfig: "",
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
			},
			want: true,
		},
		{
			name: "applied_config shell disagrees with non-shell column",
			agent: &store.Agent{
				HarnessConfig: "claude",
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
			},
			want: true,
		},
		{
			name: "both non-shell",
			agent: &store.Agent{
				HarnessConfig: "claude",
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isShellAgent(tc.agent))
		})
	}
}

func TestShellAgentAssembledRequests_NoGitHubAppAndUserScopeOnly(t *testing.T) {
	ctx := context.Background()
	s := createTestStore(t)
	project, broker := seedShellTestProjectAndBroker(t, s)

	// Attach an active GitHub App installation to the project.
	installID := int64(54321)
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID,
		AccountLogin:   "test-org",
		AccountType:    "Organization",
		AppID:          42,
		Status:         store.GitHubInstallationStatusActive,
	}))
	project.GitHubInstallationID = &installID
	require.NoError(t, s.UpdateProject(ctx, project))

	ownerID := api.NewUUID()
	ancestorID := api.NewUUID()
	hubID := api.NewUUID()
	shellAgentID := api.NewUUID()
	llmAgentID := api.NewUUID()

	// Seed env vars across every scope + progeny + agent scope.
	for _, ev := range []*store.EnvVar{
		{ID: api.NewUUID(), Key: "PROMPT_COMMAND", Value: "evil-project-cmd", Scope: store.ScopeProject, ScopeID: project.ID, InjectionMode: store.InjectionModeAlways},
		{ID: api.NewUUID(), Key: "HUB_ENV", Value: "evil-hub-val", Scope: store.ScopeHub, ScopeID: hubID, InjectionMode: store.InjectionModeAlways},
		{ID: api.NewUUID(), Key: "BROKER_ENV", Value: "evil-broker-val", Scope: store.ScopeRuntimeBroker, ScopeID: broker.ID, InjectionMode: store.InjectionModeAlways},
		{ID: api.NewUUID(), Key: "OWNER_ENV", Value: "good-owner-val", Scope: store.ScopeUser, ScopeID: ownerID, InjectionMode: store.InjectionModeAlways},
		{ID: api.NewUUID(), Key: "PROGENY_ENV", Value: "evil-progeny-val", Scope: store.ScopeUser, ScopeID: ancestorID, InjectionMode: store.InjectionModeAlways, AllowProgeny: true},
		{ID: api.NewUUID(), Key: "AGENT_ENV", Value: "evil-agent-val", Scope: store.ScopeAgent, ScopeID: shellAgentID, InjectionMode: store.InjectionModeAlways},
	} {
		require.NoError(t, s.CreateEnvVar(ctx, ev))
	}

	// Seed secrets across every scope + progeny + agent scope.
	backend := secret.NewLocalBackend(s, hubID, "")
	for _, inp := range []*secret.SetSecretInput{
		{Name: "PROJECT_SECRET", Value: "proj-sec-val", SecretType: store.SecretTypeEnvironment, Target: "PROJECT_SECRET", Scope: store.ScopeProject, ScopeID: project.ID, InjectionMode: store.InjectionModeAlways},
		{Name: "HUB_SECRET", Value: "hub-sec-val", SecretType: store.SecretTypeEnvironment, Target: "HUB_SECRET", Scope: store.ScopeHub, ScopeID: hubID, InjectionMode: store.InjectionModeAlways},
		{Name: "BROKER_SECRET", Value: "broker-sec-val", SecretType: store.SecretTypeEnvironment, Target: "BROKER_SECRET", Scope: store.ScopeRuntimeBroker, ScopeID: broker.ID, InjectionMode: store.InjectionModeAlways},
		{Name: "OWNER_SECRET", Value: "owner-sec-val", SecretType: store.SecretTypeEnvironment, Target: "OWNER_SECRET", Scope: store.ScopeUser, ScopeID: ownerID, InjectionMode: store.InjectionModeAlways},
		{Name: "PROGENY_SECRET", Value: "progeny-sec-val", SecretType: store.SecretTypeEnvironment, Target: "PROGENY_SECRET", Scope: store.ScopeUser, ScopeID: ancestorID, InjectionMode: store.InjectionModeAlways, AllowProgeny: true},
		{Name: "AGENT_SECRET", Value: "agent-sec-val", SecretType: store.SecretTypeEnvironment, Target: "AGENT_SECRET", Scope: store.ScopeAgent, ScopeID: shellAgentID, InjectionMode: store.InjectionModeAlways},
	} {
		_, _, err := backend.Set(ctx, inp)
		require.NoError(t, err)
	}

	minter := &recordingGitHubAppMinter{}
	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(hubID)
	d.SetSecretBackend(backend)
	d.SetGitHubAppMinter(minter)
	d.SetAgentSecretsEnabledProvider(func() bool { return true })

	shellAgent := &store.Agent{
		ID:              shellAgentID,
		Slug:            "shell-assembled",
		Name:            "shell-assembled",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		OwnerID:         ownerID,
		Ancestry:        []string{ancestorID, api.NewUUID()},
		HarnessConfig:   config.ShellHarnessConfigName,
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
	}
	llmAgent := &store.Agent{
		ID:              llmAgentID,
		Slug:            "llm-assembled",
		Name:            "llm-assembled",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		OwnerID:         ownerID,
		HarnessConfig:   "claude",
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}

	assertShellEnvAndSecrets := func(t *testing.T, label string, env map[string]string, secrets []ResolvedSecret, allowedOwnerKeys map[string]string) {
		t.Helper()
		for k, v := range env {
			assert.False(t, strings.HasPrefix(k, "SCION_GITHUB_"), "%s: unexpected GitHub App key %q", label, k)
			assert.NotEqual(t, "SCION_USER_GITHUB_TOKEN", k, "%s: unexpected SCION_USER_GITHUB_TOKEN", label)
			if strings.HasPrefix(k, "SCION_") {
				continue
			}
			want, ok := allowedOwnerKeys[k]
			assert.True(t, ok, "%s: non-SCION_* key %q=%q did not come from owner user scope", label, k, v)
			if ok {
				assert.Equal(t, want, v, "%s: key %q value mismatch", label, k)
			}
		}
		for wantKey, wantVal := range allowedOwnerKeys {
			assert.Equal(t, wantVal, env[wantKey], "%s: missing expected owner key %q", label, wantKey)
		}
		for _, rs := range secrets {
			_, ok := allowedOwnerKeys[rs.Name]
			assert.True(t, ok, "%s: resolved secret %q did not come from owner user scope", label, rs.Name)
		}
	}

	t.Run("no owner GITHUB_TOKEN: skips GitHub App minting on create and start", func(t *testing.T) {
		minter.called = false
		allowedOwnerKeys := map[string]string{
			"OWNER_ENV":    "good-owner-val",
			"OWNER_SECRET": "owner-sec-val",
		}

		createReq, err := d.buildCreateRequestFor(ctx, shellAgent, "DispatchAgentCreate", failOnSecretResolutionError)
		require.NoError(t, err)
		assert.False(t, minter.called, "create must not mint a project GitHub App token for an execution container")
		assertShellEnvAndSecrets(t, "create", createReq.ResolvedEnv, createReq.ResolvedSecrets, allowedOwnerKeys)

		startRes, err := d.buildStartEnv(ctx, shellAgent, "DispatchAgentStart", mintSiteStart)
		require.NoError(t, err)
		assert.False(t, minter.called, "start must not mint a project GitHub App token for an execution container")
		assertShellEnvAndSecrets(t, "start", startRes.env, startRes.secrets, allowedOwnerKeys)
	})

	t.Run("owner has user-scope GITHUB_TOKEN: preserves owner token without stamping SCION_GITHUB_*", func(t *testing.T) {
		_, _, err := backend.Set(ctx, &secret.SetSecretInput{
			Name:          "GITHUB_TOKEN",
			Value:         "owner-gh-pat",
			SecretType:    store.SecretTypeEnvironment,
			Target:        "GITHUB_TOKEN",
			Scope:         store.ScopeUser,
			ScopeID:       ownerID,
			InjectionMode: store.InjectionModeAlways,
		})
		require.NoError(t, err)

		minter.called = false
		allowedOwnerKeys := map[string]string{
			"OWNER_ENV":    "good-owner-val",
			"OWNER_SECRET": "owner-sec-val",
			"GITHUB_TOKEN": "owner-gh-pat",
		}

		createReq, err := d.buildCreateRequestFor(ctx, shellAgent, "DispatchAgentCreate", failOnSecretResolutionError)
		require.NoError(t, err)
		assert.False(t, minter.called)
		assertShellEnvAndSecrets(t, "create-with-user-gh", createReq.ResolvedEnv, createReq.ResolvedSecrets, allowedOwnerKeys)

		startRes, err := d.buildStartEnv(ctx, shellAgent, "DispatchAgentStart", mintSiteStart)
		require.NoError(t, err)
		assert.False(t, minter.called)
		assertShellEnvAndSecrets(t, "start-with-user-gh", startRes.env, startRes.secrets, allowedOwnerKeys)
	})

	t.Run("control: LLM agent in same project still gets GitHub App integration", func(t *testing.T) {
		// Use an LLM agent owned by a user without a personal GITHUB_TOKEN so the App token is minted.
		llmNoOwnerToken := *llmAgent
		llmNoOwnerToken.OwnerID = api.NewUUID()

		minter.called = false
		createReq, err := d.buildCreateRequestFor(ctx, &llmNoOwnerToken, "DispatchAgentCreate", failOnSecretResolutionError)
		require.NoError(t, err)
		assert.True(t, minter.called, "LLM control agent must mint GitHub App token on create")
		assert.Equal(t, "github-app-token", createReq.ResolvedEnv["GITHUB_TOKEN"])
		assert.Equal(t, "true", createReq.ResolvedEnv["SCION_GITHUB_APP_ENABLED"])

		minter.called = false
		llmStartEnv := make(map[string]string)
		var llmStartCls map[string]api.EnvKind
		d.deliverGitHubToken(ctx, &llmNoOwnerToken, llmStartEnv, &llmStartCls, nil, nil, "DispatchAgentStart")
		assert.True(t, minter.called, "LLM control agent must mint GitHub App token on start")
		assert.Equal(t, "github-app-token", llmStartEnv["GITHUB_TOKEN"])
		assert.Equal(t, "true", llmStartEnv["SCION_GITHUB_APP_ENABLED"])
	})
}

func TestHandleAgentGitHubTokenRefresh_ShellAgentRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	installID := int64(77777)
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID,
		AccountLogin:   "test-org",
		AccountType:    "Organization",
		AppID:          7,
		Status:         store.GitHubInstallationStatusActive,
	}))
	project := &store.Project{
		ID:                   api.NewUUID(),
		Name:                 "Shell Refresh Project",
		Slug:                 "shell-refresh-proj",
		GitHubInstallationID: &installID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	shellAgent := &store.Agent{
		ID:            api.NewUUID(),
		Slug:          "shell-refresh",
		Name:          "shell-refresh",
		ProjectID:     project.ID,
		HarnessConfig: config.ShellHarnessConfigName,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
	}
	require.NoError(t, s.CreateAgent(ctx, shellAgent))

	token := srv.generateRecordedAgentToken(t,
		shellAgent.ID, project.ID, []AgentTokenScope{ScopeAgentTokenRefresh}, nil)

	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/agents/"+shellAgent.ID+"/refresh-token", nil, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeForbidden, errResp.Error.Code)
	assert.Equal(t, "execution containers cannot refresh project GitHub App tokens", errResp.Error.Message)
}

func TestShellAgentGCPIdentity_SkipsDefaultsForcesBlockAndAllowsExplicitOwnerChoice(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "shell-gcp-id")
	_ = installHeadlessGatherDispatcher(t, f)
	setExperimentOverrides(t, f.srv, map[string]bool{experiments.ExecContainers: true})

	// Seed a verified project GCP service account and configure it as the
	// project's default GCP identity (assign mode).
	sa := &store.GCPServiceAccount{
		ID:                 api.NewUUID(),
		Scope:              store.ScopeProject,
		ScopeID:            f.proj.ID,
		Email:              "proj-default@gcp-test-proj.iam.gserviceaccount.com",
		ProjectID:          "gcp-test-proj",
		DisplayName:        "Project Default SA",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          f.creator.ID,
		CreatedAt:          time.Now(),
	}
	require.NoError(t, f.srv.store.CreateGCPServiceAccount(ctx, sa))
	if f.proj.Annotations == nil {
		f.proj.Annotations = map[string]string{}
	}
	f.proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	f.proj.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
	require.NoError(t, f.srv.store.UpdateProject(ctx, f.proj))

	createViaHTTP := func(t *testing.T, body map[string]any) *store.Agent {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents", body)
		require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
		var resp struct {
			Agent *store.Agent `json:"agent"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)
		stored, err := f.srv.store.GetAgent(ctx, resp.Agent.ID)
		require.NoError(t, err)
		return stored
	}

	t.Run("HTTP create without explicit gcp_identity skips project default and forces block", func(t *testing.T) {
		stored := createViaHTTP(t, map[string]any{
			"name":          "shell-no-gcp",
			"harnessConfig": config.ShellHarnessConfigName,
			"provisionOnly": true,
		})
		require.NotNil(t, stored.AppliedConfig)
		require.NotNil(t, stored.AppliedConfig.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeBlock, stored.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Empty(t, stored.AppliedConfig.GCPIdentity.ServiceAccountID)
		assert.Empty(t, stored.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	})

	t.Run("HTTP create with explicit owner gcp_identity preserves owner choice", func(t *testing.T) {
		stored := createViaHTTP(t, map[string]any{
			"name":          "shell-explicit-gcp",
			"harnessConfig": config.ShellHarnessConfigName,
			"provisionOnly": true,
			"gcp_identity": map[string]any{
				"metadata_mode":      store.GCPMetadataModeAssign,
				"service_account_id": sa.ID,
			},
		})
		require.NotNil(t, stored.AppliedConfig)
		require.NotNil(t, stored.AppliedConfig.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeAssign, stored.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Equal(t, sa.ID, stored.AppliedConfig.GCPIdentity.ServiceAccountID)
		assert.Equal(t, sa.Email, stored.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	})

	t.Run("headless create for shell agent skips project default and forces block", func(t *testing.T) {
		// Set project default_harness_config to shell so headless create
		// produces a shell agent without an explicit template (which S6/H1 refuses).
		projWithShellDefault, err := f.srv.store.GetProject(ctx, f.proj.ID)
		require.NoError(t, err)
		projWithShellDefault.Annotations[projectSettingDefaultHarnessConfig] = config.ShellHarnessConfigName
		require.NoError(t, f.srv.store.UpdateProject(ctx, projWithShellDefault))
		t.Cleanup(func() {
			p, err := f.srv.store.GetProject(ctx, f.proj.ID)
			if err == nil && p.Annotations != nil {
				delete(p.Annotations, projectSettingDefaultHarnessConfig)
				_ = f.srv.store.UpdateProject(ctx, p)
			}
		})

		evt := withSessionRevision(f.event("shell-headless-gcp"), f.creator.ID)
		humanAuth, humanIdent, err := f.srv.resolveRecordedAuthority(ctx, scheduledRecordedAuthority(evt), f.proj.ID, "agent.create")
		require.NoError(t, err)
		humanCeiling, humanProv, err := f.srv.fireEffectCeiling(ctx, humanAuth)
		require.NoError(t, err)
		rr := headlessResolvedRole{preCapRole: AgentRoleFull}

		created, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      humanAuth,
			Identity:       humanIdent,
			EdgeCeiling:    humanCeiling,
			EdgeProvenance: humanProv,
			resolvedRole:   &rr,
			Name:           "shell-headless-gcp",
			Detached:       true,
		})
		require.NoError(t, err)
		stored, err := f.srv.store.GetAgent(ctx, created.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.AppliedConfig)
		require.NotNil(t, stored.AppliedConfig.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeBlock, stored.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Empty(t, stored.AppliedConfig.GCPIdentity.ServiceAccountID)
	})

	t.Run("hub default_harness_config=shell skips project default GCP SA and forces block on HTTP and headless create", func(t *testing.T) {
		origDefaults := f.srv.config.AgentDefaults
		f.srv.mu.Lock()
		f.srv.config.AgentDefaults.DefaultHarnessConfig = config.ShellHarnessConfigName
		f.srv.mu.Unlock()
		t.Cleanup(func() {
			f.srv.mu.Lock()
			f.srv.config.AgentDefaults = origDefaults
			f.srv.mu.Unlock()
		})

		httpStored := createViaHTTP(t, map[string]any{
			"name":          "shell-hub-default-http",
			"provisionOnly": true,
		})
		assert.Equal(t, config.ShellHarnessConfigName, httpStored.HarnessConfig)
		require.NotNil(t, httpStored.AppliedConfig)
		assert.Equal(t, config.ShellHarnessConfigName, httpStored.AppliedConfig.HarnessConfig)
		require.NotNil(t, httpStored.AppliedConfig.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeBlock, httpStored.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Empty(t, httpStored.AppliedConfig.GCPIdentity.ServiceAccountID)
		assert.Empty(t, httpStored.AppliedConfig.GCPIdentity.ServiceAccountEmail)

		evt := withSessionRevision(f.event("shell-hub-default-headless"), f.creator.ID)
		humanAuth, humanIdent, err := f.srv.resolveRecordedAuthority(ctx, scheduledRecordedAuthority(evt), f.proj.ID, "agent.create")
		require.NoError(t, err)
		humanCeiling, humanProv, err := f.srv.fireEffectCeiling(ctx, humanAuth)
		require.NoError(t, err)
		rr := headlessResolvedRole{preCapRole: AgentRoleFull}

		created, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      humanAuth,
			Identity:       humanIdent,
			EdgeCeiling:    humanCeiling,
			EdgeProvenance: humanProv,
			resolvedRole:   &rr,
			Name:           "shell-hub-default-headless",
			Detached:       true,
		})
		require.NoError(t, err)
		headlessStored, err := f.srv.store.GetAgent(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, config.ShellHarnessConfigName, headlessStored.HarnessConfig)
		require.NotNil(t, headlessStored.AppliedConfig)
		assert.Equal(t, config.ShellHarnessConfigName, headlessStored.AppliedConfig.HarnessConfig)
		require.NotNil(t, headlessStored.AppliedConfig.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeBlock, headlessStored.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Empty(t, headlessStored.AppliedConfig.GCPIdentity.ServiceAccountID)
		assert.Empty(t, headlessStored.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	})

	t.Run("dispatch create and start metadata env fail closed to block when GCPIdentity is nil and preserve explicit assign", func(t *testing.T) {
		d := NewHTTPAgentDispatcherWithClient(f.srv.store, &mockRuntimeBrokerClient{}, false, slog.Default())
		d.SetAuthzService(f.srv.authzService)

		nilGCPShell := &store.Agent{
			ID:              api.NewUUID(),
			Slug:            "shell-nil-gcp",
			Name:            "shell-nil-gcp",
			ProjectID:       f.proj.ID,
			RuntimeBrokerID: f.broker.ID,
			OwnerID:         f.creator.ID,
			HarnessConfig:   config.ShellHarnessConfigName,
			AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName, GCPIdentity: nil},
		}
		createReq, err := d.buildCreateRequestFor(ctx, nilGCPShell, "DispatchAgentCreate", failOnSecretResolutionError)
		require.NoError(t, err)
		require.NotNil(t, createReq.Config.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeBlock, createReq.Config.GCPIdentity.MetadataMode)
		assert.Equal(t, store.GCPMetadataModeBlock, createReq.ResolvedEnv["SCION_METADATA_MODE"])

		startRes, err := d.buildStartEnv(ctx, nilGCPShell, "DispatchAgentStart", mintSiteStart)
		require.NoError(t, err)
		assert.Equal(t, store.GCPMetadataModeBlock, startRes.env["SCION_METADATA_MODE"])

		explicitGCPShell := &store.Agent{
			ID:              api.NewUUID(),
			Slug:            "shell-assign-gcp",
			Name:            "shell-assign-gcp",
			ProjectID:       f.proj.ID,
			RuntimeBrokerID: f.broker.ID,
			OwnerID:         f.creator.ID,
			HarnessConfig:   config.ShellHarnessConfigName,
			AppliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: config.ShellHarnessConfigName,
				GCPIdentity: &store.GCPIdentityConfig{
					MetadataMode:        store.GCPMetadataModeAssign,
					ServiceAccountID:    sa.ID,
					ServiceAccountEmail: sa.Email,
					ProjectID:           sa.ProjectID,
				},
			},
		}
		createExplicitReq, err := d.buildCreateRequestFor(ctx, explicitGCPShell, "DispatchAgentCreate", failOnSecretResolutionError)
		require.NoError(t, err)
		require.NotNil(t, createExplicitReq.Config.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeAssign, createExplicitReq.Config.GCPIdentity.MetadataMode)
		assert.Equal(t, sa.Email, createExplicitReq.Config.GCPIdentity.SAEmail)
		assert.Equal(t, store.GCPMetadataModeAssign, createExplicitReq.ResolvedEnv["SCION_METADATA_MODE"])

		startExplicitRes, err := d.buildStartEnv(ctx, explicitGCPShell, "DispatchAgentStart", mintSiteStart)
		require.NoError(t, err)
		assert.Equal(t, store.GCPMetadataModeAssign, startExplicitRes.env["SCION_METADATA_MODE"])
		assert.Equal(t, sa.Email, startExplicitRes.env["SCION_METADATA_SA_EMAIL"])
	})

	t.Run("control: LLM agent in same project still inherits project default GCP SA", func(t *testing.T) {
		stored := createViaHTTP(t, map[string]any{
			"name":          "llm-default-gcp",
			"harnessConfig": "claude",
			"provisionOnly": true,
		})
		require.NotNil(t, stored.AppliedConfig)
		require.NotNil(t, stored.AppliedConfig.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeAssign, stored.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Equal(t, sa.ID, stored.AppliedConfig.GCPIdentity.ServiceAccountID)
		assert.Equal(t, sa.Email, stored.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	})
}
