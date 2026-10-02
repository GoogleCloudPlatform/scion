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
// Recurring schedules (POST/PATCH .../schedules) accept the same advanced
// Payload JSON as one-shot scheduled events and are tombstoned the same way
// (ptone/scion#2192).
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
			assert.Equal(t, string(MessageDenialRawSchedulingUnsupported), errResp.Error.Details["reason"])
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
			assert.Equal(t, string(MessageDenialRawSchedulingUnsupported), errResp.Error.Details["reason"])

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
			assert.Equal(t, string(MessageDenialRawSchedulingUnsupported), errResp.Error.Details["reason"])
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
			assert.Equal(t, string(MessageDenialRawSchedulingUnsupported), errResp.Error.Details["reason"])

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
// raw_guard_test.go): a replacement Payload with a nested "raw" key (not at
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
