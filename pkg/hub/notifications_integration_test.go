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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// integrationTestEnv holds all components for a notification integration test.
// It wires together the HTTP server, event publisher, notification dispatcher,
// and a recording dispatcher to verify end-to-end notification flows.
type integrationTestEnv struct {
	srv      *Server
	store    store.Store
	pub      *ChannelEventPublisher
	recorder *recordingDispatcher
	nd       *NotificationDispatcher
	project    *store.Project
	broker   *store.RuntimeBroker
	tokenSvc *AgentTokenService
}

// setupIntegrationTest creates a test environment with:
// - An in-memory SQLite store with migrations applied
// - A Hub test server with dev auth
// - A ChannelEventPublisher wired into the server
// - A recordingDispatcher wired into the server AND the notification dispatcher
// - A NotificationDispatcher started and listening for events
// - A project and broker provisioned for agent creation
func setupIntegrationTest(t *testing.T) *integrationTestEnv {
	t.Helper()

	srv, s := testServer(t)
	ctx := context.Background()

	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	t.Cleanup(func() { pub.Close() })

	recorder := &recordingDispatcher{}
	srv.SetDispatcher(recorder)

	project := &store.Project{
		ID:         "project-integ",
		Name:       "Integration Project",
		Slug:       "integration-project",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-integ",
		Name:   "Integration Broker",
		Slug:   "integration-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	// Create and start the notification dispatcher
	nd := NewNotificationDispatcher(s, pub, func() AgentDispatcher { return recorder }, slog.Default())
	nd.Start()
	t.Cleanup(func() { nd.Stop() })

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	return &integrationTestEnv{
		srv:      srv,
		store:    s,
		pub:      pub,
		recorder: recorder,
		nd:       nd,
		project:    project,
		broker:   broker,
		tokenSvc: tokenSvc,
	}
}

// createAgentWithNotify creates a sub-agent via the API using an agent token,
// with notify=true. Returns the created agent.
func (env *integrationTestEnv) createAgentWithNotify(t *testing.T, callingAgent *store.Agent, subAgentName string) *store.Agent {
	t.Helper()

	token, err := env.tokenSvc.GenerateAgentToken(callingAgent.ID, env.project.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate,
		ScopeAgentCreate,
		ScopeAgentNotify,
	}, nil)
	require.NoError(t, err)

	body, _ := json.Marshal(CreateAgentRequest{
		Name:    subAgentName,
		ProjectID: env.project.ID,
		Task:    "do work",
		Notify:  true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
	req.Header.Set("X-Scion-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	return resp.Agent
}

// updateStatusViaAPI updates an agent's status through the HTTP API,
// which triggers an event publication through the event publisher.
// The status parameter is mapped to Activity (for activity-like values) or Phase.
func (env *integrationTestEnv) updateStatusViaAPI(t *testing.T, agentID, status, message, taskSummary string) {
	t.Helper()

	token, err := env.tokenSvc.GenerateAgentToken(agentID, env.project.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate,
	}, nil)
	require.NoError(t, err)

	// Map status string to the appropriate field
	statusUpdate := store.AgentStatusUpdate{
		Message:     message,
		TaskSummary: taskSummary,
	}
	switch status {
	case "running", "stopped", "error", "provisioning", "created":
		statusUpdate.Phase = status
	default:
		// Activities: completed, waiting_for_input, idle, limits_exceeded, etc.
		statusUpdate.Activity = status
	}
	body, _ := json.Marshal(statusUpdate)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/status", bytes.NewReader(body))
	req.Header.Set("X-Scion-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "status update failed: %s", rec.Body.String())
}

// ============================================================================
// Integration Test: Full Agent-Creates-Agent-With-Notify Flow
// ============================================================================

func TestIntegration_AgentCreatesAgentWithNotify_FullFlow(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	// Create the parent agent (subscriber)
	parent := &store.Agent{
		ID:              "agent-parent",
		Slug:            "parent-agent",
		Name:            "Parent Agent",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	// Step 1: Parent creates sub-agent with notify=true via the API
	child := env.createAgentWithNotify(t, parent, "Child Worker")

	// Step 2: Verify subscription was created
	subs, err := env.store.GetNotificationSubscriptions(ctx, child.ID)
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, store.SubscriberTypeAgent, subs[0].SubscriberType)
	assert.Equal(t, parent.Slug, subs[0].SubscriberID)

	// Step 3: Sub-agent reports COMPLETED status via API
	env.updateStatusViaAPI(t, child.ID, "completed", "", "Auth module implemented")

	// Step 4: Verify notification was dispatched to parent agent
	require.Eventually(t, func() bool {
		return len(env.recorder.getCalls()) >= 1
	}, 2*time.Second, 50*time.Millisecond, "expected notification dispatch to parent agent")

	calls := env.recorder.getCalls()
	// Find the DispatchAgentMessage call (not DispatchAgentCreate calls)
	var notifCall *dispatchCall
	for i := range calls {
		if calls[i].Agent.ID == parent.ID {
			notifCall = &calls[i]
			break
		}
	}
	require.NotNil(t, notifCall, "expected notification dispatch to parent agent")
	assert.Contains(t, notifCall.Message, "child-worker has reached a state of COMPLETED")
	assert.Contains(t, notifCall.Message, "Auth module implemented")
	assert.False(t, notifCall.Interrupt, "notifications should not interrupt")

	// Step 5: Verify notification was stored in the database
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, "COMPLETED", notifs[0].Status)
	assert.True(t, notifs[0].Dispatched)
	assert.Equal(t, child.ID, notifs[0].AgentID)
}

func TestIntegration_AgentCreatesAgentWithNotify_WaitingForInput(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:              "agent-parent-wfi",
		Slug:            "parent-agent-wfi",
		Name:            "Parent Agent WFI",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	child := env.createAgentWithNotify(t, parent, "Child Worker WFI")

	// Sub-agent reaches WAITING_FOR_INPUT
	env.updateStatusViaAPI(t, child.ID, "waiting_for_input", "Which OAuth provider should I use?", "")

	require.Eventually(t, func() bool {
		calls := env.recorder.getCalls()
		for _, c := range calls {
			if c.Agent.ID == parent.ID {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.recorder.getCalls()
	var notifCall *dispatchCall
	for i := range calls {
		if calls[i].Agent.ID == parent.ID {
			notifCall = &calls[i]
			break
		}
	}
	require.NotNil(t, notifCall)
	assert.Contains(t, notifCall.Message, "child-worker-wfi is WAITING_FOR_INPUT")
	assert.Contains(t, notifCall.Message, "Which OAuth provider should I use?")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, "WAITING_FOR_INPUT", notifs[0].Status)
}

func TestIntegration_AgentCreatesAgentWithNotify_MultipleStatusChanges(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:              "agent-parent-multi",
		Slug:            "parent-multi",
		Name:            "Parent Multi",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	child := env.createAgentWithNotify(t, parent, "Child Multi Worker")

	// First: child goes to WAITING_FOR_INPUT
	env.updateStatusViaAPI(t, child.ID, "waiting_for_input", "Need API key", "")

	require.Eventually(t, func() bool {
		calls := env.recorder.getCalls()
		count := 0
		for _, c := range calls {
			if c.Agent.ID == parent.ID {
				count++
			}
		}
		return count >= 1
	}, 2*time.Second, 50*time.Millisecond)

	// Then: child completes
	env.updateStatusViaAPI(t, child.ID, "completed", "", "Task done")

	require.Eventually(t, func() bool {
		calls := env.recorder.getCalls()
		count := 0
		for _, c := range calls {
			if c.Agent.ID == parent.ID {
				count++
			}
		}
		return count >= 2
	}, 2*time.Second, 50*time.Millisecond)

	// Verify both notifications stored
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 2)
	// Most recent first (DESC order)
	assert.Equal(t, "COMPLETED", notifs[0].Status)
	assert.Equal(t, "WAITING_FOR_INPUT", notifs[1].Status)
}

// ============================================================================
// Integration Test: Status Normalization Edge Cases
// ============================================================================

func TestIntegration_StatusNormalization_LowercaseEventMatchesUppercaseTrigger(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:              "agent-parent-case",
		Slug:            "parent-case",
		Name:            "Parent Case",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	child := env.createAgentWithNotify(t, parent, "Child Case Worker")

	// Status update uses lowercase (as runtime often does), trigger uses UPPERCASE
	env.updateStatusViaAPI(t, child.ID, "completed", "", "")

	require.Eventually(t, func() bool {
		calls := env.recorder.getCalls()
		for _, c := range calls {
			if c.Agent.ID == parent.ID {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)

	// Verify notification was stored with UPPERCASE status
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, "COMPLETED", notifs[0].Status, "stored status should be UPPERCASE regardless of input")
}

func TestIntegration_StatusNormalization_DedupAcrossCaseBoundaries(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:              "agent-parent-dedup",
		Slug:            "parent-dedup",
		Name:            "Parent Dedup",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	child := env.createAgentWithNotify(t, parent, "Child Dedup Worker")

	// First status update with lowercase
	env.updateStatusViaAPI(t, child.ID, "completed", "", "")

	require.Eventually(t, func() bool {
		calls := env.recorder.getCalls()
		for _, c := range calls {
			if c.Agent.ID == parent.ID {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)

	// Second status update with same status - should be deduped regardless of case stored in DB
	env.updateStatusViaAPI(t, child.ID, "COMPLETED", "", "")

	// Wait and verify no additional notification
	time.Sleep(300 * time.Millisecond)

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1, "duplicate status (even with different case) should be deduped")
}

func TestIntegration_StatusNormalization_NonTriggerStatusNoNotification(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:              "agent-parent-nontrig",
		Slug:            "parent-nontrig",
		Name:            "Parent NonTrig",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	child := env.createAgentWithNotify(t, parent, "Child NonTrig Worker")

	// Status updates that should NOT trigger notifications
	env.updateStatusViaAPI(t, child.ID, "running", "", "")
	env.updateStatusViaAPI(t, child.ID, "idle", "", "")

	time.Sleep(300 * time.Millisecond)

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	assert.Empty(t, notifs, "non-trigger statuses should not create notifications")

	// Now trigger a real notification
	env.updateStatusViaAPI(t, child.ID, "limits_exceeded", "Token limit reached", "")

	require.Eventually(t, func() bool {
		calls := env.recorder.getCalls()
		for _, c := range calls {
			if c.Agent.ID == parent.ID {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)

	notifs, err = env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.Equal(t, "LIMITS_EXCEEDED", notifs[0].Status)
}

// ============================================================================
// Integration Test: Subscription Cleanup on Agent Deletion
// ============================================================================

func TestIntegration_SubscriptionCleanup_HardDeleteCascades(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:              "agent-parent-hdel",
		Slug:            "parent-hdel",
		Name:            "Parent Hard Delete",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	child := env.createAgentWithNotify(t, parent, "Child Hard Delete")

	// Trigger a notification so we have both subscription AND notification records
	env.updateStatusViaAPI(t, child.ID, "completed", "", "Done")

	require.Eventually(t, func() bool {
		notifs, _ := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
		return len(notifs) >= 1
	}, 2*time.Second, 50*time.Millisecond)

	// Verify subscription and notification exist
	subs, err := env.store.GetNotificationSubscriptions(ctx, child.ID)
	require.NoError(t, err)
	require.Len(t, subs, 1)

	notifsBefore, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifsBefore, 1)

	// Hard delete via API (force=true)
	rec := doRequest(t, env.srv, http.MethodDelete, "/api/v1/agents/"+child.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// Verify subscription is cascade-deleted
	subs, err = env.store.GetNotificationSubscriptions(ctx, child.ID)
	require.NoError(t, err)
	assert.Empty(t, subs, "subscriptions should be cascade-deleted with hard-deleted agent")

	// Verify notifications are cascade-deleted (subscription FK cascade)
	notifsAfter, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	assert.Empty(t, notifsAfter, "notifications should cascade-delete when subscription is deleted")
}

func TestIntegration_SubscriptionCleanup_SoftDeleteRetainsSubscriptions(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	// Configure server for soft delete
	env.srv.config.SoftDeleteRetention = 24 * time.Hour

	parent := &store.Agent{
		ID:              "agent-parent-sdel",
		Slug:            "parent-sdel",
		Name:            "Parent Soft Delete",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	child := env.createAgentWithNotify(t, parent, "Child Soft Delete")

	// Trigger a notification
	env.updateStatusViaAPI(t, child.ID, "completed", "", "Done")

	require.Eventually(t, func() bool {
		notifs, _ := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
		return len(notifs) >= 1
	}, 2*time.Second, 50*time.Millisecond)

	// Soft delete via API (no force flag, retention > 0)
	rec := doRequest(t, env.srv, http.MethodDelete, "/api/v1/agents/"+child.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// Verify the agent is soft-deleted (DeletedAt is set but record still exists)
	agent, err := env.store.GetAgent(ctx, child.ID)
	require.NoError(t, err)
	assert.False(t, agent.DeletedAt.IsZero(), "soft-deleted agent should have non-zero DeletedAt")

	// Subscriptions should still exist for soft-deleted agents
	subs, err := env.store.GetNotificationSubscriptions(ctx, child.ID)
	require.NoError(t, err)
	assert.Len(t, subs, 1, "subscriptions should remain for soft-deleted agents")

	// Notifications should still exist
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1, "notifications should remain for soft-deleted agents")
}

// ============================================================================
// Integration Test: Human Notification Full Lifecycle
// ============================================================================

func TestIntegration_HumanNotification_FullLifecycle(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	// User creates agent with notify=true via the dev auth token
	rec := doRequest(t, env.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "User Watched Agent",
		ProjectID: env.project.ID,
		Task:    "run analysis",
		Notify:  true,
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var createResp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	agentID := createResp.Agent.ID

	// Verify user subscription was created
	subs, err := env.store.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, store.SubscriberTypeUser, subs[0].SubscriberType)

	// Agent status changes to COMPLETED
	env.updateStatusViaAPI(t, agentID, "completed", "", "Analysis complete")

	// Wait for notification to be processed
	require.Eventually(t, func() bool {
		notifs, _ := env.store.GetNotifications(ctx, store.SubscriberTypeUser, subs[0].SubscriberID, false)
		return len(notifs) >= 1
	}, 2*time.Second, 50*time.Millisecond)

	// User lists unacknowledged notifications via API
	rec = doRequest(t, env.srv, http.MethodGet, "/api/v1/notifications", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var notifs []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&notifs))
	require.Len(t, notifs, 1, "user should see one unacknowledged notification")
	assert.Equal(t, "COMPLETED", notifs[0].Status)
	assert.Contains(t, notifs[0].Message, "user-watched-agent has reached a state of COMPLETED")
	assert.False(t, notifs[0].Acknowledged)

	// User acknowledges the notification
	rec = doRequest(t, env.srv, http.MethodPost, "/api/v1/notifications/"+notifs[0].ID+"/ack", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// Verify no more unacknowledged notifications
	rec = doRequest(t, env.srv, http.MethodGet, "/api/v1/notifications", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var remaining []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&remaining))
	assert.Empty(t, remaining, "no unacknowledged notifications after ack")

	// But notification still visible when including acknowledged
	rec = doRequest(t, env.srv, http.MethodGet, "/api/v1/notifications?acknowledged=true", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var all []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&all))
	assert.Len(t, all, 1, "acknowledged notification should still be retrievable")
	assert.True(t, all[0].Acknowledged)
}

func TestIntegration_HumanNotification_AckAll(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	// Create two agents with notify for the same user
	for _, name := range []string{"Ack All Agent One", "Ack All Agent Two"} {
		rec := doRequest(t, env.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:    name,
			ProjectID: env.project.ID,
			Task:    "work",
			Notify:  true,
		})
		require.Equal(t, http.StatusCreated, rec.Code)

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		// Each agent completes
		env.updateStatusViaAPI(t, resp.Agent.ID, "completed", "", "Done")
	}

	// Wait for both notifications
	subs, err := env.store.GetNotificationSubscriptions(ctx, "")
	_ = subs
	_ = err
	require.Eventually(t, func() bool {
		rec := doRequest(t, env.srv, http.MethodGet, "/api/v1/notifications", nil)
		var notifs []store.Notification
		json.NewDecoder(rec.Body).Decode(&notifs)
		return len(notifs) >= 2
	}, 2*time.Second, 50*time.Millisecond)

	// Acknowledge all
	rec := doRequest(t, env.srv, http.MethodPost, "/api/v1/notifications/ack-all", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// Verify none unacknowledged
	rec = doRequest(t, env.srv, http.MethodGet, "/api/v1/notifications", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var remaining []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&remaining))
	assert.Empty(t, remaining, "all notifications should be acknowledged after ack-all")
}

func TestIntegration_HumanNotification_MultipleStatusTransitions(t *testing.T) {
	env := setupIntegrationTest(t)

	// User creates agent with notify
	rec := doRequest(t, env.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "Multi Status Agent",
		ProjectID: env.project.ID,
		Task:    "complex work",
		Notify:  true,
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agentID := resp.Agent.ID

	// Agent goes through: running → waiting_for_input → completed
	env.updateStatusViaAPI(t, agentID, "running", "", "")
	time.Sleep(100 * time.Millisecond)
	env.updateStatusViaAPI(t, agentID, "waiting_for_input", "Need approval", "")

	require.Eventually(t, func() bool {
		rec := doRequest(t, env.srv, http.MethodGet, "/api/v1/notifications", nil)
		var notifs []store.Notification
		json.NewDecoder(rec.Body).Decode(&notifs)
		return len(notifs) >= 1
	}, 2*time.Second, 50*time.Millisecond)

	env.updateStatusViaAPI(t, agentID, "completed", "", "All done")

	require.Eventually(t, func() bool {
		rec := doRequest(t, env.srv, http.MethodGet, "/api/v1/notifications", nil)
		var notifs []store.Notification
		json.NewDecoder(rec.Body).Decode(&notifs)
		return len(notifs) >= 2
	}, 2*time.Second, 50*time.Millisecond)

	// Verify: running should NOT have created a notification, only WAITING_FOR_INPUT and COMPLETED
	rec = doRequest(t, env.srv, http.MethodGet, "/api/v1/notifications", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var notifs []store.Notification
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&notifs))
	assert.Len(t, notifs, 2)
	// DESC order: COMPLETED first, WAITING_FOR_INPUT second
	assert.Equal(t, "COMPLETED", notifs[0].Status)
	assert.Equal(t, "WAITING_FOR_INPUT", notifs[1].Status)
}

// ============================================================================
// Integration Test: Multiple Subscribers
// ============================================================================

func TestIntegration_MultipleSubscribers_AgentAndUser(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	// Create parent agent
	parent := &store.Agent{
		ID:              "agent-parent-multi-sub",
		Slug:            "parent-multi-sub",
		Name:            "Parent Multi Sub",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	// Parent agent creates child with notify
	child := env.createAgentWithNotify(t, parent, "Multi Sub Child")

	// User also subscribes to the same child (manually, since the API doesn't support this yet)
	userSub := &store.NotificationSubscription{
		ID:                "user-sub-multi",
		AgentID:           child.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      DevUserID,
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now(),
		CreatedBy:         DevUserID,
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, userSub))

	// Child completes
	env.updateStatusViaAPI(t, child.ID, "completed", "", "Work finished")

	// Wait for agent notification dispatch
	require.Eventually(t, func() bool {
		calls := env.recorder.getCalls()
		for _, c := range calls {
			if c.Agent.ID == parent.ID {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)

	// Verify agent notification
	agentNotifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	assert.Len(t, agentNotifs, 1)

	// Verify user notification (stored, not dispatched)
	userNotifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, DevUserID, false)
	require.NoError(t, err)
	assert.Len(t, userNotifs, 1)
	assert.Equal(t, "COMPLETED", userNotifs[0].Status)
}

// ============================================================================
// Integration Test: No Notification Without --notify Flag
// ============================================================================

func TestIntegration_NoNotifyFlag_NoSubscription(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:              "agent-parent-no-notify",
		Slug:            "parent-no-notify",
		Name:            "Parent No Notify",
		ProjectID:         env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: env.broker.ID,
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, env.store.CreateAgent(ctx, parent))

	// Create sub-agent WITHOUT notify
	token, err := env.tokenSvc.GenerateAgentToken(parent.ID, env.project.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate,
		ScopeAgentCreate,
	}, nil)
	require.NoError(t, err)

	body, _ := json.Marshal(CreateAgentRequest{
		Name:    "Child No Notify",
		ProjectID: env.project.ID,
		Task:    "do work",
		Notify:  false,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
	req.Header.Set("X-Scion-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// No subscriptions should exist
	subs, err := env.store.GetNotificationSubscriptions(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Empty(t, subs)

	// Status change should not generate any notification
	env.updateStatusViaAPI(t, resp.Agent.ID, "completed", "", "")
	time.Sleep(300 * time.Millisecond)

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, parent.Slug, false)
	require.NoError(t, err)
	assert.Empty(t, notifs)
}

// ============================================================================
// Integration Tests: Phase 4 — PATCH, Bulk, Templates
// ============================================================================

func TestIntegration_PATCHSubscriptionTriggers(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	// Create a subscription via store (SubscriberID must match DevUserID)
	sub := &store.NotificationSubscription{
		ID:                "sub-patch-test",
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      DevUserID,
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now(),
		CreatedBy:         DevUserID,
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, sub))

	// PATCH trigger activities via HTTP
	patchBody, _ := json.Marshal(updateSubscriptionRequest{
		TriggerActivities: []string{"COMPLETED", "DELETED", "ERROR"},
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/notifications/subscriptions/"+sub.ID, bytes.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var result store.NotificationSubscription
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.Equal(t, []string{"COMPLETED", "DELETED", "ERROR"}, result.TriggerActivities)

	// Verify in store
	updated, err := env.store.GetNotificationSubscription(ctx, sub.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"COMPLETED", "DELETED", "ERROR"}, updated.TriggerActivities)
}

func TestIntegration_BulkCreateSubscriptions(t *testing.T) {
	env := setupIntegrationTest(t)

	reqs := []createSubscriptionRequest{
		{
			Scope:             store.SubscriptionScopeProject,
			ProjectID:           env.project.ID,
			TriggerActivities: []string{"COMPLETED"},
		},
		{
			Scope:             store.SubscriptionScopeProject,
			ProjectID:           env.project.ID,
			TriggerActivities: []string{"ERROR"},
		},
	}
	body, _ := json.Marshal(reqs)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/subscriptions/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var results []store.NotificationSubscription
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &results))
	assert.Len(t, results, 2)
}

func TestIntegration_SubscriptionTemplates_CRUD(t *testing.T) {
	env := setupIntegrationTest(t)

	// Create template
	createBody, _ := json.Marshal(createTemplateRequest{
		Name:              "All Events",
		Scope:             store.SubscriptionScopeProject,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED", "DELETED", "ERROR"},
		ProjectID:           env.project.ID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/templates", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var created store.SubscriptionTemplate
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "All Events", created.Name)
	assert.Len(t, created.TriggerActivities, 5)

	// List templates
	req = httptest.NewRequest(http.MethodGet, "/api/v1/notifications/templates?projectId="+env.project.ID, nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var templates []store.SubscriptionTemplate
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &templates))
	assert.Len(t, templates, 1)

	// Delete template
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/notifications/templates/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
}
