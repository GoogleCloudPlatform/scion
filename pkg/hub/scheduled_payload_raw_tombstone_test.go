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

// ---------------------------------------------------------------------------
// Scheduled payload validation. One-shot scheduled events (POST
// .../scheduled-events) and recurring schedules (POST/PATCH .../schedules)
// accept an advanced Payload JSON string. validateAndRejectScheduledPayload
// checks it in a fixed order: object shape (400), then the retired "raw"
// key (422 raw_input_removed, any value), then the event-type struct decode
// (400). Nothing is persisted on any rejection.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchedule_CreateRawPayloadTombstoned proves createSchedule rejects a
// "raw" key (true or false) in the advanced Payload JSON, mirroring
// createScheduledEvent's tombstone.
func TestSchedule_CreateRawPayloadTombstoned(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	for _, rawVal := range []string{"true", "false", "null"} {
		t.Run("raw:"+rawVal, func(t *testing.T) {
			req := CreateScheduleRequest{
				Name:      "raw-tombstone-" + rawVal,
				CronExpr:  "0 * * * *",
				EventType: "message",
				Payload:   `{"agentName":"test-agent","message":"hello","raw":` + rawVal + `}`,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Equal(t, messages.RawInputRemovedCode, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, "scion keys")
		})
	}

	schedules, err := s.ListSchedules(t.Context(), store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, schedules.Items, "rejected raw payload must not create a recurring schedule")
}

// TestSchedule_CreateRawTombstone_DispatchAgent covers
// ptone/scion#2200: createSchedule's "raw" tombstone applies to
// "dispatch_agent" too, not just "message".
func TestSchedule_CreateRawTombstone_DispatchAgent(t *testing.T) {
	for _, rawVal := range []string{"true", "false", "null"} {
		t.Run("raw:"+rawVal, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)

			req := CreateScheduleRequest{
				Name:      "raw-tombstone-dispatch-agent-" + rawVal,
				CronExpr:  "0 * * * *",
				EventType: "dispatch_agent",
				Payload:   `{"agentName":"scheduled-worker","raw":` + rawVal + `}`,
			}
			rec := doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, "", http.MethodPost, req)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Equal(t, messages.RawInputRemovedCode, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, "scion keys")

			schedules, err := s.ListSchedules(t.Context(), store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, schedules.Items, "rejected raw payload must not create a dispatch_agent recurring schedule")
		})
	}
}

// TestSchedule_CreateRawTombstone_CaseVariantsAndDuplicateKeys covers case-
// insensitive "raw" spellings and a duplicate "raw" key for createSchedule,
// mirroring TestCreateScheduledEvent_RawTombstone_CaseVariantsAndDuplicateKeys.
func TestSchedule_CreateRawTombstone_CaseVariantsAndDuplicateKeys(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"RAW_uppercase", `{"agentName":"test-agent","message":"hi","RAW":true}`},
		{"Raw_titlecase", `{"agentName":"test-agent","message":"hi","Raw":true}`},
		{"duplicate_key", `{"agentName":"test-agent","message":"hi","raw":false,"raw":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, projectID := setupScheduleTest(t)
			req := CreateScheduleRequest{
				Name: "raw-case-" + tc.name, CronExpr: "0 * * * *", EventType: "message", Payload: tc.payload,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// TestSchedule_CreateMalformedPayload_SanitizedBadRequest covers the
// malformed-payload pin for createSchedule: a non-JSON, non-object, or
// mistyped-field advanced Payload is rejected with a sanitized 400 before
// persistence (a prior version only checked
// json.Valid, so a bare array/string or a field type mismatch like
// {"agentName":5} still persisted). Also covers the
// bare-null cases: encoding/json treats JSON null as a no-op for any
// destination type, so it is its own shape, not a decode error.
func TestSchedule_CreateMalformedPayload_SanitizedBadRequest(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"syntax_error", `{not valid json`},
		{"non_object_array", `[]`},
		{"non_object_string", `"x"`},
		{"mistyped_field", `{"agentName":5}`},
		{"null_payload", `null`},
		{"null_with_surrounding_whitespace", "  null  "},
	}
	for _, eventType := range []string{"message", "dispatch_agent"} {
		for _, tc := range cases {
			t.Run(eventType+"/"+tc.name, func(t *testing.T) {
				srv, s, projectID := setupScheduleTest(t)

				req := CreateScheduleRequest{
					Name: "malformed-payload-" + eventType + "-" + tc.name, CronExpr: "0 * * * *",
					EventType: eventType, Payload: tc.payload,
				}
				var rec *httptest.ResponseRecorder
				if eventType == "dispatch_agent" {
					rec = doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, "", http.MethodPost, req)
				} else {
					rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
				}
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				// Also assert against the
				// JSON-escaped form -- NotContains against the raw payload
				// text alone is vacuous for a payload with quotes/braces,
				// since an actual echo would appear escaped in the response.
				escaped, err := json.Marshal(tc.payload)
				require.NoError(t, err)
				assert.NotContains(t, rec.Body.String(), tc.payload, "the malformed body must not be echoed back")
				assert.NotContains(t, rec.Body.String(), strings.Trim(string(escaped), `"`), "the malformed body must not be echoed back JSON-escaped either")

				schedules, err := s.ListSchedules(t.Context(), store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
				require.NoError(t, err)
				assert.Empty(t, schedules.Items, "a malformed payload must not be persisted")
			})
		}
	}
}

