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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type labelTestFixture struct {
	srv          *Server
	s            store.Store
	project      *store.Project
	otherProject *store.Project
	owner        *store.User
	editor       *store.User
	outsider     *store.User
	agent        *store.Agent
	sibling      *store.Agent
	child        *store.Agent
	otherAgent   *store.Agent
}

func setupLabelTestFixture(t *testing.T) *labelTestFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("lbl-owner")
	editorID := tid("lbl-editor")
	outsiderID := tid("lbl-outsider")

	createTestUserWithRole(t, s, ownerID, "lbl-owner@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	createTestUserWithRole(t, s, editorID, "lbl-editor@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	createTestUserWithRole(t, s, outsiderID, "lbl-outsider@test.com", store.UserRoleMember, store.SystemRoleHubMember)

	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)
	editor, err := s.GetUser(ctx, editorID)
	require.NoError(t, err)
	outsider, err := s.GetUser(ctx, outsiderID)
	require.NoError(t, err)

	proj := &store.Project{
		ID:        tid("lbl-proj"),
		Name:      "Label Project",
		Slug:      "lbl-proj",
		GitRemote: "https://github.com/example/lbl.git",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	createTestUserWithProjectRole(t, s, owner.ID, owner.Email, proj.ID, store.ProjectRoleMember)
	grantPermissionViaRoleBinding(t, s, editor.ID, "agent.update", store.RoleScopeProject, proj.ID)

	otherProj := &store.Project{
		ID:        tid("lbl-other-proj"),
		Name:      "Other Label Project",
		Slug:      "lbl-other-proj",
		GitRemote: "https://github.com/example/lbl-other.git",
		OwnerID:   outsider.ID,
		CreatedBy: outsider.ID,
	}
	require.NoError(t, s.CreateProject(ctx, otherProj))
	createTestUserWithProjectRole(t, s, outsider.ID, outsider.Email, otherProj.ID, store.ProjectRoleMember)

	ag := &store.Agent{
		ID:           tid("lbl-agent"),
		Slug:         "lbl-agent",
		Name:         "lbl-agent",
		ProjectID:    proj.ID,
		Phase:        string(state.PhaseRunning),
		OwnerID:      owner.ID,
		CreatedBy:    owner.ID,
		Labels:       map[string]string{"env": "dev", "team": "core"},
		StateVersion: 1,
	}
	require.NoError(t, s.CreateAgent(ctx, ag))

	sibling := &store.Agent{
		ID:           tid("lbl-sibling"),
		Slug:         "lbl-sibling",
		Name:         "lbl-sibling",
		ProjectID:    proj.ID,
		Phase:        string(state.PhaseRunning),
		OwnerID:      owner.ID,
		CreatedBy:    owner.ID,
		Labels:       map[string]string{"role": "sibling"},
		StateVersion: 1,
	}
	require.NoError(t, s.CreateAgent(ctx, sibling))

	child := &store.Agent{
		ID:           tid("lbl-child"),
		Slug:         "lbl-child",
		Name:         "lbl-child",
		ProjectID:    proj.ID,
		Phase:        string(state.PhaseRunning),
		OwnerID:      owner.ID,
		CreatedBy:    ag.ID,
		Ancestry:     []string{owner.ID, ag.ID},
		Labels:       map[string]string{"tier": "worker"},
		StateVersion: 1,
	}
	require.NoError(t, s.CreateAgent(ctx, child))

	otherAg := &store.Agent{
		ID:           tid("lbl-other-agent"),
		Slug:         "lbl-other-agent",
		Name:         "lbl-other-agent",
		ProjectID:    otherProj.ID,
		Phase:        string(state.PhaseRunning),
		OwnerID:      outsider.ID,
		CreatedBy:    outsider.ID,
		Labels:       map[string]string{"env": "other"},
		StateVersion: 1,
	}
	require.NoError(t, s.CreateAgent(ctx, otherAg))

	return &labelTestFixture{
		srv:          srv,
		s:            s,
		project:      proj,
		otherProject: otherProj,
		owner:        owner,
		editor:       editor,
		outsider:     outsider,
		agent:        ag,
		sibling:      sibling,
		child:        child,
		otherAgent:   otherAg,
	}
}

func (f *labelTestFixture) labelAudits(t *testing.T, agentID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		TargetType:   "agent",
		TargetID:     agentID,
		MutationType: mutationTypeAgentLabelsUpdate,
		Limit:        50,
	})
	require.NoError(t, err)
	return recs
}

