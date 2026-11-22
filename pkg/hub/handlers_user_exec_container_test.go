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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const execContainerPath = "/api/v1/users/me/exec-container"

func ecUser(t *testing.T, s store.Store, role string) *store.User {
	t.Helper()
	id := api.NewUUID()
	u := &store.User{
		ID:          id,
		Email:       id + "@exec-container.test",
		DisplayName: "EC User " + id[:6],
		Role:        role,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

func ecProject(t *testing.T, s store.Store, name string) *store.Project {
	t.Helper()
	id := api.NewUUID()
	p := &store.Project{
		ID:   id,
		Name: name,
		Slug: "ec-proj-" + id[:8],
	}
	require.NoError(t, s.CreateProject(context.Background(), p))
	return p
}

func ecAgent(t *testing.T, s store.Store, projectID, ownerID, createdBy, harnessCfg, phase, activity string) *store.Agent {
	t.Helper()
	id := api.NewUUID()
	a := &store.Agent{
		ID:        id,
		Name:      "ec-agent-" + id[:8],
		Slug:      "ec-agent-" + id[:8],
		ProjectID: projectID,
		OwnerID:   ownerID,
		CreatedBy: createdBy,
		Phase:     phase,
		Activity:  activity,
		RunID:     "run-" + id[:8],
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: harnessCfg,
		},
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

func doECRequest(t *testing.T, srv *Server, method string, body any, ident Identity, cred CredentialContext) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, execContainerPath, &buf)
	req.Header.Set("Content-Type", "application/json")
	ctx := req.Context()
	if ident != nil {
		ctx = contextWithIdentity(ctx, ident)
		ctx = contextWithCredentialContext(ctx, cred)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleUserMeExecContainer(rec, req)
	return rec
}

func interactiveUser(u *store.User) (UserIdentity, CredentialContext) {
	ident := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeCLI))
	return ident, CredentialContext{Kind: CredentialKindInteractive, Type: ident.Type()}
}

func decodeECResponse(t *testing.T, rec *httptest.ResponseRecorder) execContainerResponse {
	t.Helper()
	var out execContainerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

// TestExecContainer_AC3_ExperimentFlagOff verifies AC 3:
// with hub.exec_containers off, GET/PUT return 404, DELETE returns 204, and
// ResolveExecContainer returns unconfigured while keeping the stored row for
// when the flag is re-enabled.
func TestExecContainer_AC3_ExperimentFlagOff(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	u := ecUser(t, s, store.UserRoleMember)
	proj := ecProject(t, s, "EC Flag Project")
	grantProjectAccessOnly(t, s, u.ID, proj.ID)
	shellAgent := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	ident, cred := interactiveUser(u)

	// Enable flag briefly to register a container, then turn it off.
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})
	rec := doECRequest(t, srv, http.MethodPut, map[string]string{"agentId": shellAgent.ID}, ident, cred)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Turn flag off.
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: false})

	getRec := doECRequest(t, srv, http.MethodGet, nil, ident, cred)
	assert.Equal(t, http.StatusNotFound, getRec.Code)

	putRec := doECRequest(t, srv, http.MethodPut, map[string]string{"agentId": shellAgent.ID}, ident, cred)
	assert.Equal(t, http.StatusNotFound, putRec.Code)

	// Resolver returns unconfigured while flag is off.
	res, err := srv.ResolveExecContainer(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, ExecContainerStateUnconfigured, res.State)
	assert.Nil(t, res.Agent)
	assert.Empty(t, res.RunID)

	// Turning the flag back on restores the registration.
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})
	res, err = srv.ResolveExecContainer(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, ExecContainerStateReady, res.State)
	require.NotNil(t, res.Agent)
	assert.Equal(t, shellAgent.ID, res.Agent.ID)

	// Turn flag off again: DELETE is ungated and returns 204 (and clears row).
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: false})
	delRec := doECRequest(t, srv, http.MethodDelete, nil, ident, cred)
	assert.Equal(t, http.StatusNoContent, delRec.Code)

	// Idempotent DELETE when already cleared still returns 204.
	delRec2 := doECRequest(t, srv, http.MethodDelete, nil, ident, cred)
	assert.Equal(t, http.StatusNoContent, delRec2.Code)

	// Confirm row is gone once flag is re-enabled.
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})
	getAfterDel := doECRequest(t, srv, http.MethodGet, nil, ident, cred)
	require.Equal(t, http.StatusOK, getAfterDel.Code)
	assert.False(t, decodeECResponse(t, getAfterDel).Configured)
}

