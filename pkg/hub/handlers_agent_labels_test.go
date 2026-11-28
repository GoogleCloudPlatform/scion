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

	t.Run("agent caller cannot edit own, sibling, cross-project, or unscoped labels via merge, or any labels via PATCH", func(t *testing.T) {
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
			msg   string
		}{
			{"self merge by-id", selfToken, "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, ""},
			{"self merge project", selfToken, "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, ""},
			{"sibling merge by-id", selfToken, "/api/v1/agents/" + f.sibling.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, ""},
			{"unscoped self merge by-id", unscopedToken, "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, ""},
			{"other-project merge by-id", otherToken, "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, ""},
			{"self PATCH with labels by-id", selfToken, "/api/v1/agents/" + f.agent.ID, map[string]any{"labels": map[string]string{"x": "1"}}, errMsgAgentsCannotEditLabels},
			{"self PATCH with labels project", selfToken, "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug, map[string]any{"labels": map[string]string{"x": "1"}}, errMsgAgentsCannotEditLabels},
			{"child PATCH with labels by-id", selfToken, "/api/v1/agents/" + f.child.ID, map[string]any{"labels": map[string]string{"x": "1"}}, errMsgAgentsCannotEditLabels},
			{"sibling PATCH with labels by-id", selfToken, "/api/v1/agents/" + f.sibling.ID, map[string]any{"labels": map[string]string{"x": "1"}}, errMsgAgentsCannotEditLabels},
			{"unscoped PATCH with labels by-id", unscopedToken, "/api/v1/agents/" + f.agent.ID, map[string]any{"labels": map[string]string{"x": "1"}}, errMsgAgentsCannotEditLabels},
			{"other-project PATCH with labels by-id", otherToken, "/api/v1/agents/" + f.agent.ID, map[string]any{"labels": map[string]string{"x": "1"}}, errMsgAgentsCannotEditLabels},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, tc.path, tc.body, tc.token)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				if tc.msg != "" {
					assert.Contains(t, rec.Body.String(), tc.msg,
						"agent label refusal must come from the explicit agent-caller guard")
				}
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

				// 2. PATCH /api/v1/projects/{projectId}/agents/{id}/labels
				req2 := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+f.project.ID+"/agents/"+f.agent.Slug+"/labels", bytes.NewReader(bodyMerge))
				req2 = req2.WithContext(contextWithIdentity(req2.Context(), ic.id))
				rec2 := httptest.NewRecorder()
				f.srv.handleProjectAgentLabels(rec2, req2, f.project.ID, f.agent.Slug)
				assert.Equal(t, http.StatusForbidden, rec2.Code, rec2.Body.String())

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

func recordLabelTestDelegationEdge(t *testing.T, s store.Store, delegatorType, delegatorID, delegateID, projectID string, ceiling store.EffectCeiling) {
	t.Helper()
	require.NoError(t, s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    delegateID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		EffectCeiling: ceiling,
	}))
}

// TestPatchAgentLabels_CreatorAgentMatrix verifies ptone/scion#9867:
// a local agent may edit non-reserved labels on its direct children in the
// same project via PATCH .../labels, while self, sibling, parent, grandparent,
// cross-project, federated, delegated, hub-delivery, broken-chain, orphaned,
// reserved-key, and full-PATCH requests are refused.
func TestPatchAgentLabels_CreatorAgentMatrix(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	// Wire delegation chain: owner -> agent -> child -> grandchild.
	recordLabelTestDelegationEdge(t, f.s, store.DelegationPrincipalUser, f.owner.ID, f.agent.ID, f.project.ID,
		store.EffectCeiling{Kind: store.EffectCeilingPrincipal})
	recordLabelTestDelegationEdge(t, f.s, store.DelegationPrincipalAgent, f.agent.ID, f.child.ID, f.project.ID,
		store.EffectCeiling{Kind: store.EffectCeilingPrincipal})

	grandchild := &store.Agent{
		ID:           tid("lbl-grandchild"),
		Slug:         "lbl-grandchild",
		Name:         "lbl-grandchild",
		ProjectID:    f.project.ID,
		Phase:        string(state.PhaseRunning),
		OwnerID:      f.child.ID,
		CreatedBy:    f.child.ID,
		Ancestry:     []string{f.owner.ID, f.agent.ID, f.child.ID},
		Labels:       map[string]string{"tier": "leaf"},
		StateVersion: 1,
	}
	require.NoError(t, f.s.CreateAgent(ctx, grandchild))
	recordLabelTestDelegationEdge(t, f.s, store.DelegationPrincipalAgent, f.child.ID, grandchild.ID, f.project.ID,
		store.EffectCeiling{Kind: store.EffectCeilingPrincipal})

	// Cross-project agent whose CreatedBy happens to match f.agent.ID.
	crossProjChild := &store.Agent{
		ID:           tid("lbl-cross-proj-child"),
		Slug:         "lbl-cross-proj-child",
		Name:         "lbl-cross-proj-child",
		ProjectID:    f.otherProject.ID,
		Phase:        string(state.PhaseRunning),
		OwnerID:      f.agent.ID,
		CreatedBy:    f.agent.ID,
		Ancestry:     []string{f.owner.ID, f.agent.ID},
		Labels:       map[string]string{"env": "other"},
		StateVersion: 1,
	}
	require.NoError(t, f.s.CreateAgent(ctx, crossProjChild))

	agentToken := f.srv.generateRecordedAgentToken(t, f.agent.ID, f.project.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate, ScopeAgentLifecycle, ScopeAgentCreate,
	}, []string{f.owner.ID})
	childToken := f.srv.generateRecordedAgentToken(t, f.child.ID, f.project.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate, ScopeAgentLifecycle, ScopeAgentCreate,
	}, []string{f.owner.ID, f.agent.ID})
	otherToken := f.srv.generateRecordedAgentToken(t, f.otherAgent.ID, f.otherProject.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate, ScopeAgentLifecycle, ScopeAgentCreate,
	}, []string{f.outsider.ID})

	t.Run("direct creator agent allowed on by-id and project routes and audited with acting agent", func(t *testing.T) {
		// By-id route: set + remove.
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+f.child.ID+"/labels", PatchAgentLabelsRequest{
			Set:    map[string]string{"role": "lead", "team": "alpha"},
			Remove: []string{"tier"},
		}, agentToken)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := f.s.GetAgent(ctx, f.child.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"role": "lead", "team": "alpha"}, got.Labels)

		// Verify audit names the acting agent.
		audits := f.labelAudits(t, f.child.ID)
		require.Len(t, audits, 1)
		assert.Equal(t, mutationTypeAgentLabelsUpdate, audits[0].MutationType)
		assert.Equal(t, "agent", audits[0].ActorPrincipalKind)
		assert.Equal(t, f.agent.ID, audits[0].ActorPrincipalID)
		assert.Equal(t, string(CredentialKindAgentJWT), audits[0].ActorCredentialType)
		assert.NotEmpty(t, audits[0].ActorCredentialID)

		// Project-scoped route.
		recProj := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/projects/"+f.project.ID+"/agents/"+f.child.Slug+"/labels", PatchAgentLabelsRequest{
			Set:    map[string]string{"team": "beta"},
			Remove: []string{"role"},
		}, agentToken)
		require.Equal(t, http.StatusOK, recProj.Code, recProj.Body.String())

		got, err = f.s.GetAgent(ctx, f.child.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"team": "beta"}, got.Labels)
		assert.Len(t, f.labelAudits(t, f.child.ID), 2)
	})

	t.Run("multi-hop direct creator allowed (child edits its direct child grandchild)", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+grandchild.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"role": "worker"},
		}, childToken)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := f.s.GetAgent(ctx, grandchild.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"tier": "leaf", "role": "worker"}, got.Labels)

		audits := f.labelAudits(t, grandchild.ID)
		require.Len(t, audits, 1)
		assert.Equal(t, "agent", audits[0].ActorPrincipalKind)
		assert.Equal(t, f.child.ID, audits[0].ActorPrincipalID)
	})

	t.Run("self, sibling, parent, grandparent, cross-project, and full PATCH denied 403", func(t *testing.T) {
		cases := []struct {
			name   string
			token  string
			path   string
			body   any
			target string
		}{
			{"self by-id", agentToken, "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, f.agent.ID},
			{"self project", agentToken, "/api/v1/projects/" + f.project.ID + "/agents/" + f.agent.Slug + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, f.agent.ID},
			{"sibling by-id", agentToken, "/api/v1/agents/" + f.sibling.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, f.sibling.ID},
			{"parent by-id (child editing creator)", childToken, "/api/v1/agents/" + f.agent.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, f.agent.ID},
			{"grandparent by-id (direct children only)", agentToken, "/api/v1/agents/" + grandchild.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, grandchild.ID},
			{"grandparent project (direct children only)", agentToken, "/api/v1/projects/" + f.project.ID + "/agents/" + grandchild.Slug + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, grandchild.ID},
			{"cross-project other agent", otherToken, "/api/v1/agents/" + f.child.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, f.child.ID},
			{"cross-project target with matching CreatedBy", agentToken, "/api/v1/agents/" + crossProjChild.ID + "/labels", PatchAgentLabelsRequest{Set: map[string]string{"x": "1"}}, crossProjChild.ID},
			{"direct creator on full PATCH /agents/{id} stays closed", agentToken, "/api/v1/agents/" + f.child.ID, map[string]any{"labels": map[string]string{"x": "1"}}, f.child.ID},
			{"direct creator on full PATCH project route stays closed", agentToken, "/api/v1/projects/" + f.project.ID + "/agents/" + f.child.Slug, map[string]any{"labels": map[string]string{"x": "1"}}, f.child.ID},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				before, err := f.s.GetAgent(ctx, tc.target)
				require.NoError(t, err)
				beforeAudits := len(f.labelAudits(t, tc.target))

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, tc.path, tc.body, tc.token)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

				after, err := f.s.GetAgent(ctx, tc.target)
				require.NoError(t, err)
				assert.Equal(t, before.Labels, after.Labels)
				assert.Equal(t, beforeAudits, len(f.labelAudits(t, tc.target)))
			})
		}
	})

	t.Run("federated, delegated, and hub-delivery identities denied even when CreatedBy matches", func(t *testing.T) {
		fedID := tid("lbl-fed-creator")
		delID := tid("lbl-del-creator")
		hubDelID := tid("lbl-hubdel-creator")

		makeTarget := func(slug, createdBy string) *store.Agent {
			a := &store.Agent{
				ID: tid(slug), Slug: slug, Name: slug, ProjectID: f.project.ID,
				Phase: string(state.PhaseRunning), OwnerID: f.owner.ID, CreatedBy: createdBy,
				Ancestry: []string{f.owner.ID, createdBy}, Labels: map[string]string{"k": "v"}, StateVersion: 1,
			}
			require.NoError(t, f.s.CreateAgent(ctx, a))
			return a
		}
		fedTarget := makeTarget("lbl-fed-child", fedID)
		delTarget := makeTarget("lbl-del-child", delID)
		hubDelTarget := makeTarget("lbl-hubdel-child", hubDelID)

		identities := []struct {
			name   string
			id     Identity
			target *store.Agent
		}{
			{"federated_agent", NewFederatedAgentIdentity("https://peer.example", fedID, f.project.ID, "fed", f.owner.ID, []string{f.owner.ID}, nil), fedTarget},
			{"agent_delegated", &DelegatedAgentIdentity{agentID: delID, authorizingUserID: f.owner.ID}, delTarget},
			{"hub_delivery", &hubDeliveryIdentity{agentID: hubDelID, projectID: f.project.ID, ancestry: []string{f.owner.ID}}, hubDelTarget},
		}
		for _, tc := range identities {
			t.Run(tc.name, func(t *testing.T) {
				body, err := json.Marshal(PatchAgentLabelsRequest{Set: map[string]string{"role": "hijack"}})
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+tc.target.ID+"/labels", bytes.NewReader(body))
				req = req.WithContext(contextWithIdentity(req.Context(), tc.id))
				rec := httptest.NewRecorder()
				f.srv.handleAgentLabels(rec, req, tc.target.ID)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

				after, err := f.s.GetAgent(ctx, tc.target.ID)
				require.NoError(t, err)
				assert.Equal(t, map[string]string{"k": "v"}, after.Labels)
			})
		}
	})

	t.Run("broken delegation chain denied 403", func(t *testing.T) {
		// Create a separate parent/child whose delegation edge points to a non-existent user.
		brokenParent := &store.Agent{
			ID: tid("lbl-broken-parent"), Slug: "lbl-broken-parent", Name: "lbl-broken-parent",
			ProjectID: f.project.ID, Phase: string(state.PhaseRunning),
			OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Ancestry: []string{f.owner.ID}, StateVersion: 1,
		}
		require.NoError(t, f.s.CreateAgent(ctx, brokenParent))
		recordLabelTestDelegationEdge(t, f.s, store.DelegationPrincipalUser, tid("lbl-missing-user"), brokenParent.ID, f.project.ID,
			store.EffectCeiling{Kind: store.EffectCeilingPrincipal})

		brokenChild := &store.Agent{
			ID: tid("lbl-broken-child"), Slug: "lbl-broken-child", Name: "lbl-broken-child",
			ProjectID: f.project.ID, Phase: string(state.PhaseRunning),
			OwnerID: brokenParent.ID, CreatedBy: brokenParent.ID, Ancestry: []string{f.owner.ID, brokenParent.ID},
			Labels: map[string]string{"k": "v"}, StateVersion: 1,
		}
		require.NoError(t, f.s.CreateAgent(ctx, brokenChild))

		brokenTok := f.srv.generateRecordedAgentToken(t, brokenParent.ID, f.project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate, ScopeAgentLifecycle, ScopeAgentCreate,
		}, []string{f.owner.ID})

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+brokenChild.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"role": "worker"},
		}, brokenTok)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		after, err := f.s.GetAgent(ctx, brokenChild.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"k": "v"}, after.Labels)
	})

	t.Run("orphaned child (creator deleted) is human-only", func(t *testing.T) {
		// Soft-delete f.child (creator of grandchild):
		// 1. Grandparent (f.agent) is still denied.
		// 2. Deleted creator (f.child) cannot edit (delegation/standing denied).
		// 3. Human owner/editor can still edit grandchild.
		require.NoError(t, f.s.DeleteAgent(ctx, f.child.ID))

		recGrandparent := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+grandchild.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"orphan": "by-grandparent"},
		}, agentToken)
		assert.Equal(t, http.StatusForbidden, recGrandparent.Code, recGrandparent.Body.String())

		recDeletedCreator := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+grandchild.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"orphan": "by-deleted-creator"},
		}, childToken)
		assert.True(t, recDeletedCreator.Code == http.StatusUnauthorized || recDeletedCreator.Code == http.StatusForbidden,
			"deleted creator must be refused (401 or 403), got %d: %s", recDeletedCreator.Code, recDeletedCreator.Body.String())

		recHuman := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+grandchild.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"orphan": "curated-by-human"},
		})
		require.Equal(t, http.StatusOK, recHuman.Code, recHuman.Body.String())
		got, err := f.s.GetAgent(ctx, grandchild.ID)
		require.NoError(t, err)
		assert.Equal(t, "curated-by-human", got.Labels["orphan"])
	})

	t.Run("bounded V1 delegation ceiling excluding agent.update admits direct-child label merge and refuses full PATCH", func(t *testing.T) {
		boundedParent := &store.Agent{
			ID: tid("lbl-bounded-parent"), Slug: "lbl-bounded-parent", Name: "lbl-bounded-parent",
			ProjectID: f.project.ID, Phase: string(state.PhaseRunning),
			OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Ancestry: []string{f.owner.ID}, StateVersion: 1,
		}
		require.NoError(t, f.s.CreateAgent(ctx, boundedParent))
		recordLabelTestDelegationEdge(t, f.s, store.DelegationPrincipalUser, f.owner.ID, boundedParent.ID, f.project.ID,
			store.EffectCeiling{
				Kind:          store.EffectCeilingBounded,
				Version:       permissions.CeilingVersionV1,
				PermissionIDs: []string{"agent.create", "agent.read"},
			})

		boundedChild := &store.Agent{
			ID: tid("lbl-bounded-child"), Slug: "lbl-bounded-child", Name: "lbl-bounded-child",
			ProjectID: f.project.ID, Phase: string(state.PhaseRunning),
			OwnerID: boundedParent.ID, CreatedBy: boundedParent.ID, Ancestry: []string{f.owner.ID, boundedParent.ID},
			Labels: map[string]string{"tier": "worker"}, StateVersion: 1,
		}
		require.NoError(t, f.s.CreateAgent(ctx, boundedChild))

		boundedTok := f.srv.generateRecordedAgentToken(t, boundedParent.ID, f.project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate, ScopeAgentLifecycle, ScopeAgentCreate,
		}, []string{f.owner.ID})

		// Direct creator is admitted on the label merge route despite agent.update
		// being absent from the bounded V1 edge's frozen PermissionIDs.
		recMerge := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+boundedChild.ID+"/labels", PatchAgentLabelsRequest{
			Set: map[string]string{"role": "bounded-ok"},
		}, boundedTok)
		require.Equal(t, http.StatusOK, recMerge.Code, recMerge.Body.String())

		got, err := f.s.GetAgent(ctx, boundedChild.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"tier": "worker", "role": "bounded-ok"}, got.Labels)

		// Full PATCH (with labels and with non-label fields) remains refused 403.
		for _, tc := range []struct {
			name string
			body any
		}{
			{"full PATCH with labels", map[string]any{"labels": map[string]string{"role": "nope"}}},
			{"full PATCH non-label field", map[string]any{"taskSummary": "nope"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+boundedChild.ID, tc.body, boundedTok)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			})
		}

		after, err := f.s.GetAgent(ctx, boundedChild.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"tier": "worker", "role": "bounded-ok"}, after.Labels)
		assert.Empty(t, after.TaskSummary)
	})

	t.Run("intermediate agent-delegator project check refuses cross-project delegator", func(t *testing.T) {
		cases := []struct {
			name             string
			delegatorID      string
			permissionID     string
			scopeType        string
			scopeID          string
			creatorLabelEdit bool
			wantAllowed      bool
			wantReasonSubstr string
		}{
			{
				name:             "same-project delegator allowed for creator label edit",
				delegatorID:      f.agent.ID,
				permissionID:     permissionAgentUpdate,
				scopeType:        store.RoleScopeProject,
				scopeID:          f.project.ID,
				creatorLabelEdit: true,
				wantAllowed:      true,
			},
			{
				name:             "cross-project delegator denied for creator label edit",
				delegatorID:      f.otherAgent.ID,
				permissionID:     permissionAgentUpdate,
				scopeType:        store.RoleScopeProject,
				scopeID:          f.project.ID,
				creatorLabelEdit: true,
				wantAllowed:      false,
				wantReasonSubstr: "outside project",
			},
			{
				name:             "non-project scope denied for creator label edit",
				delegatorID:      f.agent.ID,
				permissionID:     permissionAgentUpdate,
				scopeType:        store.RoleScopeSystem,
				scopeID:          "",
				creatorLabelEdit: true,
				wantAllowed:      false,
				wantReasonSubstr: "outside project",
			},
			{
				name:             "same-project delegator denied when creatorLabelEdit is false",
				delegatorID:      f.agent.ID,
				permissionID:     permissionAgentUpdate,
				scopeType:        store.RoleScopeProject,
				scopeID:          f.project.ID,
				creatorLabelEdit: false,
				wantAllowed:      false,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				allowed, reason, err := f.srv.authzService.checkAgentHoldsPermissionFor(
					ctx, tc.delegatorID, tc.permissionID, tc.scopeType, tc.scopeID, tc.creatorLabelEdit,
				)
				require.NoError(t, err)
				assert.Equal(t, tc.wantAllowed, allowed, "reason=%q", reason)
				if tc.wantReasonSubstr != "" {
					assert.Contains(t, reason, tc.wantReasonSubstr)
				}
			})
		}
	})
}

