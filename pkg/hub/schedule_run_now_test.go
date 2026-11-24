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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requestAsSession(t *testing.T, srv *Server, identity Identity, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive})
	if user, ok := identity.(UserIdentity); ok {
		ctx = context.WithValue(ctx, userContextKey{}, user)
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func TestScheduleRunNow_MessageRecordsManualAndPreservesCron(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	// Create a recurring message schedule.
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
		Name:      "daily-ping",
		CronExpr:  "0 9 * * *",
		EventType: "message",
		AgentName: "all",
		Message:   "hello from schedule",
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	var created store.Schedule
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))
	require.NotNil(t, created.NextRunAt)
	origNextRun := created.NextRunAt.UTC()

	// Trigger "Run now".
	runRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/run", nil)
	require.Equal(t, http.StatusOK, runRec.Code, runRec.Body.String())

	var evtResp ScheduledEventResponse
	require.NoError(t, json.NewDecoder(runRec.Body).Decode(&evtResp))
	assert.NotEmpty(t, evtResp.ID)
	assert.Equal(t, created.ID, evtResp.ScheduleID)
	assert.True(t, evtResp.Manual, "manual run must be marked manual=true")
	assert.Equal(t, "message", evtResp.EventType)

	// Schedule's NextRunAt and Status must remain unchanged; RunCount incremented.
	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, after.NextRunAt)
	assert.True(t, after.NextRunAt.UTC().Equal(origNextRun), "NextRunAt must not change on manual run: got %v want %v", after.NextRunAt, origNextRun)
	assert.Equal(t, store.ScheduleStatusActive, after.Status)
	assert.Equal(t, 1, after.RunCount)

	// History lists the manual run with manual=true.
	histRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/history", nil)
	require.Equal(t, http.StatusOK, histRec.Code, histRec.Body.String())
	var hist ListScheduledEventsResponse
	require.NoError(t, json.NewDecoder(histRec.Body).Decode(&hist))
	require.Len(t, hist.Events, 1)
	assert.Equal(t, evtResp.ID, hist.Events[0].ID)
	assert.True(t, hist.Events[0].Manual)

	// Method check: GET /run is 405.
	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/run", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, getRec.Code)
}

func TestScheduleRunNow_PausedScheduleRunsWithoutResuming(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
		Name:      "paused-ping",
		CronExpr:  "0 9 * * *",
		EventType: "message",
		AgentName: "all",
		Message:   "manual check while paused",
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	pauseRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	pausedBefore, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, store.ScheduleStatusPaused, pausedBefore.Status)

	runRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/run", nil)
	require.Equal(t, http.StatusOK, runRec.Code, runRec.Body.String())

	var evtResp ScheduledEventResponse
	require.NoError(t, json.NewDecoder(runRec.Body).Decode(&evtResp))
	assert.True(t, evtResp.Manual)

	// Schedule remains paused with unchanged NextRunAt.
	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, after.Status)
	assert.Equal(t, 1, after.RunCount)
	if pausedBefore.NextRunAt != nil {
		require.NotNil(t, after.NextRunAt)
		assert.True(t, after.NextRunAt.UTC().Equal(pausedBefore.NextRunAt.UTC()))
	}
}