// TestExecContainer_AC4_Uniform422AndAttachEligibility verifies AC 4:
//   - Malformed agentId -> 400 invalid_agent_id.
//   - All six ineligible cases return byte-identical 422 bodies (no existence oracle).
//   - PUT succeeds for a shell agent the caller can attach to even when another
//     user created it (and for a stopped shell agent).
//   - Emits audit event user.exec_container.set.
func TestExecContainer_AC4_Uniform422AndAttachEligibility(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})

	caller := ecUser(t, s, store.UserRoleMember)
	other := ecUser(t, s, store.UserRoleMember)
	ident, cred := interactiveUser(caller)

	projMember := ecProject(t, s, "Member Project")
	grantProjectAccessOnly(t, s, caller.ID, projMember.ID)
	grantProjectAccessOnly(t, s, other.ID, projMember.ID)

	projRemoved := ecProject(t, s, "Removed Project")
	// caller has NO project membership in projRemoved (simulates removal).

	projForeign := ecProject(t, s, "Foreign Project")
	grantProjectAccessOnly(t, s, other.ID, projForeign.ID)

	// Malformed UUID -> 400 invalid_agent_id.
	badRec := doECRequest(t, srv, http.MethodPut, map[string]string{"agentId": "not-a-uuid"}, ident, cred)
	assert.Equal(t, http.StatusBadRequest, badRec.Code)
	assert.Contains(t, badRec.Body.String(), ErrCodeInvalidAgentID)

	// Six ineligible cases per AC 4:
	// 1. Nonexistent UUID
	nonexistentID := api.NewUUID()
	// 2. Soft-deleted shell agent owned by caller
	softDeleted := ecAgent(t, s, projMember.ID, caller.ID, caller.ID, config.ShellHarnessConfigName, string(state.PhaseStopped), "")
	softDeleted.DeletedAt = time.Now().UTC()
	require.NoError(t, s.UpdateAgent(ctx, softDeleted))
	// 3. Another user's shell agent in a foreign project
	otherForeign := ecAgent(t, s, projForeign.ID, other.ID, other.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	// 4. Own LLM-harness agent
	ownLLM := ecAgent(t, s, projMember.ID, caller.ID, caller.ID, "claude", string(state.PhaseRunning), string(state.ActivityIdle))
	// 5. Shell agent in caller's project that caller cannot attach to (owned by other)
	unattachableShell := ecAgent(t, s, projMember.ID, other.ID, other.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	// 6. Own shell agent in a project the caller was removed from
	removedProjShell := ecAgent(t, s, projRemoved.ID, caller.ID, caller.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))

	cases := []struct {
		name    string
		agentID string
	}{
		{"nonexistent UUID", nonexistentID},
		{"soft-deleted agent", softDeleted.ID},
		{"another user's agent", otherForeign.ID},
		{"own LLM-harness agent", ownLLM.ID},
		{"shell agent caller cannot attach to", unattachableShell.ID},
		{"own shell agent in removed project", removedProjShell.ID},
	}

	var baselineBody []byte
	for i, tc := range cases {
		rec := doECRequest(t, srv, http.MethodPut, map[string]string{"agentId": tc.agentID}, ident, cred)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "case %q: body=%s", tc.name, rec.Body.String())
		body := rec.Body.Bytes()
		assert.Contains(t, string(body), ErrCodeExecContainerIneligible, "case %q", tc.name)
		assert.Contains(t, string(body), execContainerIneligibleMessage, "case %q", tc.name)
		if i == 0 {
			baselineBody = append([]byte(nil), body...)
		} else {
			assert.Equal(t, baselineBody, body, "case %q must return byte-identical 422 body to %q", tc.name, cases[0].name)
		}
	}

	// Success: a stopped shell agent created by another user, which caller can
	// attach to (owned by caller, created by other).
	createdByOther := ecAgent(t, s, projMember.ID, caller.ID, other.ID, config.ShellHarnessConfigName, string(state.PhaseStopped), "")
	okRec := doECRequest(t, srv, http.MethodPut, map[string]string{"agentId": createdByOther.ID}, ident, cred)
	require.Equal(t, http.StatusOK, okRec.Code, okRec.Body.String())
	resp := decodeECResponse(t, okRec)
	assert.True(t, resp.Configured)
	assert.Equal(t, ExecContainerStateStopped, resp.State)
	require.NotNil(t, resp.Agent)
	assert.Equal(t, createdByOther.ID, resp.Agent.ID)
	assert.Equal(t, "Member Project", resp.Agent.ProjectName)

	// Verify audit event user.exec_container.set was recorded.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeUserExecContainerSet,
		TargetID:     caller.ID,
		Limit:        10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, audits, "expected user.exec_container.set mutation audit record")
	assert.Contains(t, audits[0].AfterSummary, createdByOther.ID)
}