// TestSchedule_CreateRawPlusMistypedField_Returns422NotBadRequest covers
// createSchedule, mirroring
// TestCreateScheduledEvent_RawPlusMistypedField_Returns422NotBadRequest: a
// valid JSON payload carrying "raw" alongside an unrelated mistyped field
// must return 422, not 400.
func TestSchedule_CreateRawPlusMistypedField_Returns422NotBadRequest(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
	}{
		{"message_raw_true_mistyped_agentName", "message", `{"raw":true,"agentName":5}`},
		{"dispatch_agent_raw_true_mistyped_task", "dispatch_agent", `{"raw":true,"agentName":"scheduled-worker","task":5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)

			req := CreateScheduleRequest{
				Name: "raw-mistyped-" + tc.name, CronExpr: "0 * * * *",
				EventType: tc.eventType, Payload: tc.payload,
			}
			var rec *httptest.ResponseRecorder
			if tc.eventType == "dispatch_agent" {
				rec = doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, "", http.MethodPost, req)
			} else {
				rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
			}
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			schedules, err := s.ListSchedules(t.Context(), store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, schedules.Items, "a raw-tombstoned payload must not create a recurring schedule")
		})
	}
}

// TestSchedule_UpdateMalformedPayload_NonObjectAndMistypedField extends
// TestSchedule_UpdateMalformedPayload_SanitizedBadRequest (syntax errors
// only) with the same non-object and mistyped-field shapes, on the
// recurring-schedule update path, for both event types. Also covers the
// bare-null cases. Each
// event type also gets one type-discriminating mistyped case:
// "interrupt":"yes" is a type mismatch only for message
// (Interrupt is a bool there, and an unknown field everywhere else), and
// "task":5 is a type mismatch only for dispatch_agent (Task is a string
// there, and unknown for message) -- so if the update path ever validated
// every payload as a fixed event type regardless of effectiveEventType, one
// of these two cases would silently pass instead of 400'ing.
func TestSchedule_UpdateMalformedPayload_NonObjectAndMistypedField(t *testing.T) {
	commonCases := []struct {
		name    string
		payload string
	}{
		{"non_object_array", `[]`},
		{"non_object_string", `"x"`},
		{"mistyped_field", `{"agentName":5}`},
		{"null_payload", `null`},
		{"null_with_surrounding_whitespace", "  null  "},
	}
	typeDiscriminatingCase := map[string]struct {
		name    string
		payload string
	}{
		"message":        {"mistyped_interrupt_field", `{"agentName":"test-agent","message":"hello","interrupt":"yes"}`},
		"dispatch_agent": {"mistyped_task_field", `{"agentName":"scheduled-worker","task":5}`},
	}
	for _, eventType := range []string{"message", "dispatch_agent"} {
		cases := append([]struct {
			name    string
			payload string
		}{}, commonCases...)
		cases = append(cases, typeDiscriminatingCase[eventType])
		for _, tc := range cases {
			t.Run(eventType+"/"+tc.name, func(t *testing.T) {
				srv, s, projectID := setupScheduleTest(t)

				var createReq CreateScheduleRequest
				if eventType == "dispatch_agent" {
					createReq = CreateScheduleRequest{
						Name: "update-malformed-" + eventType + "-" + tc.name, CronExpr: "0 * * * *",
						EventType: "dispatch_agent", AgentName: "scheduled-worker",
					}
				} else {
					createReq = CreateScheduleRequest{
						Name: "update-malformed-" + eventType + "-" + tc.name, CronExpr: "0 * * * *",
						EventType: "message", AgentName: "test-agent", Message: "hello",
					}
				}
				createRec := doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, "", http.MethodPost, createReq)
				require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
				var created struct{ ID string }
				require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))
				originalPayload := func() string {
					sched, err := s.GetSchedule(t.Context(), created.ID)
					require.NoError(t, err)
					return sched.Payload
				}()

				rec := doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, created.ID, http.MethodPatch,
					UpdateScheduleRequest{Payload: tc.payload})
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				// Also check the JSON-escaped form.
				escaped, err := json.Marshal(tc.payload)
				require.NoError(t, err)
				assert.NotContains(t, rec.Body.String(), tc.payload, "the malformed body must not be echoed back")
				assert.NotContains(t, rec.Body.String(), strings.Trim(string(escaped), `"`), "the malformed body must not be echoed back JSON-escaped either")

				sched, err := s.GetSchedule(t.Context(), created.ID)
				require.NoError(t, err)
				assert.Equal(t, originalPayload, sched.Payload, "a malformed replacement payload must not be persisted")
			})
		}
	}
}

