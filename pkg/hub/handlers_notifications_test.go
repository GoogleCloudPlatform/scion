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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupNotificationHandlerTest creates a test server with a project, agent, and
// user subscription with some notifications already stored.
func setupNotificationHandlerTest(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-notif-handler",
		Name: "Notif Handler Project",
		Slug: "notif-handler-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-watched",
		Slug:    "watched-agent",
		Name:    "Watched Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// The dev auth middleware creates a user identity with a deterministic ID.
	// We use DevUserID as the subscriber ID to match what the middleware produces.
	userID := DevUserID

	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agent.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      userID,
		ProjectID:           project.ID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedAt:         time.Now(),
		CreatedBy:         "test",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create two notifications: one acknowledged, one not
	notif1 := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        agent.ID,
		ProjectID:        project.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   userID,
		Status:         "COMPLETED",
		Message:        "watched-agent has reached a state of COMPLETED",
		Dispatched:     true,
		Acknowledged:   false,
		CreatedAt:      time.Now().Add(-10 * time.Minute),
	}
	require.NoError(t, s.CreateNotification(ctx, notif1))

	notif2 := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        agent.ID,
		ProjectID:        project.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   userID,
		Status:         "WAITING_FOR_INPUT",
		Message:        "watched-agent is WAITING_FOR_INPUT",
		Dispatched:     true,
		Acknowledged:   true,
		CreatedAt:      time.Now().Add(-5 * time.Minute),
	}
	require.NoError(t, s.CreateNotification(ctx, notif2))

	return srv, s, notif1.ID
}

func TestHandleNotifications_ListUnacknowledged(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/notifications", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var notifs []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&notifs))

	// Only the unacknowledged notification should be returned
	assert.Len(t, notifs, 1)
	assert.Equal(t, "COMPLETED", notifs[0].Status)
	assert.False(t, notifs[0].Acknowledged)
}

func TestHandleNotifications_ListAll(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/notifications?acknowledged=true", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var notifs []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&notifs))

	// Both notifications should be returned
	assert.Len(t, notifs, 2)
}

