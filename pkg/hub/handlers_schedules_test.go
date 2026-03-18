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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupScheduleTest(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())

	grove := &store.Grove{
		ID:   "grove-sched-recurring",
		Name: "Schedule Test Grove",
		Slug: "schedule-test-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	return srv, s, grove.ID
}

func TestSchedule_Create(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "daily-standup",
		CronExpr:  "0 9 * * 1-5",
		EventType: "message",
		AgentName: "all",
		Message:   "Good morning! Status update please.",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules", req)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var sched store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sched))

	assert.NotEmpty(t, sched.ID)
	assert.Equal(t, groveID, sched.GroveID)
	assert.Equal(t, "daily-standup", sched.Name)
	assert.Equal(t, "0 9 * * 1-5", sched.CronExpr)
	assert.Equal(t, "message", sched.EventType)
	assert.Equal(t, store.ScheduleStatusActive, sched.Status)
	assert.NotNil(t, sched.NextRunAt)
	assert.NotEmpty(t, sched.Payload)
}

func TestSchedule_CreateInvalidCron(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "bad-cron",
		CronExpr:  "not a cron expression",
		EventType: "message",
		AgentName: "worker-1",
		Message:   "test",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules", req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSchedule_CreateMissingFields(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)

	// Missing name
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
		CreateScheduleRequest{CronExpr: "0 * * * *", EventType: "message", AgentName: "a", Message: "m"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Missing cron
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
		CreateScheduleRequest{Name: "test", EventType: "message", AgentName: "a", Message: "m"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSchedule_List(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)

	// Create two schedules
	for _, name := range []string{"sched-1", "sched-2"} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
			CreateScheduleRequest{
				Name: name, CronExpr: "0 * * * *", EventType: "message",
				AgentName: "worker", Message: "hello",
			})
		require.Equal(t, http.StatusCreated, rec.Code)
	}

	// List
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groves/"+groveID+"/schedules", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 2, resp.TotalCount)
	assert.Len(t, resp.Schedules, 2)
}

func TestSchedule_Get(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)

	// Create
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
		CreateScheduleRequest{
			Name: "get-test", CronExpr: "30 8 * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Get
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/groves/"+groveID+"/schedules/"+created.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var got store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "get-test", got.Name)
}

func TestSchedule_PauseResume(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)

	// Create
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
		CreateScheduleRequest{
			Name: "pause-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Pause
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules/"+created.ID+"/pause", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var paused store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&paused))
	assert.Equal(t, store.ScheduleStatusPaused, paused.Status)

	// Pause again should fail
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules/"+created.ID+"/pause", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Resume
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules/"+created.ID+"/resume", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resumed store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resumed))
	assert.Equal(t, store.ScheduleStatusActive, resumed.Status)
	assert.NotNil(t, resumed.NextRunAt)
}

func TestSchedule_Delete(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)

	// Create
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
		CreateScheduleRequest{
			Name: "delete-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Delete
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+groveID+"/schedules/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Get should fail
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/groves/"+groveID+"/schedules/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSchedule_Update(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)

	// Create
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
		CreateScheduleRequest{
			Name: "update-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Update
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/groves/"+groveID+"/schedules/"+created.ID,
		UpdateScheduleRequest{Name: "updated-name", CronExpr: "30 9 * * *"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var updated store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "updated-name", updated.Name)
	assert.Equal(t, "30 9 * * *", updated.CronExpr)
}

func TestSchedule_History(t *testing.T) {
	srv, s, groveID := setupScheduleTest(t)
	ctx := context.Background()

	// Create a schedule
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
		CreateScheduleRequest{
			Name: "history-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Create some events linked to this schedule
	for i := 0; i < 3; i++ {
		evt := &store.ScheduledEvent{
			ID:         "hist-evt-" + string(rune('a'+i)),
			GroveID:    groveID,
			EventType:  "message",
			FireAt:     created.CreatedAt,
			Payload:    created.Payload,
			ScheduleID: created.ID,
		}
		require.NoError(t, s.CreateScheduledEvent(ctx, evt))
	}

	// Get history
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/groves/"+groveID+"/schedules/"+created.ID+"/history", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp ListScheduledEventsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 3, resp.TotalCount)
}

func TestSchedule_GroveIsolation(t *testing.T) {
	srv, _, groveID := setupScheduleTest(t)
	ctx := context.Background()

	// Create another grove
	otherGrove := &store.Grove{
		ID:   "grove-other-sched",
		Name: "Other Grove",
		Slug: "other-grove-sched",
	}
	require.NoError(t, srv.store.CreateGrove(ctx, otherGrove))

	// Create schedule in first grove
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+groveID+"/schedules",
		CreateScheduleRequest{
			Name: "isolated", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Try to access from another grove
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/groves/"+otherGrove.ID+"/schedules/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
