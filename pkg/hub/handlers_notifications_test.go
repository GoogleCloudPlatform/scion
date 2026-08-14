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
	"fmt"
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
		ID:   tid("project-notif-handler"),
		Name: "Notif Handler Project",
		Slug: "notif-handler-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("agent-watched"),
		Slug:      "watched-agent",
		Name:      "Watched Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
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
		ProjectID:         project.ID,
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
		ProjectID:      project.ID,
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
		ProjectID:      project.ID,
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

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/"+tid("nonexistent-id")+"/ack", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleNotifications_RejectAnonymous(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	// No identity at all: dev auth is bypassed by an (invalid) agent token,
	// which the auth middleware rejects before the handler is reached.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	req.Header.Set("X-Scion-Agent-Token", "not-a-valid-token")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
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

	// The setup already created tid("agent-watched") with user notifications for DevUserID.
	// Create a second agent that watches tid("agent-watched"), so tid("agent-watched") is the
	// subscriber (simulating notifications sent TO the watched agent).
	agent2 := &store.Agent{
		ID:        tid("agent-other"),
		Slug:      tid("other-agent"),
		Name:      "Other Agent",
		ProjectID: tid("project-notif-handler"),
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Create subscription: agent-watched subscribes to agent-other
	sub2 := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           tid("agent-other"),
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      tid("agent-watched"),
		ProjectID:         tid("project-notif-handler"),
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now(),
		CreatedBy:         "test",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub2))

	// Notification sent TO agent-watched (subscriber)
	agentNotif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub2.ID,
		AgentID:        tid("agent-other"),
		ProjectID:      tid("project-notif-handler"),
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   tid("agent-watched"),
		Status:         "COMPLETED",
		Message:        "agent-other completed (to agent-watched)",
		Dispatched:     true,
		Acknowledged:   false,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, s.CreateNotification(ctx, agentNotif))

	// GET with agentId filter
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/notifications?agentId=%s", tid("agent-watched")), nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		UserNotifications  []store.Notification `json:"userNotifications"`
		AgentNotifications []store.Notification `json:"agentNotifications"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// User notifications: 1 unacknowledged for this agent (notif1 from setup)
	require.Len(t, resp.UserNotifications, 1)
	assert.Equal(t, "COMPLETED", resp.UserNotifications[0].Status)

	// Agent notifications: notifications sent TO agent-watched
	require.Len(t, resp.AgentNotifications, 1)
	assert.Equal(t, tid("agent-watched"), resp.AgentNotifications[0].SubscriberID)
}

func TestHandleNotifications_FilterByAgent_NoResults(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	// Query for an agent with no notifications
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/notifications?agentId=%s", tid("nonexistent-agent")), nil)
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
		ID:     tid("broker-" + projectID),
		Name:   "Test Broker",
		Slug:   "test-broker-" + projectID,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:   tid(projectID),
		Name: projectName,
		Slug: projectID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
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
		AgentID:           tid("agent-watched"),
		ProjectID:         tid("project-notif-handler"),
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", req)

	// May be 201 (new) or 200 (idempotent — same subscriber already exists from setup)
	assert.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusOK,
		"expected 201 or 200, got %d: %s", rec.Code, rec.Body.String())

	var sub store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sub))
	assert.Equal(t, "agent", sub.Scope)
	assert.Equal(t, tid("agent-watched"), sub.AgentID)
	assert.Equal(t, tid("project-notif-handler"), sub.ProjectID)

	// Verify in store
	subs, err := s.GetSubscriptionsForSubscriber(context.Background(), store.SubscriberTypeUser, DevUserID)
	require.NoError(t, err)
	assert.NotEmpty(t, subs)
}

func TestHandleSubscriptions_CreateProjectScoped(t *testing.T) {
	srv, _, _ := setupNotificationHandlerTest(t)

	req := createSubscriptionRequest{
		Scope:             "project",
		ProjectID:         tid("project-notif-handler"),
		TriggerActivities: []string{"COMPLETED"},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", req)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var sub store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sub))
	assert.Equal(t, "project", sub.Scope)
	assert.Empty(t, sub.AgentID)
	assert.Equal(t, tid("project-notif-handler"), sub.ProjectID)
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
		ProjectID:         tid("project-notif-handler"),
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
		ProjectID:         tid("project-notif-handler"),
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

// =============================================================================
// Agent-caller Tests
//
// Agents authenticate with an agent token and subscribe under their slug, the
// same subscriber ID the dispatcher writes for agent subscriptions.
// =============================================================================

// setupNotificationAgentCaller creates an agent in the given project and
// returns it together with a baseline-role agent token for authenticating as
// that agent.
func setupNotificationAgentCaller(t *testing.T, srv *Server, s store.Store, projectID, slug string) (*store.Agent, string) {
	t.Helper()
	return setupNotificationAgentCallerWithScopes(t, srv, s, projectID, slug, ScopesForRole(AgentRoleBaseline))
}

// setupNotificationAgentCallerWithScopes is setupNotificationAgentCaller with
// an explicit scope set, for exercising the scope gates.
func setupNotificationAgentCallerWithScopes(t *testing.T, srv *Server, s store.Store, projectID, slug string, scopes []AgentTokenScope) (*store.Agent, string) {
	t.Helper()
	ctx := context.Background()

	agent := &store.Agent{
		// Slugs are only unique per project, so the record ID has to key off
		// both to let a test place the same slug in two projects.
		ID:        tid("agent-caller-" + projectID + "-" + slug),
		Slug:      slug,
		Name:      slug,
		ProjectID: projectID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	token, err := tokenSvc.GenerateAgentToken(agent.ID, projectID, scopes, nil)
	require.NoError(t, err)

	return agent, token
}

// newAgentSubscription stores a project-scoped subscription owned by the agent
// slug in the given project, the way the dispatcher records agent subscribers.
func newAgentSubscription(t *testing.T, s store.Store, projectID, slug string) *store.NotificationSubscription {
	t.Helper()
	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      slug,
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now(),
		CreatedBy:         slug,
	}
	require.NoError(t, s.CreateNotificationSubscription(context.Background(), sub))
	return sub
}

// newAgentNotification stores a notification addressed to the agent slug in the
// given project.
func newAgentNotification(t *testing.T, s store.Store, sub *store.NotificationSubscription, aboutAgentID, message string) *store.Notification {
	t.Helper()
	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        aboutAgentID,
		ProjectID:      sub.ProjectID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   sub.SubscriberID,
		Status:         "COMPLETED",
		Message:        message,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, s.CreateNotification(context.Background(), notif))
	return notif
}

func TestHandleNotifications_AgentListsOwnNotifications(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)

	agent, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "caller-agent")
	sub := newAgentSubscription(t, s, agent.ProjectID, agent.Slug)
	notif := newAgentNotification(t, s, sub, tid("agent-watched"), "watched-agent has reached a state of COMPLETED")

	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications", nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var notifs []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&notifs))
	require.Len(t, notifs, 1)
	assert.Equal(t, notif.ID, notifs[0].ID)
	assert.Equal(t, store.SubscriberTypeAgent, notifs[0].SubscriberType)
}

// TestHandleNotifications_AgentFiltersByAgentID pins the ?agentId= behavior for
// agent callers: it narrows the caller's own notifications to those about the
// named agent, and never widens them to another subscriber's rows.
func TestHandleNotifications_AgentFiltersByAgentID(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	other := &store.Agent{
		ID:        tid("agent-second-watched"),
		Slug:      "second-watched-agent",
		Name:      "Second Watched Agent",
		ProjectID: tid("project-notif-handler"),
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, other))

	agent, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "filter-agent")
	sub := newAgentSubscription(t, s, agent.ProjectID, agent.Slug)
	aboutWatched := newAgentNotification(t, s, sub, tid("agent-watched"), "about watched-agent")
	aboutOther := newAgentNotification(t, s, sub, other.ID, "about second-watched-agent")

	// Unfiltered: both of the caller's own notifications, and none of the
	// user's (the test setup created one for DevUserID about agent-watched).
	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications", nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var all []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&all))
	require.Len(t, all, 2)
	for _, n := range all {
		assert.Equal(t, agent.Slug, n.SubscriberID)
	}

	// Filtered by subject agent: a flat array narrowed to that agent.
	rec = doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/notifications?agentId="+tid("agent-watched"), nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var filtered []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&filtered))
	require.Len(t, filtered, 1)
	assert.Equal(t, aboutWatched.ID, filtered[0].ID)

	rec = doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/notifications?agentId="+other.ID, nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	filtered = nil
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&filtered))
	require.Len(t, filtered, 1)
	assert.Equal(t, aboutOther.ID, filtered[0].ID)
}

// TestHandleNotifications_AgentIDMustBeInCallerProject pins the containment
// check on ?agentId=: an agent caller may only name an agent in its own
// project, and an agent that does not exist is refused the same way so the
// response is not an existence oracle.
func TestHandleNotifications_AgentIDMustBeInCallerProject(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	projectB := &store.Project{
		ID:   tid("project-notif-agentid-b"),
		Name: "Project B",
		Slug: "project-notif-agentid-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	foreign := &store.Agent{
		ID:        tid("agent-foreign-subject"),
		Slug:      "foreign-subject-agent",
		Name:      "Foreign Subject Agent",
		ProjectID: projectB.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, foreign))

	agent, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "containment-agent")
	newAgentSubscription(t, s, agent.ProjectID, agent.Slug)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/notifications?agentId="+foreign.ID, nil, token)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/notifications?agentId="+tid("agent-never-created"), nil, token)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestHandleNotifications_AgentCannotReachSameSlugInOtherProject is the
// regression test for the cross-project leak: agent slugs are unique per
// project, so subscriber ID alone cannot distinguish two "shared-slug" agents.
func TestHandleNotifications_AgentCannotReachSameSlugInOtherProject(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	projectB := &store.Project{
		ID:   tid("project-notif-b"),
		Name: "Project B",
		Slug: "project-notif-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	const slug = "shared-slug"
	_, tokenA := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), slug)
	agentB, _ := setupNotificationAgentCaller(t, srv, s, projectB.ID, slug)

	subB := newAgentSubscription(t, s, projectB.ID, slug)
	notifB := newAgentNotification(t, s, subB, agentB.ID, "SECRET-PROJECT-B-CONTENT")

	// Reads must not cross the project boundary.
	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications", nil, tokenA)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "SECRET-PROJECT-B-CONTENT")

	rec = doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications/subscriptions", nil, tokenA)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var subs []store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&subs))
	assert.Empty(t, subs)

	// Writes must not either, even with the ID in hand.
	rec = doRequestWithAgentToken(t, srv, http.MethodDelete,
		"/api/v1/notifications/subscriptions/"+subB.ID, nil, tokenA)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequestWithAgentToken(t, srv, http.MethodPatch,
		"/api/v1/notifications/subscriptions/"+subB.ID,
		updateSubscriptionRequest{TriggerActivities: []string{"ERROR"}}, tokenA)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/notifications/"+notifB.ID+"/ack", nil, tokenA)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// ack-all keys on subscriber ID alone, so it is refused for agents rather
	// than silently acking every same-slug agent's notifications.
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/ack-all", nil, tokenA)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)

	// Project B's data survived intact.
	storedSub, err := s.GetNotificationSubscription(ctx, subB.ID)
	require.NoError(t, err)
	assert.NotContains(t, storedSub.TriggerActivities, "ERROR")
	storedNotif, err := s.GetNotification(ctx, notifB.ID)
	require.NoError(t, err)
	assert.False(t, storedNotif.Acknowledged)
}

func TestHandleNotifications_AgentAcknowledgesOnlyOwnNotifications(t *testing.T) {
	srv, s, userNotifID := setupNotificationHandlerTest(t)
	ctx := context.Background()

	agent, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "acking-agent")
	sub := newAgentSubscription(t, s, agent.ProjectID, agent.Slug)
	own := newAgentNotification(t, s, sub, tid("agent-watched"), "for the acking agent")

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/"+own.ID+"/ack", nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	stored, err := s.GetNotification(ctx, own.ID)
	require.NoError(t, err)
	assert.True(t, stored.Acknowledged)

	// The user's notification is off limits even though its ID is known.
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/"+userNotifID+"/ack", nil, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	stored, err = s.GetNotification(ctx, userNotifID)
	require.NoError(t, err)
	assert.False(t, stored.Acknowledged)
}

// TestHandleNotifications_AgentScopeGates checks that the notification surface
// honors agent token scopes the way every other agent-callable handler does.
func TestHandleNotifications_AgentScopeGates(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	projectID := tid("project-notif-handler")

	// readonly: reads allowed, writes refused.
	_, roToken := setupNotificationAgentCallerWithScopes(t, srv, s, projectID, "readonly-agent",
		ScopesForRole(AgentRoleReadOnly))

	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications", nil, roToken)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications/subscriptions", nil, roToken)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	createReq := createSubscriptionRequest{
		Scope:             "project",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
	}
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq, roToken)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), string(ScopeAgentNotify))

	// No read scope: reads refused too.
	_, narrowToken := setupNotificationAgentCallerWithScopes(t, srv, s, projectID, "narrow-agent",
		[]AgentTokenScope{ScopeAgentStatusUpdate})

	rec = doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications", nil, narrowToken)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), string(ScopeProjectRead))
}

// TestHandleSubscriptionTemplates_RejectsAgents pins the decision to keep agents
// off the template routes, which have no project containment of their own.
func TestHandleSubscriptionTemplates_RejectsAgents(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	tmpl := &store.SubscriptionTemplate{
		ID:                api.NewUUID(),
		Name:              "user-template",
		Scope:             store.SubscriptionScopeProject,
		TriggerActivities: []string{"COMPLETED"},
		ProjectID:         tid("project-notif-handler"),
		CreatedBy:         DevUserID,
	}
	require.NoError(t, s.CreateSubscriptionTemplate(ctx, tmpl))

	_, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "template-agent")

	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications/templates", nil, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	createReq := createTemplateRequest{
		Name:              "agent-template",
		Scope:             store.SubscriptionScopeProject,
		TriggerActivities: []string{"COMPLETED"},
		ProjectID:         tid("project-notif-handler"),
	}
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/templates", createReq, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequestWithAgentToken(t, srv, http.MethodDelete, "/api/v1/notifications/templates/"+tmpl.ID, nil, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	_, err := s.GetSubscriptionTemplate(ctx, tmpl.ID)
	assert.NoError(t, err)
}

func TestHandleSubscriptions_AgentCreateAndList(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)

	agent, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "subscriber-agent")

	createReq := createSubscriptionRequest{
		Scope:             "agent",
		AgentID:           tid("agent-watched"),
		ProjectID:         tid("project-notif-handler"),
		TriggerActivities: []string{"COMPLETED"},
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	assert.Equal(t, store.SubscriberTypeAgent, created.SubscriberType)
	assert.Equal(t, agent.Slug, created.SubscriberID)
	assert.Equal(t, agent.Slug, created.CreatedBy)

	// Listing returns only the agent's own subscriptions, not the user's.
	rec = doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/notifications/subscriptions", nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var subs []store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&subs))
	require.Len(t, subs, 1)
	assert.Equal(t, created.ID, subs[0].ID)
	assert.Equal(t, agent.Slug, subs[0].SubscriberID)
}

func TestHandleSubscriptions_AgentUpdateAndDeleteOwn(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	_, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "churn-agent")

	createReq := createSubscriptionRequest{
		Scope:             "project",
		ProjectID:         tid("project-notif-handler"),
		TriggerActivities: []string{"COMPLETED"},
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var sub store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sub))
	require.NotEmpty(t, sub.ID)

	updateReq := updateSubscriptionRequest{TriggerActivities: []string{"COMPLETED", "STALLED"}}
	rec = doRequestWithAgentToken(t, srv, http.MethodPatch, "/api/v1/notifications/subscriptions/"+sub.ID, updateReq, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored, err := s.GetNotificationSubscription(ctx, sub.ID)
	require.NoError(t, err)
	assert.Contains(t, stored.TriggerActivities, "STALLED")

	rec = doRequestWithAgentToken(t, srv, http.MethodDelete, "/api/v1/notifications/subscriptions/"+sub.ID, nil, token)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	_, err = s.GetNotificationSubscription(ctx, sub.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestHandleSubscriptions_AgentCannotTouchOtherSubscriber(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	_, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "intruder-agent")

	// The user subscription created by the test setup.
	userSubs, err := s.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, DevUserID)
	require.NoError(t, err)
	require.Len(t, userSubs, 1)
	userSubID := userSubs[0].ID

	rec := doRequestWithAgentToken(t, srv, http.MethodDelete, "/api/v1/notifications/subscriptions/"+userSubID, nil, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	updateReq := updateSubscriptionRequest{TriggerActivities: []string{"ERROR"}}
	rec = doRequestWithAgentToken(t, srv, http.MethodPatch, "/api/v1/notifications/subscriptions/"+userSubID, updateReq, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Bulk delete refuses it too, without reporting it as deleted.
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions/bulk-delete",
		map[string][]string{"ids": {userSubID}}, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var deleted map[string]int
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&deleted))
	assert.Equal(t, 0, deleted["deleted"])

	// The user's subscription is untouched.
	stored, err := s.GetNotificationSubscription(ctx, userSubID)
	require.NoError(t, err)
	assert.NotContains(t, stored.TriggerActivities, "ERROR")
}

func TestHandleSubscriptions_AgentCannotSubscribeOtherProject(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	otherProject := &store.Project{
		ID:   tid("project-other-notif"),
		Name: "Other Project",
		Slug: "other-notif-project",
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))

	_, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "scoped-agent")

	createReq := createSubscriptionRequest{
		Scope:             "project",
		ProjectID:         otherProject.ID,
		TriggerActivities: []string{"COMPLETED"},
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestHandleSubscriptions_AgentCannotWatchOtherProjectAgent covers the subtler
// half of project containment: the subscription is filed under the caller's own
// project, but watches an agent in another one, which would relay that agent's
// activity to the caller.
func TestHandleSubscriptions_AgentCannotWatchOtherProjectAgent(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	otherProject := &store.Project{
		ID:   tid("project-watch-other"),
		Name: "Watch Other Project",
		Slug: "watch-other-project",
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))

	foreign := &store.Agent{
		ID:        tid("agent-foreign-watched"),
		Slug:      "foreign-watched-agent",
		Name:      "Foreign Watched Agent",
		ProjectID: otherProject.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, foreign))

	agent, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "watcher-agent")

	// projectId is the caller's own — only the watched agent is foreign.
	createReq := createSubscriptionRequest{
		Scope:             "agent",
		AgentID:           foreign.ID,
		ProjectID:         agent.ProjectID,
		TriggerActivities: []string{"COMPLETED"},
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Same via bulk, alongside an otherwise valid entry.
	reqs := []createSubscriptionRequest{
		{Scope: "project", ProjectID: agent.ProjectID, TriggerActivities: []string{"COMPLETED"}},
		createReq,
	}
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions/bulk", reqs, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// An unknown agent is refused the same way, without confirming it is
	// merely absent.
	createReq.AgentID = tid("agent-does-not-exist")
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	stored, err := s.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeAgent, agent.Slug)
	require.NoError(t, err)
	assert.Empty(t, stored)

	// Watching an agent in the caller's own project still works.
	createReq.AgentID = tid("agent-watched")
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions", createReq, token)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestHandleSubscriptions_AgentBulkCreate(t *testing.T) {
	srv, s, _ := setupNotificationHandlerTest(t)
	ctx := context.Background()

	otherProject := &store.Project{
		ID:   tid("project-bulk-other"),
		Name: "Bulk Other Project",
		Slug: "bulk-other-project",
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))

	agent, token := setupNotificationAgentCaller(t, srv, s, tid("project-notif-handler"), "bulk-agent")

	reqs := []createSubscriptionRequest{
		{Scope: "project", ProjectID: agent.ProjectID, TriggerActivities: []string{"COMPLETED"}},
		{Scope: "agent", AgentID: tid("agent-watched"), ProjectID: agent.ProjectID, TriggerActivities: []string{"STALLED"}},
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions/bulk", reqs, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created []store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	require.Len(t, created, 2)
	for _, sub := range created {
		assert.Equal(t, store.SubscriberTypeAgent, sub.SubscriberType)
		assert.Equal(t, agent.Slug, sub.SubscriberID)
		assert.Equal(t, agent.ProjectID, sub.ProjectID)
	}

	// A project-scoped entry that also names an agent is malformed, and is
	// skipped rather than stored as an ambiguous row.
	reqs = []createSubscriptionRequest{
		{Scope: "project", AgentID: tid("agent-watched"), ProjectID: agent.ProjectID, TriggerActivities: []string{"ERROR"}},
	}
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions/bulk", reqs, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var skipped []store.NotificationSubscription
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&skipped))
	assert.Empty(t, skipped)

	// A foreign project anywhere in the batch fails the whole request rather
	// than silently dropping the entry.
	reqs = []createSubscriptionRequest{
		{Scope: "project", ProjectID: agent.ProjectID, TriggerActivities: []string{"ERROR"}},
		{Scope: "project", ProjectID: otherProject.ID, TriggerActivities: []string{"ERROR"}},
	}
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions/bulk", reqs, token)
	require.Equal(t, http.StatusForbidden, rec.Code)

	stored, err := s.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeAgent, agent.Slug)
	require.NoError(t, err)
	assert.Len(t, stored, 2)

	// Bulk delete removes the agent's own subscriptions.
	rec = doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/notifications/subscriptions/bulk-delete",
		map[string][]string{"ids": {created[0].ID, created[1].ID}}, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var deleted map[string]int
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&deleted))
	assert.Equal(t, 2, deleted["deleted"])
}