type labelUpdateFaultStore struct {
	store.Store
	onUpdate           func(ctx context.Context, a *store.Agent, next func(context.Context, *store.Agent) error) error
	onSetSuspendPolicy func(ctx context.Context, agentID, policy string, next func(context.Context, string, string) (bool, error)) (bool, error)
}

func (w *labelUpdateFaultStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if w.onUpdate != nil {
		return w.onUpdate(ctx, a, w.Store.UpdateAgent)
	}
	return w.Store.UpdateAgent(ctx, a)
}

func (w *labelUpdateFaultStore) SetAgentSuspendPolicy(ctx context.Context, agentID, policy string) (bool, error) {
	if w.onSetSuspendPolicy != nil {
		return w.onSetSuspendPolicy(ctx, agentID, policy, w.Store.SetAgentSuspendPolicy)
	}
	return w.Store.SetAgentSuspendPolicy(ctx, agentID, policy)
}

func (w *labelUpdateFaultStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return w.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&labelUpdateFaultStore{
			Store:              tx,
			onUpdate:           w.onUpdate,
			onSetSuspendPolicy: w.onSetSuspendPolicy,
		})
	})
}

func TestPatchAgentLabels_MergeSemanticsAndValidation(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	t.Run("merge add, update, and remove preserves unrelated keys", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set:    map[string]string{"env": "staging", "budget": "low"},
			Remove: []string{"team"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp store.Agent
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, map[string]string{"env": "staging", "budget": "low"}, resp.Labels)

		// Project-scoped route also merges by slug.
		rec2 := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/projects/"+f.project.ID+"/agents/"+f.agent.Slug+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"owner-tag": "alice"},
		})
		require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

		got, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"env": "staging", "budget": "low", "owner-tag": "alice"}, got.Labels)
	})

	t.Run("removing absent key is idempotent no-op without audit", func(t *testing.T) {
		beforeAudits := len(f.labelAudits(t, f.agent.ID))
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Remove: []string{"does-not-exist"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, beforeAudits, len(f.labelAudits(t, f.agent.ID)), "no-op must not write an audit record")
	})

	t.Run("overlap between set and remove is rejected", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set:    map[string]string{"env": "prod"},
			Remove: []string{"env"},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("empty remove key is rejected", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Remove: []string{""},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("merged result is validated by labels.Validate", func(t *testing.T) {
		// Exceeding max 16 labels fails and leaves stored labels unchanged.
		tooMany := make(map[string]string, 17)
		for i := 0; i < 17; i++ {
			tooMany[fmt.Sprintf("k%02d", i)] = "v"
		}
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set: tooMany,
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

		// Non-printable ASCII fails.
		rec2 := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"bad": "line1\nline2"},
		})
		assert.Equal(t, http.StatusBadRequest, rec2.Code, rec2.Body.String())

		// Value > 63 chars fails.
		rec3 := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"long": strings.Repeat("x", 64)},
		})
		assert.Equal(t, http.StatusBadRequest, rec3.Code, rec3.Body.String())
	})

	t.Run("explicit stateVersion precondition returns 409 on mismatch without audit", func(t *testing.T) {
		cur, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		beforeAudits := len(f.labelAudits(t, f.agent.ID))

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set:          map[string]string{"ver": "stale"},
			StateVersion: cur.StateVersion + 99,
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Equal(t, beforeAudits, len(f.labelAudits(t, f.agent.ID)), "stale stateVersion must not write an audit record")

		recOK := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set:          map[string]string{"ver": "ok"},
			StateVersion: cur.StateVersion,
		})
		assert.Equal(t, http.StatusOK, recOK.Code, recOK.Body.String())
	})

	t.Run("deleted agent is refused with 409", func(t *testing.T) {
		delAg := &store.Agent{
			ID:           tid("lbl-deleted"),
			Slug:         "lbl-deleted",
			Name:         "lbl-deleted",
			ProjectID:    f.project.ID,
			Phase:        string(state.PhaseStopped),
			OwnerID:      f.owner.ID,
			CreatedBy:    f.owner.ID,
			DeletedAt:    time.Now(),
			StateVersion: 1,
		}
		require.NoError(t, f.s.CreateAgent(ctx, delAg))
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+delAg.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"a": "b"},
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	})

	t.Run("non-PATCH methods return 405", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
			rec := doRequestAsUser(t, f.srv, f.owner, method, "/api/v1/agents/"+f.agent.ID+"/labels", nil)
			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "method=%s", method)
			assert.Equal(t, http.MethodPatch, rec.Header().Get("Allow"))
		}
	})

	t.Run("existing PATCH full-replace behavior unchanged and audited", func(t *testing.T) {
		beforeAudits := len(f.labelAudits(t, f.agent.ID))
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID, map[string]any{
			"labels": map[string]string{"replaced": "only"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"replaced": "only"}, got.Labels)

		audits := f.labelAudits(t, f.agent.ID)
		require.Equal(t, beforeAudits+1, len(audits))
		latest := audits[len(audits)-1]
		assert.Equal(t, f.owner.ID, latest.ActorPrincipalID)
		assert.Equal(t, f.agent.ID, latest.TargetID)
		assert.Contains(t, latest.AfterSummary, `"replaced":"only"`)
	})
}

