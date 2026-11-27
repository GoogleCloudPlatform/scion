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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
	onUpdate func(ctx context.Context, a *store.Agent, next func(context.Context, *store.Agent) error) error
}

func (w *labelUpdateFaultStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if w.onUpdate != nil {
		return w.onUpdate(ctx, a, w.Store.UpdateAgent)
	}
	return w.Store.UpdateAgent(ctx, a)
}

func (w *labelUpdateFaultStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return w.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&labelUpdateFaultStore{Store: tx, onUpdate: w.onUpdate})
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