// TestSchedule_UpdateRawPlusMistypedField_Returns422NotBadRequest covers
// the recurring-schedule update path: a valid
// JSON replacement payload carrying "raw" alongside an unrelated mistyped
// field must return 422, not 400.
func TestSchedule_UpdateRawPlusMistypedField_Returns422NotBadRequest(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "update-raw-mistyped", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "test-agent", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
	var created struct{ ID string }
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))
	originalPayload := func() string {
		sched, err := s.GetSchedule(t.Context(), created.ID)
		require.NoError(t, err)
		return sched.Payload
	}()

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID,
		UpdateScheduleRequest{Payload: `{"raw":true,"agentName":5}`})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

	sched, err := s.GetSchedule(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, originalPayload, sched.Payload, "a raw-tombstoned replacement payload must not be persisted")
}

// TestSchedule_UpdateEventTypeOnly_RevalidatesStoredPayload pins that an
// update that changes EventType
// without supplying a replacement Payload must still validate the existing
// stored Payload against the new effective event type (handlers_schedules.go,
// the "else if" branch following the req.Payload != "" check). Without
// this branch, the switch would silently succeed with a
// payload that cannot decode for the new type, failing only later at fire
// time.
func TestSchedule_UpdateEventTypeOnly_RevalidatesStoredPayload(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	// "task" is unknown-and-ignored for a message payload, so this is valid
	// JSON for EventType "message" and is accepted at create time.
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "update-event-type-only", CronExpr: "0 * * * *", EventType: "message",
			Payload: `{"agentName":"test-agent","message":"hi","task":5}`,
		})
	require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
	var created struct{ ID string }
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))
	originalEventType := func() string {
		sched, err := s.GetSchedule(t.Context(), created.ID)
		require.NoError(t, err)
		return sched.EventType
	}()
	originalPayload := func() string {
		sched, err := s.GetSchedule(t.Context(), created.ID)
		require.NoError(t, err)
		return sched.Payload
	}()

	// Switching to "dispatch_agent" with no new Payload must re-validate the
	// carried-over stored Payload against the new type: "task" is a string
	// field there, so 5 is a type mismatch.
	rec := doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, created.ID, http.MethodPatch,
		UpdateScheduleRequest{EventType: "dispatch_agent"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	sched, err := s.GetSchedule(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, originalEventType, sched.EventType, "a rejected event-type switch must not change the stored event type")
	assert.Equal(t, originalPayload, sched.Payload, "a rejected event-type switch must not change the stored payload")
}