func TestScheduleRunNow_AuthzAndCrossProjectIsolation(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	// Create a second project to verify cross-project isolation.
	otherProj := &store.Project{
		ID:   uuid.NewString(),
		Name: "other-project",
		Slug: "other-project",
	}
	require.NoError(t, s.CreateProject(ctx, otherProj))

	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
		Name:      "authz-check",
		CronExpr:  "0 9 * * *",
		EventType: "message",
		AgentName: "all",
		Message:   "ping",
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var sched store.Schedule
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&sched))

	// 1. Cross-project access must fail with 404 (schedule does not belong to otherProj).
	crossRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+otherProj.ID+"/schedules/"+sched.ID+"/run", nil)
	assert.Equal(t, http.StatusNotFound, crossRec.Code, crossRec.Body.String())

	// 2. Read-only user (scheduled_event.read, no scheduled_event.update) -> 403.
	reader := setupNonAdminUser(t, s, []string{"scheduled_event.read", "scheduled_event.list", "project.read"})
	readerRec := requestAsIdentity(t, srv, reader, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+sched.ID+"/run", nil)
	assert.Equal(t, http.StatusForbidden, readerRec.Code, readerRec.Body.String())

	// 3. Creator who lost scheduled_event.update MUST NOT get the creatorStop exemption (403).
	creatorID := uuid.NewString()
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          creatorID,
		Email:       "creator-no-update@test.local",
		DisplayName: "Creator No Update",
		Role:        store.UserRoleMember,
		Status:      "active",
	}))
	bindPermsToUser(t, s, creatorID, []string{"scheduled_event.create", "scheduled_event.read", "project.read", "agent.read", "agent.message"})
	creator := NewAuthenticatedUser(creatorID, "creator-no-update@test.local", "Creator No Update", store.UserRoleMember, "api")
	// Create a schedule owned by creator.
	creatorSched := store.Schedule{
		ID:                   uuid.NewString(),
		ProjectID:            projectID,
		Name:                 "creator-no-update",
		CronExpr:             "0 9 * * *",
		EventType:            "message",
		Payload:              `{"agentName":"all","message":"hi"}`,
		Status:               store.ScheduleStatusActive,
		CreatedBy:            creator.ID(),
		InitiatorAttribution: store.InitiatorAttribution{InitiatorPrincipalKind: "user", InitiatorPrincipalID: creator.ID(), InitiatorCredentialKind: "session", AttributionVersion: 1, AuthorizationRevision: 1},
	}
	require.NoError(t, s.CreateSchedule(ctx, &creatorSched))

	// Creator CAN pause via creatorStop, but CANNOT run now without scheduled_event.update.
	creatorRunRec := requestAsSession(t, srv, creator, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+creatorSched.ID+"/run", nil)
	assert.Equal(t, http.StatusForbidden, creatorRunRec.Code, creatorRunRec.Body.String())
}

