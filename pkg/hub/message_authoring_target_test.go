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
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMessageAuthoringTargetRule_TriggerParity pins Stage 1 §16 criterion 17
// and rev 8.32 option (a) for the trigger caller:
//   - an unresolved target is refused with 404;
//   - an unreadable cross-project target answers byte-identically to a
//     nonexistent target (whether cross-project messaging is disabled or
//     enabled);
//   - a conflicting agentId + agentName returns the same 404;
//   - a readable cross-project target returns 403 when cross-project messaging
//     is disabled and succeeds when enabled.
func TestMessageAuthoringTargetRule_TriggerParity(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Create an active webhook endpoint in f.projectID owned by f.owner.
	epRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
		"/api/v1/projects/"+f.projectID+"/webhook-endpoints",
		CreateWebhookEndpointRequest{Name: "parity-hook"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var epResp struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &epResp))

	// Create an agent in f.otherProjID that f.member (who has no role in
	// f.otherProjID) cannot read and may not message.
	unreadableTarget := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "secret-cross-agent",
		Name:      "secret-cross-agent",
		ProjectID: f.otherProjID,
		Phase:     "running",
	}
	require.NoError(t, f.store.CreateAgent(ctx, unreadableTarget))

	nonexistentID := api.NewUUID()

	createTrigger := func(caller *store.User, name string, action TriggerActionSpec) (int, string) {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, caller, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/triggers",
			CreateTriggerRequest{
				Name:   name,
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epResp.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action: action,
			})
		return rec.Code, rec.Body.String()
	}

	for _, crossProjectEnabled := range []bool{false, true} {
		label := "cross_project_disabled"
		if crossProjectEnabled {
			label = "cross_project_enabled"
			enableCPM(t, f.srv, f.store)
		} else {
			f.srv.SetOperationalSettings(nil)
		}
		t.Run(label, func(t *testing.T) {
			codeMissing, bodyMissing := createTrigger(f.member, "tr-missing-"+label, TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: nonexistentID,
				Message: "ping",
			})
			codeUnreadable, bodyUnreadable := createTrigger(f.member, "tr-unreadable-"+label, TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: unreadableTarget.ID,
				Message: "ping",
			})
			assert.Equal(t, http.StatusNotFound, codeMissing)
			assert.Equal(t, http.StatusNotFound, codeUnreadable)
			assert.Equal(t, bodyMissing, bodyUnreadable,
				"unreadable cross-project target must answer byte-identically to nonexistent target")

			// Direct messageAction.Validate parity (HTTP-free layer).
			act := &messageAction{srv: f.srv}
			_, errMissing := act.Validate(ctx, f.memberAuth, f.projectID,
				`{"agentId":"`+nonexistentID+`","message":"ping"}`)
			_, errUnreadable := act.Validate(ctx, f.memberAuth, f.projectID,
				`{"agentId":"`+unreadableTarget.ID+`","message":"ping"}`)
			var aeMissing, aeUnreadable *messageAuthoringError
			require.ErrorAs(t, errMissing, &aeMissing)
			require.ErrorAs(t, errUnreadable, &aeUnreadable)
			assert.Equal(t, aeMissing.httpStatus, aeUnreadable.httpStatus)
			assert.Equal(t, aeMissing.notFoundResource, aeUnreadable.notFoundResource)
			assert.Equal(t, http.StatusNotFound, aeUnreadable.httpStatus)
			assert.Equal(t, "Agent", aeUnreadable.notFoundResource)
		})
	}

	t.Run("conflicting agentId and agentName returns 404 identical to nonexistent", func(t *testing.T) {
		codeMissing, bodyMissing := createTrigger(f.owner, "tr-missing-conflict", TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: nonexistentID,
			Message: "ping",
		})
		codeConflict, bodyConflict := createTrigger(f.owner, "tr-conflict", TriggerActionSpec{
			Type:      store.TriggerActionMessage,
			AgentID:   f.targetAgent.ID,
			AgentName: "wrong-slug",
			Message:   "ping",
		})
		assert.Equal(t, http.StatusNotFound, codeMissing)
		assert.Equal(t, http.StatusNotFound, codeConflict)
		assert.Equal(t, bodyMissing, bodyConflict)
	})

	t.Run("readable cross-project target obeys cross-project setting", func(t *testing.T) {
		// f.owner owns f.otherProjID so unreadableTarget is readable to f.owner.
		f.srv.SetOperationalSettings(nil)
		codeDisabled, bodyDisabled := createTrigger(f.owner, "tr-xproj-off", TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: unreadableTarget.ID,
			Message: "ping",
		})
		assert.Equal(t, http.StatusForbidden, codeDisabled, bodyDisabled)
		assert.Contains(t, bodyDisabled, "cross-project trigger messages require cross-project messaging to be enabled")

		enableCPM(t, f.srv, f.store)
		codeEnabled, bodyEnabled := createTrigger(f.owner, "tr-xproj-on", TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: unreadableTarget.ID,
			Message: "ping",
		})
		assert.Equal(t, http.StatusCreated, codeEnabled, bodyEnabled)
	})
}

