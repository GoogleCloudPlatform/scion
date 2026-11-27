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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

const decisionRetentionAdminPath = "/api/v1/admin/decision-retention"

func decisionRetentionAdminServer(t *testing.T, on bool) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()
	srv, s := testServerWithOps(t, nil)
	ctx := context.Background()
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "dr-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	member, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "dr-member", store.SystemRoleHubMember))
	require.NoError(t, err)
	if on {
		ops := srv.GetOperationalSettings()
		doc := fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.DecisionRetention)
		_, err := ops.Update(ctx, "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
		require.NoError(t, err)
	}
	return srv, s, admin, member
}

func decodeDecisionRetention(t *testing.T, rec *httptest.ResponseRecorder) decisionRetentionResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body decisionRetentionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// TestAdminDecisionRetention_ExperimentOff404 verifies GET and PUT return 404
// while hub.decision_retention is off and nothing is stored.
func TestAdminDecisionRetention_ExperimentOff404(t *testing.T) {
	srv, _, admin, _ := decisionRetentionAdminServer(t, false)
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, decisionRetentionAdminPath, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "retention_days": 30, "expected_revision": 0,
	})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), srv.GetOperationalSettings().SectionRevision(opsettings.DecisionRetentionSectionName))
}

// TestAdminDecisionRetention_NonAdmin403 verifies non-admin users receive 403.
func TestAdminDecisionRetention_NonAdmin403(t *testing.T) {
	srv, _, _, member := decisionRetentionAdminServer(t, true)
	rec := doRequestAsUser(t, srv, member, http.MethodGet, decisionRetentionAdminPath, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, member, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "expected_revision": 0,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestAdminDecisionRetention_TokenRefused verifies bearer tokens (UATs) are
// refused on this session-only route (authzop catalog TestRef).
func TestAdminDecisionRetention_TokenRefused(t *testing.T) {
	srv, _, admin, _ := decisionRetentionAdminServer(t, true)
	key := mintHubConfigToken(t, srv, admin.ID, hubBoundary(), "hub_config:read", "hub_config:update")
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"enabled": true, "retention_days": 30, "expected_revision": 0}
		}
		rec := doRequestWithToken(t, srv, key, method, decisionRetentionAdminPath, body)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s with UAT must be refused: %s", method, rec.Body.String())
		_, details := errorCodeAndDetails(t, rec)
		require.Equal(t, "IRREVERSIBLE_CASCADE", details["reason"], "%s: %s", method, rec.Body.String())
	}
	assert.False(t, srv.GetOperationalSettings().DecisionRetention().Enabled)
}