func TestPatchAgentLabels_AuditDetails(t *testing.T) {
	f := setupLabelTestFixture(t)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
		Set:    map[string]string{"env": "prod", "added": "yes"},
		Remove: []string{"team"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	audits := f.labelAudits(t, f.agent.ID)
	require.Len(t, audits, 1)
	rec0 := audits[0]
	assert.Equal(t, mutationTypeAgentLabelsUpdate, rec0.MutationType)
	assert.Equal(t, "agent", rec0.TargetType)
	assert.Equal(t, f.agent.ID, rec0.TargetID)
	assert.Equal(t, "user", rec0.ActorPrincipalKind)
	assert.Equal(t, f.owner.ID, rec0.ActorPrincipalID)

	var beforeDiff, afterDiff map[string]string
	require.NoError(t, json.Unmarshal([]byte(rec0.BeforeSummary), &beforeDiff))
	require.NoError(t, json.Unmarshal([]byte(rec0.AfterSummary), &afterDiff))
	assert.Equal(t, map[string]string{"env": "dev", "team": "core"}, beforeDiff)
	assert.Equal(t, map[string]string{"env": "prod", "added": "yes"}, afterDiff)
}

func TestPatchAgentLabels_Authorization(t *testing.T) {
	f := setupLabelTestFixture(t)

	t.Run("owner with agent.update is allowed", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"by": "owner"},
		})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("non-owner with project agent.update is allowed", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.editor, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"by": "editor"},
		})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("non-owner without agent.update is denied 403", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.outsider, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"by": "outsider"},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		recProj := doRequestAsUser(t, f.srv, f.outsider, http.MethodPatch, "/api/v1/projects/"+f.project.ID+"/agents/"+f.agent.Slug+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"by": "outsider"},
		})
		assert.Equal(t, http.StatusForbidden, recProj.Code, recProj.Body.String())
	})

	t.Run("agent caller cannot edit own, child, sibling, cross-project, or unscoped labels via merge or PATCH", func(t *testing.T) {
		selfToken := f.srv.generateRecordedAgentToken(t, f.agent.ID, f.project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate, ScopeAgentLifecycle, ScopeAgentCreate,
		}, nil)
		unscopedToken := f.srv.generateRecordedAgentToken(t, f.agent.ID, f.project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
		}, nil)
		otherToken := f.srv.generateRecordedAgentToken(t, f.otherAgent.ID, f.otherProject.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate, ScopeAgentLifecycle,
		}, nil)

		cases := []struct {
			name  string
			token string
			path  string
			body  any
		}{
			{"self merge by-id", selfToken, "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}},
			{"self merge project", selfToken, "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}},
			{"child merge by-id", selfToken, "/api/v1/agents/" + f.child.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}},
			{"sibling merge by-id", selfToken, "/api/v1/agents/" + f.sibling.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}},
			{"unscoped merge by-id", unscopedToken, "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}},
			{"other-project merge by-id", otherToken, "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}},
			{"self PATCH with labels by-id", selfToken, "/api/v1/agents/" + f.agent.ID, map[string]any{"labels": map[string]string{"x": "1"}}},
			{"self PATCH with labels project", selfToken, "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug, map[string]any{"labels": map[string]string{"x": "1"}}},
			{"child PATCH with labels by-id", selfToken, "/api/v1/agents/" + f.child.ID, map[string]any{"labels": map[string]string{"x": "1"}}},
			{"sibling PATCH with labels by-id", selfToken, "/api/v1/agents/" + f.sibling.ID, map[string]any{"labels": map[string]string{"x": "1"}}},
			{"unscoped PATCH with labels by-id", unscopedToken, "/api/v1/agents/" + f.agent.ID, map[string]any{"labels": map[string]string{"x": "1"}}},
			{"other-project PATCH with labels by-id", otherToken, "/api/v1/agents/" + f.agent.ID, map[string]any{"labels": map[string]string{"x": "1"}}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, tc.path, tc.body, tc.token)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), errMsgAgentsCannotEditLabels,
					"agent label refusal must come from the explicit agent-caller guard")
			})
		}

		// Non-label PATCH from an agent still goes through standard ActionUpdate
		// authorization (not the label-specific refusal).
		recNonLabel := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+f.child.ID, map[string]any{
			"taskSummary": "no labels here",
		}, selfToken)
		assert.Equal(t, http.StatusForbidden, recNonLabel.Code, recNonLabel.Body.String())
		assert.NotContains(t, recNonLabel.Body.String(), errMsgAgentsCannotEditLabels)
	})

	t.Run("federated and delegated agent callers are refused explicitly on merge and PATCH routes", func(t *testing.T) {
		ctx := context.Background()
		fedIdent := NewFederatedAgentIdentity("https://peer.example", tid("lbl-fed-agent"), f.project.ID, "fed-agent", f.owner.ID, []string{f.owner.ID}, nil)
		delegatedIdent := &DelegatedAgentIdentity{agentID: tid("lbl-delegated-agent")}

		identities := []struct {
			name string
			id   Identity
		}{
			{"federated_agent", fedIdent},
			{"delegated_agent", delegatedIdent},
		}

		for _, ic := range identities {
			t.Run(ic.name, func(t *testing.T) {
				beforeAgent, err := f.s.GetAgent(ctx, f.agent.ID)
				require.NoError(t, err)

				// 1. PATCH /api/v1/agents/{id}/labels
				bodyMerge, err := json.Marshal(PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}})
				require.NoError(t, err)
				req1 := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", bytes.NewReader(bodyMerge))
				req1 = req1.WithContext(contextWithIdentity(req1.Context(), ic.id))
				rec1 := httptest.NewRecorder()
				f.srv.handleAgentLabels(rec1, req1, f.agent.ID)
				assert.Equal(t, http.StatusForbidden, rec1.Code, rec1.Body.String())
				assert.Contains(t, rec1.Body.String(), errMsgAgentsCannotEditLabels)

				// 2. PATCH /api/v1/projects/{projectId}/agents/{id}/labels
				req2 := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+f.project.ID+"/agents/"+f.agent.Slug+"/labels", bytes.NewReader(bodyMerge))
				req2 = req2.WithContext(contextWithIdentity(req2.Context(), ic.id))
				rec2 := httptest.NewRecorder()
				f.srv.handleProjectAgentLabels(rec2, req2, f.project.ID, f.agent.Slug)
				assert.Equal(t, http.StatusForbidden, rec2.Code, rec2.Body.String())
				assert.Contains(t, rec2.Body.String(), errMsgAgentsCannotEditLabels)

				// 3. PATCH /api/v1/agents/{id} with labels
				bodyPatch, err := json.Marshal(map[string]any{"labels": map[string]string{"x": "1"}})
				require.NoError(t, err)
				req3 := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+f.agent.ID, bytes.NewReader(bodyPatch))
				req3 = req3.WithContext(contextWithIdentity(req3.Context(), ic.id))
				rec3 := httptest.NewRecorder()
				f.srv.applyAgentUpdate(rec3, req3, beforeAgent)
				assert.Equal(t, http.StatusForbidden, rec3.Code, rec3.Body.String())
				assert.Contains(t, rec3.Body.String(), errMsgAgentsCannotEditLabels)

				afterAgent, err := f.s.GetAgent(ctx, f.agent.ID)
				require.NoError(t, err)
				assert.Equal(t, beforeAgent.Labels, afterAgent.Labels)
			})
		}
	})
}