// TestExecContainer_AC5_StateResolution verifies AC 5:
// GET returns the documented state across unconfigured, starting, ready,
// stopped, suspended (phase suspended and idle-suspend claim active), and
// unavailable (deleted, owner changed, membership removed), and unavailable
// responses contain no agent fields.
func TestExecContainer_AC5_StateResolution(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})

	u := ecUser(t, s, store.UserRoleMember)
	other := ecUser(t, s, store.UserRoleMember)
	ident, cred := interactiveUser(u)

	proj := ecProject(t, s, "State Project")
	grantProjectAccessOnly(t, s, u.ID, proj.ID)

	// Unconfigured
	rec := doECRequest(t, srv, http.MethodGet, nil, ident, cred)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, decodeECResponse(t, rec).Configured)

	// Register a running/idle shell agent
	agent := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	putRec := doECRequest(t, srv, http.MethodPut, map[string]string{"agentId": agent.ID}, ident, cred)
	require.Equal(t, http.StatusOK, putRec.Code)

	stateCases := []struct {
		name     string
		phase    string
		activity string
		want     ExecContainerState
	}{
		{"provisioning", string(state.PhaseProvisioning), "", ExecContainerStateStarting},
		{"starting", string(state.PhaseStarting), "", ExecContainerStateStarting},
		{"running initializing", string(state.PhaseRunning), string(state.ActivityInitializing), ExecContainerStateStarting},
		{"running idle", string(state.PhaseRunning), string(state.ActivityIdle), ExecContainerStateReady},
		{"stopped", string(state.PhaseStopped), "", ExecContainerStateStopped},
		{"error", string(state.PhaseError), "", ExecContainerStateStopped},
		{"suspended", string(state.PhaseSuspended), "", ExecContainerStateSuspended},
	}

	for _, tc := range stateCases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			a.Phase = tc.phase
			a.Activity = tc.activity
			require.NoError(t, s.UpdateAgent(ctx, a))

			getRec := doECRequest(t, srv, http.MethodGet, nil, ident, cred)
			require.Equal(t, http.StatusOK, getRec.Code)
			resp := decodeECResponse(t, getRec)
			assert.True(t, resp.Configured)
			assert.Equal(t, tc.want, resp.State)
			require.NotNil(t, resp.Agent)
			assert.Equal(t, agent.ID, resp.Agent.ID)
			assert.Equal(t, proj.ID, resp.Agent.ProjectID)
			assert.Equal(t, proj.Name, resp.Agent.ProjectName)
			assert.Empty(t, resp.Reason)
		})
	}

	// Idle-suspend claim active on a running agent maps to suspended.
	t.Run("idle-suspend claim active", func(t *testing.T) {
		got := mapExecContainerState(&store.Agent{
			Phase:                  string(state.PhaseRunning),
			Activity:               string(state.ActivityIdle),
			IdleSuspendClaimActive: true,
		})
		assert.Equal(t, ExecContainerStateSuspended, got)
	})

	assertUnavailable := func(t *testing.T, label string) {
		t.Helper()
		getRec := doECRequest(t, srv, http.MethodGet, nil, ident, cred)
		require.Equal(t, http.StatusOK, getRec.Code, label)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &raw), label)
		assert.Equal(t, true, raw["configured"], label)
		assert.Equal(t, string(ExecContainerStateUnavailable), raw["state"], label)
		assert.Equal(t, execContainerUnavailableReason, raw["reason"], label)
		_, hasAgent := raw["agent"]
		assert.False(t, hasAgent, "%s: unavailable response must not expose agent metadata", label)
	}

	// Owner changed -> unavailable, no agent metadata
	t.Run("owner changed", func(t *testing.T) {
		a, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.OwnerID = other.ID
		require.NoError(t, s.UpdateAgent(ctx, a))
		assertUnavailable(t, "owner changed")

		// Restore owner for next subtest.
		a.OwnerID = u.ID
		require.NoError(t, s.UpdateAgent(ctx, a))
	})

	// Project membership removed -> unavailable, no agent metadata
	t.Run("membership removed", func(t *testing.T) {
		bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
		require.NoError(t, err)
		for _, b := range bindings {
			if b.ScopeType == store.RoleScopeProject && b.ScopeID == proj.ID {
				require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
			}
		}
		assertUnavailable(t, "membership removed")

		// Restore membership for next subtest.
		grantProjectAccessOnly(t, s, u.ID, proj.ID)
	})

	// Soft-deleted -> unavailable, no agent metadata
	t.Run("deleted", func(t *testing.T) {
		a, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		a.DeletedAt = time.Now().UTC()
		require.NoError(t, s.UpdateAgent(ctx, a))
		assertUnavailable(t, "soft-deleted")
	})
}