func TestHandleNotifications_AcknowledgeSingle(t *testing.T) {
	srv, s, notifID := setupNotificationHandlerTest(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/"+notifID+"/ack", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "ok", resp["status"])

	// Verify the notification is now acknowledged
	notifs, err := s.GetNotifications(context.Background(), "user", DevUserID, true)
	require.NoError(t, err)
	for _, n := range notifs {
		if n.ID == notifID {
			assert.True(t, n.Acknowledged)
		}
	}
}

func TestHandleNotifications_AcknowledgeAll(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/ack-all", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "ok", resp["status"])

	// All notifications should now be acknowledged
	notifs, err := s.GetNotifications(context.Background(), "user", DevUserID, true)
	require.NoError(t, err)
	for _, n := range notifs {
		assert.True(t, n.Acknowledged, "notification %s should be acknowledged", n.ID)
	}
}

func TestHandleNotifications_AcknowledgeNotFound(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/nonexistent-id/ack", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleNotifications_RejectAgentToken(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	// Create an agent and generate a token for it
	project := &store.Project{
		ID:   "project-agent-auth",
		Name: "Agent Auth Project",
		Slug: "agent-auth-project",
	}
	_ = s.CreateProject(ctx, project)

	agent := &store.Agent{
		ID:      "agent-auth-test",
		Slug:    "auth-agent",
		Name:    "Auth Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	agentToken, err := tokenSvc.GenerateAgentToken(agent.ID, project.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	// Try to access notifications with an agent token
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	req.Header.Set("X-Scion-Agent-Token", agentToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandleNotifications_MethodNotAllowed(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	// POST to the list endpoint should fail
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandleNotifications_FilterByAgent(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	// The setup already created "agent-watched" with user notifications for DevUserID.
	// Create a second agent that watches "agent-watched", so "agent-watched" is the
	// subscriber (simulating notifications sent TO the watched agent).
	agent2 := &store.Agent{
		ID:      "agent-other",
		Slug:    "other-agent",
		Name:    "Other Agent",
		ProjectID: "project-notif-handler",
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Create subscription: agent-watched subscribes to agent-other
	sub2 := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           "agent-other",
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      "agent-watched",
		ProjectID:           "project-notif-handler",
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now(),
		CreatedBy:         "test",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub2))

	// Notification sent TO agent-watched (subscriber)
	agentNotif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub2.ID,
		AgentID:        "agent-other",
		ProjectID:        "project-notif-handler",
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "agent-watched",
		Status:         "COMPLETED",
		Message:        "agent-other completed (to agent-watched)",
		Dispatched:     true,
		Acknowledged:   false,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, s.CreateNotification(ctx, agentNotif))

	// GET with agentId filter
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/notifications?agentId=agent-watched", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		UserNotifications  []store.Notification `json:"userNotifications"`
		AgentNotifications []store.Notification `json:"agentNotifications"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// User notifications: 1 unacknowledged for this agent (notif1 from setup)
	assert.Len(t, resp.UserNotifications, 1)
	assert.Equal(t, "COMPLETED", resp.UserNotifications[0].Status)

	// Agent notifications: notifications sent TO agent-watched
	assert.Len(t, resp.AgentNotifications, 1)
	assert.Equal(t, "agent-watched", resp.AgentNotifications[0].SubscriberID)
}

func TestHandleNotifications_FilterByAgent_NoResults(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	// Query for an agent with no notifications
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/notifications?agentId=nonexistent-agent", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		UserNotifications  []store.Notification `json:"userNotifications"`
		AgentNotifications []store.Notification `json:"agentNotifications"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.UserNotifications)
	assert.Empty(t, resp.AgentNotifications)
}

func TestHandleNotifications_EmptyList(t *testing.T) {
	srv, _ := testServer(t)

	// No notifications exist for this user
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/notifications", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var notifs []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&notifs))
	assert.Empty(t, notifs)
}

// setupProjectWithBroker creates a project with a registered runtime broker for
// agent creation tests.
func setupProjectWithBroker(t *testing.T, s store.Store, projectID, projectName string) *store.Project {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     "broker-" + projectID,
		Name:   "Test Broker",
		Slug:   "test-broker-" + projectID,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:   projectID,
		Name: projectName,
		Slug: projectID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	provider := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	return project
}

func TestCreateProjectAgent_NotifyCreatesSubscription(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := setupProjectWithBroker(t, s, "project-notify-test", "Notify Test Project")

	// Create an agent via the project-scoped endpoint with notify=true
	req := CreateAgentRequest{
		Name:   "notify-agent",
		Notify: true,
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", req)

	// Accept 201 (created) or 202 (env-gather) — either should create the subscription
	assert.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted,
		"expected 201 or 202, got %d: %s", rec.Code, rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Agent)

	// Verify a notification subscription was created for the user
	subs, err := s.GetNotificationSubscriptions(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.Len(t, subs, 1, "expected exactly 1 notification subscription for the agent")
	assert.Equal(t, store.SubscriberTypeUser, subs[0].SubscriberType)
	assert.Equal(t, DevUserID, subs[0].SubscriberID)
	assert.Equal(t, project.ID, subs[0].ProjectID)
	assert.Contains(t, subs[0].TriggerActivities, "COMPLETED")
	assert.Contains(t, subs[0].TriggerActivities, "WAITING_FOR_INPUT")
	assert.Contains(t, subs[0].TriggerActivities, "LIMITS_EXCEEDED")
	assert.Contains(t, subs[0].TriggerActivities, "STALLED")
	assert.Contains(t, subs[0].TriggerActivities, "ERROR")
}

func TestCreateProjectAgent_NoNotifyNoSubscription(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := setupProjectWithBroker(t, s, "project-no-notify-test", "No Notify Test Project")

	// Create an agent without notify
	req := CreateAgentRequest{
		Name: "no-notify-agent",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", req)
	assert.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted,
		"expected 201 or 202, got %d: %s", rec.Code, rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Agent)

	// Verify no subscription was created
	subs, err := s.GetNotificationSubscriptions(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Empty(t, subs, "expected no notification subscriptions when notify is false")
}

// =============================================================================
// Subscription CRUD Endpoint Tests
// =============================================================================

func TestHandleSubscriptions_CreateAgentScoped(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)

	req := createSubscriptionRequest{
		Scope:             "agent",
		AgentID:           "agent-watched",
		ProjectID:           "project-notif-handler",
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", req)

	// May be 201 (new) or 200 (idempotent — same subscriber already exists from setup)
	assert.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusOK,
		"expected 201 or 200, got %d: %s", rec.Code, rec.Body.String())

	var sub store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sub))
	assert.Equal(t, "agent", sub.Scope)
	assert.Equal(t, "agent-watched", sub.AgentID)
	assert.Equal(t, "project-notif-handler", sub.ProjectID)

	// Verify in store
	subs, err := s.GetSubscriptionsForSubscriber(context.Background(), store.SubscriberTypeUser, DevUserID)
	require.NoError(t, err)
	assert.NotEmpty(t, subs)
}

func TestHandleSubscriptions_CreateProjectScoped(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	req := createSubscriptionRequest{
		Scope:             "project",
		ProjectID:           "project-notif-handler",
		TriggerActivities: []string{"COMPLETED"},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", req)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var sub store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sub))
	assert.Equal(t, "project", sub.Scope)
	assert.Empty(t, sub.AgentID)
	assert.Equal(t, "project-notif-handler", sub.ProjectID)
}

func TestHandleSubscriptions_CreateValidation(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	tests := []struct {
		name string
		req  createSubscriptionRequest
	}{
		{"invalid scope", createSubscriptionRequest{Scope: "bad", ProjectID: "g", TriggerActivities: []string{"COMPLETED"}}},
		{"agent scope no agentId", createSubscriptionRequest{Scope: "agent", ProjectID: "g", TriggerActivities: []string{"COMPLETED"}}},
		{"project scope with agentId", createSubscriptionRequest{Scope: "project", AgentID: "a", ProjectID: "g", TriggerActivities: []string{"COMPLETED"}}},
		{"no projectId", createSubscriptionRequest{Scope: "agent", AgentID: "a", TriggerActivities: []string{"COMPLETED"}}},
		{"no triggers", createSubscriptionRequest{Scope: "agent", AgentID: "a", ProjectID: "g"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", tt.req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestHandleSubscriptions_List(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	// Create a project-scoped subscription
	createReq := createSubscriptionRequest{
		Scope:             "project",
		ProjectID:           "project-notif-handler",
		TriggerActivities: []string{"COMPLETED"},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)

	// List all subscriptions
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/notifications/subscriptions", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var subs []store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&subs))
	// At least 2: one from setup (agent-scoped) + one we just created (project-scoped)
	assert.GreaterOrEqual(t, len(subs), 2)

	// Filter by scope
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/notifications/subscriptions?scope=project", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var projectSubs []store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&projectSubs))
	assert.Len(t, projectSubs, 1)
	assert.Equal(t, "project", projectSubs[0].Scope)
}

func TestHandleSubscriptions_Delete(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	// Create a new subscription to delete
	createReq := createSubscriptionRequest{
		Scope:             "project",
		ProjectID:           "project-notif-handler",
		TriggerActivities: []string{"COMPLETED"},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)

	var sub store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sub))
	require.NotEmpty(t, sub.ID)

	// Delete it
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/notifications/subscriptions/"+sub.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify deleted
	_, err := s.GetNotificationSubscription(ctx, sub.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestHandleSubscriptions_DeleteNotFound(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/notifications/subscriptions/nonexistent-id", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
