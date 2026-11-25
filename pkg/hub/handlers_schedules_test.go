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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchedule_Create(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "daily-standup",
		CronExpr:  "0 9 * * 1-5",
		EventType: "message",
		AgentName: "all",
		Message:   "Good morning! Status update please.",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var sched store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sched))

	assert.NotEmpty(t, sched.ID)
	assert.Equal(t, projectID, sched.ProjectID)
	assert.Equal(t, "daily-standup", sched.Name)
	assert.Equal(t, "0 9 * * 1-5", sched.CronExpr)
	assert.Equal(t, "message", sched.EventType)
	assert.Equal(t, store.ScheduleStatusActive, sched.Status)
	assert.NotNil(t, sched.NextRunAt)
	assert.NotEmpty(t, sched.Payload)
}

// TestSchedule_CreateWithWakeFlag: wake on the create request reaches the
// stored message payload (ptone/scion#7734).
func TestSchedule_CreateWithWakeFlag(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "wake-poke",
		CronExpr:  "*/5 * * * *",
		EventType: "message",
		AgentName: "all",
		Message:   "recheck",
		Wake:      true,
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var sched store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sched))
	var payload MessageEventPayload
	require.NoError(t, json.Unmarshal([]byte(sched.Payload), &payload))
	assert.True(t, payload.Wake)
	assert.True(t, scheduledPayloadWakes(sched.Payload))

	// wake is refused on any event type but message.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
		Name: "wake-dispatch", CronExpr: "*/5 * * * *", EventType: "dispatch_agent", AgentName: "new-agent", Wake: true,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "wake applies to message events only")
}

func TestSchedule_CreateDispatchAgentRequiresAgentCreateScope(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "spawn-worker",
		CronExpr:  "0 * * * *",
		EventType: "dispatch_agent",
		AgentName: "scheduled-worker",
	}

	rec := doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead), projectID, "", http.MethodPost, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), string(ScopeAgentCreate))

	rec = doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, "", http.MethodPost, req)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// An agent caller's PATCH converting a message schedule to dispatch_agent
// is refused, and the schedule keeps its type. Agent callers do not edit
// schedule definitions; the agent:create gate on this conversion is
// covered for user callers by
// TestSchedule_UpdateDispatchAgentScopedUATNeedsUpdateAndAgentCreate.
func TestSchedule_UpdateToDispatchAgentRefusedForAgentCaller(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name:      "message-first",
			CronExpr:  "0 * * * *",
			EventType: "message",
			AgentName: "worker",
			Message:   "ping",
		})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var sched store.Schedule
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&sched))

	rec := doScheduleAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead), projectID, sched.ID, http.MethodPatch,
		UpdateScheduleRequest{EventType: "dispatch_agent", Payload: `{"agentName":"scheduled-worker"}`})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.JSONEq(t, agentScopeForbiddenBody, rec.Body.String())
	stored, err := s.GetSchedule(context.Background(), sched.ID)
	require.NoError(t, err)
	assert.Equal(t, "message", stored.EventType)
}