// TestSchedule_CreateNonRawPayloadStillWorks is a negative control for
// TestSchedule_CreateRawPayloadTombstoned.
func TestSchedule_CreateNonRawPayloadStillWorks(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "raw-tombstone-control",
		CronExpr:  "0 * * * *",
		EventType: "message",
		AgentName: "test-agent",
		Message:   "hello",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// TestSchedule_UpdateRawPayloadTombstoned proves updateSchedule rejects a
// "raw" key (true or false) in a caller-supplied Payload replacement. Only
// checked when the update actually sets Payload — an update that leaves
// Payload untouched must not retroactively fail on an existing stored
// value (this is why the test creates with a benign payload first).
func TestSchedule_UpdateRawPayloadTombstoned(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "update-raw-tombstone", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "test-agent", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, createRec.Code)
	var created struct{ ID string }
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	for _, rawVal := range []string{"true", "false", "null"} {
		t.Run("raw:"+rawVal, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID,
				UpdateScheduleRequest{
					Payload: `{"agentName":"test-agent","message":"updated","raw":` + rawVal + `}`,
				})
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Equal(t, messages.RawInputRemovedCode, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, "scion keys")
		})
	}

	// The schedule must be untouched by the rejected updates.
	sched, err := s.GetSchedule(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Contains(t, sched.Payload, `"message":"hello"`, "rejected update must not modify the stored payload")
}

// TestSchedule_UpdateRawTombstone_DispatchAgent covers the "on any
// replacement payload supplied in an update" rule for a dispatch_agent
// schedule: updateSchedule's tombstone must fire for dispatch_agent, not
// just message, mirroring TestSchedule_UpdateRawPayloadTombstoned.
func TestSchedule_UpdateRawTombstone_DispatchAgent(t *testing.T) {
	for _, rawVal := range []string{"true", "false", "null"} {
		t.Run("raw:"+rawVal, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)

			createRec := doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, "", http.MethodPost,
				CreateScheduleRequest{
					Name: "update-raw-tombstone-dispatch-" + rawVal, CronExpr: "0 * * * *", EventType: "dispatch_agent",
					AgentName: "scheduled-worker",
				})
			require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
			var created struct{ ID string }
			require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

			rec := doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, created.ID, http.MethodPatch,
				UpdateScheduleRequest{Payload: `{"agentName":"scheduled-worker","raw":` + rawVal + `}`})
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Equal(t, messages.RawInputRemovedCode, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, "scion keys")

			sched, err := s.GetSchedule(t.Context(), created.ID)
			require.NoError(t, err)
			assert.NotContains(t, sched.Payload, `"raw"`, "rejected update must not modify the stored payload")
		})
	}
}

// TestSchedule_UpdateMalformedPayload_SanitizedBadRequest covers the
// malformed-payload pin for updateSchedule's replacement payload.
func TestSchedule_UpdateMalformedPayload_SanitizedBadRequest(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "update-malformed", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "test-agent", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, createRec.Code)
	var created struct{ ID string }
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID,
		UpdateScheduleRequest{Payload: `{not valid json`})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "not valid json", "the malformed body must not be echoed back")

	sched, err := s.GetSchedule(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Contains(t, sched.Payload, `"message":"hello"`, "a malformed replacement payload must not be persisted")
}

