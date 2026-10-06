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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the reincarnate patch flags (ptone/scion#3302).

// patchTestSA persists a GCP service account scoped to projectID.
func patchTestSA(t *testing.T, s store.Store, projectID string, verified bool, createdBy string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     fmt.Sprintf("sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: createdBy,
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// saAssigningAgent makes a fixture agent able to assign a project service
// account itself: the SA assign gate admits an agent caller only with a
// full stored role (its permissions come from the agent role binding), the
// sa_assign scope on its token, and a GCP identity of its own (actAs).
func saAssigningAgent(t *testing.T, s store.Store, projectID string) func(a *store.Agent) {
	own := patchTestSA(t, s, projectID, true, "someone")
	return func(a *store.Agent) {
		a.AppliedConfig.AgentRole = string(AgentRoleFull)
		a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
			MetadataMode:        store.GCPMetadataModeAssign,
			ServiceAccountID:    own.ID,
			ServiceAccountEmail: own.Email,
		}
	}
}

// reincarnateAsDev runs a reincarnate request as the dev user through the
// full HTTP stack.
func reincarnateAsDev(t *testing.T, srv *Server, agentID string, body ReincarnateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/reincarnate", body)
}

// agentSnapshot is what a refused reincarnation must leave unchanged.
type agentSnapshot struct {
	stateVersion int64
	phase        string
	generation   int
	brokerID     string
	reincState   string
	applied      []byte
}

func snapshotAgent(t *testing.T, s store.Store, agentID string) agentSnapshot {
	t.Helper()
	a, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	applied, err := json.Marshal(a.AppliedConfig.ResponseView(true))
	require.NoError(t, err)
	return agentSnapshot{
		stateVersion: a.StateVersion,
		phase:        a.Phase,
		generation:   a.Generation,
		brokerID:     a.RuntimeBrokerID,
		reincState:   a.ReincarnationState,
		applied:      applied,
	}
}

// assertAgentUntouched checks a refused request changed nothing: the agent
// row (phase, generation, broker, AppliedConfig, optimistic-lock version),
// its reincarnation history, and the dispatcher (no stop).
func assertAgentUntouched(t *testing.T, s store.Store, disp *reincarnateTestDispatcher, agentID string, before agentSnapshot) {
	t.Helper()
	after := snapshotAgent(t, s, agentID)
	assert.Equal(t, before.stateVersion, after.stateVersion, "state_version must not change")
	assert.Equal(t, before.phase, after.phase, "phase must not change")
	assert.Equal(t, before.generation, after.generation, "generation must not change")
	assert.Equal(t, before.brokerID, after.brokerID, "broker must not change")
	assert.Equal(t, before.reincState, after.reincState, "reincarnation state must not change")
	assert.JSONEq(t, string(before.applied), string(after.applied), "AppliedConfig must not change")
	list, err := s.ListAgentReincarnations(context.Background(), agentID)
	require.NoError(t, err)
	assert.Empty(t, list, "no reincarnation record may be created")
	disp.mu.Lock()
	defer disp.mu.Unlock()
	assert.Zero(t, disp.stopCalls, "the agent must not be stopped")
	assert.Zero(t, disp.reprovisionCalls)
}

// TestReincarnatePatch_EachFlagAppliesAndPersists: each flag changes the
// next generation's applied config, and a second reincarnation without the
// flag keeps the value.
func TestReincarnatePatch_EachFlagAppliesAndPersists(t *testing.T) {
	cases := []struct {
		name  string
		body  func(saID string) ReincarnateAgentRequest
		check func(t *testing.T, cfg *store.AgentAppliedConfig, saID string)
	}{
		{
			name: "image",
			body: func(string) ReincarnateAgentRequest { return ReincarnateAgentRequest{Image: "patched-image:v9"} },
			check: func(t *testing.T, cfg *store.AgentAppliedConfig, _ string) {
				assert.Equal(t, "patched-image:v9", cfg.Image)
				require.NotNil(t, cfg.CreateInputs.InlineConfig)
				assert.Equal(t, "patched-image:v9", cfg.CreateInputs.InlineConfig.Image)
			},
		},
		{
			name: "model",
			body: func(string) ReincarnateAgentRequest { return ReincarnateAgentRequest{Model: "patched-model-1"} },
			check: func(t *testing.T, cfg *store.AgentAppliedConfig, _ string) {
				assert.Equal(t, "patched-model-1", cfg.Model)
				require.NotNil(t, cfg.CreateInputs.InlineConfig)
				assert.Equal(t, "patched-model-1", cfg.CreateInputs.InlineConfig.Model)
			},
		},
		{
			name: "thinking level",
			body: func(string) ReincarnateAgentRequest { return ReincarnateAgentRequest{ThinkingLevel: intPtr(42)} },
			check: func(t *testing.T, cfg *store.AgentAppliedConfig, _ string) {
				require.NotNil(t, cfg.ThinkingLevel)
				assert.Equal(t, 42, *cfg.ThinkingLevel)
				require.NotNil(t, cfg.CreateInputs.ThinkingLevel)
				assert.Equal(t, 42, *cfg.CreateInputs.ThinkingLevel)
			},
		},
		{
			name: "harness auth",
			body: func(string) ReincarnateAgentRequest { return ReincarnateAgentRequest{HarnessAuth: "vertex-ai"} },
			check: func(t *testing.T, cfg *store.AgentAppliedConfig, _ string) {
				assert.Equal(t, "vertex-ai", cfg.HarnessAuth)
				assert.Equal(t, "vertex-ai", cfg.CreateInputs.HarnessAuth)
			},
		},
		{
			name: "role",
			body: func(string) ReincarnateAgentRequest { return ReincarnateAgentRequest{Role: "readonly"} },
			check: func(t *testing.T, cfg *store.AgentAppliedConfig, _ string) {
				assert.Equal(t, "readonly", cfg.AgentRole)
			},
		},
		{
			// --role none must also drop injected credentials (A3.1), in
			// this generation and the next.
			name: "role none sets NoAuth",
			body: func(string) ReincarnateAgentRequest { return ReincarnateAgentRequest{Role: "none"} },
			check: func(t *testing.T, cfg *store.AgentAppliedConfig, _ string) {
				assert.Equal(t, "none", cfg.AgentRole)
				assert.True(t, cfg.NoAuth, "role none must imply NoAuth")
			},
		},
		{
			name: "service account",
			body: func(saID string) ReincarnateAgentRequest { return ReincarnateAgentRequest{ServiceAccount: saID} },
			check: func(t *testing.T, cfg *store.AgentAppliedConfig, saID string) {
				require.NotNil(t, cfg.GCPIdentity)
				assert.Equal(t, store.GCPMetadataModeAssign, cfg.GCPIdentity.MetadataMode)
				assert.Equal(t, saID, cfg.GCPIdentity.ServiceAccountID)
				assert.NotEmpty(t, cfg.GCPIdentity.ServiceAccountEmail)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, nil)
			sa := patchTestSA(t, s, project.ID, true, "someone")

			rec := reincarnateAsDev(t, srv, agent.ID, tc.body(sa.ID))
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			waitForReincarnationSettled(t, s, agent.ID)
			gen2, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			require.Equal(t, 2, gen2.Generation)
			tc.check(t, gen2.AppliedConfig, sa.ID)

			// Second reincarnation, no flags: the value is kept.
			rec = reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{})
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			deadline := time.Now().Add(5 * time.Second)
			for {
				got, err := s.GetAgent(context.Background(), agent.ID)
				require.NoError(t, err)
				if got.Generation == 3 && got.ReincarnationState == store.ReincarnationStateNone {
					break
				}
				require.True(t, time.Now().Before(deadline), "second reincarnation did not settle")
				time.Sleep(5 * time.Millisecond)
			}
			gen3, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			tc.check(t, gen3.AppliedConfig, sa.ID)
		})
	}
}

