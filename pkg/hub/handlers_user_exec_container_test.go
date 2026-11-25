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
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
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

	// Turn flag off again: DELETE is ungated and returns 204 (and clears row),
	// emitting user.exec_container.clear (F5).
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: false})
	delRec := doECRequest(t, srv, http.MethodDelete, nil, ident, cred)
	assert.Equal(t, http.StatusNoContent, delRec.Code)

	clearAudits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeUserExecContainerClear,
		TargetID:     u.ID,
		Limit:        10,
	})
	require.NoError(t, err)
	require.Len(t, clearAudits, 1, "expected user.exec_container.clear audit when a row was cleared")
	assert.Contains(t, clearAudits[0].BeforeSummary, shellAgent.ID)

	// Idempotent DELETE when already cleared still returns 204 and emits no
	// second clear audit record.
	delRec2 := doECRequest(t, srv, http.MethodDelete, nil, ident, cred)
	assert.Equal(t, http.StatusNoContent, delRec2.Code)

	clearAudits2, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeUserExecContainerClear,
		TargetID:     u.ID,
		Limit:        10,
	})
	require.NoError(t, err)
	assert.Len(t, clearAudits2, 1, "idempotent DELETE with no row must not emit a duplicate clear audit")

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

	// Verify audit event user.exec_container.set was recorded (initial set has
	// empty BeforeSummary).
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeUserExecContainerSet,
		TargetID:     caller.ID,
		Limit:        10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, audits, "expected user.exec_container.set mutation audit record")
	assert.Contains(t, audits[0].AfterSummary, createdByOther.ID)
	assert.Empty(t, audits[0].BeforeSummary)

	// Replacing with a different shell agent records BeforeSummary with the
	// previous agent_id (O6).
	secondShell := ecAgent(t, s, projMember.ID, caller.ID, caller.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	okRec2 := doECRequest(t, srv, http.MethodPut, map[string]string{"agentId": secondShell.ID}, ident, cred)
	require.Equal(t, http.StatusOK, okRec2.Code, okRec2.Body.String())

	audits2, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeUserExecContainerSet,
		TargetID:     caller.ID,
		Limit:        10,
	})
	require.NoError(t, err)
	var foundReplace bool
	for _, rec := range audits2 {
		if rec.AfterSummary == "agent_id="+secondShell.ID && rec.BeforeSummary == "agent_id="+createdByOther.ID {
			foundReplace = true
			break
		}
	}
	assert.True(t, foundReplace, "expected user.exec_container.set audit with BeforeSummary=%q and AfterSummary=%q", "agent_id="+createdByOther.ID, "agent_id="+secondShell.ID)
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

	// Owner changed -> unavailable, no agent metadata.
	// Store.UpdateAgent enforces S8 (execution container ownership is immutable),
	// so simulate an out-of-band DB owner change directly to exercise the
	// resolver's defence-in-depth eligibility recheck.
	t.Run("owner changed", func(t *testing.T) {
		a, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		require.NoError(t, s.UpdateAgent(ctx, a))

		db := s.(*entadapter.CompositeStore).DB()
		_, err = db.ExecContext(ctx, "UPDATE agents SET owner_id = ? WHERE id = ?", other.ID, agent.ID)
		require.NoError(t, err)
		assertUnavailable(t, "owner changed")

		// Restore owner for next subtest.
		_, err = db.ExecContext(ctx, "UPDATE agents SET owner_id = ? WHERE id = ?", u.ID, agent.ID)
		require.NoError(t, err)
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

// TestShellAgentCreateAdmission_Headless verifies R4: createAgentHeadless
// enforces S6 (agent principal refused), hub.exec_containers (flag off refused),
// and task refusal (task_not_supported) after deriveAgentConfig resolves a
// template whose default harness config is "shell".
func TestShellAgentCreateAdmission_Headless(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hc-shell-adm")
	_ = installHeadlessGatherDispatcher(t, f)

	// Create a global template whose default harness config is "shell" so both
	// human and agent principals resolve it during createAgentHeadless.
	tpl := &store.Template{
		ID:                   api.NewUUID(),
		Name:                 "shell-default-tpl",
		Slug:                 "shell-default-tpl",
		Scope:                store.TemplateScopeGlobal,
		Status:               store.TemplateStatusActive,
		Harness:              config.ShellHarnessConfigName,
		DefaultHarnessConfig: config.ShellHarnessConfigName,
		CreatedBy:            f.creator.ID,
	}
	require.NoError(t, f.srv.store.CreateTemplate(ctx, tpl))

	// Resolve human authority for flag-off and task-refusal cases.
	evt := withSessionRevision(f.event("shell-headless"), f.creator.ID)
	humanAuth, humanIdent, err := f.srv.resolveRecordedAuthority(ctx, scheduledRecordedAuthority(evt), f.proj.ID, "agent.create")
	require.NoError(t, err)
	humanCeiling, humanProv, err := f.srv.fireEffectCeiling(ctx, humanAuth)
	require.NoError(t, err)

	rr := headlessResolvedRole{preCapRole: AgentRoleFull}

	// 1. Agent principal with template defaulting to shell (flag ON) -> refused (S6).
	t.Run("agent_principal_refused", func(t *testing.T) {
		setExperimentOverrides(t, f.srv, map[string]bool{experiments.ExecContainers: true})
		callerAgent := ecAgent(t, f.srv.store, f.proj.ID, f.creator.ID, f.creator.ID, "claude", string(state.PhaseRunning), string(state.ActivityIdle))
		agentIdent := &agentIdentityWrapper{
			AgentTokenClaims: &AgentTokenClaims{
				ProjectID: f.proj.ID,
				Scopes:    []AgentTokenScope{ScopeAgentCreate, ScopeAgentLifecycle},
			},
		}
		agentIdent.Subject = callerAgent.ID
		agentAuth := humanAuth
		agentAuth.PrincipalKind = "agent"
		agentAuth.PrincipalID = callerAgent.ID
		agentProv := humanProv
		agentProv.SourcePrincipalKind = "agent"
		agentProv.SourcePrincipalID = callerAgent.ID

		_, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      agentAuth,
			Identity:       agentIdent,
			EdgeCeiling:    humanCeiling,
			EdgeProvenance: agentProv,
			resolvedRole:   &rr,
			Name:           "agent-spawned-shell",
			Template:       tpl.Slug,
			Detached:       true,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "execution container: human principals only")
	})

	// 2. Human principal with hub.exec_containers OFF -> refused (experiment_disabled).
	t.Run("flag_off_refused", func(t *testing.T) {
		setExperimentOverrides(t, f.srv, map[string]bool{experiments.ExecContainers: false})
		_, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      humanAuth,
			Identity:       humanIdent,
			EdgeCeiling:    humanCeiling,
			EdgeProvenance: humanProv,
			resolvedRole:   &rr,
			Name:           "flag-off-shell",
			Template:       tpl.Slug,
			Detached:       true,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), ErrCodeExperimentDisabled)
	})

	// 3. Human principal with hub.exec_containers ON and a Task -> refused (task_not_supported).
	t.Run("task_refused", func(t *testing.T) {
		setExperimentOverrides(t, f.srv, map[string]bool{experiments.ExecContainers: true})
		_, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      humanAuth,
			Identity:       humanIdent,
			EdgeCeiling:    humanCeiling,
			EdgeProvenance: humanProv,
			resolvedRole:   &rr,
			Name:           "task-shell",
			Template:       tpl.Slug,
			Task:           "echo should-not-run",
			Detached:       true,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), ErrCodeTaskNotSupported)
	})
}