// TestSchedule_UpdateNonPayloadFieldsStillWork is a negative control: an
// update that does not touch Payload must succeed even though the schedule
// already has a stored (pre-guard) payload — the tombstone must not
// retroactively break unrelated updates.
func TestSchedule_UpdateNonPayloadFieldsStillWork(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "update-control", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "test-agent", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, createRec.Code)
	var created struct{ ID string }
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID,
		UpdateScheduleRequest{Name: "renamed"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// TestSchedule_UpdateRawProbe_NotRecursive_PlainUnaffected is the recurring-
// schedule-update positive control
// (alongside the one-shot-event message/dispatch_agent create controls in
// this file): a replacement Payload with a nested "raw" key (not at
// the top level), "raw" inside a string value, or a top-level "plain" key
// must update successfully (200, persisted), not be mistaken for the
// tombstoned top-level "raw" key.
func TestSchedule_UpdateRawProbe_NotRecursive_PlainUnaffected(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"nested_raw_key_not_top_level", `{"agentName":"test-agent","message":"hi","x":{"raw":true}}`},
		{"raw_inside_string_value", `{"agentName":"test-agent","message":"use raw:true in your reply"}`},
		{"top_level_plain_true", `{"agentName":"test-agent","message":"hi","plain":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)

			createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
				CreateScheduleRequest{
					Name: "update-not-recursive-" + tc.name, CronExpr: "0 * * * *", EventType: "message",
					AgentName: "test-agent", Message: "hello",
				})
			require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
			var created struct{ ID string }
			require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID,
				UpdateScheduleRequest{Payload: tc.payload})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			sched, err := s.GetSchedule(t.Context(), created.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.payload, sched.Payload, "the replacement payload must be persisted")
		})
	}
}

// TestCreateScheduledEvent_RawPayloadTombstoned proves the advanced scheduled
// event payload cannot carry a "raw" key through to scheduled dispatch.
// MessageEventPayload has no Raw field, so this key is rejected explicitly
// (422) at decode — this test locks that tombstone in place. Uses the same
// setupScheduledEventTest / doRequest convention as
// handlers_scheduled_events_test.go.
func TestCreateScheduledEvent_RawPayloadTombstoned(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "1h",
		Payload:   `{"agentName":"test-agent","message":"hello","raw":true}`,
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, messages.RawInputRemovedCode, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "scion keys")

	events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, events.Items, "rejected raw scheduled payload must not create a scheduled event")

	// raw:false and raw:null must be tombstoned too — permissive decoding
	// must not treat an explicit false, or a present-but-null value, as
	// "not present, safe to ignore".
	for _, rawVal := range []string{"false", "null"} {
		recOther := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events",
			CreateScheduledEventRequest{
				EventType: "message",
				FireIn:    "1h",
				Payload:   `{"agentName":"test-agent","message":"hello","raw":` + rawVal + `}`,
			})
		require.Equal(t, http.StatusUnprocessableEntity, recOther.Code, "raw:%s must also be tombstoned; body: %s", rawVal, recOther.Body.String())
	}
}

// TestCreateScheduledEvent_RawTombstone_DispatchAgent covers
// ptone/scion#2200: the top-level payload "raw" tombstone
// applies to the "dispatch_agent" event type too, not just "message" — the
// advanced Payload field is accepted verbatim for dispatch_agent as well
// (DispatchAgentEventPayload has no Raw field either), and before this fix
// the raw-key probe was only called inside the "message" branch.
func TestCreateScheduledEvent_RawTombstone_DispatchAgent(t *testing.T) {
	for _, rawVal := range []string{"true", "false", "null"} {
		t.Run("raw:"+rawVal, func(t *testing.T) {
			srv, s, projectID := setupScheduledEventTest(t)

			req := CreateScheduledEventRequest{
				EventType: "dispatch_agent",
				FireIn:    "1h",
				Payload:   `{"agentName":"scheduled-worker","raw":` + rawVal + `}`,
			}
			rec := doScheduledEventAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, req)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Equal(t, messages.RawInputRemovedCode, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, "scion keys")

			events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, events.Items, "rejected raw scheduled payload must not create a dispatch_agent scheduled event")
		})
	}
}

// TestCreateScheduledEvent_RawTombstone_CaseVariantsAndDuplicateKeys covers
// additional pinned cases: case-insensitive "raw" key spellings
// (encoding/json's own field matching is case-insensitive, so "RAW"/"Raw"
// already match the "raw" tag — this locks that behaviour in rather than
// relying on it being incidental), and a duplicate "raw" key (the decoder
// keeps the last occurrence; either value still tombstones the payload).
// Exercised for both supported event types.
func TestCreateScheduledEvent_RawTombstone_CaseVariantsAndDuplicateKeys(t *testing.T) {
	messageCases := []struct {
		name    string
		payload string
	}{
		{"RAW_uppercase", `{"agentName":"test-agent","message":"hi","RAW":true}`},
		{"Raw_titlecase", `{"agentName":"test-agent","message":"hi","Raw":true}`},
		{"duplicate_key_last_true", `{"agentName":"test-agent","message":"hi","raw":false,"raw":true}`},
		{"duplicate_key_last_false", `{"agentName":"test-agent","message":"hi","raw":true,"raw":false}`},
	}
	for _, tc := range messageCases {
		t.Run("message/"+tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduledEventTest(t)
			req := CreateScheduledEventRequest{EventType: "message", FireIn: "1h", Payload: tc.payload}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, events.Items)
		})
	}

	dispatchAgentCases := []struct {
		name    string
		payload string
	}{
		{"RAW_uppercase", `{"agentName":"scheduled-worker","RAW":true}`},
		{"Raw_titlecase", `{"agentName":"scheduled-worker","Raw":true}`},
		{"duplicate_key_last_true", `{"agentName":"scheduled-worker","raw":false,"raw":true}`},
	}
	for _, tc := range dispatchAgentCases {
		t.Run("dispatch_agent/"+tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduledEventTest(t)
			req := CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", Payload: tc.payload}
			rec := doScheduledEventAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, req)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, events.Items)
		})
	}
}

// TestCreateScheduledEvent_MalformedPayload_SanitizedBadRequest covers
// a pinned case: a non-JSON advanced Payload must be rejected with a
// sanitized 400 before persistence, for both event types — not silently
// stored. Before this fix, authorizeScheduledMessageAuthoring's own decode
// (used only for target resolution) tolerated a parse failure by falling
// back to convenience fields, and the raw-key probe likewise treated
// a decode failure as "nothing to do here", so a malformed payload reached
// storage unvalidated.
func TestCreateScheduledEvent_MalformedPayload_SanitizedBadRequest(t *testing.T) {
	// A prior version of this fix used only
	// json.Valid, which accepts a bare array, a bare string, or an object
	// whose fields don't match the event's payload type -- all three
	// persisted (201) before this fix, failing only later at fire time.
	// "non_object_array" and "mistyped_field" pin exactly those two shapes
	// in addition to the pre-existing syntax-error case.
	//
	// A bare `null` payload is also its own shape, not
	// a decode error -- encoding/json treats JSON null as a no-op for any
	// destination type, so neither a struct decode nor json.Valid catches
	// it. "null_payload" and "null_with_surrounding_whitespace" pin that a
	// top-level null is rejected the same as any other non-object shape.
	cases := []struct {
		name    string
		payload string
	}{
		{"syntax_error", `{not valid json`},
		{"non_object_array", `[]`},
		{"non_object_string", `"x"`},
		{"mistyped_field", `{"agentName":5}`},
		{"null_payload", `null`},
		{"null_with_surrounding_whitespace", "  null  "},
	}

	for _, tc := range cases {
		t.Run("message/"+tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduledEventTest(t)
			req := CreateScheduledEventRequest{EventType: "message", FireIn: "1h", Payload: tc.payload}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			// NotContains against the raw payload
			// text is vacuous for a payload containing quotes or braces --
			// any echo would appear JSON-escaped in the response body, so the
			// literal payload text can never match. Assert against the
			// JSON-escaped form too, which is what an actual echo would
			// produce.
			escaped, err := json.Marshal(tc.payload)
			require.NoError(t, err)
			assert.NotContains(t, rec.Body.String(), tc.payload, "the malformed body must not be echoed back")
			assert.NotContains(t, rec.Body.String(), strings.Trim(string(escaped), `"`), "the malformed body must not be echoed back JSON-escaped either")

			events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, events.Items, "a malformed payload must not be persisted")
		})

		t.Run("dispatch_agent/"+tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduledEventTest(t)
			req := CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", Payload: tc.payload}
			rec := doScheduledEventAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

			events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, events.Items, "a malformed payload must not be persisted")
		})
	}
}