// TestReincarnatePatch_DoesNotMutateOutgoingCreateInputs: the patch is
// recorded into a copy; the record of the outgoing generation keeps its
// own CreateInputs.
func TestReincarnatePatch_DoesNotMutateOutgoingCreateInputs(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Model: "patched-model-2", ThinkingLevel: intPtr(7)})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.NotNil(t, r.PreviousAppliedConfig)
	require.NotNil(t, r.PreviousAppliedConfig.CreateInputs)
	assert.Nil(t, r.PreviousAppliedConfig.CreateInputs.InlineConfig, "previous CreateInputs must not carry the patch")
	assert.Nil(t, r.PreviousAppliedConfig.CreateInputs.ThinkingLevel)
}

// TestReincarnatePatch_DryRunShowsOldAndNewPerFlag: the dry-run plan has
// the old and new value of every patched field, and writes nothing.
func TestReincarnatePatch_DryRunShowsOldAndNewPerFlag(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	oldSA := patchTestSA(t, s, project.ID, true, "someone")
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Model = "old-model"
		a.AppliedConfig.ThinkingLevel = intPtr(10)
		a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
			MetadataMode:        store.GCPMetadataModeAssign,
			ServiceAccountID:    oldSA.ID,
			ServiceAccountEmail: oldSA.Email,
		}
	})
	newSA := patchTestSA(t, s, project.ID, true, "someone")
	before := snapshotAgent(t, s, agent.ID)

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{
		DryRun:         true,
		ServiceAccount: newSA.ID,
		Role:           "readonly",
		Image:          "new-image:v2",
		Model:          "new-model",
		ThinkingLevel:  intPtr(80),
		HarnessAuth:    "vertex-ai",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	p := resp.Plan

	assert.Equal(t, []string{"serviceAccount", "role", "image", "model", "thinkingLevel", "harnessAuth"}, p.Patched)
	require.NotNil(t, p.ServiceAccount)
	assert.Equal(t, FieldChange{Old: oldSA.Email, New: newSA.Email}, *p.ServiceAccount)
	require.NotNil(t, p.Role)
	assert.Equal(t, FieldChange{Old: "baseline", New: "readonly"}, *p.Role)
	assert.Equal(t, FieldChange{Old: "old-image:v1", New: "new-image:v2"}, p.Image)
	assert.Equal(t, FieldChange{Old: "old-model", New: "new-model"}, p.Model)
	require.NotNil(t, p.ThinkingLevel)
	assert.Equal(t, FieldChange{Old: "10", New: "80"}, *p.ThinkingLevel)
	require.NotNil(t, p.HarnessAuth)
	assert.Equal(t, FieldChange{Old: "api-key", New: "vertex-ai"}, *p.HarnessAuth)

	assertAgentUntouched(t, s, disp, agent.ID, before)
}