// TestMessageAuthoringTargetRule_ScheduleParity pins Stage 1 rev 8.35 / L24
// (ptone/scion#8588) for message schedules and one-shot scheduled events:
//   - contract 1: create and target-changing update refuse unresolved targets
//     (not found, deleted, conflicting agentId/agentName) with 404, mask
//     unreadable targets as byte-identical 404 (whether cross-project messaging
//     is off or on), refuse readable cross-project targets with 403 when off
//     and admit them when on, and preserve 400 for conflicting payload vs
//     convenience fields;
//   - contract 2: resume of a schedule whose stored target is unresolved
//     (deleted / target_deleted, or never existed) or unreadable is refused
//     with the same generic 404 until PATCHed to a live target;
//   - contract 3: keeping an unresolved target on PATCH is refused with the
//     same generic 404 ONLY for dispatch-changing updates (payload, cronExpr,
//     timeZone, eventType, enable), while name-only PATCH stays accepted;
//   - contract 4: pause and delete stay allowed regardless of target state,
//     and a readable-but-unauthorized existing target stays 403;
//   - contract 5 & 6: stored schedules are not migrated, GET/list keep showing
//     statusReason (target_deleted), fire-time refusal is unchanged, and every
//     404 body is byte-identical to not-found.
func TestMessageAuthoringTargetRule_ScheduleParity(t *testing.T) {
	f := setupTriggersTestFixture(t)
	f.srv.scheduler = NewScheduler(f.store, slog.Default())
	f.srv.scheduler.srv = f.srv
	ctx := context.Background()

	// Zero value of messageAuthoringCaller must remain the strictest mode.
	var zeroCaller messageAuthoringCaller
	assert.Equal(t, messageAuthoringCallerTrigger, zeroCaller)

	// Create an agent in f.otherProjID that f.member cannot read and may not
	// message, while f.owner (who owns f.otherProjID) can read it.
	unreadableTarget := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "sched-secret-cross-agent",
		Name:      "sched-secret-cross-agent",
		ProjectID: f.otherProjID,
		Phase:     "running",
	}
	require.NoError(t, f.store.CreateAgent(ctx, unreadableTarget))

	// Create a soft-deleted agent in f.projectID.
	deletedAt := time.Now().UTC().Add(-time.Hour)
	deletedTarget := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "sched-deleted-agent",
		Name:      "sched-deleted-agent",
		ProjectID: f.projectID,
		Phase:     "stopped",
		DeletedAt: deletedAt,
	}
	require.NoError(t, f.store.CreateAgent(ctx, deletedTarget))

	nonexistentID := api.NewUUID()

	createSched := func(caller *store.User, req CreateScheduleRequest) (int, string, store.Schedule) {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, caller, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/schedules", req)
		var sc store.Schedule
		if rec.Code == http.StatusCreated {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sc))
		}
		return rec.Code, rec.Body.String(), sc
	}
	createEvent := func(caller *store.User, req CreateScheduledEventRequest) (int, string) {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, caller, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/scheduled-events", req)
		return rec.Code, rec.Body.String()
	}

	// Capture canonical trigger 404 body to assert cross-surface parity (contract 6).
	f.srv.SetOperationalSettings(nil)
	codeMissingRef, bodyMissingRef, _ := createSched(f.member, CreateScheduleRequest{
		Name:      "sched-missing-ref",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   `{"agentId":"` + nonexistentID + `","message":"hello"}`,
	})
	require.Equal(t, http.StatusNotFound, codeMissingRef, bodyMissingRef)
	assert.Contains(t, bodyMissingRef, "Agent not found")

	// Contract 1: create unresolved (not found, deleted, conflicting id/slug)
	// and create unreadable (cross-project off and on) for both recurring
	// schedules and one-shot scheduled events.
	for _, crossProjectEnabled := range []bool{false, true} {
		label := "cross_project_disabled"
		if crossProjectEnabled {
			label = "cross_project_enabled"
			enableCPM(t, f.srv, f.store)
		} else {
			f.srv.SetOperationalSettings(nil)
		}
		t.Run("create/"+label, func(t *testing.T) {
			cases := []struct {
				name    string
				payload string
			}{
				{"missing", `{"agentId":"` + nonexistentID + `","message":"hello"}`},
				{"deleted", `{"agentId":"` + deletedTarget.ID + `","message":"hello"}`},
				{"conflicting_id_slug", `{"agentId":"` + f.targetAgent.ID + `","agentName":"wrong-slug","message":"hello"}`},
				{"unreadable_cross_project", `{"agentId":"` + unreadableTarget.ID + `","message":"hello"}`},
			}
			for _, tc := range cases {
				code, body, _ := createSched(f.member, CreateScheduleRequest{
					Name:      "sched-" + tc.name + "-" + label,
					CronExpr:  "0 * * * *",
					EventType: "message",
					Payload:   tc.payload,
				})
				assert.Equal(t, http.StatusNotFound, code, "schedule %s: %s", tc.name, body)
				assert.Equal(t, bodyMissingRef, body, "schedule %s must be byte-identical to not-found", tc.name)

				evCode, evBody := createEvent(f.member, CreateScheduledEventRequest{
					EventType: "message",
					FireIn:    "30m",
					Payload:   tc.payload,
				})
				assert.Equal(t, http.StatusNotFound, evCode, "event %s: %s", tc.name, evBody)
				assert.Equal(t, bodyMissingRef, evBody, "event %s must be byte-identical to not-found", tc.name)
			}
		})
	}

	// Contract 1: readable cross-project target -> 403 when disabled, 201 when enabled.
	t.Run("create/readable_cross_project", func(t *testing.T) {
		f.srv.SetOperationalSettings(nil)
		codeOff, bodyOff, _ := createSched(f.owner, CreateScheduleRequest{
			Name:      "sched-xproj-off",
			CronExpr:  "0 * * * *",
			EventType: "message",
			Payload:   `{"agentId":"` + unreadableTarget.ID + `","message":"hello"}`,
		})
		assert.Equal(t, http.StatusForbidden, codeOff, bodyOff)
		assert.Contains(t, bodyOff, "cross-project scheduled messages require cross-project messaging to be enabled")

		evCodeOff, evBodyOff := createEvent(f.owner, CreateScheduledEventRequest{
			EventType: "message",
			FireIn:    "30m",
			AgentID:   unreadableTarget.ID,
			Message:   "hello",
		})
		assert.Equal(t, http.StatusForbidden, evCodeOff, evBodyOff)
		assert.Contains(t, evBodyOff, "cross-project scheduled messages require cross-project messaging to be enabled")

		enableCPM(t, f.srv, f.store)
		codeOn, bodyOn, _ := createSched(f.owner, CreateScheduleRequest{
			Name:      "sched-xproj-on",
			CronExpr:  "0 * * * *",
			EventType: "message",
			Payload:   `{"agentId":"` + unreadableTarget.ID + `","message":"hello"}`,
		})
		assert.Equal(t, http.StatusCreated, codeOn, bodyOn)

		evCodeOn, evBodyOn := createEvent(f.owner, CreateScheduledEventRequest{
			EventType: "message",
			FireIn:    "30m",
			AgentID:   unreadableTarget.ID,
			Message:   "hello",
		})
		assert.Equal(t, http.StatusCreated, evCodeOn, evBodyOn)
	})

	// Schedule payload validation: conflicting payload vs convenience fields stays 400.
	t.Run("create/conflicting_payload_vs_convenience_stays_400", func(t *testing.T) {
		code, body, _ := createSched(f.member, CreateScheduleRequest{
			Name:      "sched-conflict",
			CronExpr:  "0 * * * *",
			EventType: "message",
			AgentName: f.targetAgent.Slug,
			Payload:   `{"agentName":"other-slug","message":"hello"}`,
		})
		assert.Equal(t, http.StatusBadRequest, code, body)
		assert.Contains(t, body, "conflicting target")
	})

	// Contract 1: update changing target.
	t.Run("update/changing_target", func(t *testing.T) {
		f.srv.SetOperationalSettings(nil)
		code, body, sc := createSched(f.owner, CreateScheduleRequest{
			Name:      "sched-retarget",
			CronExpr:  "0 * * * *",
			EventType: "message",
			AgentName: f.targetAgent.Slug,
			Message:   "hello",
		})
		require.Equal(t, http.StatusCreated, code, body)

		// Unresolved target -> 404 byte-identical.
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
			"/api/v1/projects/"+f.projectID+"/schedules/"+sc.ID,
			UpdateScheduleRequest{Payload: `{"agentId":"` + nonexistentID + `","message":"hello"}`})
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.Equal(t, bodyMissingRef, rec.Body.String())

		// Unreadable cross-project target -> masked 404 byte-identical (for a
		// caller who has scheduled_event.update in f.projectID but cannot read
		// f.otherProjID).
		projAdmin := &store.User{
			ID: api.NewUUID(), Email: "proj-admin-parity@test.com",
			DisplayName: "Proj Admin", Role: "member", Status: store.UserStatusActive,
		}
		require.NoError(t, f.store.CreateUser(ctx, projAdmin))
		ensureHubMembership(ctx, f.store, projAdmin.ID)
		grantProjectRole(t, f.store, projAdmin.ID, f.projectID, store.ProjectRoleAdmin)

		rec = doRequestAsUser(t, f.srv, projAdmin, http.MethodPatch,
			"/api/v1/projects/"+f.projectID+"/schedules/"+sc.ID,
			UpdateScheduleRequest{Payload: `{"agentId":"` + unreadableTarget.ID + `","message":"hello"}`})
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.Equal(t, bodyMissingRef, rec.Body.String())

		// Readable cross-project target with CPM off -> 403.
		rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
			"/api/v1/projects/"+f.projectID+"/schedules/"+sc.ID,
			UpdateScheduleRequest{Payload: `{"agentId":"` + unreadableTarget.ID + `","message":"hello"}`})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "cross-project scheduled messages require cross-project messaging to be enabled")
	})

	// Seed stored schedules with unresolved/deleted/unreadable targets directly
	// in the store (simulating pre-existing rows; contract 5: no migration).
	seedStoredSchedule := func(name, payload, status, statusReason string) *store.Schedule {
		t.Helper()
		rev := withSessionRevision(store.ScheduledEvent{}, f.owner.ID)
		nextRun := time.Now().UTC().Add(time.Hour)
		sc := &store.Schedule{
			ID:                   api.NewUUID(),
			ProjectID:            f.projectID,
			Name:                 name,
			CronExpr:             "0 * * * *",
			EventType:            "message",
			Payload:              payload,
			Status:               status,
			StatusReason:         statusReason,
			NextRunAt:            &nextRun,
			CreatedBy:            f.owner.ID,
			InitiatorAttribution: rev.InitiatorAttribution,
			AuthorityCeiling:     rev.AuthorityCeiling,
		}
		require.NoError(t, f.store.CreateSchedule(ctx, sc))
		return sc
	}

	// Contract 2, 3, 4, 5, 6: resume, dispatch-changing PATCH, metadata-only
	// PATCH, pause, delete, GET/list statusReason, and fire-time refusal.
	t.Run("existing_unresolved_target_lifecycle", func(t *testing.T) {
		f.srv.SetOperationalSettings(nil)
		deletedPaused := seedStoredSchedule(
			"sched-stored-target-deleted",
			`{"agentId":"`+deletedTarget.ID+`","message":"hello"}`,
			store.ScheduleStatusPaused,
			store.ScheduleStatusReasonTargetDeleted,
		)
		neverExistedActive := seedStoredSchedule(
			"sched-stored-never-existed",
			`{"agentId":"`+nonexistentID+`","message":"hello"}`,
			store.ScheduleStatusActive,
			store.ScheduleStatusReasonNone,
		)

		// Contract 5: GET and list keep showing statusReason (target_deleted) unchanged.
		getRec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
			"/api/v1/projects/"+f.projectID+"/schedules/"+deletedPaused.ID, nil)
		require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
		var getResp ScheduleResponse
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
		assert.Equal(t, store.ScheduleStatusReasonTargetDeleted, getResp.StatusReason)

		listRec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
			"/api/v1/projects/"+f.projectID+"/schedules?name=sched-stored-target-deleted", nil)
		require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
		var listResp ListSchedulesResponse
		require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listResp))
		require.Len(t, listResp.Schedules, 1)
		assert.Equal(t, store.ScheduleStatusReasonTargetDeleted, listResp.Schedules[0].StatusReason)

		// Contract 2 & 6: resume with deleted (target_deleted) or never-existed
		// target is refused with the same generic 404 (never revealing whether
		// the agent once existed).
		resumeDel := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/schedules/"+deletedPaused.ID+"/resume", nil)
		assert.Equal(t, http.StatusNotFound, resumeDel.Code, resumeDel.Body.String())
		assert.Equal(t, bodyMissingRef, resumeDel.Body.String())

		// Contract 3: dispatch-changing updates keeping an unresolved target are
		// refused with the same masked 404 (payload, cronExpr, timeZone, eventType, enable).
		dispatchAgentSched := seedStoredSchedule(
			"sched-stored-dispatch-to-msg",
			`{"agentId":"`+nonexistentID+`","message":"hello"}`,
			store.ScheduleStatusActive,
			store.ScheduleStatusReasonNone,
		)
		dispatchAgentSched.EventType = "dispatch_agent"
		require.NoError(t, f.store.UpdateSchedule(ctx, dispatchAgentSched, store.ScheduleFieldMask{EventType: true},
			dispatchAgentSched.AuthorizationRevision, dispatchAgentSched.AuthorizationRevision != 0, nil))

		tz := "America/Los_Angeles"
		dispatchUpdates := []struct {
			name string
			id   string
			req  UpdateScheduleRequest
		}{
			{"payload", neverExistedActive.ID, UpdateScheduleRequest{Payload: `{"agentId":"` + nonexistentID + `","message":"updated text"}`}},
			{"cronExpr", neverExistedActive.ID, UpdateScheduleRequest{CronExpr: "*/15 * * * *"}},
			{"timeZone", neverExistedActive.ID, UpdateScheduleRequest{TimeZone: &tz}},
			{"eventType", dispatchAgentSched.ID, UpdateScheduleRequest{EventType: "message"}},
			{"enable", deletedPaused.ID, UpdateScheduleRequest{Status: store.ScheduleStatusActive}},
		}
		for _, tc := range dispatchUpdates {
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
				"/api/v1/projects/"+f.projectID+"/schedules/"+tc.id, tc.req)
			assert.Equal(t, http.StatusNotFound, rec.Code, "dispatch update %s: %s", tc.name, rec.Body.String())
			assert.Equal(t, bodyMissingRef, rec.Body.String(), "dispatch update %s body", tc.name)
		}

		// Contract 3: name-only PATCH (no revision bump) stays accepted.
		renameRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
			"/api/v1/projects/"+f.projectID+"/schedules/"+neverExistedActive.ID,
			UpdateScheduleRequest{Name: "sched-stored-never-existed-renamed"})
		assert.Equal(t, http.StatusOK, renameRec.Code, renameRec.Body.String())

		// Contract 4: pause and delete stay allowed regardless of target state.
		pauseRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/schedules/"+neverExistedActive.ID+"/pause", nil)
		assert.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

		// Resume of never-existed paused schedule is also refused with the same 404.
		resumeNever := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/schedules/"+neverExistedActive.ID+"/resume", nil)
		assert.Equal(t, http.StatusNotFound, resumeNever.Code, resumeNever.Body.String())
		assert.Equal(t, bodyMissingRef, resumeNever.Body.String())

		// Contract 5: fire-time refusal for stored unresolved schedule is unchanged.
		out := f.srv.fireAction(ctx, hubfire.FireRequest{
			ProjectID: f.projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    neverExistedActive.InitiatorAttribution,
				Ceiling:        neverExistedActive.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindSchedule,
				DefinitionID:   neverExistedActive.ID,
				FireID:         api.NewUUID(),
			},
			ActionType: "message",
			Payload:    neverExistedActive.Payload,
			FireAt:     time.Now().UTC(),
		})
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, errScheduledMessageRefused.Error(), out.Error)

		// Delete stays allowed regardless of target state.
		delRec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete,
			"/api/v1/projects/"+f.projectID+"/schedules/"+neverExistedActive.ID, nil)
		assert.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

		// Contract 2: PATCHing a live target allows subsequent resume to succeed.
		retargetRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
			"/api/v1/projects/"+f.projectID+"/schedules/"+deletedPaused.ID,
			UpdateScheduleRequest{Payload: `{"agentId":"` + f.targetAgent.ID + `","message":"retargeted"}`})
		require.Equal(t, http.StatusOK, retargetRec.Code, retargetRec.Body.String())

		resumeLive := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/schedules/"+deletedPaused.ID+"/resume", nil)
		assert.Equal(t, http.StatusOK, resumeLive.Code, resumeLive.Body.String())
	})

	// Contract 4: readable-but-unauthorized existing target stays 403 on resume
	// and dispatch-changing PATCH.
	t.Run("readable_unauthorized_existing_stays_403", func(t *testing.T) {
		f.srv.SetOperationalSettings(nil)
		storedCross := seedStoredSchedule(
			"sched-stored-readable-cross",
			`{"agentId":"`+unreadableTarget.ID+`","message":"hello"}`,
			store.ScheduleStatusPaused,
			store.ScheduleStatusReasonNone,
		)
		// f.owner can read unreadableTarget (in f.otherProjID), but CPM is off -> 403.
		resumeRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/schedules/"+storedCross.ID+"/resume", nil)
		assert.Equal(t, http.StatusForbidden, resumeRec.Code, resumeRec.Body.String())
		assert.Contains(t, resumeRec.Body.String(), "cross-project scheduled messages require cross-project messaging to be enabled")

		patchRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
			"/api/v1/projects/"+f.projectID+"/schedules/"+storedCross.ID,
			UpdateScheduleRequest{CronExpr: "*/10 * * * *"})
		assert.Equal(t, http.StatusForbidden, patchRec.Code, patchRec.Body.String())
		assert.Contains(t, patchRec.Body.String(), "cross-project scheduled messages require cross-project messaging to be enabled")
	})
}

