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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupScheduledEventTest(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	// Initialize the scheduler (normally done by Server.Start)
	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())

	project := &store.Project{
		ID:   "project-sched-test",
		Name: "Scheduler Test Project",
		Slug: "sched-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	return srv, s, project.ID
}

func TestScheduledEvent_Create(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "30m",
		AgentName: "test-agent",
		Message:   "Hello from scheduler",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var evt store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evt))

	assert.NotEmpty(t, evt.ID)
	assert.Equal(t, projectID, evt.ProjectID)
	assert.Equal(t, "message", evt.EventType)
	assert.Equal(t, store.ScheduledEventPending, evt.Status)
	assert.NotEmpty(t, evt.Payload)

	// Verify the fire time is approximately 30 minutes from now
	expectedFireAt := time.Now().Add(30 * time.Minute)
	assert.WithinDuration(t, expectedFireAt, evt.FireAt, 5*time.Second)
}

func TestScheduledEvent_CreateWithFireAt(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	futureTime := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireAt:    futureTime.Format(time.RFC3339),
		AgentName: "test-agent",
		Message:   "Scheduled for later",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var evt store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evt))

	assert.WithinDuration(t, futureTime, evt.FireAt, 2*time.Second)
}

func TestScheduledEvent_CreateWithPlainFlag(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "10m",
		AgentName: "test-agent",
		Message:   "plain message",
		Plain:     true,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var evt store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evt))

	// Verify the Plain flag is preserved in the payload
	var payload MessageEventPayload
	require.NoError(t, json.Unmarshal([]byte(evt.Payload), &payload))
	assert.True(t, payload.Plain, "plain flag should be preserved in payload")
	assert.Equal(t, "plain message", payload.Message)
}

func TestScheduledEvent_CreateValidation(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)
	basePath := "/api/v1/projects/" + projectID + "/scheduled-events"

	tests := []struct {
		name string
		req  CreateScheduledEventRequest
		msg  string
	}{
		{
			name: "missing event type",
			req:  CreateScheduledEventRequest{FireIn: "30m", AgentName: "a", Message: "m"},
			msg:  "eventType is required",
		},
		{
			name: "unsupported event type",
			req:  CreateScheduledEventRequest{EventType: "unknown", FireIn: "30m"},
			msg:  "unsupported event type",
		},
		{
			name: "missing fire time",
			req:  CreateScheduledEventRequest{EventType: "message", AgentName: "a", Message: "m"},
			msg:  "either fireAt or fireIn is required",
		},
		{
			name: "both fire times",
			req:  CreateScheduledEventRequest{EventType: "message", FireAt: "2030-01-01T00:00:00Z", FireIn: "30m", AgentName: "a", Message: "m"},
			msg:  "fireAt and fireIn are mutually exclusive",
		},
		{
			name: "past fire time",
			req:  CreateScheduledEventRequest{EventType: "message", FireAt: "2020-01-01T00:00:00Z", AgentName: "a", Message: "m"},
			msg:  "fireAt must be in the future",
		},
		{
			name: "invalid fire at format",
			req:  CreateScheduledEventRequest{EventType: "message", FireAt: "not-a-timestamp", AgentName: "a", Message: "m"},
			msg:  "fireAt must be a valid",
		},
		{
			name: "invalid fire in format",
			req:  CreateScheduledEventRequest{EventType: "message", FireIn: "invalid", AgentName: "a", Message: "m"},
			msg:  "fireIn must be a valid",
		},
		{
			name: "negative fire in",
			req:  CreateScheduledEventRequest{EventType: "message", FireIn: "-5m", AgentName: "a", Message: "m"},
			msg:  "fireIn must be a positive",
		},
		{
			name: "missing message",
			req:  CreateScheduledEventRequest{EventType: "message", FireIn: "30m", AgentName: "a"},
			msg:  "message is required",
		},
		{
			name: "missing agent",
			req:  CreateScheduledEventRequest{EventType: "message", FireIn: "30m", Message: "m"},
			msg:  "agentId or agentName is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, basePath, tc.req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for %s", tc.name)

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Contains(t, errResp.Error.Message, tc.msg)
		})
	}
}

func TestScheduledEvent_List(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ctx := context.Background()

	// Create a couple of events directly in the store
	for i, status := range []string{store.ScheduledEventPending, store.ScheduledEventFired} {
		evt := &store.ScheduledEvent{
			ID:        "list-evt-" + string(rune('a'+i)),
			ProjectID:   projectID,
			EventType: "message",
			FireAt:    time.Now().Add(time.Duration(i+1) * time.Hour),
			Payload:   `{"message":"test"}`,
			Status:    status,
			CreatedAt: time.Now(),
		}
		require.NoError(t, s.CreateScheduledEvent(ctx, evt))
	}

	// List all events
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp ListScheduledEventsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Events, 2)
	assert.False(t, resp.ServerTime.IsZero())

	// Filter by status
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events?status=pending", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Events, 1)
	assert.Equal(t, store.ScheduledEventPending, resp.Events[0].Status)
}

func TestScheduledEvent_Get(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ctx := context.Background()

	evt := &store.ScheduledEvent{
		ID:        "get-evt-1",
		ProjectID:   projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   `{"message":"get me"}`,
		Status:    store.ScheduledEventPending,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events/get-evt-1", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var got store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "get-evt-1", got.ID)
	assert.Equal(t, "message", got.EventType)
}

func TestScheduledEvent_GetNotFound(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestScheduledEvent_GetWrongProject(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ctx := context.Background()

	// Create a second project
	project2 := &store.Project{
		ID:   "project-sched-other",
		Name: "Other Project",
		Slug: "other-project",
	}
	require.NoError(t, s.CreateProject(ctx, project2))

	// Create event in first project
	evt := &store.ScheduledEvent{
		ID:        "wrong-project-evt",
		ProjectID:   projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   `{}`,
		Status:    store.ScheduledEventPending,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	// Try to get it from the other project
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project2.ID+"/scheduled-events/wrong-project-evt", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestScheduledEvent_Cancel(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ctx := context.Background()

	evt := &store.ScheduledEvent{
		ID:        "cancel-evt-1",
		ProjectID:   projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   `{"message":"cancel me"}`,
		Status:    store.ScheduledEventPending,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+projectID+"/scheduled-events/cancel-evt-1", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify it was cancelled in the store
	got, err := s.GetScheduledEvent(ctx, "cancel-evt-1")
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventCancelled, got.Status)
}

func TestScheduledEvent_CancelNotFound(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+projectID+"/scheduled-events/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestScheduledEvent_Unauthenticated(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestScheduledEvent_MethodNotAllowed(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	// PATCH on collection
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/scheduled-events", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	// POST on individual event
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events/some-id", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