func TestPatchAgentLabels_AgentGuardDefenceInDepth(t *testing.T) {
	// Proves the explicit agent-caller guard is load-bearing even if agent.update
	// were ever granted an agent JWT scope in the permission registry: non-label
	// PATCH succeeds through the kernel, while every label-mutation surface is
	// still refused with 403 and leaves labels and audit records untouched.
	idx := -1
	for i := range permissions.Registry {
		if permissions.Registry[i].ID == "agent.update" {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "agent.update must exist in permissions.Registry")
	origScopes := append([]string(nil), permissions.Registry[idx].AgentScopes...)
	permissions.Registry[idx].AgentScopes = []string{string(ScopeAgentLifecycle)}
	t.Cleanup(func() {
		permissions.Registry[idx].AgentScopes = origScopes
	})

	f := setupLabelTestFixture(t)
	ctx := context.Background()
	beforeAudits := len(f.labelAudits(t, f.agent.ID))

	agentToken := f.srv.generateRecordedAgentToken(t, f.agent.ID, f.project.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate, ScopeAgentLifecycle,
	}, nil)

	// Confirm the kernel now admits agent.update for a non-label PATCH.
	recNonLabel := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+f.agent.ID, map[string]any{
		"taskSummary": "allowed-under-simulated-agent-scope",
	}, agentToken)
	require.Equal(t, http.StatusOK, recNonLabel.Code, "simulated agent.update scope must allow non-label PATCH: %s", recNonLabel.Body.String())

	// Every label-mutation surface must still be refused by the explicit guard.
	cases := []struct {
		name string
		path string
		body any
	}{
		{"merge by-id", "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}},
		{"merge project", "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}},
		{"PATCH labels by-id", "/api/v1/agents/" + f.agent.ID, map[string]any{"labels": map[string]string{"x": "1"}}},
		{"PATCH labels project", "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug, map[string]any{"labels": map[string]string{"x": "1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, tc.path, tc.body, agentToken)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), errMsgAgentsCannotEditLabels)
		})
	}

	got, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "dev", "team": "core"}, got.Labels)
	assert.Equal(t, beforeAudits, len(f.labelAudits(t, f.agent.ID)))
}