// TestMessageAuthoringTargetRule_OutOfRangeCallerKindFailsClosed pins S2C-I1:
// any out-of-range messageAuthoringCaller value (zero, negative, or non-zero
// unknown) must fail closed to strict trigger mode and never fall into
// schedule-style acceptance for unresolved or unreadable targets.
func TestMessageAuthoringTargetRule_OutOfRangeCallerKindFailsClosed(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	unreadableTarget := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "s2c-i1-secret-agent",
		Name:      "s2c-i1-secret-agent",
		ProjectID: f.otherProjID,
		Phase:     "running",
	}
	require.NoError(t, f.store.CreateAgent(ctx, unreadableTarget))

	nonexistentID := api.NewUUID()
	outOfRangeKinds := []messageAuthoringCaller{
		messageAuthoringCaller(0),
		messageAuthoringCaller(-1),
		messageAuthoringCaller(99),
	}

	for _, kind := range outOfRangeKinds {
		for _, tc := range []struct {
			name    string
			payload string
		}{
			{name: "unresolved", payload: `{"agentId":"` + nonexistentID + `","message":"ping"}`},
			{name: "unreadable_cross_project", payload: `{"agentId":"` + unreadableTarget.ID + `","message":"ping"}`},
		} {
			_, err := f.srv.validateMessageAuthoringTarget(ctx, f.memberAuth, f.projectID, kind, tc.payload, "", "")
			var authErr *messageAuthoringError
			require.ErrorAs(t, err, &authErr, "callerKind=%d case=%s must fail closed", kind, tc.name)
			assert.Equal(t, http.StatusNotFound, authErr.httpStatus, "callerKind=%d case=%s", kind, tc.name)
			assert.Equal(t, "Agent", authErr.notFoundResource, "callerKind=%d case=%s", kind, tc.name)
		}
	}
}
