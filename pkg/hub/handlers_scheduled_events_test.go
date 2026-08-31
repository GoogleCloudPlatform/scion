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
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		ID:   tid("project-sched-test"),
		Name: "Scheduler Test Project",
		Slug: "sched-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	return srv, s, project.ID
}

func doScheduledEventAgentRequest(t *testing.T, srv *Server, identity Identity, projectID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}

	rec := httptest.NewRecorder()
	srv.handleScheduledEvents(rec, req, projectID, "")
	return rec
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

func TestScheduledEvent_CreateDispatchAgentRequiresAgentCreateScope(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	req := CreateScheduledEventRequest{
		EventType: "dispatch_agent",
		FireIn:    "30m",
		AgentName: "scheduled-worker",
	}

	rec := doScheduledEventAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead), projectID, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), string(ScopeAgentCreate))

	rec = doScheduledEventAgentRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, req)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
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
			ID:        tid("list-evt-" + string(rune('a'+i))),
			ProjectID: projectID,
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
		ID:        tid("get-evt-1"),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   `{"message":"get me"}`,
		Status:    store.ScheduledEventPending,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events/"+tid("get-evt-1")+"", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var got store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, tid("get-evt-1"), got.ID)
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
		ID:   tid("project-sched-other"),
		Name: "Other Project",
		Slug: "other-project",
	}
	require.NoError(t, s.CreateProject(ctx, project2))

	// Create event in first project
	evt := &store.ScheduledEvent{
		ID:        tid("wrong-project-evt"),
		ProjectID: projectID,
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
		ID:        tid("cancel-evt-1"),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   `{"message":"cancel me"}`,
		Status:    store.ScheduledEventPending,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+projectID+"/scheduled-events/"+tid("cancel-evt-1")+"", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify it was cancelled in the store
	got, err := s.GetScheduledEvent(ctx, tid("cancel-evt-1"))
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

// doScheduledEventUserRequest makes a request with the given user identity
// and calls handleScheduledEvents directly (bypasses router auth middleware).
func doScheduledEventUserRequest(t *testing.T, srv *Server, identity Identity, method, projectID, eventPath string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	urlPath := "/api/v1/projects/" + projectID + "/scheduled-events"
	if eventPath != "" {
		urlPath += "/" + eventPath
	}
	req := httptest.NewRequest(method, urlPath, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}

	rec := httptest.NewRecorder()
	srv.handleScheduledEvents(rec, req, projectID, eventPath)
	return rec
}

func TestScheduledEvent_NonMemberUserDenied(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ctx := context.Background()

	// Create a non-member user (not in the project's members group).
	// Use tid() for a valid UUID.
	nonMemberID := tid("sched-evt-non-member")
	nonMember := NewAuthenticatedUser(nonMemberID, "nonmember@test.com", "Non Member", "member", "api")

	// Ensure the user exists in the store (required for authz group lookups)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          nonMemberID,
		Email:       nonMember.Email(),
		DisplayName: nonMember.DisplayName(),
		Role:        "member",
		Status:      "active",
	}))

	// Create an event to test get/cancel on
	evt := &store.ScheduledEvent{
		ID:        tid("authz-test-evt"),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   `{"message":"test"}`,
		Status:    store.ScheduledEventPending,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	t.Run("list denied", func(t *testing.T) {
		rec := doScheduledEventUserRequest(t, srv, nonMember, http.MethodGet, projectID, "", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("get denied", func(t *testing.T) {
		rec := doScheduledEventUserRequest(t, srv, nonMember, http.MethodGet, projectID, evt.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("create denied", func(t *testing.T) {
		req := CreateScheduledEventRequest{
			EventType: "message",
			FireIn:    "30m",
			AgentName: "test-agent",
			Message:   "Hello",
		}
		rec := doScheduledEventUserRequest(t, srv, nonMember, http.MethodPost, projectID, "", req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("cancel denied", func(t *testing.T) {
		rec := doScheduledEventUserRequest(t, srv, nonMember, http.MethodDelete, projectID, evt.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestScheduledEvent_AdminUserAllowed(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	// The dev token in doRequest creates an admin user that passes the admin
	// bypass in CheckAccess. All four operations should succeed.

	t.Run("list allowed", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("create allowed", func(t *testing.T) {
		req := CreateScheduledEventRequest{
			EventType: "message",
			FireIn:    "30m",
			AgentName: "test-agent",
			Message:   "Hello",
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
		assert.Equal(t, http.StatusCreated, rec.Code)
	})
}

func TestScheduledEvent_HubAdminCrossProjectAllowed(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ctx := context.Background()

	// Seed role definitions so the hub-admin role exists with the
	// scheduled_event permissions we added to hubAdminPermissionIDs().
	seedRoleDefinitions(ctx, s)

	// Create a hub-admin user (role="member", not "admin") who is NOT a
	// member of the target project. The only authority this user has over
	// the project's scheduled events comes from the hub-admin role binding.
	hubAdminUserID := tid("sched-hub-admin")
	hubAdminUser := NewAuthenticatedUser(hubAdminUserID, "hubadmin@test.com", "Hub Admin", "member", "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          hubAdminUserID,
		Email:       hubAdminUser.Email(),
		DisplayName: hubAdminUser.DisplayName(),
		Role:        "member",
		Status:      "active",
	}))

	// Give the user a system-scoped hub-admin role binding.
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin role definition must exist after seeding")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      hubAdminUserID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.NoError(t, err)

	t.Run("list allowed", func(t *testing.T) {
		rec := doScheduledEventUserRequest(t, srv, hubAdminUser, http.MethodGet, projectID, "", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("create allowed", func(t *testing.T) {
		req := CreateScheduledEventRequest{
			EventType: "message",
			FireIn:    "30m",
			AgentName: "test-agent",
			Message:   "Hello from hub admin",
		}
		rec := doScheduledEventUserRequest(t, srv, hubAdminUser, http.MethodPost, projectID, "", req)
		assert.Equal(t, http.StatusCreated, rec.Code)
	})
}

func TestScheduledEvent_ProjectOwnerAllowed(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ctx := context.Background()

	// Create a user with a valid UUID
	ownerUserID := tid("sched-evt-owner-user")
	ownerUser := NewAuthenticatedUser(ownerUserID, "schedowner@test.com", "Owner", "member", "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          ownerUserID,
		Email:       ownerUser.Email(),
		DisplayName: ownerUser.DisplayName(),
		Role:        "member",
		Status:      "active",
	}))

	// Look up the project for its slug
	project, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)

	// Create the project's members group and add user as owner
	srv.createProjectMembersGroup(ctx, project)

	// Create a project-owner role binding — isProjectOwnerOrAdmin checks role
	// bindings, not group membership.
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, ownerUserID))

	t.Run("list allowed", func(t *testing.T) {
		rec := doScheduledEventUserRequest(t, srv, ownerUser, http.MethodGet, projectID, "", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("create allowed", func(t *testing.T) {
		req := CreateScheduledEventRequest{
			EventType: "message",
			FireIn:    "30m",
			AgentName: "test-agent",
			Message:   "Hello",
		}
		rec := doScheduledEventUserRequest(t, srv, ownerUser, http.MethodPost, projectID, "", req)
		assert.Equal(t, http.StatusCreated, rec.Code)
	})
}

func TestScheduledEvent_FederatedUserAllowed(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ctx := context.Background()

	// Create a test identity that returns type "federated_user" and
	// implements UserIdentity with a UUID ID. This verifies that the
	// "federated_user" case in authorizeScheduledEventAccess reaches the
	// CheckAccess path instead of falling to the default deny.
	//
	// We use a UUID-based ID because the store requires UUIDs for user
	// records. A real FederatedUserIdentity uses "issuer:subject" as ID,
	// but the authorization code path under test only checks identity.Type()
	// and the UserIdentity type assertion — both of which this test identity
	// exercises faithfully.
	fedUserID := tid("sched-evt-fed-user")
	fedUser := &federatedTestIdentity{
		id:          fedUserID,
		email:       "fedsched@example.com",
		displayName: "Fed User",
		role:        "member",
	}

	// Register a store user so authz group/membership lookups succeed.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          fedUserID,
		Email:       fedUser.Email(),
		DisplayName: fedUser.DisplayName(),
		Role:        "member",
		Status:      "active",
	}))

	// Set up project membership infrastructure and owner role binding.
	project, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	srv.createProjectMembersGroup(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, fedUserID))

	t.Run("list allowed", func(t *testing.T) {
		rec := doScheduledEventUserRequest(t, srv, fedUser, http.MethodGet, projectID, "", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("create allowed", func(t *testing.T) {
		req := CreateScheduledEventRequest{
			EventType: "message",
			FireIn:    "30m",
			AgentName: "test-agent",
			Message:   "Hello from federated user",
		}
		rec := doScheduledEventUserRequest(t, srv, fedUser, http.MethodPost, projectID, "", req)
		assert.Equal(t, http.StatusCreated, rec.Code)
	})
}

// federatedTestIdentity implements UserIdentity with Type() = "federated_user"
// and a UUID-based ID, enabling end-to-end authorization tests through the
// store layer which requires UUID user IDs.
type federatedTestIdentity struct {
	id          string
	email       string
	displayName string
	role        string
}

func (f *federatedTestIdentity) ID() string          { return f.id }
func (f *federatedTestIdentity) Type() string        { return "federated_user" }
func (f *federatedTestIdentity) Email() string       { return f.email }
func (f *federatedTestIdentity) DisplayName() string { return f.displayName }
func (f *federatedTestIdentity) Role() string        { return f.role }

func TestScheduledEvent_UnknownIdentityTypeDenied(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	// Create a mock identity with an unknown type to verify fail-closed behavior
	unknown := &unknownTestIdentity{id: "unknown-user"}
	rec := doScheduledEventUserRequest(t, srv, unknown, http.MethodGet, projectID, "", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// unknownTestIdentity is a test identity with a type not handled by the
// authorization switch, verifying fail-closed behavior.
type unknownTestIdentity struct {
	id string
}

func (u *unknownTestIdentity) ID() string   { return u.id }
func (u *unknownTestIdentity) Type() string { return "unknown_test_type" }

func TestScheduledEvent_MethodNotAllowed(t *testing.T) {
	srv, _, projectID := setupScheduledEventTest(t)

	// PATCH on collection
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/scheduled-events", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	// POST on individual event
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events/some-id", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
