//go:build !no_sqlite

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
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/rebuildpreflight"
	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func seedFailedRebuildRun(t *testing.T, s store.Store, id string, startedAt time.Time, result map[string]any) {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	finished := startedAt.Add(time.Second)
	if err := s.CreateMaintenanceRun(context.Background(), &store.MaintenanceOperationRun{
		ID:           id,
		OperationKey: "rebuild-server",
		Status:       store.MaintenanceStatusFailed,
		StartedAt:    startedAt,
		CompletedAt:  &finished,
		StartedBy:    "admin@example.com",
		Result:       string(b),
	}); err != nil {
		t.Fatalf("CreateMaintenanceRun(%s): %v", id, err)
	}
}

func TestExecuteOperation_ForceValidationAndCreateRerunOf(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	blockingResult := func() map[string]any {
		return map[string]any{
			"branch":         "main",
			"branchPinnable": true,
			"commit":         sha,
			"preflight": map[string]any{
				"version":  1,
				"checks":   []string{"settings.profiles"},
				"blocking": 1,
				"warnings": 0,
				"findings": []map[string]any{
					{
						"check":    "settings.profiles",
						"code":     "settings.profiles.missing",
						"severity": "block",
						"message":  "profile missing",
						"data":     map[string]any{"profile": "dev", "project": "p1"},
					},
				},
			},
		}
	}

	postForce := func(srv *Server, key, body string) *httptest.ResponseRecorder {
		admin := NewAuthenticatedUser("user-admin-99", "admin@example.com", "Admin", "admin", "cli")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/operations/"+key+"/run", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithIdentity(req.Context(), admin))
		rr := httptest.NewRecorder()
		srv.handleAdminMaintenanceOps(rr, req)
		return rr
	}

	t.Run("400 on malformed force request and unknown rerunOf", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		for _, tc := range []struct {
			key, body string
		}{
			{"rebuild-server", `{"params":{"force":true,"rerunOf":"r1"}}`},
			{"rebuild-server", `{"params":{"force":"yes","rerunOf":"r1"}}`},
			{"rebuild-server", `{"params":{"force":"true","rerunOf":"r1","branch":"main"}}`},
			{"rebuild-web", `{"params":{"force":"true","rerunOf":"r1"}}`},
			{"rebuild-server", `{"params":{"force":"true","rerunOf":"nonexistent-run"}}`},
		} {
			rr := postForce(srv, tc.key, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s %s: status = %d, want 400 (body=%s)", tc.key, tc.body, rr.Code, rr.Body.String())
			}
		}
	})

	t.Run("409 when a newer rebuild-server run exists (S1)", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		now := time.Now().UTC()
		oldID := api.NewUUID()
		newID := api.NewUUID()
		seedFailedRebuildRun(t, s, oldID, now.Add(-2*time.Minute), blockingResult())
		seedFailedRebuildRun(t, s, newID, now.Add(-1*time.Minute), blockingResult())

		rr := postForce(srv, "rebuild-server", `{"params":{"force":"true","rerunOf":"`+oldID+`"}}`)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body=%s)", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "newer run exists") {
			t.Errorf("body = %s, want newer run message", rr.Body.String())
		}
	})

	t.Run("409 when stored pin is invalid (R1)", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		bad := blockingResult()
		bad["branch"] = "-bad-branch"
		badID := api.NewUUID()
		seedFailedRebuildRun(t, s, badID, time.Now().UTC().Add(-time.Minute), bad)

		rr := postForce(srv, "rebuild-server", `{"params":{"force":"true","rerunOf":"`+badID+`"}}`)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body=%s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("CN5-c: accepted forced request writes rerunOf at run creation", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		targetID := api.NewUUID()
		seedFailedRebuildRun(t, s, targetID, time.Now().UTC().Add(-time.Minute), blockingResult())

		rr := postForce(srv, "rebuild-server", `{"params":{"force":"true","rerunOf":"`+targetID+`"}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		newRunID, _ := resp["runId"].(string)
		if newRunID == "" {
			t.Fatalf("missing runId in response: %s", rr.Body.String())
		}
		// Wait briefly for the background goroutine to finish (it will fail because no repo is configured).
		deadline := time.Now().Add(3 * time.Second)
		var stored *store.MaintenanceOperationRun
		for time.Now().Before(deadline) {
			r, err := s.GetMaintenanceRun(context.Background(), newRunID)
			if err == nil {
				stored = r
				if r.Status != store.MaintenanceStatusRunning {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
		}
		if stored == nil {
			t.Fatalf("created run %s not found", newRunID)
		}
		if !strings.Contains(stored.Result, `"rerunOf":"`+targetID+`"`) {
			t.Errorf("stored Result = %s, want rerunOf=%s", stored.Result, targetID)
		}
	})
}

func TestMaintenanceCheckpoint_GuardA_And_Write1b(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const sha = "0123456789abcdef0123456789abcdef01234567"

	failedID := api.NewUUID()
	forcedID := api.NewUUID()
	seedFailedRebuildRun(t, s, failedID, now.Add(-time.Minute), map[string]any{
		"branch": "main",
		"commit": sha,
	})
	forcedRun := &store.MaintenanceOperationRun{
		ID:           forcedID,
		OperationKey: "rebuild-server",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    now,
		StartedBy:    "admin@example.com",
		Result:       `{"rerunOf":"` + failedID + `"}`,
	}
	if err := s.CreateMaintenanceRun(ctx, forcedRun); err != nil {
		t.Fatal(err)
	}

	grant := &rebuildpreflight.ForceGrant{
		RerunOf:      failedID,
		ActorID:      "user-42",
		ActorEmail:   "admin@example.com",
		PinnedBranch: "main",
		PinnedCommit: sha,
		FailedFindings: []preflight.Finding{{
			Code: "settings.profiles.missing", Severity: preflight.SeverityBlock, Message: "m",
		}},
	}
	ov := rebuildpreflight.NewOverride(grant, rebuildpreflight.Outcome{
		Report: &preflight.Report{
			Blocking: 1,
			Findings: []preflight.Finding{{Code: "settings.profiles.missing", Severity: preflight.SeverityBlock, Message: "m"}},
		},
	}, now)

	buf := &runLogBuffer{}
	_, _ = buf.Write([]byte("PREFLIGHT OVERRIDDEN\n"))
	cp := srv.makeMaintenanceCheckpoint(forcedRun, buf, grant)

	err := cp(map[string]any{
		"branch":          "main",
		"commit":          sha,
		"rerunOf":         failedID,
		"forced":          true,
		"overrideApplied": false,
		"override":        ov,
	})
	if err != nil {
		t.Fatalf("checkpoint failed: %v", err)
	}

	// Verify Write 1b on forced run: overrideApplied == true.
	gotForced, err := s.GetMaintenanceRun(ctx, forcedID)
	if err != nil {
		t.Fatal(err)
	}
	var forcedMap map[string]any
	if err := json.Unmarshal([]byte(gotForced.Result), &forcedMap); err != nil {
		t.Fatal(err)
	}
	if forcedMap["overrideApplied"] != true {
		t.Errorf("forced run overrideApplied = %v, want true", forcedMap["overrideApplied"])
	}

	// Verify Write 2 (Guard A, R5): failed run has forcedBy == override + runId.
	gotFailed, err := s.GetMaintenanceRun(ctx, failedID)
	if err != nil {
		t.Fatal(err)
	}
	var failedMap map[string]any
	if err := json.Unmarshal([]byte(gotFailed.Result), &failedMap); err != nil {
		t.Fatal(err)
	}
	fb, ok := failedMap["forcedBy"].(map[string]any)
	if !ok {
		t.Fatalf("Guard A: failed run missing forcedBy: %s", gotFailed.Result)
	}
	if fb["runId"] != forcedID {
		t.Errorf("forcedBy.runId = %v, want %s", fb["runId"], forcedID)
	}
	delete(fb, "runId")
	if !reflect.DeepEqual(fb, forcedMap["override"]) {
		t.Errorf("R5: forcedBy minus runId != override:\nfb=%+v\nov=%+v", fb, forcedMap["override"])
	}

	// Re-running Write 2 on an already-forced failed run must fail ("was forced meanwhile").
	err = cp(map[string]any{
		"forced":          true,
		"overrideApplied": false,
		"override":        ov,
	})
	if err == nil || !strings.Contains(err.Error(), "forced meanwhile") {
		t.Errorf("second write-2 = %v, want 'forced meanwhile'", err)
	}
}

func TestOptionA_MarkRunRestartingAndFailStalePreserveResultKeys(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	run := &store.MaintenanceOperationRun{
		ID:           api.NewUUID(),
		OperationKey: "rebuild-server",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    now,
		StartedBy:    "admin@example.com",
		Result:       `{"rerunOf":"run-0","forced":true,"overrideApplied":true}`,
	}
	if err := s.CreateMaintenanceRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	err := srv.markRunRestartingWithResult(ctx, run, &runLogBuffer{}, maintenance.PendingRestart{
		From:     "v0.1.0",
		To:       "git-0123456789ab",
		Strategy: maintenance.StrategySymlink,
		Commit:   "0123456789abcdef0123456789abcdef01234567",
	}, map[string]any{
		"installed": true,
		"branch":    "main",
	})
	if err != nil {
		t.Fatalf("markRunRestartingWithResult: %v", err)
	}

	got, err := s.GetMaintenanceRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got.Result), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"rerunOf", "forced", "overrideApplied", "installed", "branch", "from", "to", "strategy", "commit"} {
		if _, ok := m[k]; !ok {
			t.Errorf("markRunRestartingWithResult dropped key %q (result=%s)", k, got.Result)
		}
	}

	// Now failStaleRestartingRuns must preserve all existing JSON-object keys.
	if err := srv.failStaleRestartingRuns(ctx); err != nil {
		t.Fatalf("failStaleRestartingRuns: %v", err)
	}
	got2, err := s.GetMaintenanceRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != store.MaintenanceStatusFailed {
		t.Fatalf("status = %q, want failed", got2.Status)
	}
	var m2 map[string]any
	if err := json.Unmarshal([]byte(got2.Result), &m2); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"rerunOf", "forced", "overrideApplied", "installed", "error"} {
		if _, ok := m2[k]; !ok {
			t.Errorf("failStaleRestartingRuns dropped key %q (result=%s)", k, got2.Result)
		}
	}
}

func TestLogAuditLogger_MaintenanceOverrideEvent_Authority(t *testing.T) {
	buf := captureAuditLogs(t)
	l := NewLogAuditLogger("[Test]", false)
	ev := &rebuildpreflight.MaintenanceOverrideEvent{
		EventType:  rebuildpreflight.MaintenanceOverrideEventType,
		ActorID:    "user-42",
		ActorEmail: "admin@example.com",
		Timestamp:  time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC),
		RunID:      "run-forced-2",
		RerunOf:    "run-failed-1",
		Operation:  rebuildpreflight.RebuildServerKey,
		Commit:     "0123456789abcdef0123456789abcdef01234567",
		Branch:     "main",
		Blocking:   2,
	}
	if err := l.LogMaintenanceOverrideEvent(context.Background(), ev); err != nil {
		t.Fatalf("LogMaintenanceOverrideEvent: %v", err)
	}
	rec := auditRecordWithMsg(t, buf, "maintenance preflight override audit event")
	if rec == nil {
		t.Fatalf("missing audit record in:\n%s", buf.String())
	}
	wantRecord, wantAuth := ev.Authority()
	if rec["record"] != wantRecord || rec["authoritative"] != wantAuth {
		t.Errorf("S6 authority = (%v, %v), want (%q, %q)", rec["record"], rec["authoritative"], wantRecord, wantAuth)
	}
	if rec["actor_id"] != "user-42" || rec["run_id"] != "run-forced-2" || rec["rerun_of"] != "run-failed-1" {
		t.Errorf("unexpected audit fields: %+v", rec)
	}
}