func TestPatchAgentLabels_ReservedKeysAndUnauthenticated(t *testing.T) {
	f := setupLabelTestFixture(t)
	ctx := context.Background()

	recordLabelTestDelegationEdge(t, f.s, store.DelegationPrincipalUser, f.owner.ID, f.agent.ID, f.project.ID,
		store.EffectCeiling{Kind: store.EffectCeilingPrincipal})

	// Seed a reserved label on child via human owner first so we can test Remove too.
	seedRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.child.ID+"/labels", PatchAgentLabelsRequest{
		Set: map[string]string{
			"scion.io/template":                     "default",
			"scion.dev/github-token-source-project": "proj-x",
			"scion.run_id":                          "run-1",
			"trigger":                               "t1",
			"trigger_event":                         "push",
			"trigger_source":                        "github",
			"scion_project_id":                      "p1",
			"agent_id":                              "a1",
		},
	})
	require.Equal(t, http.StatusOK, seedRec.Code, "humans must be allowed to set reserved keys: %s", seedRec.Body.String())

	agentToken := f.srv.generateRecordedAgentToken(t, f.agent.ID, f.project.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate, ScopeAgentLifecycle, ScopeAgentCreate,
	}, []string{f.owner.ID})

	reservedKeys := []string{
		"scion.io/template",
		"scion.dev/github-token-source-project",
		"scion.run_id",
		"Scion.Dev/MixedCase",
		"trigger",
		"trigger_event",
		"trigger_source",
		"scion_project_id",
		"agent_id",
	}

	t.Run("agent cannot set or remove any reserved key (atomic whole-request refusal)", func(t *testing.T) {
		for _, rk := range reservedKeys {
			t.Run("set_"+rk, func(t *testing.T) {
				before, err := f.s.GetAgent(ctx, f.child.ID)
				require.NoError(t, err)
				beforeAudits := len(f.labelAudits(t, f.child.ID))

				// Mixed request: one valid key + one reserved key must be refused atomically.
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+f.child.ID+"/labels", PatchAgentLabelsRequest{
					Set: map[string]string{"role": "should-not-apply", rk: "spoof"},
				}, agentToken)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "reserved_label_key")

				after, err := f.s.GetAgent(ctx, f.child.ID)
				require.NoError(t, err)
				assert.Equal(t, before.Labels, after.Labels, "no partial apply allowed")
				assert.Equal(t, beforeAudits, len(f.labelAudits(t, f.child.ID)))
			})

			t.Run("remove_"+rk, func(t *testing.T) {
				before, err := f.s.GetAgent(ctx, f.child.ID)
				require.NoError(t, err)
				beforeAudits := len(f.labelAudits(t, f.child.ID))

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+f.child.ID+"/labels", PatchAgentLabelsRequest{
					Set:    map[string]string{"role": "should-not-apply"},
					Remove: []string{rk},
				}, agentToken)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "reserved_label_key")

				after, err := f.s.GetAgent(ctx, f.child.ID)
				require.NoError(t, err)
				assert.Equal(t, before.Labels, after.Labels, "no partial apply allowed")
				assert.Equal(t, beforeAudits, len(f.labelAudits(t, f.child.ID)))
			})
		}
	})

	t.Run("human can remove reserved keys", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+f.child.ID+"/labels", PatchAgentLabelsRequest{
			Remove: []string{"scion.io/template", "scion.dev/github-token-source-project", "scion.run_id"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := f.s.GetAgent(ctx, f.child.ID)
		require.NoError(t, err)
		_, hasIO := got.Labels["scion.io/template"]
		_, hasDev := got.Labels["scion.dev/github-token-source-project"]
		_, hasDot := got.Labels["scion.run_id"]
		assert.False(t, hasIO)
		assert.False(t, hasDev)
		assert.False(t, hasDot)
	})

	t.Run("unauthenticated request returns 401 before any body read", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			path string
		}{
			{"by-id route", "/api/v1/agents/" + f.child.ID + "/labels"},
			{"project route", "/api/v1/projects/" + f.project.ID + "/agents/" + f.child.Slug + "/labels"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				body := &countingReadCloser{r: bytes.NewReader([]byte(`{"set":{"role":"x"}}`))}
				req := httptest.NewRequest(http.MethodPatch, tc.path, body)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				f.srv.Handler().ServeHTTP(rec, req)

				assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
				assert.Zero(t, body.n.Load(), "unauthenticated caller must not trigger any body read")
			})
		}
	})
}

