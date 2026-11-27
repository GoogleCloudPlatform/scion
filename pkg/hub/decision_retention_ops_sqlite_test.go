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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

func decisionRetentionOpPath(key string) string {
	return "/api/v1/admin/maintenance/operations/" + key + "/run"
}

func waitMaintenanceRunDone(t *testing.T, s store.Store, runID string) *store.MaintenanceOperationRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.GetMaintenanceRun(context.Background(), runID)
		require.NoError(t, err)
		if r.Status != store.MaintenanceStatusRunning {
			return r
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("maintenance run %s did not finish in time", runID)
	return nil
}

// TestDecisionRetentionOps_ExperimentOff404AndNonAdmin403 verifies that
// decision-retention-preview and decision-retention-run are hidden and 404
// while hub.decision_retention is off, and 403 for non-admin users while on.
func TestDecisionRetentionOps_ExperimentOff404AndNonAdmin403(t *testing.T) {
	srv, s, admin, member := decisionRetentionAdminServer(t, false)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())

	// Experiment OFF: operations hidden from list and 404 on direct GET/POST.
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), entadapter.DecisionRetentionPreviewKey)
	assert.NotContains(t, rec.Body.String(), entadapter.DecisionRetentionRunKey)

	for _, key := range []string{entadapter.DecisionRetentionPreviewKey, entadapter.DecisionRetentionRunKey} {
		rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/operations/"+key, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "GET %s should 404 when off", key)
		rec = doRequestAsUser(t, srv, admin, http.MethodPost, decisionRetentionOpPath(key), map[string]any{})
		assert.Equal(t, http.StatusNotFound, rec.Code, "POST %s should 404 when off", key)
	}

	// Turn experiment ON.
	ops := srv.GetOperationalSettings()
	_, err := ops.Update(context.Background(), "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.DecisionRetention)),
		"test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)

	// Listed for admin, 403 for non-admin.
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), entadapter.DecisionRetentionPreviewKey)
	assert.Contains(t, rec.Body.String(), entadapter.DecisionRetentionRunKey)

	for _, key := range []string{entadapter.DecisionRetentionPreviewKey, entadapter.DecisionRetentionRunKey} {
		rec = doRequestAsUser(t, srv, member, http.MethodPost, decisionRetentionOpPath(key), map[string]any{})
		assert.Equal(t, http.StatusForbidden, rec.Code, "non-admin POST %s must be 403", key)
	}
}