func TestPatchAgentLabels_PatchMultiFieldAuditAndRollback(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()
	setExperimentOverrides(t, f.srv, map[string]bool{
		experiments.NeverSuspend: true,
	})

	newCreatedAgent := func(t *testing.T, suffix string) *store.Agent {
		t.Helper()
		ag := &store.Agent{
			ID:            tid("lbl-mf-" + suffix),
			Slug:          "lbl-mf-" + suffix,
			Name:          "lbl-mf-" + suffix,
			ProjectID:     f.project.ID,
			Phase:         string(state.PhaseCreated),
			SuspendPolicy: store.SuspendPolicyAuto,
			OwnerID:       f.owner.ID,
			CreatedBy:     f.owner.ID,
			Labels:        map[string]string{"env": "dev"},
			StateVersion:  1,
		}
		require.NoError(t, f.s.CreateAgent(ctx, ag))
		return ag
	}

	cases := []struct {
		name   string
		suffix string
		extra  map[string]any
		labels map[string]string
	}{
		{
			name:   "labels only",
			suffix: "only",
			labels: map[string]string{"env": "prod", "branch": "only"},
		},
		{
			name:   "labels + taskSummary",
			suffix: "task",
			extra:  map[string]any{"taskSummary": "updated summary"},
			labels: map[string]string{"env": "prod", "branch": "task"},
		},
		{
			name:   "labels + name",
			suffix: "name",
			extra:  map[string]any{"name": "lbl-mf-renamed"},
			labels: map[string]string{"env": "prod", "branch": "name"},
		},
		{
			name:   "labels + suspendPolicy",
			suffix: "susp",
			extra:  map[string]any{"suspendPolicy": store.SuspendPolicyNever},
			labels: map[string]string{"env": "prod", "branch": "susp"},
		},
		{
			name:   "labels + gcp_identity",
			suffix: "gcp",
			extra:  map[string]any{"gcp_identity": map[string]any{"metadata_mode": store.GCPMetadataModeBlock}},
			labels: map[string]string{"env": "prod", "branch": "gcp"},
		},
		{
			name:   "labels + name + suspendPolicy + gcp_identity",
			suffix: "all",
			extra: map[string]any{
				"name":          "lbl-mf-all-renamed",
				"suspendPolicy": store.SuspendPolicyNever,
				"gcp_identity":  map[string]any{"metadata_mode": store.GCPMetadataModeBlock},
			},
			labels: map[string]string{"env": "prod", "branch": "all"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ag := newCreatedAgent(t, tc.suffix)
			body := map[string]any{"labels": tc.labels}
			for k, v := range tc.extra {
				body[k] = v
			}

			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+ag.ID, body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := f.s.GetAgent(ctx, ag.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.labels, got.Labels)

			audits := f.labelAudits(t, ag.ID)
			require.Len(t, audits, 1, "expected exactly one agent.labels.update audit record")
			a := audits[0]
			assert.Equal(t, mutationTypeAgentLabelsUpdate, a.MutationType)
			assert.Equal(t, "agent", a.TargetType)
			assert.Equal(t, ag.ID, a.TargetID)
			assert.Equal(t, "user", a.ActorPrincipalKind)
			assert.Equal(t, f.owner.ID, a.ActorPrincipalID)

			var beforeDiff, afterDiff map[string]string
			require.NoError(t, json.Unmarshal([]byte(a.BeforeSummary), &beforeDiff))
			require.NoError(t, json.Unmarshal([]byte(a.AfterSummary), &afterDiff))
			assert.Equal(t, map[string]string{"env": "dev"}, beforeDiff)
			assert.Equal(t, tc.labels, afterDiff)
		})
	}

	t.Run("labels + suspendPolicy rollback leaves no label change and no audit record", func(t *testing.T) {
		ag := newCreatedAgent(t, "rb-susp")
		installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *labelUpdateFaultStore {
			return &labelUpdateFaultStore{
				Store: inner,
				onSetSuspendPolicy: func(context.Context, string, string, func(context.Context, string, string) (bool, error)) (bool, error) {
					return false, fmt.Errorf("injected suspend policy failure")
				},
			}
		})

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+ag.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
			"labels":        map[string]string{"env": "rolledback"},
		})
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

		got, err := f.s.GetAgent(ctx, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"env": "dev"}, got.Labels, "rolled-back transaction must not persist label changes")
		assert.Equal(t, store.SuspendPolicyAuto, got.SuspendPolicy)
		assert.Empty(t, f.labelAudits(t, ag.ID), "rolled-back runPatchTx must leave zero label audit records")
	})

	t.Run("labels + name rollback leaves no label change and no audit record", func(t *testing.T) {
		ag := newCreatedAgent(t, "rb-name")
		installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *labelUpdateFaultStore {
			return &labelUpdateFaultStore{
				Store: inner,
				onUpdate: func(context.Context, *store.Agent, func(context.Context, *store.Agent) error) error {
					return fmt.Errorf("injected update failure")
				},
			}
		})

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+ag.ID, map[string]any{
			"name":   "lbl-mf-rb-name-new",
			"labels": map[string]string{"env": "rolledback"},
		})
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

		got, err := f.s.GetAgent(ctx, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"env": "dev"}, got.Labels)
		assert.Empty(t, f.labelAudits(t, ag.ID), "rolled-back PATCH must leave zero label audit records")
	})
}

