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
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.AgentRole = "readonly"
	})
	self := agentIdentityFor(agent.ID, project.ID) // no scopes

	// No patch: allowed (D2).
	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	before := snapshotAgent(t, s, agent.ID)
	for _, body := range []ReincarnateAgentRequest{
		{Model: "other-model"},
		{Image: "other:v1"},
		{ThinkingLevel: intPtr(5)},
		{HarnessAuth: "vertex-ai"},
	} {
		req := reincarnateRequest(t, agent.ID, self, body)
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%+v: %s", body, rec.Body.String())
	}
	assertAgentUntouched(t, s, disp, agent.ID, before)

	// With the lifecycle scope, the same self patch is allowed.
	selfWithLifecycle := agentIdentityFor(agent.ID, project.ID, ScopeAgentLifecycle)
	req = reincarnateRequest(t, agent.ID, selfWithLifecycle, ReincarnateAgentRequest{DryRun: true, Model: "other-model"})
	rec = httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
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