// TestDecisionRetentionOps_PreviewEqualsManualRunAndRespectsEnabled proves:
//  1. With enabled=false (legal hold), decision-retention-run is refused (409)
//     and deletes nothing, while decision-retention-preview reports exact counts
//     and deletes nothing.
//  2. With enabled=true and mixed per-project overrides (hub default 30d,
//     keep_forever, 60d override), decision-retention-preview writes a preview
//     run row (dry_run=true, actor) and deletes nothing, and
//     decision-retention-run writes a manual run row (dry_run=false, actor)
//     whose per-project counts equal the preview's.
func TestDecisionRetentionOps_PreviewEqualsManualRunAndRespectsEnabled(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	projDef := uuid.NewString()
	projLong := uuid.NewString()
	projHold := uuid.NewString()
	for i, pid := range []string{projDef, projLong, projHold} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("ops-p-%d", i), Slug: fmt.Sprintf("ops-p-%d", i), Created: time.Now().UTC(),
		}))
	}
	agentID := uuid.NewString()
	userID := admin.ID

	_, decDef45 := seedOldSettledDecision(t, ctx, cs, projDef, agentID, userID, "OP-DEF45", 45*24*time.Hour)
	_, decLong75 := seedOldSettledDecision(t, ctx, cs, projLong, agentID, userID, "OP-LONG75", 75*24*time.Hour)
	_, decLong45 := seedOldSettledDecision(t, ctx, cs, projLong, agentID, userID, "OP-LONG45", 45*24*time.Hour)
	_, decHold90 := seedOldSettledDecision(t, ctx, cs, projHold, agentID, userID, "OP-HOLD90", 90*24*time.Hour)

	// Save policy with enabled=false (legal hold), hub default 30d, projLong=60d, projHold=keep_forever.
	got := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled":        false,
		"retention_days": 30,
		"project_overrides": map[string]any{
			projLong: map[string]any{"mode": "days", "days": 60},
			projHold: map[string]any{"mode": "keep_forever"},
		},
		"expected_revision": 0,
	}))

	// 1. Manual run refused with 409 while enabled=false; direct call also refuses and deletes nothing.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, decisionRetentionOpPath(entadapter.DecisionRetentionRunKey), map[string]any{})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var buf bytes.Buffer
	_, _, directErr := srv.runDecisionRetentionManual(ctx, &buf, admin.ID)
	require.Error(t, directErr, "direct manual run must refuse while enabled=false")

	// 2. Preview works even while enabled=false, reports exact counts, writes preview run row, deletes nothing.
	runOp := func(key string) *store.MaintenanceOperationRun {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, decisionRetentionOpPath(key), map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var startResp struct {
			RunID string `json:"runId"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &startResp))
		require.NotEmpty(t, startResp.RunID)
		return waitMaintenanceRunDone(t, s, startResp.RunID)
	}

	prevMaint := runOp(entadapter.DecisionRetentionPreviewKey)
	require.Equal(t, store.MaintenanceStatusCompleted, prevMaint.Status, prevMaint.Log)

	runs, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{
		Trigger: store.DecisionRetentionTriggerPreview, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	previewRun := runs[0]
	assert.True(t, previewRun.DryRun)
	assert.Equal(t, admin.ID, previewRun.Actor)
	assert.Equal(t, store.DecisionRetentionStatusOK, previewRun.Status)
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: 1, Answers: 1}, previewRun.Counts[projDef])
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: 1, Answers: 1}, previewRun.Counts[projLong])
	assert.Zero(t, previewRun.Counts[projHold])

	// Nothing was deleted by preview.
	for _, id := range []string{decDef45, decLong75, decLong45, decHold90} {
		_, err := cs.GetDecision(ctx, id)
		require.NoError(t, err, "preview must not delete %s", id)
	}

	// 3. Enable retention and run manual purge.
	decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled":           true,
		"expected_revision": got.Revision,
	}))

	manualMaint := runOp(entadapter.DecisionRetentionRunKey)
	require.Equal(t, store.MaintenanceStatusCompleted, manualMaint.Status, manualMaint.Log)

	manualRuns, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{
		Trigger: store.DecisionRetentionTriggerManual, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, manualRuns, 1)
	manualRun := manualRuns[0]
	assert.False(t, manualRun.DryRun)
	assert.Equal(t, admin.ID, manualRun.Actor)
	assert.Equal(t, store.DecisionRetentionStatusOK, manualRun.Status)
	assert.Equal(t, previewRun.Counts, manualRun.Counts, "manual run counts must equal preview counts")

	// Verify exact kept vs purged rows.
	for _, gone := range []string{decDef45, decLong75} {
		_, err := cs.GetDecision(ctx, gone)
		assert.ErrorIs(t, err, store.ErrNotFound, "decision %s should be purged", gone)
	}
	for _, kept := range []string{decLong45, decHold90} {
		_, err := cs.GetDecision(ctx, kept)
		assert.NoError(t, err, "decision %s should be kept", kept)
	}

	// Verify GET /api/v1/admin/decision-retention returns both runs with counts.
	adminResp := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodGet, decisionRetentionAdminPath, nil))
	require.Len(t, adminResp.Runs, 2)
	assert.Equal(t, store.DecisionRetentionTriggerManual, adminResp.Runs[0].Trigger)
	assert.Equal(t, int64(2), adminResp.Runs[0].Totals.Decisions)
	assert.Equal(t, store.DecisionRetentionTriggerPreview, adminResp.Runs[1].Trigger)
	assert.True(t, adminResp.Runs[1].DryRun)
	assert.Equal(t, int64(2), adminResp.Runs[1].Totals.Decisions)
}

// TestDecisionRetention_DecisionsResponseRetentionField verifies the retention
// field on GET /api/v1/projects/{id}/decisions and GET /api/v1/users/me/decisions
// (design-8984 §3.5): absent when experiment is off, disabled, or no effective
// window; {active: true, days: N} per project when active; per-project map on
// profile responses with mixed projects.
func TestDecisionRetention_DecisionsResponseRetentionField(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	cs := s.(*entadapter.CompositeStore)
	ctx := context.Background()

	user, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "ret-resp-user", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	createProject := func(name, slug string) string {
		rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/projects", map[string]any{
			"name": name, "slug": slug,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var p struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		require.NotEmpty(t, p.ID)
		return p.ID
	}

	projDef := createProject("resp-p-0", "resp-p-0")
	projLong := createProject("resp-p-1", "resp-p-1")
	projHold := createProject("resp-p-2", "resp-p-2")
	// Non-member project (no membership binding for user) with a days override.
	projNonMember := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projNonMember, Name: "resp-non-member", Slug: "resp-non-member", Created: time.Now().UTC(),
	}))
	for i, pid := range []string{projDef, projLong, projHold, projNonMember} {
		// Seed one settled decision per project so ProjectsWithDecisions returns all member projects.
		seedOldSettledDecision(t, ctx, cs, pid, uuid.NewString(), user.ID, fmt.Sprintf("R%d", i), 5*24*time.Hour)
	}

	ops := srv.GetOperationalSettings()
	setExps := func(decRetOn bool) {
		doc := fmt.Sprintf(`{"overrides":{%q:true,%q:%t}}`, experiments.Decisions, experiments.DecisionRetention, decRetOn)
		_, err := ops.Update(ctx, "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
		require.NoError(t, err)
	}
	setRet := func(doc string) {
		_, err := ops.Update(ctx, opsettings.DecisionRetentionSectionName, json.RawMessage(doc), "test", ops.SectionRevision(opsettings.DecisionRetentionSectionName), "managed")
		require.NoError(t, err)
	}

	getJSON := func(path string) map[string]any {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", path, rec.Body.String())
		var m map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
		return m
	}

	// 1. hub.decision_retention OFF (even if settings are enabled + 30d) -> retention absent.
	setExps(false)
	setRet(`{"enabled":true,"retention_days":30}`)
	assert.Nil(t, getJSON("/api/v1/projects/" + projDef + "/decisions?tab=made")["retention"])
	assert.Nil(t, getJSON("/api/v1/users/me/decisions?tab=made&sort=recent")["retention"])
	assert.Nil(t, getJSON("/api/v1/users/me/decisions?tab=made&sort=project")["retention"])

	// 2. hub.decision_retention ON, enabled=false -> retention absent.
	setExps(true)
	setRet(`{"enabled":false,"retention_days":30}`)
	assert.Nil(t, getJSON("/api/v1/projects/" + projDef + "/decisions?tab=made")["retention"])
	assert.Nil(t, getJSON("/api/v1/users/me/decisions?tab=made&sort=recent")["retention"])

	// 3. hub.decision_retention ON, enabled=true, no window -> retention absent.
	setRet(`{"enabled":true}`)
	assert.Nil(t, getJSON("/api/v1/projects/" + projDef + "/decisions?tab=made")["retention"])
	assert.Nil(t, getJSON("/api/v1/users/me/decisions?tab=made&sort=recent")["retention"])

	// 4. hub.decision_retention ON, enabled=true, hub default 30d, projLong=60d, projHold=keep_forever, projNonMember=45d.
	setRet(fmt.Sprintf(`{
		"enabled": true,
		"retention_days": 30,
		"project_overrides": {
			%q: {"mode": "days", "days": 60},
			%q: {"mode": "keep_forever"},
			%q: {"mode": "days", "days": 45}
		}
	}`, projLong, projHold, projNonMember))

	// Project endpoints:
	defRet, ok := getJSON("/api/v1/projects/" + projDef + "/decisions?tab=made")["retention"].(map[string]any)
	require.True(t, ok, "projDef must have retention object")
	assert.Equal(t, true, defRet["active"])
	assert.Equal(t, float64(30), defRet["days"])

	longRet, ok := getJSON("/api/v1/projects/" + projLong + "/decisions?tab=made")["retention"].(map[string]any)
	require.True(t, ok, "projLong must have retention object")
	assert.Equal(t, true, longRet["active"])
	assert.Equal(t, float64(60), longRet["days"])

	assert.Nil(t, getJSON("/api/v1/projects/" + projHold + "/decisions?tab=made")["retention"], "keep_forever project must omit retention")

	// Profile Made endpoints (both sort=recent and sort=project):
	for _, sortMode := range []string{"recent", "project"} {
		profRet, ok := getJSON("/api/v1/users/me/decisions?tab=made&sort=" + sortMode)["retention"].(map[string]any)
		require.True(t, ok, "profile sort=%s must have per-project retention map", sortMode)
		assert.Len(t, profRet, 2, "only member projects with active windows appear")
		assert.Equal(t, map[string]any{"active": true, "days": float64(30)}, profRet[projDef])
		assert.Equal(t, map[string]any{"active": true, "days": float64(60)}, profRet[projLong])
		assert.Nil(t, profRet[projHold], "keep_forever project must be omitted from profile retention map")
		assert.Nil(t, profRet[projNonMember], "non-member project must not appear in profile retention map")
	}

	// Profile Made filtered to keep_forever or non-member project -> retention absent.
	assert.Nil(t, getJSON("/api/v1/users/me/decisions?tab=made&project=" + projHold)["retention"])
	assert.Nil(t, getJSON("/api/v1/users/me/decisions?tab=made&project=" + projNonMember)["retention"],
		"filtering /users/me/decisions to a non-member project must yield no retention entry")
}

// TestDecisionRetentionOps_ManualRunMidRunHoldAndPerPageAudit verifies that
// runDecisionRetentionManual wires per-page policy re-reads, persists the
// running manual run row after page 1 before page 2 begins, and stops
// immediately when a legal hold (enabled=false) is set mid-run.
func TestDecisionRetentionOps_ManualRunMidRunHoldAndPerPageAudit(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "man-hold", Slug: "man-hold", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	const total = 60 // page_size=50 -> page 1 purges 50, page 2 would purge 10
	decIDs := make([]string, total)
	for i := 0; i < total; i++ {
		_, decIDs[i] = seedOldSettledDecision(t, ctx, cs, projID, agentID, admin.ID, fmt.Sprintf("MH%02d", i), 60*24*time.Hour)
	}

	ops := srv.GetOperationalSettings()
	_, err := ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30,"sweep":{"page_size":50,"tick_interval_minutes":1}}`),
		"test", 0, "managed")
	require.NoError(t, err)

	sleepCalls := 0
	srv.decisionRetention.sleepHook = func(ctx context.Context, _ time.Duration) error {
		sleepCalls++
		// Verify running manual run row is already persisted with page 1 counts.
		midRuns, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{
			Trigger: store.DecisionRetentionTriggerManual, Limit: 10,
		})
		require.NoError(t, err)
		require.Len(t, midRuns, 1)
		assert.Equal(t, store.DecisionRetentionStatusRunning, midRuns[0].Status)
		assert.Nil(t, midRuns[0].FinishedAt)
		assert.Equal(t, admin.ID, midRuns[0].Actor)
		assert.Equal(t, int64(50), midRuns[0].Counts[projID].Decisions)
		assert.Equal(t, int64(50), midRuns[0].Counts[projID].Answers)

		// Flip enabled=false mid-run (legal hold).
		_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
			json.RawMessage(`{"enabled":false,"retention_days":30,"sweep":{"page_size":50,"tick_interval_minutes":1}}`),
			"test", ops.SectionRevision(opsettings.DecisionRetentionSectionName), "managed")
		require.NoError(t, err)
		return nil
	}

	var logBuf bytes.Buffer
	run, res, err := srv.runDecisionRetentionManual(ctx, &logBuf, admin.ID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, 1, sleepCalls)
	assert.Equal(t, 1, res.Pages, "mid-run hold must stop manual run before page 2")
	assert.Equal(t, 50, res.PurgedMessages)
	assert.False(t, res.ReachedCutoff)

	remaining := 0
	for _, id := range decIDs {
		if _, err := cs.GetDecision(ctx, id); err == nil {
			remaining++
		}
	}
	assert.Equal(t, 10, remaining, "page 2 rows must remain untouched after mid-run hold")

	finalRuns, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{
		Trigger: store.DecisionRetentionTriggerManual, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, finalRuns, 1)
	assert.Equal(t, store.DecisionRetentionStatusPartial, finalRuns[0].Status)
	assert.NotNil(t, finalRuns[0].FinishedAt)
	assert.Equal(t, int64(50), finalRuns[0].Counts[projID].Decisions)
}