func TestScheduleRunNow_ExecCaptureOutputAndAuthz(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	ctx := context.Background()

	// Create an exec schedule with captureOutput=tail whose runtime output
	// ("hello-run-now-output") does not appear literally in argv, so we can
	// assert that /run and /scheduled-events/{id} never leak captured output
	// even to an attach-holding caller.
	p := scheduledExecPayload{
		AgentID:        f.launched.ID,
		AgentName:      f.launched.Slug,
		Command:        []string{"sh", "-c", "printf '%s-%s' 'hello-run-now' 'output'"},
		TimeoutSeconds: 10,
		CaptureOutput:  agentcontrol.ExecCaptureTail,
	}
	raw, err := p.encode()
	require.NoError(t, err)
	futureNext := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	sc := withScheduleSessionRevision(store.Schedule{
		ID:        uuid.NewString(),
		ProjectID: f.launched.ProjectID,
		Name:      "exec-capture-run-now",
		CronExpr:  "0 */6 * * *",
		EventType: scheduledEventTypeExec,
		Payload:   raw,
		Status:    store.ScheduleStatusActive,
		NextRunAt: &futureNext,
		CreatedBy: f.launched.OwnerID,
	}, f.launched.OwnerID)
	require.NoError(t, f.store.CreateSchedule(ctx, &sc))

	owner := f.owner
	bindPermsToUser(t, f.store, owner.ID(), []string{"scheduled_event.read", "scheduled_event.update"})

	// 1. Project admin WITHOUT agent.attach on f.launched MUST be refused (403) — no confused deputy.
	noAttachAdmin := setupNonAdminUser(t, f.store, []string{
		"scheduled_event.read", "scheduled_event.update", "agent.read", "project.read",
	})
	deniedRec := requestAsIdentity(t, f.srv, noAttachAdmin, http.MethodPost,
		"/api/v1/projects/"+f.launched.ProjectID+"/schedules/"+sc.ID+"/run", nil)
	assert.Equal(t, http.StatusForbidden, deniedRec.Code, deniedRec.Body.String())

	// 2. Owner WITH agent.attach triggers Run now -> 202 Accepted, manual=true.
	runRec := requestAsSession(t, f.srv, owner, http.MethodPost,
		"/api/v1/projects/"+f.launched.ProjectID+"/schedules/"+sc.ID+"/run", nil)
	require.Equal(t, http.StatusAccepted, runRec.Code, runRec.Body.String())

	var evtResp ScheduledEventResponse
	require.NoError(t, json.NewDecoder(runRec.Body).Decode(&evtResp))
	assert.True(t, evtResp.Manual)
	assert.Equal(t, sc.ID, evtResp.ScheduleID)
	// Output must never be embedded in /run response.
	assert.NotContains(t, runRec.Body.String(), "hello-run-now-output")

	// Wait for the exec run to finish and record its outcome.
	gotSc := waitScheduleRunCount(t, f.store, sc.ID, 1)
	assert.Equal(t, 1, gotSc.RunCount)
	assert.Equal(t, 0, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunSuccess, gotSc.LastRunStatus)
	require.NotNil(t, gotSc.NextRunAt)
	assert.True(t, gotSc.NextRunAt.UTC().Equal(futureNext), "cron NextRunAt must be unaffected by manual run")

	// Verify event GET includes execSummary with exitCode=0, captureMode=tail, outputStatus=captured.
	evtRec := requestAsIdentity(t, f.srv, owner, http.MethodGet,
		"/api/v1/projects/"+f.launched.ProjectID+"/scheduled-events/"+evtResp.ID, nil)
	require.Equal(t, http.StatusOK, evtRec.Code, evtRec.Body.String())
	assert.NotContains(t, evtRec.Body.String(), "hello-run-now-output")
	var finishedEvt ScheduledEventResponse
	require.NoError(t, json.NewDecoder(evtRec.Body).Decode(&finishedEvt))
	assert.True(t, finishedEvt.Manual)
	require.NotNil(t, finishedEvt.ExecSummary)
	assert.Equal(t, store.ScheduledExecOutcomeOK, finishedEvt.ExecSummary.Outcome)
	require.NotNil(t, finishedEvt.ExecSummary.ExitCode)
	assert.Equal(t, 0, *finishedEvt.ExecSummary.ExitCode)
	assert.Equal(t, agentcontrol.ExecCaptureTail, finishedEvt.ExecSummary.CaptureMode)
	assert.Equal(t, agentcontrol.ExecOutputCaptured, finishedEvt.ExecSummary.OutputStatus)

	// 3. Owner with agent.attach CAN fetch captured output from /output.
	outRec := requestAsIdentity(t, f.srv, owner, http.MethodGet,
		"/api/v1/projects/"+f.launched.ProjectID+"/scheduled-events/"+evtResp.ID+"/output", nil)
	require.Equal(t, http.StatusOK, outRec.Code, outRec.Body.String())
	assert.Equal(t, "no-store", outRec.Header().Get("Cache-Control"))
	var outResp ScheduledExecOutputResponse
	require.NoError(t, json.NewDecoder(outRec.Body).Decode(&outResp))
	assert.Equal(t, agentcontrol.ExecCaptureTail, outResp.CaptureMode)
	assert.Equal(t, agentcontrol.ExecOutputCaptured, outResp.OutputStatus)
	assert.Equal(t, "hello-run-now-output", outResp.Output)

	// 4. User without agent.attach gets 403 on /output even with scheduled_event.read.
	deniedOutRec := requestAsIdentity(t, f.srv, noAttachAdmin, http.MethodGet,
		"/api/v1/projects/"+f.launched.ProjectID+"/scheduled-events/"+evtResp.ID+"/output", nil)
	assert.Equal(t, http.StatusForbidden, deniedOutRec.Code, deniedOutRec.Body.String())
	assert.NotContains(t, deniedOutRec.Body.String(), "hello-run-now-output")

	// 5. Turning off hub.exec_schedules refuses Run now on exec schedule (400).
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: false})
	offRec := requestAsIdentity(t, f.srv, owner, http.MethodPost,
		"/api/v1/projects/"+f.launched.ProjectID+"/schedules/"+sc.ID+"/run", nil)
	assert.Equal(t, http.StatusBadRequest, offRec.Code, offRec.Body.String())
}