// TestShellHarnessTypeReservedOnHub verifies O1 on the Hub: non-reserved
// harness configs declaring harness: shell are refused with 400 reserved_name
// on create, update (PUT), patch, and clone.
func TestShellHarnessTypeReservedOnHub(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devIdent := NewDevUser(DevUserConfig{})
	devCred := CredentialContext{Kind: CredentialKindDev}
	withAdmin := func(req *http.Request) *http.Request {
		return req.WithContext(contextWithCredentialContext(contextWithIdentity(req.Context(), devIdent), devCred))
	}

	// Create with non-reserved slug "myshell" but harness: "shell" -> 400 reserved_name.
	t.Run("create_refused", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"name":    "myshell",
			"slug":    "myshell",
			"harness": config.ShellHarnessConfigName,
			"scope":   store.HarnessConfigScopeUser,
		})
		req := withAdmin(httptest.NewRequest(http.MethodPost, "/api/v1/harness-configs", bytes.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.createHarnessConfig(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeReservedName)
	})

	// Seed a legacy row with harness: "shell" and non-reserved slug.
	legacy := &store.HarnessConfig{
		ID:        api.NewUUID(),
		Name:      "legacy-shell",
		Slug:      "legacy-shell",
		Harness:   config.ShellHarnessConfigName,
		Scope:     store.HarnessConfigScopeUser,
		ScopeID:   devIdent.ID(),
		OwnerID:   devIdent.ID(),
		CreatedBy: devIdent.ID(),
		Status:    store.HarnessConfigStatusActive,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, legacy))

	t.Run("update_refused", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"name": "legacy-shell-renamed",
			"slug": "legacy-shell-renamed",
		})
		req := withAdmin(httptest.NewRequest(http.MethodPut, "/api/v1/harness-configs/"+legacy.ID, bytes.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.updateHarnessConfig(rec, req, legacy)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeReservedName)
	})

	t.Run("patch_refused", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"displayName": "Still Shell",
		})
		req := withAdmin(httptest.NewRequest(http.MethodPatch, "/api/v1/harness-configs/"+legacy.ID, bytes.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.patchHarnessConfig(rec, req, legacy)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeReservedName)
	})

	t.Run("clone_refused", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"name":  "cloned-shell",
			"scope": store.HarnessConfigScopeUser,
		})
		req := withAdmin(httptest.NewRequest(http.MethodPost, "/api/v1/harness-configs/"+legacy.ID+"/clone", bytes.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleHarnessConfigClone(rec, req, legacy)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeReservedName)
	})
}