func TestCreatorLabelEditHolds_Unit(t *testing.T) {
	projID := tid("cl-unit-proj")
	otherProjID := tid("cl-unit-other-proj")
	ownerID := tid("cl-unit-owner")
	parentID := tid("cl-unit-parent")
	childID := tid("cl-unit-child")
	grandchildID := tid("cl-unit-grandchild")

	parent := progenyPairAgent(parentID, projID, []string{ownerID}, []AgentTokenScope{ScopeAgentStatusUpdate})

	child := &store.Agent{ID: childID, ProjectID: projID, CreatedBy: parentID, Ancestry: []string{ownerID, parentID}}
	grandchild := &store.Agent{ID: grandchildID, ProjectID: projID, CreatedBy: childID, Ancestry: []string{ownerID, parentID, childID}}
	self := &store.Agent{ID: parentID, ProjectID: projID, CreatedBy: parentID, Ancestry: []string{ownerID}}
	otherProj := &store.Agent{ID: tid("cl-unit-xproj"), ProjectID: otherProjID, CreatedBy: parentID, Ancestry: []string{ownerID, parentID}}
	sibling := &store.Agent{ID: tid("cl-unit-sib"), ProjectID: projID, CreatedBy: ownerID, Ancestry: []string{ownerID}}

	assert.True(t, creatorLabelEditHolds(parent, agentLabelsUpdateResource(child), ActionUpdate, permissionAgentUpdate),
		"direct child in same project must be admitted")
	assert.False(t, creatorLabelEditHolds(parent, agentResource(child), ActionUpdate, permissionAgentUpdate),
		"plain agentResource without labelEditTarget must be refused")
	assert.False(t, creatorLabelEditHolds(parent, agentLabelsUpdateResource(child), ActionRead, permissionAgentUpdate),
		"wrong action must be refused")
	assert.False(t, creatorLabelEditHolds(parent, agentLabelsUpdateResource(child), ActionUpdate, "agent.read"),
		"wrong permission must be refused")
	assert.False(t, creatorLabelEditHolds(parent, agentLabelsUpdateResource(grandchild), ActionUpdate, permissionAgentUpdate),
		"grandchild must be refused (direct children only)")
	assert.False(t, creatorLabelEditHolds(parent, agentLabelsUpdateResource(self), ActionUpdate, permissionAgentUpdate),
		"self-edit must be refused even if CreatedBy equals self")
	assert.False(t, creatorLabelEditHolds(parent, agentLabelsUpdateResource(otherProj), ActionUpdate, permissionAgentUpdate),
		"cross-project target must be refused")
	assert.False(t, creatorLabelEditHolds(parent, agentLabelsUpdateResource(sibling), ActionUpdate, permissionAgentUpdate),
		"sibling must be refused")
	assert.False(t, creatorLabelEditHolds(&hubDeliveryIdentity{agentID: parentID, projectID: projID}, agentLabelsUpdateResource(child), ActionUpdate, permissionAgentUpdate),
		"hubDeliveryIdentity must be refused")

	// creatorLabelCandidate refuses federated and delegated principals even when ID matches CreatedBy.
	svc := &AuthzService{}
	fed := NewFederatedAgentIdentity("https://peer.example", parentID, projID, "fed", ownerID, []string{ownerID}, nil)
	_, ok := svc.creatorLabelCandidate(PrincipalContext{Kind: PrincipalKindFederatedAgent, ID: parentID, Identity: fed}, agentLabelsUpdateResource(child), ActionUpdate, permissionAgentUpdate)
	assert.False(t, ok, "federated agent must be refused")
	_, ok = svc.creatorLabelCandidate(PrincipalContext{Kind: PrincipalKindFederatedAgent, ID: parentID, Identity: parent}, agentLabelsUpdateResource(child), ActionUpdate, permissionAgentUpdate)
	assert.False(t, ok, "non-local principal kind must be refused even with AgentIdentity")
	_, ok = svc.creatorLabelCandidate(PrincipalContext{Kind: PrincipalKindAgentDelegated, ID: parentID, Identity: &DelegatedAgentIdentity{agentID: parentID}}, agentLabelsUpdateResource(child), ActionUpdate, permissionAgentUpdate)
	assert.False(t, ok, "delegated agent must be refused")
	_, ok = svc.creatorLabelCandidate(PrincipalContext{Kind: PrincipalKindAgent, ID: parentID, Identity: parent}, agentLabelsUpdateResource(child), ActionUpdate, permissionAgentUpdate)
	assert.True(t, ok, "local agent creator must produce creator_label candidate")
}