func TestPatchAgentLabels_ConcurrentMerge(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	// Start with an empty label map so we can add 10 distinct keys concurrently.
	recReset := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
		Remove: []string{"env", "team"},
	})
	require.Equal(t, http.StatusOK, recReset.Code, recReset.Body.String())

	const workers = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]string, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			key := fmt.Sprintf("k%02d", idx)
			val := fmt.Sprintf("v%02d", idx)
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
				Set: map[string]string{key: val},
			})
			if rec.Code != http.StatusOK {
				errs[idx] = fmt.Sprintf("worker %d: status %d: %s", idx, rec.Code, rec.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, e := range errs {
		assert.Empty(t, e)
	}

	got, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	require.Len(t, got.Labels, workers, "all concurrent label merges must be preserved without lost updates")
	for i := 0; i < workers; i++ {
		assert.Equal(t, fmt.Sprintf("v%02d", i), got.Labels[fmt.Sprintf("k%02d", i)])
	}
}

func TestPatchAgentLabels_RetriesOnVersionConflict(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	var conflicts atomic.Int32
	installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *labelUpdateFaultStore {
		return &labelUpdateFaultStore{
			Store: inner,
			onUpdate: func(c context.Context, a *store.Agent, next func(context.Context, *store.Agent) error) error {
				if conflicts.Add(1) == 1 {
					return store.ErrVersionConflict
				}
				return next(c, a)
			},
		}
	})

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
		Set: map[string]string{"retried": "yes"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int32(2), conflicts.Load(), "handler must retry once after ErrVersionConflict")

	got, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "yes", got.Labels["retried"])
	assert.Equal(t, "dev", got.Labels["env"])
}