// TestDecisionRetentionOps_ReconcileInterruptedManualAndPreviewRuns verifies:
//  1. Startup reconciliation marks interrupted running manual and preview rows
//     started before server startTime as failed (preserving committed counts),
//     leaves rows started after startTime running, and is idempotent.
//  2. Running preview and manual ops does not overwrite or reset the sweep's
//     persisted (created_at, id, ReachedCutoff) cursor.
func TestDecisionRetentionOps_ReconcileInterruptedManualAndPreviewRuns(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)
	cs := s.(*entadapter.CompositeStore)
	st := cs.DecisionRetention()
	srv.SetDecisionRetentionStore(st)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	srv.mu.Lock()
	srv.startTime = now
	srv.mu.Unlock()

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "reconcile-ops", Slug: "reconcile-ops", Created: now,
	}))

	// Seed interrupted running manual & preview rows (before startTime) plus a live preview (after startTime).
	intManual := &store.DecisionRetentionRun{
		Trigger:   store.DecisionRetentionTriggerManual,
		DryRun:    false,
		StartedAt: now.Add(-10 * time.Minute),
		Status:    store.DecisionRetentionStatusRunning,
		Actor:     admin.ID,
		Policy:    store.DecisionRetentionPolicy{RetentionDays: 30, Cutoff: now.Add(-30 * 24 * time.Hour)},
		Counts:    map[string]store.DecisionRetentionCounts{projID: {Decisions: 12, Answers: 12}},
	}
	intPreview := &store.DecisionRetentionRun{
		Trigger:   store.DecisionRetentionTriggerPreview,
		DryRun:    true,
		StartedAt: now.Add(-5 * time.Minute),
		Status:    store.DecisionRetentionStatusRunning,
		Actor:     admin.ID,
		Policy:    store.DecisionRetentionPolicy{RetentionDays: 30, Cutoff: now.Add(-30 * 24 * time.Hour)},
		Counts:    map[string]store.DecisionRetentionCounts{},
	}
	livePreview := &store.DecisionRetentionRun{
		Trigger:   store.DecisionRetentionTriggerPreview,
		DryRun:    true,
		StartedAt: now.Add(time.Minute),
		Status:    store.DecisionRetentionStatusRunning,
		Actor:     admin.ID,
		Policy:    store.DecisionRetentionPolicy{RetentionDays: 30, Cutoff: now.Add(-30 * 24 * time.Hour)},
		Counts:    map[string]store.DecisionRetentionCounts{},
	}
	for _, r := range []*store.DecisionRetentionRun{intManual, intPreview, livePreview} {
		require.NoError(t, st.SaveDecisionRetentionRun(ctx, r))
	}

	assert.Equal(t, 2, srv.ReconcileInterruptedDecisionRetentionRuns(ctx))
	assert.Equal(t, 0, srv.ReconcileInterruptedDecisionRetentionRuns(ctx), "reconcile must be idempotent")

	runs, err := st.ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
	require.NoError(t, err)
	byID := make(map[string]store.DecisionRetentionRun, len(runs))
	for _, r := range runs {
		byID[r.ID] = r
	}
	assert.Equal(t, store.DecisionRetentionStatusFailed, byID[intManual.ID].Status)
	assert.Equal(t, decisionRetentionInterruptedError, byID[intManual.ID].Error)
	assert.Equal(t, int64(12), byID[intManual.ID].Counts[projID].Decisions, "manual committed counts preserved")
	assert.Equal(t, store.DecisionRetentionStatusFailed, byID[intPreview.ID].Status)
	assert.Equal(t, decisionRetentionInterruptedError, byID[intPreview.ID].Error)
	assert.Equal(t, store.DecisionRetentionStatusRunning, byID[livePreview.ID].Status)

	// Verify preview and manual run preserve an existing sweep cursor (including ReachedCutoff=false).
	resetAt := now.Add(-2 * time.Hour)
	savedCursor := store.DecisionRetentionCursor{
		Position:      store.DecisionRetentionPosition{CreatedAt: now.Add(-45 * 24 * time.Hour), ID: uuid.NewString()},
		ResetAt:       &resetAt,
		ReachedCutoff: false,
	}
	require.NoError(t, st.SaveDecisionRetentionCursor(ctx, savedCursor))

	ops := srv.GetOperationalSettings()
	_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30}`), "test", 0, "managed")
	require.NoError(t, err)

	var buf bytes.Buffer
	_, err = srv.runDecisionRetentionPreview(ctx, &buf, admin.ID)
	require.NoError(t, err)
	_, _, err = srv.runDecisionRetentionManual(ctx, &buf, admin.ID)
	require.NoError(t, err)

	gotCur, err := st.GetDecisionRetentionCursor(ctx)
	require.NoError(t, err)
	assert.Equal(t, savedCursor.Position.ID, gotCur.Position.ID, "preview/manual run must not clobber sweep cursor")
	assert.False(t, gotCur.ReachedCutoff, "preview/manual run must not alter sweep ReachedCutoff")
}

// TestDecisionRetentionOps_PreviewCrossPageDedup verifies that Preview's
// cross-page seenMsg deduplication counts a multi-decision prompt straddling
// a page boundary (positions page_size and page_size+1) exactly once, matching
// a manual run.
func TestDecisionRetentionOps_PreviewCrossPageDedup(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	tBase := now.Add(-60 * 24 * time.Hour)

	projFill := uuid.NewString()
	projStraddle := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projFill, Name: "dedup-fill", Slug: "dedup-fill", Created: now}))
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projStraddle, Name: "dedup-straddle", Slug: "dedup-straddle", Created: now}))

	db := cs.DB()
	const fillCount = 49 // page_size=50 -> rows 1..49 in projFill; straddle prompt rows sit at 50 and 51
	for i := 0; i < fillCount; i++ {
		agentID := uuid.NewString()
		_, decID := seedOldSettledDecision(t, ctx, cs, projFill, agentID, admin.ID, fmt.Sprintf("F%02d", i), 60*24*time.Hour)
		ts := tBase.Add(time.Duration(i) * time.Second)
		uid := uuid.MustParse(decID)
		_, err := db.ExecContext(ctx, "UPDATE decisions SET created_at = ?, decided_at = ? WHERE id = ?", ts, ts, uid)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?", ts, uid)
		require.NoError(t, err)
	}

	// Seed a 2-decision settled prompt whose rows sit at positions 50 and 51.
	straddleAgent := uuid.NewString()
	msgID := uuid.NewString()
	promptRes, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: msgID, ProjectID: projStraddle, ThreadID: "topic-straddle",
			Sender: "agent:straddle", SenderID: straddleAgent, Recipient: "user:" + admin.ID, RecipientID: admin.ID,
		},
		AskerAgentID:   straddleAgent,
		AskerAgentName: "straddle",
		Decisions: []messages.Decision{
			{Key: "S1", Question: "Straddle 1", Choices: []messages.Choice{{ID: "1", Label: "Yes"}}},
			{Key: "S2", Question: "Straddle 2", Choices: []messages.Choice{{ID: "1", Label: "Yes"}}},
		},
	})
	require.NoError(t, err)
	require.Len(t, promptRes.Decisions, 2)

	tStraddle := tBase.Add(fillCount * time.Second)
	for _, d := range promptRes.Decisions {
		_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID: d.RowID, ChoiceID: "1",
			AnsweredByUserID: admin.ID, AnsweredByName: "Admin", AnsweredByEmail: "admin@test.com",
			Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: projStraddle, ThreadID: "topic-straddle",
				Sender: "user:" + admin.ID, SenderID: admin.ID, Recipient: "agent:straddle", RecipientID: straddleAgent,
			},
		})
		require.NoError(t, err)
		uid := uuid.MustParse(d.RowID)
		_, err = db.ExecContext(ctx, "UPDATE decisions SET created_at = ?, decided_at = ? WHERE id = ?", tStraddle, tStraddle, uid)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?", tStraddle, uid)
		require.NoError(t, err)
	}

	ops := srv.GetOperationalSettings()
	_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30,"sweep":{"page_size":50,"tick_interval_minutes":1}}`),
		"test", 0, "managed")
	require.NoError(t, err)

	var previewLog bytes.Buffer
	previewRun, err := srv.runDecisionRetentionPreview(ctx, &previewLog, admin.ID)
	require.NoError(t, err)
	require.Equal(t, store.DecisionRetentionStatusOK, previewRun.Status)
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: 2, Answers: 2}, previewRun.Counts[projStraddle],
		"straddling 2-decision prompt must be counted once across pages")
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: fillCount, Answers: fillCount}, previewRun.Counts[projFill])

	var manualLog bytes.Buffer
	manualRun, _, err := srv.runDecisionRetentionManual(ctx, &manualLog, admin.ID)
	require.NoError(t, err)
	require.Equal(t, store.DecisionRetentionStatusOK, manualRun.Status)
	assert.Equal(t, previewRun.Counts, manualRun.Counts, "preview ByProject must equal manual-run counts")
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: 2, Answers: 2}, manualRun.Counts[projStraddle])
}