// TestHandleExistingAgent_ShellTaskRefused verifies the Nit fix: starting or
// resuming an existing shell agent via create with a non-empty task is refused
// with 400 task_not_supported.
func TestHandleExistingAgent_ShellTaskRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	u := ecUser(t, s, store.UserRoleMember)
	proj := ecProject(t, s, "Existing Shell Project")
	grantProjectAccessOnly(t, s, u.ID, proj.ID)
	shellAgent := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseStopped), "")

	ident, cred := interactiveUser(u)
	reqCtx := contextWithCredentialContext(contextWithIdentity(ctx, ident), cred)
	rec := httptest.NewRecorder()
	var carriedSecrets []*secret.SecretWithValue
	res := srv.handleExistingAgent(
		reqCtx,
		rec,
		shellAgent,
		proj,
		"",
		CreateAgentRequest{Name: shellAgent.Name, Task: "echo nope"},
		"", "", u.ID,
		&carriedSecrets,
		nil,
	)
	assert.Empty(t, carriedSecrets)
	assert.Equal(t, existingAgentErrored, res)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeTaskNotSupported)
}

// TestExecContainer_HomePersistent verifies P4 (ptone/scion#8820, design §3.6 & §4.2):
// ResolveExecContainer and GET/PUT /api/v1/users/me/exec-container report
// homePersistent=true, homePersistent=false (explicitly serialized), and omit
// homePersistent when unknown, and applyBrokerResponse records it from the broker.
func TestExecContainer_HomePersistent(t *testing.T) {
	srv, s := testServer(t)
	setExperimentOverrides(t, srv, map[string]bool{experiments.ExecContainers: true})
	ctx := context.Background()

	u := ecUser(t, s, store.UserRoleMember)
	proj := ecProject(t, s, "Home Persist Project")
	grantProjectAccessOnly(t, s, u.ID, proj.ID)
	ident, cred := interactiveUser(u)

	// 1. Unknown (no home_persistent token in RuntimeState) -> nil / omitted in JSON.
	unknownAgent := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	rec := doECRequest(t, srv, http.MethodPut, map[string]any{"agentId": unknownAgent.ID}, ident, cred)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"homePersistent"`)
	res, err := srv.ResolveExecContainer(ctx, u.ID)
	require.NoError(t, err)
	assert.Nil(t, res.HomePersistent)

	// 2. Persistent (home_persistent=true) -> true in resolver, PUT, and GET.
	persistAgent := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	persistAgent.RuntimeState = "container:c-1;home_persistent=true"
	require.NoError(t, s.UpdateAgent(ctx, persistAgent))

	putRec := doECRequest(t, srv, http.MethodPut, map[string]any{"agentId": persistAgent.ID}, ident, cred)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())
	assert.Contains(t, putRec.Body.String(), `"homePersistent":true`)
	putOut := decodeECResponse(t, putRec)
	require.NotNil(t, putOut.Agent)
	require.NotNil(t, putOut.Agent.HomePersistent)
	assert.True(t, *putOut.Agent.HomePersistent)

	getRec := doECRequest(t, srv, http.MethodGet, nil, ident, cred)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	assert.Contains(t, getRec.Body.String(), `"homePersistent":true`)

	res, err = srv.ResolveExecContainer(ctx, u.ID)
	require.NoError(t, err)
	require.NotNil(t, res.HomePersistent)
	assert.True(t, *res.HomePersistent)

	// 3. Non-persistent (home_persistent=false) -> false explicitly serialized in JSON.
	ephemeralAgent := ecAgent(t, s, proj.ID, u.ID, u.ID, config.ShellHarnessConfigName, string(state.PhaseRunning), string(state.ActivityIdle))
	ephemeralAgent.RuntimeState = "container:c-2;home_persistent=false"
	require.NoError(t, s.UpdateAgent(ctx, ephemeralAgent))

	putRec = doECRequest(t, srv, http.MethodPut, map[string]any{"agentId": ephemeralAgent.ID}, ident, cred)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())
	assert.Contains(t, putRec.Body.String(), `"homePersistent":false`)
	putOut = decodeECResponse(t, putRec)
	require.NotNil(t, putOut.Agent)
	require.NotNil(t, putOut.Agent.HomePersistent)
	assert.False(t, *putOut.Agent.HomePersistent)

	getRec = doECRequest(t, srv, http.MethodGet, nil, ident, cred)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	assert.Contains(t, getRec.Body.String(), `"homePersistent":false`)

	res, err = srv.ResolveExecContainer(ctx, u.ID)
	require.NoError(t, err)
	require.NotNil(t, res.HomePersistent)
	assert.False(t, *res.HomePersistent)

	// 4. applyBrokerResponse preserves container:<id> and appends home_persistent.
	d := &HTTPAgentDispatcher{store: s, log: slog.Default()}
	a := &store.Agent{ID: ephemeralAgent.ID, Name: ephemeralAgent.Name}
	d.applyBrokerResponse(ctx, a, &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:           "ctr-99",
			RuntimeState: "home_persistent=false",
		},
	})
	assert.Equal(t, "container:ctr-99;home_persistent=false", a.RuntimeState)
	p, ok := api.ParseHomePersistentRuntimeState(a.RuntimeState)
	assert.True(t, ok)
	assert.False(t, p)

	// 5. mergeBrokerRuntimeState matches container: only as a segment prefix.
	got, valid := mergeBrokerRuntimeState("ctr-1", "foo_container:bar;home_persistent=true")
	assert.True(t, valid)
	assert.Equal(t, "container:ctr-1;foo_container:bar;home_persistent=true", got)
	got, valid = mergeBrokerRuntimeState("ctr-1", "container:existing;home_persistent=true")
	assert.True(t, valid)
	assert.Equal(t, "container:existing;home_persistent=true", got)
}

// TestAgentLaunchReport_SucceededRecordsHomePersistentRuntimeState verifies that
// async launch succeeded reports persist broker-reported RuntimeState (prefixed
// with container:<id>;) and ignore oversized or control-character values.
func TestAgentLaunchReport_SucceededRecordsHomePersistentRuntimeState(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := ecProject(t, s, "LR Home Persist Project")

	newLaunchingAgent := func(name, initialRuntimeState string) (*store.Agent, string) {
		t.Helper()
		id := api.NewUUID()
		a := &store.Agent{
			ID:              id,
			Slug:            name,
			Name:            name,
			ProjectID:       proj.ID,
			Phase:           string(state.PhaseCreated),
			RuntimeBrokerID: "broker-1",
			RuntimeState:    initialRuntimeState,
			StateVersion:    1,
			AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
			Created:         time.Now(),
			Updated:         time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
		require.NoError(t, err)
		return a, launchID
	}

	t.Run("records_prefixed_home_persistent", func(t *testing.T) {
		a, launchID := newLaunchingAgent("lr-hp-ok", "")
		rec := postLaunchReport(t, srv, "broker-1", a.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1", State: "succeeded",
			Agent: &RemoteAgentInfo{ID: "ctr-async-1", RuntimeState: "home_persistent=false"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "container:ctr-async-1;home_persistent=false", got.RuntimeState)
	})

	t.Run("oversized_runtime_state_leaves_stored_unchanged", func(t *testing.T) {
		a, launchID := newLaunchingAgent("lr-hp-big", "container:prior;home_persistent=true")
		oversized := "home_persistent=false;" + string(bytes.Repeat([]byte("a"), maxBrokerRuntimeStateBytes))
		rec := postLaunchReport(t, srv, "broker-1", a.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1", State: "succeeded",
			Agent: &RemoteAgentInfo{ID: "ctr-async-2", RuntimeState: oversized},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "container:prior;home_persistent=true", got.RuntimeState)
	})

	t.Run("control_char_runtime_state_leaves_stored_unchanged", func(t *testing.T) {
		a, launchID := newLaunchingAgent("lr-hp-ctrl", "container:prior;home_persistent=true")
		rec := postLaunchReport(t, srv, "broker-1", a.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1", State: "succeeded",
			Agent: &RemoteAgentInfo{ID: "ctr-async-3", RuntimeState: "home_persistent=false\ninjected"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "container:prior;home_persistent=true", got.RuntimeState)
	})
}