func TestPatchAgentLabels_AuditRollbackOnStoreFailure(t *testing.T) {
	f := setupLabelTestFixture(t)
	beforeAudits := len(f.labelAudits(t, f.agent.ID))

	installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *labelUpdateFaultStore {
		return &labelUpdateFaultStore{
			Store: inner,
			onUpdate: func(context.Context, *store.Agent, func(context.Context, *store.Agent) error) error {
				return fmt.Errorf("injected update failure")
			},
		}
	})

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
		Set: map[string]string{"fail": "true"},
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, beforeAudits, len(f.labelAudits(t, f.agent.ID)), "failed write must leave no audit record")
}

func TestPatchAgentLabels_ConcurrentSameKeyMerge(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	const workers = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]string, workers)
	expected := make(map[string]bool, workers)
	for i := 0; i < workers; i++ {
		val := fmt.Sprintf("v%d", i)
		expected[val] = true
		wg.Add(1)
		go func(idx int, v string) {
			defer wg.Done()
			<-start
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID+"/labels", PatchAgentLabelsRequest{
				Set: map[string]string{"shared": v},
			})
			if rec.Code != http.StatusOK {
				errs[idx] = fmt.Sprintf("worker %d: status %d: %s", idx, rec.Code, rec.Body.String())
			}
		}(i, val)
	}
	close(start)
	wg.Wait()

	for _, e := range errs {
		assert.Empty(t, e)
	}
	got, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.True(t, expected[got.Labels["shared"]], "final shared label %q must be one of the concurrent values", got.Labels["shared"])
	assert.Equal(t, "dev", got.Labels["env"], "unrelated labels must remain intact")
}

func TestPatchAgentLabels_PatchNoopNoAudit(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	t.Run("PATCH resending identical labels writes zero audit records", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID, map[string]any{
			"labels": map[string]string{"env": "dev", "team": "core"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"env": "dev", "team": "core"}, got.Labels)
		assert.Empty(t, f.labelAudits(t, f.agent.ID), "no-op PATCH resending identical labels must write zero audit records")
	})

	t.Run("PATCH resending identical labels with another field change writes zero label audit records", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID, map[string]any{
			"labels":      map[string]string{"env": "dev", "team": "core"},
			"taskSummary": "updated without changing labels",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"env": "dev", "team": "core"}, got.Labels)
		assert.Equal(t, "updated without changing labels", got.TaskSummary)
		assert.Empty(t, f.labelAudits(t, f.agent.ID), "no-op labels alongside another field update must write zero label audit records")
	})
}

func TestPatchAgentLabels_PatchAgentPreReadBodyLimit(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	t.Run("agent caller oversized PATCH body is rejected with 413 before authz", func(t *testing.T) {
		agentToken := f.srv.generateRecordedAgentToken(t, f.agent.ID, f.project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentLifecycle,
		}, nil)

		oversized := `{"labels":{"env":"hacked"},"pad":"` + strings.Repeat("a", maxSettingsBodySize+1024) + `"}`
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+f.agent.ID, bytes.NewReader([]byte(oversized)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+agentToken)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "payload_too_large")

		got, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"env": "dev", "team": "core"}, got.Labels)
		assert.Empty(t, f.labelAudits(t, f.agent.ID))
	})

	t.Run("normal caller oversized PATCH body is rejected with 413", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID, map[string]any{
			"labels":      map[string]string{"env": "prod", "team": "core"},
			"annotations": map[string]string{"pad": strings.Repeat("a", maxSettingsBodySize+1024)},
		})
		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "payload_too_large")

		got, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"env": "dev", "team": "core"}, got.Labels)
		assert.Empty(t, f.labelAudits(t, f.agent.ID))
	})

	t.Run("agent caller pre-read restores r.Body for downstream readers", func(t *testing.T) {
		payload := []byte(`{"labels":{"env":"hacked"}}`)
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+f.agent.ID, bytes.NewReader(payload))
		req = req.WithContext(contextWithIdentity(req.Context(), &DelegatedAgentIdentity{agentID: f.agent.ID}))
		rec := httptest.NewRecorder()
		f.srv.applyAgentUpdate(rec, req, f.agent)

		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		restored, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, restored, "r.Body must be restored after agent pre-read")
	})
}