func TestScheduleRunNow_ExecWithoutCaptureHasNoOutput(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)

	futureNext := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	sc := f.createExecSchedule(t, "exec-no-capture", futureNext, 10, "sh", "-c", "printf 'discarded-output'; exit 3")

	owner := f.owner
	bindPermsToUser(t, f.store, owner.ID(), []string{"scheduled_event.read", "scheduled_event.update"})
	runRec := requestAsSession(t, f.srv, owner, http.MethodPost,
		"/api/v1/projects/"+f.launched.ProjectID+"/schedules/"+sc.ID+"/run", nil)
	require.Equal(t, http.StatusAccepted, runRec.Code, runRec.Body.String())

	var evtResp ScheduledEventResponse
	require.NoError(t, json.NewDecoder(runRec.Body).Decode(&evtResp))

	gotSc := waitScheduleRunCount(t, f.store, sc.ID, 1)
	assert.Equal(t, 1, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)

	// /output returns 404 when capture is off, even for attach holder.
	outRec := requestAsIdentity(t, f.srv, owner, http.MethodGet,
		"/api/v1/projects/"+f.launched.ProjectID+"/scheduled-events/"+evtResp.ID+"/output", nil)
	assert.Equal(t, http.StatusNotFound, outRec.Code, outRec.Body.String())
	assert.NotContains(t, outRec.Body.String(), "discarded-output")
}

func TestScheduleRunNow_OverlapWithCronAndConcurrentManual(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	ctx := context.Background()

	// 1. In-flight manual guard returns 409 Conflict when a manual run is already active.
	futureNext := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	sc := f.createExecSchedule(t, "exec-overlap", futureNext, 10, "sleep", "2")
	owner := f.owner
	bindPermsToUser(t, f.store, owner.ID(), []string{"scheduled_event.read", "scheduled_event.update"})

	require.True(t, f.srv.acquireManualScheduleRun(sc.ID))
	conflictRec := requestAsIdentity(t, f.srv, owner, http.MethodPost,
		"/api/v1/projects/"+f.launched.ProjectID+"/schedules/"+sc.ID+"/run", nil)
	assert.Equal(t, http.StatusConflict, conflictRec.Code, conflictRec.Body.String())
	f.srv.releaseManualScheduleRun(sc.ID)

	// 2. Manual run in progress in container + cron tick racing -> cron tick is skipped ("previous run in progress").
	runRec := requestAsSession(t, f.srv, owner, http.MethodPost,
		"/api/v1/projects/"+f.launched.ProjectID+"/schedules/"+sc.ID+"/run", nil)
	require.Equal(t, http.StatusAccepted, runRec.Code, runRec.Body.String())
	var manualEvt ScheduledEventResponse
	require.NoError(t, json.NewDecoder(runRec.Body).Decode(&manualEvt))

	// Wait until the manual run row is admitted in the DB (status starting/running).
	require.Eventually(t, func() bool {
		run, err := f.store.GetScheduledExecRun(ctx, manualEvt.ID)
		return err == nil && run != nil
	}, 5*time.Second, 20*time.Millisecond)

	// Fire a cron tick while the manual run is still running in the container.
	now := time.Now().UTC()
	scDue := sc
	dueAt := now.Add(-time.Second)
	scDue.NextRunAt = &dueAt
	f.srv.executeSchedule(ctx, scDue, now)

	// Both runs complete (runCount == 2): the cron tick was skipped due to overlap,
	// while the manual run completed ok (errorCount == 0).
	gotSc := waitScheduleRunCount(t, f.store, sc.ID, 2)
	assert.Equal(t, 2, gotSc.RunCount)
	assert.Equal(t, 0, gotSc.ErrorCount)

	res, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{
		ProjectID:  f.launched.ProjectID,
		ScheduleID: sc.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Items, 2)

	var foundManualFired, foundCronSkipped bool
	for _, ev := range res.Items {
		if ev.ID == manualEvt.ID {
			assert.True(t, ev.Manual)
			assert.Equal(t, store.ScheduledEventFired, ev.Status)
			foundManualFired = true
		} else {
			assert.False(t, ev.Manual)
			assert.Equal(t, store.ScheduledEventSkipped, ev.Status)
			assert.Equal(t, execReasonOverlap, ev.Error)
			foundCronSkipped = true
		}
	}
	assert.True(t, foundManualFired, "expected manual run to succeed")
	assert.True(t, foundCronSkipped, "expected overlapping cron tick to be skipped")
}