// TestExecContainer_AC12_CredentialGateAndPatchIsolation verifies AC 12:
//   - GET/PUT/DELETE with UAT or agent token -> 403 (with INTERACTIVE_STATE for UAT).
//   - Generic PATCH /api/v1/users/{id} (self or admin) cannot change or clear
//     the execution container registration.
func TestExecContainer_AC12_CredentialGateAndPatchIsolation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})

	u := ecUser(t, s, store.UserRoleMember)
	admin := ecUser(t, s, store.UserRoleAdmin)
	proj := ecProject(t, s, "AC12 Project")
	grantProjectAccessOnly(t, s, u.ID, proj.ID)
	shellAgent := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))

	ident, cred := interactiveUser(u)
	putRec := doECRequest(t, srv, http.MethodPut, map[string]string{"agentId": shellAgent.ID}, ident, cred)
	require.Equal(t, http.StatusOK, putRec.Code)

	// UAT (scoped user token) is refused on GET/PUT/DELETE with INTERACTIVE_STATE.
	scoped := NewScopedUserIdentity(ident, proj.ID, []string{"agent:read", "agent:attach"})
	uatCred := CredentialContext{Kind: CredentialKindUAT, ID: "uat-1", ProjectID: proj.ID}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := doECRequest(t, srv, method, map[string]string{"agentId": shellAgent.ID}, scoped, uatCred)
		requireSessionOnlyRefusal(t, rec, authzop.ReasonInteractiveState, "UAT "+method)
	}

	// Agent token is refused on GET/PUT/DELETE with 403.
	agentIdent := &agentIdentityWrapper{
		AgentTokenClaims: &AgentTokenClaims{
			ProjectID: proj.ID,
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		},
	}
	agentIdent.Subject = shellAgent.ID
	agentCred := CredentialContext{Kind: CredentialKindAgentJWT, ID: "jti-1"}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := doECRequest(t, srv, method, map[string]string{"agentId": shellAgent.ID}, agentIdent, agentCred)
		assert.Equal(t, http.StatusForbidden, rec.Code, "agent token %s", method)
	}

	// Generic PATCH /api/v1/users/{id} by self or admin cannot alter the
	// user_exec_containers row (even if a payload attempts to pass an
	// execContainerAgentId field in preferences or top-level).
	otherShell := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	patchPayload := map[string]any{
		"displayName":          "Updated Name",
		"execContainerAgentId": otherShell.ID,
		"preferences": map[string]any{
			"execContainerAgentId": otherShell.ID,
		},
	}

	adminIdent, adminCred := interactiveUser(admin)
	for _, actor := range []struct {
		name  string
		ident UserIdentity
		cred  CredentialContext
	}{
		{"self", ident, cred},
		{"admin", adminIdent, adminCred},
	} {
		var buf bytes.Buffer
		require.NoError(t, json.NewEncoder(&buf).Encode(patchPayload))
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+u.ID, &buf)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithCredentialContext(contextWithIdentity(req.Context(), actor.ident), actor.cred))
		rec := httptest.NewRecorder()
		srv.handleUserByID(rec, req)
		// Whether PATCH succeeds (ignoring unknown fields) or rejects them,
		// the registered execution container must remain shellAgent.ID.
		row, err := s.GetUserExecContainer(ctx, u.ID)
		require.NoError(t, err, actor.name)
		assert.Equal(t, shellAgent.ID, row.AgentID, "%s PATCH must not change execution container registration", actor.name)
	}
}

// TestExecContainer_AC13_IndeterminateAuthzFailsClosed verifies AC 13:
// ResolveExecContainer returns unavailable (Agent == nil) when the attach
// authz decision is indeterminate (store fault during authz resolution).
func TestExecContainer_AC13_IndeterminateAuthzFailsClosed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})

	u := ecUser(t, s, store.UserRoleMember)
	proj := ecProject(t, s, "Indeterminate Project")
	grantProjectAccessOnly(t, s, u.ID, proj.ID)
	shellAgent := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))

	_, err := s.PutUserExecContainer(ctx, u.ID, shellAgent.ID)
	require.NoError(t, err)

	// Baseline: resolves to ready when authz store is healthy.
	res, err := srv.ResolveExecContainer(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, ExecContainerStateReady, res.State)
	require.NotNil(t, res.Agent)

	// Swap authzService to one backed by a store whose effective-group lookup
	// fails, producing an indeterminate authz decision (DenyCauseResolutionError).
	failing := &failEffectiveGroupsStore{Store: s, failErr: errors.New("injected authz store fault")}
	srv.authzService = NewAuthzService(failing, slog.Default())

	res, err = srv.ResolveExecContainer(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, ExecContainerStateUnavailable, res.State)
	assert.Nil(t, res.Agent)
	assert.Empty(t, res.RunID)
}