func TestPatchAgentLabels_MergeBodyLimit(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	oversized := PatchAgentLabelsRequest{
		Set: map[string]string{
			"env": "prod",
			"pad": strings.Repeat("a", maxSettingsBodySize+1024),
		},
	}
	for _, tc := range []struct {
		name string
		path string
	}{
		{"by-id route", "/api/v1/agents/" + f.agent.ID + "/labels"},
		{"project-scoped route", "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug + "/labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, tc.path, oversized)

			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "payload_too_large")

			got, err := f.s.GetAgent(ctx, f.agent.ID)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"env": "dev", "team": "core"}, got.Labels)
			assert.Empty(t, f.labelAudits(t, f.agent.ID))
		})
	}
}

type countingReadCloser struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReadCloser) Close() error { return nil }

func TestUpdateAgent_PatchBodyLimit(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	userBearer := func(t *testing.T, user *store.User) string {
		t.Helper()
		token, _, _, err := f.srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
		)
		require.NoError(t, err)
		return "Bearer " + token
	}

	routes := []struct {
		name string
		path string
	}{
		{"by-id route", "/api/v1/agents/" + f.agent.ID},
		{"project-scoped route", "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug},
	}

	oversizedJSON := []byte(`{"taskSummary":"should-not-apply","annotations":{"pad":"` + strings.Repeat("a", maxSettingsBodySize+1024) + `"}}`)

	t.Run("oversized body from authorized owner gets 413 payload_too_large", func(t *testing.T) {
		for _, rt := range routes {
			t.Run(rt.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPatch, rt.path, bytes.NewReader(oversizedJSON))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", userBearer(t, f.owner))
				rec := httptest.NewRecorder()
				f.srv.Handler().ServeHTTP(rec, req)

				assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "payload_too_large")

				got, err := f.s.GetAgent(ctx, f.agent.ID)
				require.NoError(t, err)
				assert.Empty(t, got.TaskSummary)
				assert.Equal(t, map[string]string{"env": "dev", "team": "core"}, got.Labels)
				assert.Empty(t, f.labelAudits(t, f.agent.ID))
			})
		}
	})

	t.Run("unauthenticated caller gets 401 with zero bytes consumed", func(t *testing.T) {
		for _, rt := range routes {
			t.Run(rt.name, func(t *testing.T) {
				body := &countingReadCloser{r: bytes.NewReader(oversizedJSON)}
				req := httptest.NewRequest(http.MethodPatch, rt.path, body)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				f.srv.Handler().ServeHTTP(rec, req)

				assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
				assert.Zero(t, body.n.Load(), "unauthenticated caller must not trigger any body read")
			})
		}
	})

	t.Run("unauthorized outsider gets 403 with zero bytes consumed", func(t *testing.T) {
		for _, rt := range routes {
			t.Run(rt.name, func(t *testing.T) {
				body := &countingReadCloser{r: bytes.NewReader(oversizedJSON)}
				req := httptest.NewRequest(http.MethodPatch, rt.path, body)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", userBearer(t, f.outsider))
				rec := httptest.NewRecorder()
				f.srv.Handler().ServeHTTP(rec, req)

				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.Zero(t, body.n.Load(), "unauthorized caller must not trigger any body read")
			})
		}
	})

	t.Run("body exactly at maxSettingsBodySize succeeds", func(t *testing.T) {
		prefix := `{"taskSummary":"exact-limit","annotations":{"pad":"`
		suffix := `"}}`
		padLen := maxSettingsBodySize - len(prefix) - len(suffix)
		require.Greater(t, padLen, 0)
		exactBody := []byte(prefix + strings.Repeat("a", padLen) + suffix)
		require.Equal(t, maxSettingsBodySize, len(exactBody))

		req := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+f.agent.ID, bytes.NewReader(exactBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", userBearer(t, f.owner))
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "exact-limit", got.TaskSummary)
		assert.Equal(t, padLen, len(got.Annotations["pad"]))
	})

	t.Run("chunked oversized body gets 413", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+f.agent.ID, io.NopCloser(bytes.NewReader(oversizedJSON)))
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", userBearer(t, f.owner))
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "payload_too_large")
	})

	t.Run("lying small Content-Length oversized body gets 413", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+f.agent.ID, io.NopCloser(bytes.NewReader(oversizedJSON)))
		req.ContentLength = 16
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", userBearer(t, f.owner))
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "payload_too_large")
	})

	t.Run("normal PATCH body behaves unchanged", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.agent.ID, map[string]any{
			"taskSummary": "normal-update",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "normal-update", got.TaskSummary)
	})
}
