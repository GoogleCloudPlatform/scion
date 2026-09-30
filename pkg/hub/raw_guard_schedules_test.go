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

	for _, rawVal := range []string{"true", "false"} {
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

	for _, rawVal := range []string{"true", "false"} {
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