// TestAdminDecisionRetention_AdminRoundTrip verifies defaults, updates, null
// resets, bounds validation, and unknown-field rejection.
func TestAdminDecisionRetention_AdminRoundTrip(t *testing.T) {
	srv, _, admin, _ := decisionRetentionAdminServer(t, true)

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, decisionRetentionAdminPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"enabled":false,"retention_days":null,"project_overrides":{},"sweep":{"page_size":200,"tick_budget_seconds":30,"tick_interval_minutes":60},"revision":0,"runs":[]}`, rec.Body.String())

	got := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "retention_days": 45, "sweep": map[string]any{"page_size": 100}, "expected_revision": 0,
	}))
	assert.True(t, got.Enabled)
	require.NotNil(t, got.RetentionDays)
	assert.Equal(t, 45, *got.RetentionDays)
	assert.Empty(t, got.ProjectOverrides)
	assert.Equal(t, decisionRetentionSweepJSON{PageSize: 100, TickBudgetSeconds: 30, TickIntervalMinutes: 60}, got.Sweep)
	require.Positive(t, got.Revision)

	// Sweep-only update does not require expected_revision.
	got = decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"sweep": map[string]any{"tick_interval_minutes": 15, "page_size": nil},
	}))
	assert.Equal(t, decisionRetentionSweepJSON{PageSize: 200, TickBudgetSeconds: 30, TickIntervalMinutes: 15}, got.Sweep)
	assert.True(t, got.Enabled)

	// Null retention_days resets to keep forever; null sweep resets tuning.
	got = decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"retention_days": nil, "sweep": nil, "expected_revision": got.Revision,
	}))
	assert.Nil(t, got.RetentionDays)
	assert.Equal(t, decisionRetentionSweepJSON{PageSize: 200, TickBudgetSeconds: 30, TickIntervalMinutes: 60}, got.Sweep)

	// Bounds and unknown fields fail with 400.
	for _, bad := range []map[string]any{
		{"retention_days": 29, "expected_revision": got.Revision},
		{"retention_days": 36501, "expected_revision": got.Revision},
		{"unknown_field": true, "expected_revision": got.Revision},
		{"sweep": map[string]any{"page_size": 10}},
	} {
		rec = doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, bad)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "payload %v should be 400: %s", bad, rec.Body.String())
	}
}

// TestAdminDecisionRetention_RevisionConflictAndAudit verifies expected_revision
// enforcement, 409 conflicts, and mutation audit logging.
func TestAdminDecisionRetention_RevisionConflictAndAudit(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)

	// Missing expected_revision when changing enabled/retention_days -> 400.
	rec := doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "retention_days": 30,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Valid update -> 200 and audit record written.
	got := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "retention_days": 30, "expected_revision": 0,
	}))
	require.Positive(t, got.Revision)

	// Stale expected_revision -> 409.
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": false, "expected_revision": 0,
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// Verify mutation audit record exists with surface admin-decision-retention.
	audits, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		TargetID: opsettings.DecisionRetentionSectionName,
		Limit:    100,
	})
	require.NoError(t, err)
	require.NotEmpty(t, audits)
	assert.Equal(t, decisionRetentionAuditSurface, audits[0].ExecutorID)
	assert.Contains(t, audits[0].BeforeSummary, "enabled=false,retention_days=null")
	assert.Contains(t, audits[0].AfterSummary, "enabled=true,retention_days=30")
}

// TestAdminDecisionRetention_ProjectOverridesAndDeletedPruning verifies
// project_overrides validation, revision enforcement, mutation audit recording,
// and automatic dropping of overrides for deleted projects on the next save.
func TestAdminDecisionRetention_ProjectOverridesAndDeletedPruning(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)
	ctx := context.Background()

	p1 := &store.Project{ID: "11111111-1111-1111-1111-111111111111", Name: "P1", Slug: "p1"}
	p2 := &store.Project{ID: "22222222-2222-2222-2222-222222222222", Name: "P2", Slug: "p2"}
	require.NoError(t, s.CreateProject(ctx, p1))
	require.NoError(t, s.CreateProject(ctx, p2))

	// Missing expected_revision when changing project_overrides -> 400.
	rec := doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"project_overrides": map[string]any{
			p1.ID: map[string]any{"mode": "keep_forever"},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Invalid overrides fail closed with 400.
	for _, bad := range []map[string]any{
		{"project_overrides": map[string]any{"not-a-uuid": map[string]any{"mode": "keep_forever"}}, "expected_revision": 0},
		{"project_overrides": map[string]any{p1.ID: map[string]any{"mode": "days", "days": 29}}, "expected_revision": 0},
		{"project_overrides": map[string]any{p1.ID: map[string]any{"mode": "days"}}, "expected_revision": 0},
		{"project_overrides": map[string]any{p1.ID: map[string]any{"mode": "keep_forever", "days": 30}}, "expected_revision": 0},
		{"project_overrides": map[string]any{p1.ID: map[string]any{"mode": "unknown"}}, "expected_revision": 0},
		{"project_overrides": map[string]any{p1.ID: map[string]any{"mode": "keep_forever", "extra": 1}}, "expected_revision": 0},
	} {
		rec = doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, bad)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "bad payload %v: %s", bad, rec.Body.String())
	}

	// Ghost UUID mapped to keep_forever in THIS request must return 400 with a
	// field error on project_overrides.<id> and save nothing.
	ghostID := "33333333-3333-3333-3333-333333333333"
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled":        true,
		"retention_days": 30,
		"project_overrides": map[string]any{
			p1.ID:   map[string]any{"mode": "keep_forever"},
			ghostID: map[string]any{"mode": "keep_forever"},
		},
		"expected_revision": 0,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "ghost project override must return 400: %s", rec.Body.String())
	var valErr struct {
		Error  string `json:"error"`
		Errors []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &valErr))
	assert.Equal(t, "validation_failed", valErr.Error)
	require.Len(t, valErr.Errors, 1)
	assert.Equal(t, "project_overrides."+ghostID, valErr.Errors[0].Path)
	// Confirm nothing was saved.
	unchanged := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodGet, decisionRetentionAdminPath, nil))
	assert.Equal(t, int64(0), unchanged.Revision)
	assert.False(t, unchanged.Enabled)
	assert.Empty(t, unchanged.ProjectOverrides)

	// Valid save with two live projects succeeds.
	got := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled":        true,
		"retention_days": 30,
		"project_overrides": map[string]any{
			p1.ID: map[string]any{"mode": "keep_forever"},
			p2.ID: map[string]any{"mode": "days", "days": 90},
		},
		"expected_revision": 0,
	}))
	require.Len(t, got.ProjectOverrides, 2)
	assert.Equal(t, opsettings.DecisionRetentionOverrideModeKeepForever, got.ProjectOverrides[p1.ID].Mode)
	assert.Nil(t, got.ProjectOverrides[p1.ID].Days)
	assert.Equal(t, opsettings.DecisionRetentionOverrideModeDays, got.ProjectOverrides[p2.ID].Mode)
	require.NotNil(t, got.ProjectOverrides[p2.ID].Days)
	assert.Equal(t, 90, *got.ProjectOverrides[p2.ID].Days)

	// Mutation audit recorded the override summary.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		TargetID: opsettings.DecisionRetentionSectionName,
		Limit:    10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, audits)
	assert.Contains(t, audits[0].AfterSummary, p1.ID+":keep_forever")
	assert.Contains(t, audits[0].AfterSummary, p2.ID+":90d")

	// Delete p2: CompositeStore.DeleteProject immediately prunes p2's override
	// from storage (ptone/scion#9439). After refreshing opsettings (as
	// ProjectDeletionService.onProjectDeleted does in production), GET omits p2
	// with the bumped revision, and subsequent PUTs keep p1 intact.
	require.NoError(t, s.DeleteProject(ctx, p2.ID))
	_, err = srv.GetOperationalSettings().Refresh(ctx)
	require.NoError(t, err)
	gotAfterDelete := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodGet, decisionRetentionAdminPath, nil))
	assert.Equal(t, got.Revision+1, gotAfterDelete.Revision, "project delete pruning an override must bump revision")
	require.Len(t, gotAfterDelete.ProjectOverrides, 1)
	assert.Contains(t, gotAfterDelete.ProjectOverrides, p1.ID)
	assert.NotContains(t, gotAfterDelete.ProjectOverrides, p2.ID, "GET must omit override for deleted project")

	got = decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"retention_days":    45,
		"expected_revision": gotAfterDelete.Revision,
	}))
	require.Len(t, got.ProjectOverrides, 1)
	assert.Contains(t, got.ProjectOverrides, p1.ID)
	assert.NotContains(t, got.ProjectOverrides, p2.ID, "deleted project override must remain absent after save")

	// Clearing overrides via empty object works.
	got = decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"project_overrides": map[string]any{},
		"expected_revision": got.Revision,
	}))
	assert.Empty(t, got.ProjectOverrides)
}

// TestAdminDecisionRetention_ProjectDeletePrunesOverrideAndSameUUIDStartsClean
// verifies L1 (ptone/scion#9439) end-to-end through ProjectDeletionService and
// HTTP handlers:
//  1. Deleting a project via DELETE /api/v1/projects/{id} prunes its override
//     from storage AND refreshes the server's cached operational settings.
//  2. Other projects' overrides (including keep_forever holds) are untouched.
//  3. Re-creating a project with the exact same client-chosen UUID starts clean
//     with the hub default (90d), not the deleted project's shorter 30d override,
//     verified both on the retention API and by running a retention sweep.
func TestAdminDecisionRetention_ProjectDeletePrunesOverrideAndSameUUIDStartsClean(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	// Enable Decisions experiment too so /projects/{id}/decisions?tab=made works.
	ops := srv.GetOperationalSettings()
	_, err := ops.Update(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true,%q:true}}`, experiments.Decisions, experiments.DecisionRetention)),
		"test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)

	createProjWithID := func(id, name, slug string) {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects", map[string]any{
			"id": id, "name": name, "slug": slug,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "POST /api/v1/projects: %s", rec.Body.String())
	}

	pKeep := uuid.NewString()
	pOther := uuid.NewString()
	pReuse := uuid.NewString()
	createProjWithID(pKeep, "l1-keep", "l1-keep")
	createProjWithID(pOther, "l1-other", "l1-other")
	createProjWithID(pReuse, "l1-reuse", "l1-reuse")

	// Configure hub default 90d, pKeep=keep_forever, pOther=60d, pReuse=30d.
	saved := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled":        true,
		"retention_days": 90,
		"project_overrides": map[string]any{
			pKeep:  map[string]any{"mode": "keep_forever"},
			pOther: map[string]any{"mode": "days", "days": 60},
			pReuse: map[string]any{"mode": "days", "days": 30},
		},
		"sweep":             map[string]any{"tick_interval_minutes": 1},
		"expected_revision": 0,
	}))
	require.Len(t, saved.ProjectOverrides, 3)

	// Delete pReuse through the governed HTTP delete path.
	delRec := doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/projects/"+pReuse, nil)
	require.True(t, delRec.Code == http.StatusNoContent || delRec.Code == http.StatusOK,
		"DELETE project failed (%d): %s", delRec.Code, delRec.Body.String())

	// Cached settings must already have pReuse pruned (without manual Refresh),
	// while pKeep (keep_forever) and pOther (60d) remain untouched.
	cached := ops.DecisionRetention()
	assert.NotContains(t, cached.ProjectOverrides, pReuse, "cached overrides must drop deleted project immediately")
	require.Contains(t, cached.ProjectOverrides, pKeep)
	assert.Equal(t, opsettings.DecisionRetentionOverrideModeKeepForever, cached.ProjectOverrides[pKeep].Mode)
	require.Contains(t, cached.ProjectOverrides, pOther)
	require.NotNil(t, cached.ProjectOverrides[pOther].Days)
	assert.Equal(t, 60, *cached.ProjectOverrides[pOther].Days)

	// Re-create a project with the EXACT SAME client-chosen UUID pReuse.
	createProjWithID(pReuse, "l1-reuse-new", "l1-reuse-new")

	// Admin GET still shows only pKeep and pOther (no revived 30d override).
	afterRecreate := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodGet, decisionRetentionAdminPath, nil))
	assert.NotContains(t, afterRecreate.ProjectOverrides, pReuse)
	require.Contains(t, afterRecreate.ProjectOverrides, pKeep)
	assert.Equal(t, opsettings.DecisionRetentionOverrideModeKeepForever, afterRecreate.ProjectOverrides[pKeep].Mode)
	require.Contains(t, afterRecreate.ProjectOverrides, pOther)

	// Project Made endpoint reports hub default 90d (not the old 30d override).
	madeRec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+pReuse+"/decisions?tab=made", nil)
	require.Equal(t, http.StatusOK, madeRec.Code, madeRec.Body.String())
	var madeBody map[string]any
	require.NoError(t, json.Unmarshal(madeRec.Body.Bytes(), &madeBody))
	retObj, ok := madeBody["retention"].(map[string]any)
	require.True(t, ok, "re-created project must report hub default retention")
	assert.Equal(t, true, retObj["active"])
	assert.Equal(t, float64(90), retObj["days"], "re-created same-UUID project must use 90d default, not deleted 30d override")

	// Seed a 45-day-old settled decision in re-created pReuse (would be purged
	// under old 30d override, must survive under 90d default) and a 120-day-old
	// settled decision in pKeep (must survive under keep_forever hold) and a
	// 75-day-old settled decision in pOther (must be purged under 60d override).
	agentID := uuid.NewString()
	_, decReuse45 := seedOldSettledDecision(t, ctx, cs, pReuse, agentID, admin.ID, "REUSE45", 45*24*time.Hour)
	_, decKeep120 := seedOldSettledDecision(t, ctx, cs, pKeep, agentID, admin.ID, "KEEP120", 120*24*time.Hour)
	_, decOther75 := seedOldSettledDecision(t, ctx, cs, pOther, agentID, admin.ID, "OTHER75", 75*24*time.Hour)

	srv.decisionRetentionHandler()(ctx)

	_, err = cs.GetDecision(ctx, decReuse45)
	assert.NoError(t, err, "45d decision in re-created same-UUID project must survive under 90d hub default")
	_, err = cs.GetDecision(ctx, decKeep120)
	assert.NoError(t, err, "120d decision in keep_forever project must survive")
	_, err = cs.GetDecision(ctx, decOther75)
	assert.ErrorIs(t, err, store.ErrNotFound, "75d decision in 60d-override project must be purged")
}