// TestDecisionRetentionOps_PreviewBoundedDedupAcrossPages verifies L2
// (ptone/scion#9439): Preview bounds its dedup map to the current created_at
// timestamp group while still producing identical counts to a manual run across
// 3+ pages, including:
//  1. a 2-decision prompt straddling page 1 -> page 2 (positions 50 and 51);
//  2. two distinct 2-decision prompts sharing the exact same created_at
//     timestamp straddling page 2 -> page 3 (positions 99..102);
//  3. MaxDedupEntries staying <= 2 (not growing with the 120+ scanned prompts).
func TestDecisionRetentionOps_PreviewBoundedDedupAcrossPages(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)
	cs := s.(*entadapter.CompositeStore)
	st := cs.DecisionRetention()
	srv.SetDecisionRetentionStore(st)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	tBase := now.Add(-60 * 24 * time.Hour)

	projFill := uuid.NewString()
	projStraddle := uuid.NewString()
	projSameTS := uuid.NewString()
	for i, pid := range []string{projFill, projStraddle, projSameTS} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("bdedup-%d", i), Slug: fmt.Sprintf("bdedup-%d", i), Created: now,
		}))
	}

	db := cs.DB()
	setTimes := func(decID string, ts time.Time) {
		uid := uuid.MustParse(decID)
		_, err := db.ExecContext(ctx, "UPDATE decisions SET created_at = ?, decided_at = ? WHERE id = ?", ts, ts, uid)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?", ts, uid)
		require.NoError(t, err)
	}
	seedMulti := func(pid, prefix string, ts time.Time) {
		aid := uuid.NewString()
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, ThreadID: "topic-" + prefix,
				Sender: "agent:" + prefix, SenderID: aid, Recipient: "user:" + admin.ID, RecipientID: admin.ID,
			},
			AskerAgentID:   aid,
			AskerAgentName: prefix,
			Decisions: []messages.Decision{
				{Key: prefix + "1", Question: prefix + " Q1", Choices: []messages.Choice{{ID: "1", Label: "Yes"}}},
				{Key: prefix + "2", Question: prefix + " Q2", Choices: []messages.Choice{{ID: "1", Label: "Yes"}}},
			},
		})
		require.NoError(t, err)
		for _, d := range res.Decisions {
			_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
				DecisionRowID: d.RowID, ChoiceID: "1",
				AnsweredByUserID: admin.ID, AnsweredByName: "Admin", AnsweredByEmail: "admin@test.com",
				Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
				ResponseMessage: &store.Message{
					ID: uuid.NewString(), ProjectID: pid, ThreadID: "topic-" + prefix,
					Sender: "user:" + admin.ID, SenderID: admin.ID, Recipient: "agent:" + prefix, RecipientID: aid,
				},
			})
			require.NoError(t, err)
			setTimes(d.RowID, ts)
		}
	}

	// Rows 1..49: distinct timestamps in projFill.
	for i := 0; i < 49; i++ {
		_, decID := seedOldSettledDecision(t, ctx, cs, projFill, uuid.NewString(), admin.ID, fmt.Sprintf("BF1_%02d", i), 60*24*time.Hour)
		setTimes(decID, tBase.Add(time.Duration(i)*time.Second))
	}
	// Rows 50..51: 2-decision prompt straddling page 1 -> page 2.
	seedMulti(projStraddle, "BS1", tBase.Add(49*time.Second))

	// Rows 52..98 (47 rows): distinct timestamps in projFill.
	for i := 0; i < 47; i++ {
		_, decID := seedOldSettledDecision(t, ctx, cs, projFill, uuid.NewString(), admin.ID, fmt.Sprintf("BF2_%02d", i), 60*24*time.Hour)
		setTimes(decID, tBase.Add(time.Duration(50+i)*time.Second))
	}
	// Rows 99..102: two 2-decision prompts sharing the EXACT same created_at
	// timestamp straddling page 2 -> page 3.
	tSame := tBase.Add(100 * time.Second)
	seedMulti(projSameTS, "BT1", tSame)
	seedMulti(projSameTS, "BT2", tSame)

	// Rows 103..122 (20 rows): distinct timestamps in projFill on page 3.
	for i := 0; i < 20; i++ {
		_, decID := seedOldSettledDecision(t, ctx, cs, projFill, uuid.NewString(), admin.ID, fmt.Sprintf("BF3_%02d", i), 60*24*time.Hour)
		setTimes(decID, tBase.Add(time.Duration(101+i)*time.Second))
	}

	const totalFill = 49 + 47 + 20

	ops := srv.GetOperationalSettings()
	_, err := ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30,"sweep":{"page_size":50,"tick_interval_minutes":1}}`),
		"test", 0, "managed")
	require.NoError(t, err)

	p, err := srv.decisionRetentionPolicyAt(ctx, now)
	require.NoError(t, err)
	sw := newDecisionRetentionSweeper(st, true)
	prevRes, err := sw.Preview(ctx, p, now.Add(time.Hour))
	require.NoError(t, err)
	assert.True(t, prevRes.ReachedCutoff)
	assert.GreaterOrEqual(t, prevRes.Pages, 3, "walk must span at least 3 pages")
	assert.LessOrEqual(t, prevRes.MaxDedupEntries, 2,
		"bounded dedup map must stay bounded by per-timestamp group size, not total messages (%d)", totalFill+3)
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: totalFill, Answers: totalFill}, prevRes.ByProject[projFill])
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: 2, Answers: 2}, prevRes.ByProject[projStraddle])
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: 4, Answers: 4}, prevRes.ByProject[projSameTS])

	// Confirm Preview output matches a real manual run exactly.
	var manualLog bytes.Buffer
	manualRun, _, err := srv.runDecisionRetentionManual(ctx, &manualLog, admin.ID)
	require.NoError(t, err)
	require.Equal(t, store.DecisionRetentionStatusOK, manualRun.Status)
	assert.Equal(t, prevRes.ByProject, manualRun.Counts, "bounded-dedup preview counts must equal manual-run counts")
}

type faultyProjectSummariesStore struct {
	store.Store
	fault *storeFaultSwitch
	err   error
}

func (f *faultyProjectSummariesStore) ListProjectSummaries(ctx context.Context, filter store.ProjectFilter, opts store.ListOptions) (*store.ListResult[store.Project], error) {
	if !f.fault.Active() {
		return f.Store.ListProjectSummaries(ctx, filter, opts)
	}
	return nil, f.err
}

// TestDecisionRetentionOps_ManualRunValidateStoreError500 verifies that a store
// failure during ValidateParams (e.g. ListProjectSummaries) returns HTTP 500
// internal_error with a generic message, creates no run rows, and deletes nothing.
func TestDecisionRetentionOps_ManualRunValidateStoreError500(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *faultyProjectSummariesStore {
		return &faultyProjectSummariesStore{
			Store: inner,
			fault: f,
			err:   errors.New("injected store fault: secret details"),
		}
	})
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "fault-proj", Slug: "fault-proj", Created: time.Now().UTC(),
	}))
	_, decID := seedOldSettledDecision(t, ctx, cs, projID, uuid.NewString(), admin.ID, "FLT1", 60*24*time.Hour)

	ops := srv.GetOperationalSettings()
	_, err := ops.Update(ctx, opsettings.DecisionRetentionSectionName, json.RawMessage(fmt.Sprintf(`{
		"enabled": true,
		"retention_days": 30,
		"project_overrides": {%q: {"mode": "days", "days": 45}}
	}`, projID)), "test", 0, "managed")
	require.NoError(t, err)

	// Arm the ListProjectSummaries fault.
	fault.Arm()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, decisionRetentionOpPath(entadapter.DecisionRetentionRunKey), map[string]any{})
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeInternalError)
	assert.NotContains(t, rec.Body.String(), "secret details", "internal store error text must not leak to client")

	// No maintenance run row, no decision retention run row, and no deletions.
	maintRuns, err := cs.ListMaintenanceRuns(ctx, entadapter.DecisionRetentionRunKey, 10)
	require.NoError(t, err)
	assert.Empty(t, maintRuns)

	retRuns, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, retRuns)

	_, err = cs.GetDecision(ctx, decID)
	require.NoError(t, err, "seeded decision must survive ValidateParams store fault")
}