// TestSchedule_CreateDispatchAgentScopedUATNeedsBothSelectors covers
// recurring-schedule create of a dispatch_agent schedule by a scoped UAT of
// a project owner: a project or hub token is admitted when it carries
// scheduled_event:create and agent:create, and refused with 403 when either
// selector is missing. The same unscoped user is admitted.
func TestSchedule_CreateDispatchAgentScopedUATNeedsBothSelectors(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ownerUser := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-create-dispatch-owner"))

	request := func(name string) CreateScheduleRequest {
		return CreateScheduleRequest{
			Name:      name,
			CronExpr:  "0 * * * *",
			EventType: "dispatch_agent",
			AgentName: "scoped-worker",
		}
	}

	t.Run("unscoped project owner allowed", func(t *testing.T) {
		rec := doScheduleAgentRequest(t, srv, ownerUser, projectID, "", http.MethodPost, request("scoped-dispatch-session"))
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})

	for _, boundary := range []struct {
		name     string
		boundary TokenBoundary
	}{{"project-scoped", projectBoundary(projectID)}, {"hub-scoped", hubBoundary()}} {
		t.Run(boundary.name+" UAT with both selectors admitted", func(t *testing.T) {
			scoped := scheduleTestUAT(t, ownerUser, boundary.boundary, "scheduled_event:create", "agent:create")
			rec := doScheduleAgentRequest(t, srv, scoped, projectID, "", http.MethodPost, request("scoped-dispatch-"+boundary.name))
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		})
		t.Run(boundary.name+" UAT without scheduled_event:create refused", func(t *testing.T) {
			scoped := scheduleTestUAT(t, ownerUser, boundary.boundary, "scheduled_event:read", "agent:create")
			rec := doScheduleAgentRequest(t, srv, scoped, projectID, "", http.MethodPost, request("scoped-dispatch-nocreate-"+boundary.name))
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
		t.Run(boundary.name+" UAT without agent:create refused", func(t *testing.T) {
			scoped := scheduleTestUAT(t, ownerUser, boundary.boundary, "scheduled_event:create", "agent:read")
			rec := doScheduleAgentRequest(t, srv, scoped, projectID, "", http.MethodPost, request("scoped-dispatch-noagent-"+boundary.name))
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
}

// TestSchedule_UpdateDispatchAgentScopedUATNeedsUpdateAndAgentCreate covers
// every alternate mutation of an existing schedule that changes what a
// future dispatch_agent dispatch does or who it runs as: converting a
// message schedule to dispatch_agent, and re-targeting (editing the payload
// of) an existing dispatch_agent schedule. A project-scoped UAT needs
// scheduled_event:update and agent:create: with both it is admitted and the
// revision records the token's bounded ceiling; without either it is
// refused with 403 and nothing changes. The same unscoped user is admitted.
func TestSchedule_UpdateDispatchAgentScopedUATNeedsUpdateAndAgentCreate(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ownerUser := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-update-dispatch-owner"))
	scoped := NewScopedUserIdentity(ownerUser, projectID, []string{"scheduled_event:update", "agent:create"})
	refused := map[string]Identity{
		"without scheduled_event:update": NewScopedUserIdentity(ownerUser, projectID, []string{"scheduled_event:create", "agent:create"}),
		"without agent:create":           NewScopedUserIdentity(ownerUser, projectID, []string{"scheduled_event:update", "agent:read"}),
	}

	create := func(t *testing.T, req CreateScheduleRequest) string {
		t.Helper()
		createRec := doScheduleAgentRequest(t, srv, ownerUser, projectID, "", http.MethodPost, req)
		require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
		var sched store.Schedule
		require.NoError(t, json.NewDecoder(createRec.Body).Decode(&sched))
		return sched.ID
	}
	check := func(t *testing.T, id string, update UpdateScheduleRequest) {
		t.Helper()
		before := loadScheduleRevision(t, s, id)
		for label, identity := range refused {
			rec := doAuthoredScheduleRequest(t, srv, identity, projectID, id, http.MethodPatch, update)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
			assert.Equal(t, before, loadScheduleRevision(t, s, id), label)
		}
		rec := doAuthoredScheduleRequest(t, srv, scoped, projectID, id, http.MethodPatch, update)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after := loadScheduleRevision(t, s, id)
		assert.Equal(t, store.EffectCeilingBounded, after.Ceiling.Kind)
		assert.Equal(t, string(BoundaryKindProject), after.Ceiling.BoundaryKind)
		assert.Equal(t, projectID, after.Ceiling.BoundaryProjectID)
		assert.Equal(t, store.InitiatorCredentialKindUAT, after.Attribution.InitiatorCredentialKind)
		assert.Equal(t, before.Attribution.AuthorizationRevision+1, after.Attribution.AuthorizationRevision)
	}

	t.Run("convert message schedule to dispatch_agent", func(t *testing.T) {
		id := create(t, CreateScheduleRequest{
			Name: "message-first-scoped", CronExpr: "0 * * * *",
			EventType: "message", AgentName: "worker", Message: "ping",
		})
		check(t, id, UpdateScheduleRequest{EventType: "dispatch_agent", Payload: `{"agentName":"worker-c"}`})
	})

	t.Run("re-target existing dispatch_agent schedule", func(t *testing.T) {
		id := create(t, CreateScheduleRequest{
			Name: "existing-dispatch-scoped", CronExpr: "0 * * * *",
			EventType: "dispatch_agent", AgentName: "worker-a",
		})
		check(t, id, UpdateScheduleRequest{Payload: `{"agentName":"worker-b"}`})

		// The same unscoped user is admitted.
		rec := doScheduleAgentRequest(t, srv, ownerUser, projectID, id, http.MethodPatch,
			UpdateScheduleRequest{Payload: `{"agentName":"worker-d"}`})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

// TestSchedule_ResumeDispatchAgentScopedUATNeedsUpdateAndAgentCreate covers
// resume: resuming a paused dispatch_agent schedule re-arms future runs. A
// project-scoped UAT needs scheduled_event:update and agent:create: without
// either it is refused with 403 and the schedule stays paused; with both it
// resumes the schedule. The unscoped project owner resumes it too.
func TestSchedule_ResumeDispatchAgentScopedUATNeedsUpdateAndAgentCreate(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ownerUser := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-resume-dispatch-owner"))
	scoped := NewScopedUserIdentity(ownerUser, projectID, []string{"scheduled_event:update", "agent:create"})

	createRec := doScheduleAgentRequest(t, srv, ownerUser, projectID, "", http.MethodPost,
		CreateScheduleRequest{
			Name: "resume-dispatch-scoped", CronExpr: "0 * * * *",
			EventType: "dispatch_agent", AgentName: "resume-worker",
		})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var sched store.Schedule
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&sched))
	pause := func(t *testing.T) {
		t.Helper()
		pauseRec := doScheduleAgentRequest(t, srv, ownerUser, projectID, sched.ID+"/pause", http.MethodPost, nil)
		require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())
	}
	pause(t)

	t.Run("scoped UAT without a required selector is refused", func(t *testing.T) {
		for label, identity := range map[string]Identity{
			"without scheduled_event:update": NewScopedUserIdentity(ownerUser, projectID, []string{"scheduled_event:create", "agent:create"}),
			"without agent:create":           NewScopedUserIdentity(ownerUser, projectID, []string{"scheduled_event:update", "agent:read"}),
		} {
			rec := doScheduleAgentRequest(t, srv, identity, projectID, sched.ID+"/resume", http.MethodPost, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
			got, err := s.GetSchedule(context.Background(), sched.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ScheduleStatusPaused, got.Status, label)
		}
	})

	t.Run("scoped UAT with both selectors resumes it", func(t *testing.T) {
		rec := doScheduleAgentRequest(t, srv, scoped, projectID, sched.ID+"/resume", http.MethodPost, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("unscoped project owner can resume it", func(t *testing.T) {
		pause(t)
		rec := doScheduleAgentRequest(t, srv, ownerUser, projectID, sched.ID+"/resume", http.MethodPost, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

func TestSchedule_CreateInvalidCron(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "bad-cron",
		CronExpr:  "not a cron expression",
		EventType: "message",
		AgentName: "worker-1",
		Message:   "test",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSchedule_CreateMissingFields(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	// Missing name
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{CronExpr: "0 * * * *", EventType: "message", AgentName: "a", Message: "m"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Missing cron
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{Name: "test", EventType: "message", AgentName: "a", Message: "m"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSchedule_List(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	// Create two schedules
	for _, name := range []string{"sched-1", "sched-2"} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
			CreateScheduleRequest{
				Name: name, CronExpr: "0 * * * *", EventType: "message",
				AgentName: "worker", Message: "hello",
			})
		require.Equal(t, http.StatusCreated, rec.Code)
	}

	// List
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 2, resp.TotalCount)
	assert.Len(t, resp.Schedules, 2)
}

func TestSchedule_Get(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	// Create
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "get-test", CronExpr: "30 8 * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Get
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var got store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "get-test", got.Name)
}

func TestSchedule_PauseResume(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	// Create
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "pause-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Pause
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var paused store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&paused))
	assert.Equal(t, store.ScheduleStatusPaused, paused.Status)

	// Pause again should fail
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Resume
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/resume", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resumed store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resumed))
	assert.Equal(t, store.ScheduleStatusActive, resumed.Status)
	assert.NotNil(t, resumed.NextRunAt)
}

func TestSchedule_Delete(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	// Create
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "delete-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Delete
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Get should fail
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSchedule_Update(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	// Create
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "update-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Update
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID,
		UpdateScheduleRequest{Name: "updated-name", CronExpr: "30 9 * * *"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var updated store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "updated-name", updated.Name)
	assert.Equal(t, "30 9 * * *", updated.CronExpr)
}

func TestSchedule_History(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	// Create a schedule
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
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
			ID:         tid("hist-evt-" + string(rune('a'+i))),
			ProjectID:  projectID,
			EventType:  "message",
			FireAt:     created.CreatedAt,
			Payload:    created.Payload,
			ScheduleID: created.ID,
		}
		require.NoError(t, s.CreateScheduledEvent(ctx, evt))
	}

	// Get history
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/history", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp ListScheduledEventsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 3, resp.TotalCount)
}

// doScheduleUserRequest makes a request with the given user identity
// and calls handleSchedules directly (bypasses router auth middleware).
func doScheduleUserRequest(t *testing.T, srv *Server, identity Identity, method, projectID, schedulePath string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	urlPath := "/api/v1/projects/" + projectID + "/schedules"
	if schedulePath != "" {
		urlPath += "/" + schedulePath
	}
	req := httptest.NewRequest(method, urlPath, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}

	rec := httptest.NewRecorder()
	srv.handleSchedules(rec, req, projectID, schedulePath)
	return rec
}

func TestSchedule_NonMemberUserDenied(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	// Create a non-member user with a valid UUID
	nonMemberID := tid("sched-non-member")
	nonMember := NewAuthenticatedUser(nonMemberID, "schednonmember@test.com", "Non Member", "member", "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          nonMemberID,
		Email:       nonMember.Email(),
		DisplayName: nonMember.DisplayName(),
		Role:        "member",
		Status:      "active",
	}))

	// Create a schedule via the admin user to test get/update/delete on
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "authz-test-sched", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, createRec.Code)
	var created store.Schedule
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	t.Run("list denied", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, nonMember, http.MethodGet, projectID, "", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("get denied", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, nonMember, http.MethodGet, projectID, created.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("create denied", func(t *testing.T) {
		req := CreateScheduleRequest{
			Name: "denied", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		}
		rec := doScheduleUserRequest(t, srv, nonMember, http.MethodPost, projectID, "", req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("update denied", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, nonMember, http.MethodPatch, projectID, created.ID,
			UpdateScheduleRequest{Name: "hacked"})
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("delete denied", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, nonMember, http.MethodDelete, projectID, created.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("pause denied", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, nonMember, http.MethodPost, projectID, created.ID+"/pause", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("resume denied", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, nonMember, http.MethodPost, projectID, created.ID+"/resume", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("history denied", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, nonMember, http.MethodGet, projectID, created.ID+"/history", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestSchedule_AdminUserAllowed(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	// Admin user via dev token — all operations should succeed
	t.Run("list allowed", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("create allowed", func(t *testing.T) {
		req := CreateScheduleRequest{
			Name: "admin-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
		assert.Equal(t, http.StatusCreated, rec.Code)
	})
}

func TestSchedule_ProjectOwnerAllowed(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	// Create an owner user for the project with a valid UUID
	ownerUserID := tid("sched-owner-user")
	ownerUser := NewAuthenticatedUser(ownerUserID, "schedowner@test.com", "Owner", "member", "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          ownerUserID,
		Email:       ownerUser.Email(),
		DisplayName: ownerUser.DisplayName(),
		Role:        "member",
		Status:      "active",
	}))

	// Create a project-owner role binding — isProjectOwnerOrAdmin checks role
	// bindings, not group membership.
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, ownerUserID))

	t.Run("list allowed", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, ownerUser, http.MethodGet, projectID, "", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("create allowed", func(t *testing.T) {
		req := CreateScheduleRequest{
			Name: "owner-test", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		}
		rec := doScheduleUserRequest(t, srv, ownerUser, http.MethodPost, projectID, "", req)
		assert.Equal(t, http.StatusCreated, rec.Code)
	})
}

func TestSchedule_ProjectIsolation(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)
	ctx := context.Background()

	// Create another project
	otherProject := &store.Project{
		ID:   tid("project-other-sched"),
		Name: "Other Project",
		Slug: "other-project-sched",
	}
	require.NoError(t, srv.store.CreateProject(ctx, otherProject))

	// Create schedule in first project
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "isolated", CronExpr: "0 * * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// Try to access from another project
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+otherProject.ID+"/schedules/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSchedule_ListFilterByCreatedByAndTargetAgent(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	agentA, err := s.GetAgent(ctx, authzHelperAgentID)
	require.NoError(t, err)
	agentB := &store.Agent{
		ID:        tid("sched-filter-agent-b"),
		Slug:      "beta-agent",
		Name:      "Beta Agent",
		ProjectID: projectID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Seed schedules covering all relations:
	// 1. Created by agentA, targeting agentB by slug
	schedCreatedByA := &store.Schedule{
		ID:        tid("sched-created-by-a"),
		ProjectID: projectID,
		Name:      "created-by-a",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   `{"agentName":"beta-agent","message":"hi beta"}`,
		Status:    store.ScheduleStatusActive,
		CreatedBy: agentA.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    agentA.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
	}
	// 2. Created by user, targeting agentA by slug with wake=true
	schedTargetABySlug := &store.Schedule{
		ID:        tid("sched-target-a-slug"),
		ProjectID: projectID,
		Name:      "target-a-wake-slug",
		CronExpr:  "*/15 * * * *",
		EventType: "message",
		Payload:   `{"agentName":"` + agentA.Slug + `","message":"wake up","wake":true}`,
		Status:    store.ScheduleStatusActive,
		CreatedBy: tid("user-creator"),
	}
	// 3. Created by agentB, targeting agentA by agentId (message)
	schedTargetAByID := &store.Schedule{
		ID:        tid("sched-target-a-id"),
		ProjectID: projectID,
		Name:      "target-a-msg-id",
		CronExpr:  "0 8 * * *",
		EventType: "message",
		Payload:   `{"agentId":"` + agentA.ID + `","message":"by id"}`,
		Status:    store.ScheduleStatusActive,
		CreatedBy: agentB.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    agentB.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
	}
	// 4. Created by user, targeting agentA via exec payload
	schedTargetAExec := &store.Schedule{
		ID:        tid("sched-target-a-exec"),
		ProjectID: projectID,
		Name:      "target-a-exec",
		CronExpr:  "0 12 * * *",
		EventType: scheduledEventTypeExec,
		Payload:   `{"agentId":"` + agentA.ID + `","agentName":"` + agentA.Slug + `","command":["echo","ok"]}`,
		Status:    store.ScheduleStatusActive,
		CreatedBy: tid("user-creator"),
	}
	// 5. Self-targeted schedule: created by agentA AND targeting agentA
	schedSelfA := &store.Schedule{
		ID:        tid("sched-self-a"),
		ProjectID: projectID,
		Name:      "self-a",
		CronExpr:  "0 6 * * *",
		EventType: "message",
		Payload:   `{"agentName":"` + agentA.Slug + `","message":"self check"}`,
		Status:    store.ScheduleStatusActive,
		CreatedBy: agentA.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    agentA.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
	}
	// 6. Unrelated dispatch_agent schedule
	schedDispatch := &store.Schedule{
		ID:        tid("sched-dispatch-unrelated"),
		ProjectID: projectID,
		Name:      "dispatch-other",
		CronExpr:  "0 0 * * *",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"` + agentA.Slug + `","template":"default"}`,
		Status:    store.ScheduleStatusActive,
		CreatedBy: tid("user-creator"),
	}
	for _, sc := range []*store.Schedule{
		schedCreatedByA, schedTargetABySlug, schedTargetAByID,
		schedTargetAExec, schedSelfA, schedDispatch,
	} {
		require.NoError(t, s.CreateSchedule(ctx, sc))
	}

	namesOf := func(resp ListSchedulesResponse) []string {
		out := make([]string, len(resp.Schedules))
		for i, sc := range resp.Schedules {
			out[i] = sc.Name
		}
		return out
	}

	t.Run("createdBy filter returns schedules created by agentA", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?createdBy="+agentA.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.ElementsMatch(t, []string{"created-by-a", "self-a"}, namesOf(resp))
		assert.Equal(t, 2, resp.TotalCount)
	})

	t.Run("targetAgentId filter returns message, wake, and exec schedules targeting agentA", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?targetAgentId="+agentA.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.ElementsMatch(t, []string{
			"target-a-wake-slug",
			"target-a-msg-id",
			"target-a-exec",
			"self-a",
		}, namesOf(resp))
		assert.Equal(t, 4, resp.TotalCount)
	})

	t.Run("targetAgentId with limit returns all matching rows unpaginated", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?limit=1&targetAgentId="+agentA.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.ElementsMatch(t, []string{
			"target-a-wake-slug",
			"target-a-msg-id",
			"target-a-exec",
			"self-a",
		}, namesOf(resp))
		assert.Empty(t, resp.NextCursor)
		assert.Equal(t, 4, resp.TotalCount)
	})

	t.Run("targetAgentId with cursor returns 400", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?cursor=opaque&targetAgentId="+agentA.ID, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "cursor cannot be combined with targetAgentId")
	})

	t.Run("unknown targetAgentId returns empty list", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?targetAgentId="+tid("no-such-agent"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Empty(t, resp.Schedules)
		assert.Equal(t, 0, resp.TotalCount)
	})

	t.Run("non-member user is denied for both filters", func(t *testing.T) {
		nonMember := NewAuthenticatedUser(tid("sched-filter-nonmember"), "outsider@test.com", "Outsider", "member", "api")

		reqCreated := authoredRequest(t, nonMember, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?createdBy="+agentA.ID, nil)
		recCreated := httptest.NewRecorder()
		srv.handleSchedules(recCreated, reqCreated, projectID, "")
		assert.Equal(t, http.StatusForbidden, recCreated.Code)

		reqTarget := authoredRequest(t, nonMember, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?targetAgentId="+agentA.ID, nil)
		recTarget := httptest.NewRecorder()
		srv.handleSchedules(recTarget, reqTarget, projectID, "")
		assert.Equal(t, http.StatusForbidden, recTarget.Code)
	})

	t.Run("agent caller cannot see another principal's schedules via createdBy or targetAgentId", func(t *testing.T) {
		callerA := authzHelperAgent(projectID, ScopeProjectRead)

		// Asking for createdBy=agentB returns empty because callerA holds none of agentB's schedules.
		reqCreated := authoredRequest(t, callerA, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?createdBy="+agentB.ID, nil)
		recCreated := httptest.NewRecorder()
		srv.handleSchedules(recCreated, reqCreated, projectID, "")
		require.Equal(t, http.StatusOK, recCreated.Code, recCreated.Body.String())
		var respCreated ListSchedulesResponse
		require.NoError(t, json.NewDecoder(recCreated.Body).Decode(&respCreated))
		assert.Empty(t, respCreated.Schedules)
		assert.Equal(t, ScheduleListScopeCaller, respCreated.Scope)

		// Asking for targetAgentId=agentA as callerA only returns self-a (held by callerA),
		// never user-created or agentB-created schedules targeting agentA.
		reqTarget := authoredRequest(t, callerA, http.MethodGet,
			"/api/v1/projects/"+projectID+"/schedules?targetAgentId="+agentA.ID, nil)
		recTarget := httptest.NewRecorder()
		srv.handleSchedules(recTarget, reqTarget, projectID, "")
		require.Equal(t, http.StatusOK, recTarget.Code, recTarget.Body.String())
		var respTarget ListSchedulesResponse
		require.NoError(t, json.NewDecoder(recTarget.Body).Decode(&respTarget))
		assert.Equal(t, []string{"self-a"}, namesOf(respTarget))
		assert.Equal(t, ScheduleListScopeCaller, respTarget.Scope)
	})
}

func TestSchedule_CreatorNameResolution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	// 1. User with display name
	aliceID := tid("sched-creator-alice")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          aliceID,
		Email:       "alice@example.com",
		DisplayName: "Alice Example",
		Role:        "member",
		Status:      "active",
	}))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, aliceID))

	// 2. User without display name (falls back to email)
	bobID := tid("sched-creator-bob")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          bobID,
		Email:       "bob@example.com",
		DisplayName: "",
		Role:        "member",
		Status:      "active",
	}))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, bobID))

	// 3. Agent in the same project
	agentID := tid("sched-creator-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Slug:      "ops-bot",
		Name:      "Ops Bot",
		ProjectID: projectID,
		Phase:     "running",
	}))

	// 4. Agent in another project (viewer without access to that project must not see its slug)
	otherProjID := tid("sched-creator-other-proj")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:   otherProjID,
		Name: "Secret Project",
		Slug: "secret-project",
	}))
	otherAgentID := tid("sched-creator-other-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        otherAgentID,
		Slug:      "secret-bot",
		Name:      "Secret Bot",
		ProjectID: otherProjID,
		Phase:     "running",
	}))

	// 5. Soft-deleted agent (must fall back to UUID; exercises DeletedAt guard)
	deletedAgentID := tid("sched-creator-deleted-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        deletedAgentID,
		Slug:      "gone-bot",
		Name:      "Gone Bot",
		ProjectID: projectID,
		Phase:     "stopped",
	}))
	deletedAgentRow, err := s.GetAgent(ctx, deletedAgentID)
	require.NoError(t, err)
	deletedAgentRow.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, deletedAgentRow))
	reloadedDeleted, err := s.GetAgent(ctx, deletedAgentID)
	require.NoError(t, err)
	require.False(t, reloadedDeleted.DeletedAt.IsZero(), "agent must remain soft-deleted in store")

	missingID := tid("sched-creator-missing")

	mkSched := func(name, createdBy string) string {
		id := tid("sched-row-" + name)
		sc := &store.Schedule{
			ID:        id,
			ProjectID: projectID,
			Name:      name,
			CronExpr:  "0 9 * * *",
			EventType: "message",
			Payload:   `{"agentName":"worker","message":"hi"}`,
			Status:    store.ScheduleStatusActive,
			CreatedBy: createdBy,
		}
		require.NoError(t, s.CreateSchedule(ctx, sc))
		return id
	}

	aliceSchedID := mkSched("by-alice", aliceID)
	bobSchedID := mkSched("by-bob", bobID)
	agentSchedID := mkSched("by-agent", agentID)
	otherAgentSchedID := mkSched("by-other-agent", otherAgentID)
	deletedAgentSchedID := mkSched("by-deleted-agent", deletedAgentID)
	missingSchedID := mkSched("by-missing", missingID)

	aliceUser := NewAuthenticatedUser(aliceID, "alice@example.com", "Alice Example", "member", "web")

	t.Run("list resolves user display name, user email, agent slug, and omits unresolvable or unauthorized", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, aliceUser, http.MethodGet, projectID, "", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		byID := make(map[string]ScheduleResponse, len(resp.Schedules))
		for _, sc := range resp.Schedules {
			byID[sc.ID] = sc
		}

		assert.Equal(t, "Alice Example", byID[aliceSchedID].CreatedByName)
		assert.Equal(t, "user", byID[aliceSchedID].CreatedByType)

		assert.Equal(t, "bob@example.com", byID[bobSchedID].CreatedByName)
		assert.Equal(t, "user", byID[bobSchedID].CreatedByType)

		assert.Equal(t, "ops-bot", byID[agentSchedID].CreatedByName)
		assert.Equal(t, "agent", byID[agentSchedID].CreatedByType)

		// Agent in another project that alice cannot read: omitted (falls back to UUID)
		assert.Empty(t, byID[otherAgentSchedID].CreatedByName)
		assert.Empty(t, byID[otherAgentSchedID].CreatedByType)
		assert.Equal(t, otherAgentID, byID[otherAgentSchedID].CreatedBy)

		// Deleted agent: omitted (falls back to UUID)
		assert.Empty(t, byID[deletedAgentSchedID].CreatedByName)
		assert.Empty(t, byID[deletedAgentSchedID].CreatedByType)
		assert.Equal(t, deletedAgentID, byID[deletedAgentSchedID].CreatedBy)

		// Non-existent creator: omitted (falls back to UUID)
		assert.Empty(t, byID[missingSchedID].CreatedByName)
		assert.Empty(t, byID[missingSchedID].CreatedByType)
		assert.Equal(t, missingID, byID[missingSchedID].CreatedBy)
	})

	t.Run("get single schedule resolves creator and omits soft-deleted agent", func(t *testing.T) {
		rec := doScheduleUserRequest(t, srv, aliceUser, http.MethodGet, projectID, agentSchedID, nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ScheduleResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, "ops-bot", resp.CreatedByName)
		assert.Equal(t, "agent", resp.CreatedByType)

		// Single-row GET uses GetAgent (which returns soft-deleted rows), so this
		// exercises the DeletedAt guard in resolveCreatorAgent.
		delRec := doScheduleUserRequest(t, srv, aliceUser, http.MethodGet, projectID, deletedAgentSchedID, nil)
		require.Equal(t, http.StatusOK, delRec.Code)

		var delResp ScheduleResponse
		require.NoError(t, json.NewDecoder(delRec.Body).Decode(&delResp))
		assert.Empty(t, delResp.CreatedByName)
		assert.Empty(t, delResp.CreatedByType)
		assert.Equal(t, deletedAgentID, delResp.CreatedBy)
	})

	t.Run("scoped token without user.read does not resolve users", func(t *testing.T) {
		scopedAlice := NewScopedUserIdentity(aliceUser, projectID, []string{"scheduled_event:read", "scheduled_event:list", "agent:read"})
		rec := doScheduleUserRequest(t, srv, scopedAlice, http.MethodGet, projectID, "", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		byID := make(map[string]ScheduleResponse, len(resp.Schedules))
		for _, sc := range resp.Schedules {
			byID[sc.ID] = sc
		}

		// Mirrors GET /api/v1/users/{id}: no user.read means no user names (self or other)
		assert.Empty(t, byID[aliceSchedID].CreatedByName)
		assert.Empty(t, byID[aliceSchedID].CreatedByType)
		assert.Equal(t, aliceID, byID[aliceSchedID].CreatedBy)

		assert.Empty(t, byID[bobSchedID].CreatedByName)
		assert.Empty(t, byID[bobSchedID].CreatedByType)
		assert.Equal(t, bobID, byID[bobSchedID].CreatedBy)

		// Token has agent:read, so same-project agent still resolves
		assert.Equal(t, "ops-bot", byID[agentSchedID].CreatedByName)
		assert.Equal(t, "agent", byID[agentSchedID].CreatedByType)
	})

	t.Run("agent caller sees own slug only, never user names or other agents", func(t *testing.T) {
		peerAgentID := tid("sched-creator-peer-agent")
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID:        peerAgentID,
			Slug:      "peer-bot",
			Name:      "Peer Bot",
			ProjectID: projectID,
			Phase:     "running",
		}))

		mkAgentOwnedSched := func(name, createdBy string) string {
			id := tid("sched-agent-owned-" + name)
			sc := &store.Schedule{
				ID:        id,
				ProjectID: projectID,
				Name:      name,
				CronExpr:  "0 9 * * *",
				EventType: "message",
				Payload:   `{"agentName":"worker","message":"hi"}`,
				Status:    store.ScheduleStatusActive,
				CreatedBy: createdBy,
				InitiatorAttribution: store.InitiatorAttribution{
					InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
					InitiatorPrincipalID:    agentID,
					InitiatorCredentialKind: "agent",
					AttributionVersion:      1,
				},
			}
			require.NoError(t, s.CreateSchedule(ctx, sc))
			return id
		}

		selfOwnedID := mkAgentOwnedSched("owned-by-self", agentID)
		userCreatedID := mkAgentOwnedSched("owned-created-by-user", aliceID)
		peerCreatedID := mkAgentOwnedSched("owned-created-by-peer", peerAgentID)

		agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: agentID},
			ProjectID: projectID,
			Scopes:    []AgentTokenScope{ScopeProjectRead},
		}}
		rec := doScheduleAgentRequest(t, srv, agentIdent, projectID, "", http.MethodGet, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		byID := make(map[string]ScheduleResponse, len(resp.Schedules))
		for _, sc := range resp.Schedules {
			byID[sc.ID] = sc
		}
		require.Contains(t, byID, selfOwnedID)
		require.Contains(t, byID, userCreatedID)
		require.Contains(t, byID, peerCreatedID)

		// Self resolves to own slug
		assert.Equal(t, "ops-bot", byID[selfOwnedID].CreatedByName)
		assert.Equal(t, "agent", byID[selfOwnedID].CreatedByType)

		// User creator is hidden from agent callers
		assert.Empty(t, byID[userCreatedID].CreatedByName)
		assert.Empty(t, byID[userCreatedID].CreatedByType)
		assert.Equal(t, aliceID, byID[userCreatedID].CreatedBy)

		// Another agent's slug is hidden from agent callers
		assert.Empty(t, byID[peerCreatedID].CreatedByName)
		assert.Empty(t, byID[peerCreatedID].CreatedByType)
		assert.Equal(t, peerAgentID, byID[peerCreatedID].CreatedBy)
	})

	t.Run("admin scheduler authorizes creator visibility per viewer and keeps exec argv redacted", func(t *testing.T) {
		execSentinel := "SUPER_SECRET_ADMIN_EXEC_ARGV"
		execSchedID := tid("sched-row-admin-exec")
		require.NoError(t, s.CreateSchedule(ctx, &store.Schedule{
			ID:        execSchedID,
			ProjectID: projectID,
			Name:      "admin-exec-check",
			CronExpr:  "0 9 * * *",
			EventType: scheduledEventTypeExec,
			Payload:   `{"agentId":"` + agentID + `","agentName":"ops-bot","command":["sh","-c","echo ` + execSentinel + `"]}`,
			Status:    store.ScheduleStatusActive,
			CreatedBy: aliceID,
		}))

		superID := hubConfigTokenUser(t, s, "sched-admin-viewer", store.SystemRoleSuperAdmin)

		// 1. Hub token with hub_scheduler:read only (no user:read, no agent:read)
		schedOnlyKey := mintHubConfigToken(t, srv, superID, hubBoundary(), "hub_scheduler:read")

		userRec := doRequestWithToken(t, srv, schedOnlyKey, http.MethodGet, "/api/v1/users/"+aliceID, nil)
		require.Equal(t, http.StatusForbidden, userRec.Code, "token without user:read must be denied GET /users/{id}")

		adminRec := doRequestWithToken(t, srv, schedOnlyKey, http.MethodGet, "/api/v1/admin/scheduler", nil)
		require.Equal(t, http.StatusOK, adminRec.Code, adminRec.Body.String())
		assert.NotContains(t, adminRec.Body.String(), execSentinel)

		var adminResp struct {
			RecurringSchedules []ScheduleResponse `json:"recurringSchedules"`
		}
		require.NoError(t, json.NewDecoder(adminRec.Body).Decode(&adminResp))
		byID := make(map[string]ScheduleResponse, len(adminResp.RecurringSchedules))
		for _, sc := range adminResp.RecurringSchedules {
			byID[sc.ID] = sc
		}
		require.Contains(t, byID, aliceSchedID)
		require.Contains(t, byID, agentSchedID)

		// Must NOT expose user display name/email or agent slug without user:read / agent:read
		assert.Empty(t, byID[aliceSchedID].CreatedByName)
		assert.Empty(t, byID[aliceSchedID].CreatedByType)
		assert.Equal(t, aliceID, byID[aliceSchedID].CreatedBy)

		assert.Empty(t, byID[bobSchedID].CreatedByName)
		assert.Empty(t, byID[bobSchedID].CreatedByType)

		assert.Empty(t, byID[agentSchedID].CreatedByName)
		assert.Empty(t, byID[agentSchedID].CreatedByType)
		assert.Equal(t, agentID, byID[agentSchedID].CreatedBy)

		// 2. Hub token with hub_scheduler:read + user:read + agent:read DOES resolve creators
		fullReadKey := mintHubConfigToken(t, srv, superID, hubBoundary(), "hub_scheduler:read", "user:read", "agent:read")
		fullRec := doRequestWithToken(t, srv, fullReadKey, http.MethodGet, "/api/v1/admin/scheduler", nil)
		require.Equal(t, http.StatusOK, fullRec.Code, fullRec.Body.String())
		assert.NotContains(t, fullRec.Body.String(), execSentinel)

		var fullResp struct {
			RecurringSchedules []ScheduleResponse `json:"recurringSchedules"`
		}
		require.NoError(t, json.NewDecoder(fullRec.Body).Decode(&fullResp))
		fullByID := make(map[string]ScheduleResponse, len(fullResp.RecurringSchedules))
		for _, sc := range fullResp.RecurringSchedules {
			fullByID[sc.ID] = sc
		}
		assert.Equal(t, "Alice Example", fullByID[aliceSchedID].CreatedByName)
		assert.Equal(t, "user", fullByID[aliceSchedID].CreatedByType)
		assert.Equal(t, "ops-bot", fullByID[agentSchedID].CreatedByName)
		assert.Equal(t, "agent", fullByID[agentSchedID].CreatedByType)
		assert.Empty(t, fullByID[deletedAgentSchedID].CreatedByName)
		assert.Empty(t, fullByID[deletedAgentSchedID].CreatedByType)

		// 3. Interactive super-admin session resolves creators while still redacting exec argv
		superUser := NewAuthenticatedUser(superID, superID+"@test.com", "Super Admin", store.UserRoleAdmin, "web")
		sessRec := requestAsIdentity(t, srv, superUser, http.MethodGet, "/api/v1/admin/scheduler", nil)
		require.Equal(t, http.StatusOK, sessRec.Code, sessRec.Body.String())
		assert.NotContains(t, sessRec.Body.String(), execSentinel, "admin view must still redact exec argv")

		var sessResp struct {
			RecurringSchedules []ScheduleResponse `json:"recurringSchedules"`
		}
		require.NoError(t, json.NewDecoder(sessRec.Body).Decode(&sessResp))
		sessByID := make(map[string]ScheduleResponse, len(sessResp.RecurringSchedules))
		for _, sc := range sessResp.RecurringSchedules {
			sessByID[sc.ID] = sc
		}
		assert.Equal(t, "Alice Example", sessByID[aliceSchedID].CreatedByName)
		assert.Equal(t, "user", sessByID[aliceSchedID].CreatedByType)
		assert.Equal(t, "ops-bot", sessByID[agentSchedID].CreatedByName)
		assert.Equal(t, "agent", sessByID[agentSchedID].CreatedByType)
	})
}

type scheduleCreatorLookupSpyStore struct {
	store.Store
	fault           *storeFaultSwitch
	getUserCalls    atomic.Int64
	getAgentCalls   atomic.Int64
	getUsersByIDs   atomic.Int64
	getAgentsByIDs  atomic.Int64
	failUsersBatch  atomic.Bool
	failAgentsBatch atomic.Bool
}

func (s *scheduleCreatorLookupSpyStore) reset() {
	s.getUserCalls.Store(0)
	s.getAgentCalls.Store(0)
	s.getUsersByIDs.Store(0)
	s.getAgentsByIDs.Store(0)
	s.failUsersBatch.Store(false)
	s.failAgentsBatch.Store(false)
}

func (s *scheduleCreatorLookupSpyStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.fault.Active() {
		s.getUserCalls.Add(1)
	}
	return s.Store.GetUser(ctx, id)
}

func (s *scheduleCreatorLookupSpyStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fault.Active() {
		s.getAgentCalls.Add(1)
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *scheduleCreatorLookupSpyStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*store.User, error) {
	if s.fault.Active() {
		s.getUsersByIDs.Add(1)
		if s.failUsersBatch.Load() {
			return nil, errors.New("injected batch user lookup failure")
		}
	}
	return s.Store.GetUsersByIDs(ctx, ids)
}

func (s *scheduleCreatorLookupSpyStore) GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	if s.fault.Active() {
		s.getAgentsByIDs.Add(1)
		if s.failAgentsBatch.Load() {
			return nil, errors.New("injected batch agent lookup failure")
		}
	}
	return s.Store.GetAgentsByIDs(ctx, ids)
}

func TestSchedule_CreatorNameResolutionBatchesLookups(t *testing.T) {
	srv, s, spy, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *scheduleCreatorLookupSpyStore {
		return &scheduleCreatorLookupSpyStore{Store: inner, fault: f}
	})
	srv, s, projectID := initScheduleTest(t, srv, s)
	ctx := context.Background()

	u1ID := tid("batch-creator-u1")
	u2ID := tid("batch-creator-u2")
	for _, u := range []*store.User{
		{ID: u1ID, Email: "u1@example.com", DisplayName: "User One", Role: "member", Status: "active"},
		{ID: u2ID, Email: "u2@example.com", DisplayName: "", Role: "member", Status: "active"},
	} {
		require.NoError(t, s.CreateUser(ctx, u))
	}
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, u1ID))

	a1ID := tid("batch-creator-a1")
	a2ID := tid("batch-creator-a2")
	for _, a := range []*store.Agent{
		{ID: a1ID, Slug: "batch-bot-1", Name: "Batch Bot 1", ProjectID: projectID, Phase: "running"},
		{ID: a2ID, Slug: "batch-bot-2", Name: "Batch Bot 2", ProjectID: projectID, Phase: "running"},
	} {
		require.NoError(t, s.CreateAgent(ctx, a))
	}

	missingID := tid("batch-creator-missing")
	mkSched := func(projID, name, createdBy string) string {
		id := tid("batch-sched-" + name)
		require.NoError(t, s.CreateSchedule(ctx, &store.Schedule{
			ID:        id,
			ProjectID: projID,
			Name:      name,
			CronExpr:  "0 9 * * *",
			EventType: "message",
			Payload:   `{"agentName":"worker","message":"hi"}`,
			Status:    store.ScheduleStatusActive,
			CreatedBy: createdBy,
		}))
		return id
	}

	sU1 := mkSched(projectID, "u1", u1ID)
	sU2 := mkSched(projectID, "u2", u2ID)
	sA1 := mkSched(projectID, "a1", a1ID)
	sA2 := mkSched(projectID, "a2", a2ID)
	sMissing := mkSched(projectID, "missing", missingID)

	// Separate project with only user-created schedules to verify agent batch is skipped.
	userOnlyProjID := tid("batch-user-only-proj")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:   userOnlyProjID,
		Name: "User Only Schedules",
		Slug: "user-only-schedules",
	}))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, userOnlyProjID, u1ID))
	mkSched(userOnlyProjID, "only-u1", u1ID)
	mkSched(userOnlyProjID, "only-u2", u2ID)

	viewer := NewAuthenticatedUser(u1ID, "u1@example.com", "User One", "member", "web")
	fault.Arm()

	t.Run("mixed creators resolved in one user batch and one agent batch", func(t *testing.T) {
		spy.reset()
		rec := doScheduleUserRequest(t, srv, viewer, http.MethodGet, projectID, "", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.Equal(t, int64(1), spy.getUsersByIDs.Load(), "expected one GetUsersByIDs batch call")
		assert.Equal(t, int64(1), spy.getAgentsByIDs.Load(), "expected one GetAgentsByIDs batch call")
		assert.Zero(t, spy.getUserCalls.Load(), "expected zero per-row GetUser calls")
		assert.Zero(t, spy.getAgentCalls.Load(), "expected zero per-row GetAgent calls")

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		byID := make(map[string]ScheduleResponse, len(resp.Schedules))
		for _, sc := range resp.Schedules {
			byID[sc.ID] = sc
		}
		assert.Equal(t, "User One", byID[sU1].CreatedByName)
		assert.Equal(t, "user", byID[sU1].CreatedByType)
		assert.Equal(t, "u2@example.com", byID[sU2].CreatedByName)
		assert.Equal(t, "user", byID[sU2].CreatedByType)
		assert.Equal(t, "batch-bot-1", byID[sA1].CreatedByName)
		assert.Equal(t, "agent", byID[sA1].CreatedByType)
		assert.Equal(t, "batch-bot-2", byID[sA2].CreatedByName)
		assert.Equal(t, "agent", byID[sA2].CreatedByType)
		assert.Empty(t, byID[sMissing].CreatedByName)
		assert.Empty(t, byID[sMissing].CreatedByType)
	})

	t.Run("all user creators skips agent batch lookup", func(t *testing.T) {
		spy.reset()
		rec := doScheduleUserRequest(t, srv, viewer, http.MethodGet, userOnlyProjID, "", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.Equal(t, int64(1), spy.getUsersByIDs.Load())
		assert.Zero(t, spy.getAgentsByIDs.Load())
		assert.Zero(t, spy.getUserCalls.Load())
		assert.Zero(t, spy.getAgentCalls.Load())
	})

	t.Run("batch lookup error fails closed without per-row fallback", func(t *testing.T) {
		spy.reset()
		spy.failUsersBatch.Store(true)
		rec := doScheduleUserRequest(t, srv, viewer, http.MethodGet, projectID, "", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.Equal(t, int64(1), spy.getUsersByIDs.Load())
		assert.Equal(t, int64(1), spy.getAgentsByIDs.Load())
		assert.Zero(t, spy.getUserCalls.Load())
		assert.Zero(t, spy.getAgentCalls.Load())

		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		byID := make(map[string]ScheduleResponse, len(resp.Schedules))
		for _, sc := range resp.Schedules {
			byID[sc.ID] = sc
		}
		assert.Empty(t, byID[sU1].CreatedByName)
		assert.Equal(t, "batch-bot-1", byID[sA1].CreatedByName)

		spy.reset()
		spy.failAgentsBatch.Store(true)
		rec = doScheduleUserRequest(t, srv, viewer, http.MethodGet, projectID, "", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.Equal(t, int64(1), spy.getUsersByIDs.Load())
		assert.Equal(t, int64(1), spy.getAgentsByIDs.Load())
		assert.Zero(t, spy.getUserCalls.Load())
		assert.Zero(t, spy.getAgentCalls.Load())

		resp = ListSchedulesResponse{}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		byID = make(map[string]ScheduleResponse, len(resp.Schedules))
		for _, sc := range resp.Schedules {
			byID[sc.ID] = sc
		}
		assert.Equal(t, "User One", byID[sU1].CreatedByName)
		assert.Empty(t, byID[sA1].CreatedByName)
	})
}