// TestValidateScheduledEventPayloadJSON_UnknownEventType_FailsClosed pins a
// Gemini code-review finding on GoogleCloudPlatform/scion#2286: the
// default branch of validateScheduledEventPayloadJSON previously fell back
// to a lenient syntax-only check (json.Valid) for any eventType outside
// {"message", "dispatch_agent"}, rather than rejecting it outright. Every
// current caller already restricts eventType to that closed set before
// calling this function, so the branch is unreachable in production today --
// but if a third event type is ever added to the closed set without a
// matching case here, its payload would silently skip structural
// validation instead of failing closed. This drives the method directly
// (the only way to reach the default branch at all, since no HTTP call site
// can) and asserts it now rejects an otherwise-well-formed JSON object for
// an unrecognized type.
func TestValidateScheduledEventPayloadJSON_UnknownEventType_FailsClosed(t *testing.T) {
	srv, _, _ := setupScheduledEventTest(t)

	rec := httptest.NewRecorder()
	ok := srv.validateScheduledEventPayloadJSON(rec, "some_future_event_type", `{"agentName":"test-agent"}`)

	assert.False(t, ok, "an unrecognized event type must fail closed, not fall back to a syntax-only check")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

// TestCreateScheduledEvent_RawPlusMistypedField_Returns422NotBadRequest
// covers: a valid JSON object carrying a "raw" key
// alongside some unrelated mistyped field must still return 422 (the
// dedicated raw-tombstone outcome), not 400 (the generic struct-decode
// outcome) -- the ruling is "valid JSON carrying a raw key stays 422"
// unconditionally, not only when every other field happens to be
// well-typed. Before this fix, validateScheduledEventPayloadJSON's struct
// decode ran before the raw-key probe, so these cases collapsed into
// 400.
func TestCreateScheduledEvent_RawPlusMistypedField_Returns422NotBadRequest(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
	}{
		{"message_raw_true_mistyped_agentName", "message", `{"raw":true,"agentName":5}`},
		{"message_raw_true_mistyped_message", "message", `{"raw":true,"message":5,"agentName":"test-agent"}`},
		{"message_raw_false_mistyped_interrupt", "message", `{"raw":false,"interrupt":"yes","agentName":"test-agent","message":"hi"}`},
		{"dispatch_agent_raw_true_mistyped_task", "dispatch_agent", `{"raw":true,"agentName":"scheduled-worker","task":5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduledEventTest(t)
			req := CreateScheduledEventRequest{EventType: tc.eventType, FireIn: "1h", Payload: tc.payload}
			var rec *httptest.ResponseRecorder
			if tc.eventType == "dispatch_agent" {
				rec = doScheduledEventAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, req)
			} else {
				rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
			}
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, events.Items, "a raw-tombstoned payload must not be persisted")
		})
	}
}

// TestCreateScheduledEvent_RawProbe_NotRecursive_PlainUnaffected covers a
// gap: the raw-tombstone controls before this
// commit only ever used payloads with no "raw"/"plain" keys at all, so
// nothing would have caught a future change that made the raw probe
// recursive (rejecting "raw" anywhere in the JSON tree, not just the top
// level) or that started treating a top-level "plain" key as somehow
// related to raw scheduling. These are positive controls: production
// already behaves correctly (confirmed by scratch testing in review), so
// each case here asserts 201 plus a persisted row, for both event types.
func TestCreateScheduledEvent_RawProbe_NotRecursive_PlainUnaffected(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"nested_raw_key_not_top_level", `{"agentName":"test-agent","message":"hi","x":{"raw":true}}`},
		{"raw_inside_string_value", `{"agentName":"test-agent","message":"use raw:true in your reply"}`},
		{"top_level_plain_true", `{"agentName":"test-agent","message":"hi","plain":true}`},
	}
	for _, tc := range cases {
		t.Run("message/"+tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduledEventTest(t)
			req := CreateScheduledEventRequest{EventType: "message", FireIn: "1h", Payload: tc.payload}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

			events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, events.Items, 1, "a non-recursive, non-plain-colliding payload must be persisted")
		})
	}

	dispatchAgentCases := []struct {
		name    string
		payload string
	}{
		{"nested_raw_key_not_top_level", `{"agentName":"scheduled-worker","x":{"raw":true}}`},
		{"raw_inside_string_value", `{"agentName":"scheduled-worker","task":"mention raw:true in the PR"}`},
	}
	for _, tc := range dispatchAgentCases {
		t.Run("dispatch_agent/"+tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduledEventTest(t)
			req := CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", Payload: tc.payload}
			rec := doScheduledEventAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, req)
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

			events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, events.Items, 1, "a non-recursive payload must be persisted")
		})
	}
}

// TestCreateScheduledEvent_NonRawPayloadStillWorks is a negative control: a
// normal scheduled-message payload (no "raw" key at all) is unaffected by
// the tombstone.
func TestCreateScheduledEvent_NonRawPayloadStillWorks(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "1h",
		AgentName: "test-agent",
		Message:   "hello",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
}

// TestScheduledPayload_RawTombstoneIsContentFree pins that rejecting a
// retired raw key in a scheduled-event or recurring-schedule payload emits
// the content-free audit line (route=message_raw_removed,
// ingress=scheduled_payload) and that neither the response nor any log
// line carries the payload's message text.
func TestScheduledPayload_RawTombstoneIsContentFree(t *testing.T) {
	const secret = "scheduled-raw-content-sentinel-7f3a"
	payload := `{"agentName":"test-agent","message":"` + secret + `","raw":true}`

	cases := []struct {
		name string
		do   func(t *testing.T) *httptest.ResponseRecorder
	}{
		{"scheduled event", func(t *testing.T) *httptest.ResponseRecorder {
			srv, _, projectID := setupScheduledEventTest(t)
			return doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events",
				CreateScheduledEventRequest{EventType: "message", FireIn: "1h", Payload: payload})
		}},
		{"recurring schedule", func(t *testing.T) *httptest.ResponseRecorder {
			srv, _, projectID := setupScheduleTest(t)
			return doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
				CreateScheduleRequest{Name: "raw-content-free", CronExpr: "0 * * * *", EventType: "message", Payload: payload})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := installSentinelLogCapture(t)
			rec := tc.do(t)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(strings.NewReader(rec.Body.String())).Decode(&errResp))
			assert.Equal(t, messages.RawInputRemovedCode, errResp.Error.Code)
			assert.NotContains(t, rec.Body.String(), secret, "the rejection must not echo payload content")

			out := log.String()
			assert.Contains(t, out, "route="+string(agentKeysRouteRawRemoved))
			assert.Contains(t, out, "ingress="+string(rawIngressScheduledPayload))
			assert.NotContains(t, out, secret, "audit and logs must not contain payload content")
		})
	}
}