// TestReincarnatePatch_NoFlagsPlanUnchanged: without patch flags the plan
// carries no patch entries.
func TestReincarnatePatch_NoFlagsPlanUnchanged(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw struct {
		Plan map[string]json.RawMessage `json:"plan"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.NotEmpty(t, raw.Plan)
	for _, k := range []string{"patched", "role", "serviceAccount", "thinkingLevel", "harnessAuth"} {
		_, ok := raw.Plan[k]
		assert.False(t, ok, "plan must not carry %q without patch flags", k)
	}
}

// TestReincarnatePatch_ServiceAccountRefusals: an unauthorized service
// account is refused before any side effect, through create's checks.
func TestReincarnatePatch_ServiceAccountRefusals(t *testing.T) {
	t.Run("caller cannot use the service account", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		user := newReincarnateAuthzUser(t, s, "sa-denied")
		grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
		grantAgentDelegationAtProject(t, s, user.ID, project.ID)
		// agent.update too, so the patch-flag update gate (D4) passes and
		// the refusal below is the service account's.
		grantPermissionViaRoleBinding(t, s, user.ID, "agent.update", store.RoleScopeProject, project.ID)
		// Created by someone else; nothing grants this user read on it.
		sa := patchTestSA(t, s, project.ID, true, "someone-else")
		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")

		// Precondition: the same user may reincarnate without the flag, so
		// the refusal below is the service account's.
		req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{DryRun: true})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		require.Equal(t, http.StatusOK, rec.Code, "precondition: %s", rec.Body.String())

		before := snapshotAgent(t, s, agent.ID)
		req = reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{ServiceAccount: sa.ID})
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertAgentUntouched(t, s, disp, agent.ID, before)
	})

	t.Run("project admin may use the same service account", func(t *testing.T) {
		// The admitted arm: the predicate runs and admits a caller with
		// access, so the refusal above is not a blanket one.
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		user := newReincarnateAuthzUser(t, s, "sa-admin")
		grantProjectRole(t, s, user.ID, project.ID, store.ProjectRoleAdmin)
		sa := patchTestSA(t, s, project.ID, true, "someone-else")
		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")

		req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{DryRun: true, ServiceAccount: sa.ID})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	for _, tc := range []struct {
		name     string
		mkSA     func(t *testing.T, s store.Store, projectID string) string
		contains string
	}{
		{"another project's account", func(t *testing.T, s store.Store, _ string) string {
			return patchTestSA(t, s, tid("some-other-project"), true, "someone").ID
		}, msgSANotAvailableInProject},
		{"unknown account", func(*testing.T, store.Store, string) string { return uuid.New().String() }, msgSANotAvailableInProject},
		{"unverified account", func(t *testing.T, s store.Store, projectID string) string {
			return patchTestSA(t, s, projectID, false, "someone").ID
		}, "not verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, nil)
			saID := tc.mkSA(t, s, project.ID)
			before := snapshotAgent(t, s, agent.ID)
			rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{ServiceAccount: saID})
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.contains)
			assertAgentUntouched(t, s, disp, agent.ID, before)
		})
	}
}

// TestReincarnatePatch_RoleRefusals: a role create would refuse is refused
// before any side effect.
func TestReincarnatePatch_RoleRefusals(t *testing.T) {
	t.Run("above the project maximum", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		project.Annotations = map[string]string{projectSettingMaxAgentRole: string(AgentRoleBaseline)}
		require.NoError(t, s.UpdateProject(context.Background(), project))
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		before := snapshotAgent(t, s, agent.ID)

		rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Role: "full"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "project maximum")
		assertAgentUntouched(t, s, disp, agent.ID, before)
	})

	t.Run("self cannot raise its own role", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil) // baseline
		before := snapshotAgent(t, s, agent.ID)
		// Even with every scope on the token, the stored role caps it.
		self := agentIdentityFor(agent.ID, project.ID, append(ScopesForRole(AgentRoleFull), ScopeAgentLifecycle)...)
		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Role: "full"})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertAgentUntouched(t, s, disp, agent.ID, before)
	})

	t.Run("self may lower its own role", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil) // baseline
		self := agentIdentityFor(agent.ID, project.ID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentLifecycle)...)
		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true, Role: "readonly"})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("self lowering still needs the new role's scopes", func(t *testing.T) {
		// Create runs CanDelegate for an agent granting a role, so a self
		// --role does too: a token with only the lifecycle scope holds none
		// of readonly's scopes and cannot delegate it, even though readonly
		// is below the agent's stored role.
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil) // baseline
		before := snapshotAgent(t, s, agent.ID)
		self := agentIdentityFor(agent.ID, project.ID, ScopeAgentLifecycle)
		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Role: "readonly"})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertAgentUntouched(t, s, disp, agent.ID, before)
	})

	t.Run("requester cannot delegate the new role", func(t *testing.T) {
		// The coordinator's stored role is full, so create's lattice admits
		// a full role; its token carries only baseline scopes, so
		// delegating full must fail CanDelegate / the ceiling for the NEW
		// role (the stored baseline role would pass).
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.ID = tid("patch-coordinator")
			a.Slug = "patch-coordinator-" + tidSlugSafe(t.Name())
			a.Name = "Coordinator"
			a.AppliedConfig.AgentRole = string(AgentRoleFull)
		})
		before := snapshotAgent(t, s, agent.ID)
		requester := delegatingRequesterFor(coordinator.ID, project.ID)

		// Precondition: the same requester may reincarnate without --role.
		req := reincarnateRequest(t, agent.ID, requester, ReincarnateAgentRequest{DryRun: true})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		require.Equal(t, http.StatusOK, rec.Code, "precondition: %s", rec.Body.String())

		req = reincarnateRequest(t, agent.ID, requester, ReincarnateAgentRequest{Role: "full"})
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertAgentUntouched(t, s, disp, agent.ID, before)
	})
}

// TestReincarnatePatch_SelfWithPatchNeedsLifecycle pins decision D1: the
// D2 self exemption covers only a request without patch flags.
func TestReincarnatePatch_SelfWithPatchNeedsLifecycle(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, saAssigningAgent(t, s, project.ID))
	self := agentIdentityFor(agent.ID, project.ID) // no scopes

	// No patch: allowed (D2).
	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// An SA this agent could otherwise assign (in-project, verified; the
	// token below carries sa_assign, which the assign gate needs).
	sa := patchTestSA(t, s, project.ID, true, "someone")
	// Every scope a role or SA patch needs except lifecycle, so each 403
	// below is D1's and not CanDelegate's or the SA gate's.
	selfNoLifecycle := agentIdentityFor(agent.ID, project.ID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentSAAssign)...)

	before := snapshotAgent(t, s, agent.ID)
	for _, body := range []ReincarnateAgentRequest{
		{Model: "other-model"},
		{Image: "other:v1"},
		{ThinkingLevel: intPtr(5)},
		{HarnessAuth: "vertex-ai"},
		{ServiceAccount: sa.ID},
		{Role: "readonly"},
		{Role: "none"},
	} {
		req := reincarnateRequest(t, agent.ID, selfNoLifecycle, body)
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%+v: %s", body, rec.Body.String())
	}
	assertAgentUntouched(t, s, disp, agent.ID, before)

	// With the lifecycle scope added, the same self patches are allowed:
	// the refusals above are D1's alone.
	selfWithLifecycle := agentIdentityFor(agent.ID, project.ID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentSAAssign, ScopeAgentLifecycle)...)
	for _, body := range []ReincarnateAgentRequest{
		{DryRun: true, Model: "other-model"},
		{DryRun: true, ServiceAccount: sa.ID},
		{DryRun: true, Role: "readonly"},
	} {
		req = reincarnateRequest(t, agent.ID, selfWithLifecycle, body)
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusOK, rec.Code, "%+v: %s", body, rec.Body.String())
	}
}

// TestReincarnatePatch_CombinesWithBroker: a dry-run move carries the
// patch in its plan.
func TestReincarnatePatch_CombinesWithBroker(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	count := f.agentCount(t)
	caller := agentIdentityFor(f.agent.ID, f.project.ID, ScopeAgentCreate, ScopeAgentLifecycle)
	req := reincarnateRequest(t, f.agent.ID, caller, ReincarnateAgentRequest{
		DryRun: true, TargetBroker: f.dst.ID, Model: "moved-model", ThinkingLevel: intPtr(33),
	})
	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, req, f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.MoveVerdict)
	assert.Equal(t, f.dst.ID, resp.TargetBrokerID)
	assert.Equal(t, []string{"model", "thinkingLevel"}, resp.Plan.Patched)
	assert.Equal(t, "moved-model", resp.Plan.Model.New)
	require.NotNil(t, resp.Plan.ThinkingLevel)
	assert.Equal(t, "33", resp.Plan.ThinkingLevel.New)
	f.assertNoMoveSideEffects(t, count)
}

// TestReincarnatePatch_ServiceAccountGateForAgentCallers: create's SA
// assign gate also applies to agent callers, self and not self. An agent
// needs the project:agent:sa_assign scope to assign a project service account.
func TestReincarnatePatch_ServiceAccountGateForAgentCallers(t *testing.T) {
	t.Run("another agent", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		canAssignSA := saAssigningAgent(t, s, project.ID)
		coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.ID = tid("sa-coordinator")
			a.Slug = "sa-coordinator-" + tidSlugSafe(t.Name())
			a.Name = "Coordinator"
			canAssignSA(a)
		})
		sa := patchTestSA(t, s, project.ID, true, "someone")
		noAssign := delegatingRequesterFor(coordinator.ID, project.ID) // lifecycle + baseline, no sa_assign

		// Precondition: this requester may reincarnate without the flag.
		req := reincarnateRequest(t, agent.ID, noAssign, ReincarnateAgentRequest{DryRun: true})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		require.Equal(t, http.StatusOK, rec.Code, "precondition: %s", rec.Body.String())

		before := snapshotAgent(t, s, agent.ID)
		req = reincarnateRequest(t, agent.ID, noAssign, ReincarnateAgentRequest{ServiceAccount: sa.ID})
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertAgentUntouched(t, s, disp, agent.ID, before)

		// Admitted arm: with sa_assign the same requester may assign it.
		canAssign := agentIdentityFor(coordinator.ID, project.ID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentLifecycle, ScopeAgentSAAssign)...)
		req = reincarnateRequest(t, agent.ID, canAssign, ReincarnateAgentRequest{DryRun: true, ServiceAccount: sa.ID})
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("self", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, saAssigningAgent(t, s, project.ID))
		sa := patchTestSA(t, s, project.ID, true, "someone")
		// Lifecycle passes D1; no sa_assign, so the SA gate refuses.
		self := agentIdentityFor(agent.ID, project.ID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentLifecycle)...)
		before := snapshotAgent(t, s, agent.ID)
		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{ServiceAccount: sa.ID})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertAgentUntouched(t, s, disp, agent.ID, before)

		selfCanAssign := agentIdentityFor(agent.ID, project.ID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentLifecycle, ScopeAgentSAAssign)...)
		req = reincarnateRequest(t, agent.ID, selfCanAssign, ReincarnateAgentRequest{DryRun: true, ServiceAccount: sa.ID})
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

// TestReincarnatePatch_RoleRaisedFromNoAuthWarns: raising the role of an
// agent created with no credentials keeps NoAuth, and the plan says so.
func TestReincarnatePatch_RoleRaisedFromNoAuthWarns(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.AgentRole = string(AgentRoleNone)
		a.AppliedConfig.NoAuth = true
		a.AppliedConfig.CreateInputs.NoAuth = true
	})

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true, Role: "baseline"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.Plan.Warnings, reincarnateNoAuthKeptWarning)

	// No warning for an agent without the create-time no-credentials request.
	plain := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("noauth-plain")
		a.Slug = "noauth-plain-" + tidSlugSafe(t.Name())
	})
	rec = reincarnateAsDev(t, srv, plain.ID, ReincarnateAgentRequest{DryRun: true, Role: "readonly"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp = ReincarnateAgentResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotContains(t, resp.Plan.Warnings, reincarnateNoAuthKeptWarning)
}

// TestReincarnatePatch_PatchedFieldPinnedOthersFollowTemplate: a patched
// image survives a later template change, while the unpatched model keeps
// following the template.
func TestReincarnatePatch_PatchedFieldPinnedOthersFollowTemplate(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	template := &store.Template{
		ID:          tid("tmpl-" + t.Name()),
		Name:        "t",
		Slug:        "patch-template-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.TemplateScopeGlobal,
		Status:      store.TemplateStatusActive,
		ContentHash: "hash-v1",
		Config:      &store.TemplateConfig{Image: "template-image:v1", Model: "template-model-v1"},
	}
	require.NoError(t, s.CreateTemplate(ctx, template))
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = template.Slug
	})

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Image: "patched-image:v1"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	template.Config = &store.TemplateConfig{Image: "template-image:v2", Model: "template-model-v2"}
	template.ContentHash = "hash-v2"
	require.NoError(t, s.UpdateTemplate(ctx, template))

	rec = reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "patched-image:v1", resp.Plan.Image.New, "the patched image is pinned")
	assert.Equal(t, "template-model-v2", resp.Plan.Model.New, "the unpatched model follows the template")
}

// TestReincarnatePatch_FailureRestoresPreviousConfig: when the worker fails
// before reprovision succeeds, the agent is restored to the previous
// config, patch included (role, SA, CreateInputs).
func TestReincarnatePatch_FailureRestoresPreviousConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("broker refused: reprovision refused: container is still running")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	sa := patchTestSA(t, s, project.ID, true, "someone")

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{
		Role: "readonly", ServiceAccount: sa.ID, Model: "patched-model", ThinkingLevel: intPtr(9),
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	cfg := final.AppliedConfig
	assert.Equal(t, "baseline", cfg.AgentRole)
	assert.Nil(t, cfg.GCPIdentity)
	assert.Empty(t, cfg.Model)
	assert.Nil(t, cfg.ThinkingLevel)
	require.NotNil(t, cfg.CreateInputs)
	assert.Nil(t, cfg.CreateInputs.InlineConfig)
	assert.Nil(t, cfg.CreateInputs.ThinkingLevel)
}

// TestReincarnatePatch_LegacyAgent: an agent without CreateInputs gets the
// reconstructed inputs plus the patch, and keeps the patch afterwards.
func TestReincarnatePatch_LegacyAgent(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.CreateInputs = nil
	})

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Model: "legacy-patched-model"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)
	gen2, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, 2, gen2.Generation)
	assert.Equal(t, "legacy-patched-model", gen2.AppliedConfig.Model)
	require.NotNil(t, gen2.AppliedConfig.CreateInputs)
	require.NotNil(t, gen2.AppliedConfig.CreateInputs.InlineConfig)
	assert.Equal(t, "legacy-patched-model", gen2.AppliedConfig.CreateInputs.InlineConfig.Model)

	rec = reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "legacy-patched-model", resp.Plan.Model.New)
}

// TestReincarnatePatch_SelfRoleChangeDoesNotReRecordEdge: a self request
// that changes the role passes CanDelegate and the ceiling, but does not
// re-record the delegation edge with the agent as its own delegator (D1).
func TestReincarnatePatch_SelfRoleChangeDoesNotReRecordEdge(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil) // baseline
	ctx := context.Background()
	parent := seedAgentEdge(t, s, tid("user-creator"), agent)
	edgesBefore, err := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	require.Len(t, edgesBefore, 1, "fixture: the parent edge")

	self := agentIdentityFor(agent.ID, project.ID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentLifecycle)...)
	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", Role: "readonly"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "readonly", final.AppliedConfig.AgentRole, "the role patch applied")

	edgesAfter, err := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, len(edgesBefore), len(edgesAfter), "a self request must not add or replace delegation edges")
	require.Len(t, edgesAfter, 1)
	assert.Equal(t, parent.ID, edgesAfter[0].ID, "the parent edge stays the active edge")
	assert.Equal(t, tid("user-creator"), edgesAfter[0].DelegatorID, "the delegator is unchanged")
	assert.Equal(t, store.DelegationPrincipalUser, edgesAfter[0].DelegatorType)
}

// TestReincarnatePatch_UserNeedsUpdateForPatch pins decision D4: with a
// patch flag, a user caller needs agent.update on the target as well as
// lifecycle, like the agent PATCH.
func TestReincarnatePatch_UserNeedsUpdateForPatch(t *testing.T) {
	t.Run("lifecycle without update is refused", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		user := newReincarnateAuthzUser(t, s, "lifecycle-no-update")
		grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
		grantAgentDelegationAtProject(t, s, user.ID, project.ID)
		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")

		// Precondition: no-flag reincarnate is allowed for this user.
		req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{DryRun: true})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		require.Equal(t, http.StatusOK, rec.Code, "precondition: %s", rec.Body.String())

		before := snapshotAgent(t, s, agent.ID)
		for _, body := range []ReincarnateAgentRequest{
			{Image: "other:v1"},
			{Role: "readonly"},
			{DryRun: true, Image: "other:v1"},
			{DryRun: true, Role: "readonly"},
		} {
			req := reincarnateRequest(t, agent.ID, identity, body)
			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, req, agent.ID)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%+v: %s", body, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "agent.update")
		}
		assertAgentUntouched(t, s, disp, agent.ID, before)

		// Admitted arm: the same user with agent.update may patch.
		grantPermissionViaRoleBinding(t, s, user.ID, "agent.update", store.RoleScopeProject, project.ID)
		req = reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{DryRun: true, Image: "other:v1", Role: "readonly"})
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("UAT cannot patch but can reincarnate", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		user := newReincarnateAuthzUser(t, s, "uat-patch")
		grantProjectRole(t, s, user.ID, project.ID, store.ProjectRoleAdmin) // holds agent.update
		identity := scopedIdentityFor(user, project.ID, append(minimalSelectors(t), "agent:lifecycle"))

		req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{DryRun: true})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		require.Equal(t, http.StatusOK, rec.Code, "no-flag reincarnate must still work: %s", rec.Body.String())

		before := snapshotAgent(t, s, agent.ID)
		req = reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Model: "other-model"})
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "user access token")
		assertAgentUntouched(t, s, disp, agent.ID, before)
	})

	t.Run("owner may patch", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		user := newReincarnateAuthzUser(t, s, "owner-patch")
		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.OwnerID = user.ID
			a.CreatedBy = user.ID
			a.Ancestry = []string{user.ID}
		})
		grantAgentDelegationAtProject(t, s, user.ID, project.ID)
		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
		req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{DryRun: true, Image: "owner:v1"})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

// TestReincarnatePatch_RollbackKeepsSeededCreateInputs: the patch is
// recorded into a deep copy of CreateInputs. With a seeded InlineConfig
// (scalars and an env map), a failed reincarnation restores CreateInputs
// exactly as it was, and the record's previous config is clean too; a
// shallow copy would write the patch through into both.
func TestReincarnatePatch_RollbackKeepsSeededCreateInputs(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("broker refused: reprovision refused: container is still running")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = "seed-image:v1"
		a.AppliedConfig.Model = "seed-model"
		a.AppliedConfig.HarnessAuth = "api-key"
		a.AppliedConfig.ThinkingLevel = intPtr(3)
		a.AppliedConfig.CreateInputs.HarnessAuth = "api-key"
		a.AppliedConfig.CreateInputs.ThinkingLevel = intPtr(3)
		a.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{
			Image:            "seed-image:v1",
			Model:            "seed-model",
			AuthSelectedType: "api-key",
			ThinkingLevel:    intPtr(3),
			Env:              map[string]string{"SEED_KEY": "seed-value"},
		}
	})
	seeded, err := json.Marshal(agent.AppliedConfig.CreateInputs)
	require.NoError(t, err)

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{
		Image: "patched-image:v2", Model: "patched-model", ThinkingLevel: intPtr(90), HarnessAuth: "vertex-ai",
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	got, err := json.Marshal(final.AppliedConfig.CreateInputs)
	require.NoError(t, err)
	assert.JSONEq(t, string(seeded), string(got), "rollback must restore the seeded CreateInputs unchanged")
	assert.Equal(t, "seed-model", final.AppliedConfig.Model)

	require.NotNil(t, r.PreviousAppliedConfig)
	prev, err := json.Marshal(r.PreviousAppliedConfig.CreateInputs)
	require.NoError(t, err)
	assert.JSONEq(t, string(seeded), string(prev), "the record's previous CreateInputs must not carry the patch")
}

// TestReincarnatePatch_UnsupportedHarnessAuthRefused: a harness-auth value
// the agent's harness does not support is refused with 400 before any side
// effect, the same validation PATCH applies.
func TestReincarnatePatch_UnsupportedHarnessAuthRefused(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		// codex declares vertex_ai unsupported.
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "codex"}
	})
	before := snapshotAgent(t, s, agent.ID)

	for _, dryRun := range []bool{true, false} {
		rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessAuth: "vertex-ai"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "harnessAuth is not supported by harness codex")
	}
	assertAgentUntouched(t, s, disp, agent.ID, before)

	// A value the harness supports is accepted.
	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true, HarnessAuth: "api-key"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
