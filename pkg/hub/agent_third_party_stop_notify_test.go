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
	"regexp"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rfc3339UTCRe matches an RFC3339 UTC timestamp embedded in a notification message.
var rfc3339UTCRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)

type thirdPartyNotifyEnv struct {
	*notificationTestEnv
	srv *Server
}

func setupThirdPartyNotifyEnv(t *testing.T) *thirdPartyNotifyEnv {
	t.Helper()
	env := setupNotificationTest(t)
	// Start from no subscriptions; each test adds its own.
	require.NoError(t, env.store.DeleteNotificationSubscription(context.Background(), env.sub.ID))

	srv := &Server{
		store:                  env.store,
		events:                 env.pub,
		notificationDispatcher: env.nd,
		agentLifecycleLog:      slog.Default(),
	}
	return &thirdPartyNotifyEnv{notificationTestEnv: env, srv: srv}
}

func (e *thirdPartyNotifyEnv) createUser(t *testing.T, id, displayName, email string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          id,
		Email:       email,
		DisplayName: displayName,
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, e.store.CreateUser(context.Background(), u))
	return u
}

// waitForNotificationDispatcherDrain publishes a status event for a probe
// agent on pub and waits until NotificationDispatcher has persisted the probe
// notification. Because NotificationDispatcher consumes the subscription
// channel sequentially in FIFO order, every status event published before this
// call has been completely handled once the probe notification lands.
func waitForNotificationDispatcherDrain(t *testing.T, s store.Store, pub EventPublisher, projectID string) {
	t.Helper()
	ctx := context.Background()
	probeUserID := api.NewUUID()
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          probeUserID,
		Email:       probeUserID + "@probe.test",
		DisplayName: "Probe User",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}))
	probeAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "drain-probe-" + probeUserID[:8],
		Slug:      "drain-probe-" + probeUserID[:8],
		ProjectID: projectID,
		Phase:     string(state.PhaseStopped),
		Template:  "claude",
	}
	require.NoError(t, s.CreateAgent(ctx, probeAgent))
	probeSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           probeAgent.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      probeUserID,
		ProjectID:         projectID,
		TriggerActivities: []string{"STOPPED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         probeUserID,
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, probeSub))

	pub.PublishAgentStatus(ctx, probeAgent)
	require.Eventually(t, func() bool {
		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, probeUserID, false)
		return err == nil && len(notifs) == 1
	}, 5*time.Second, 5*time.Millisecond, "timed out waiting for NotificationDispatcher to drain status events")
}

func TestThirdPartyLifecycleNotify_UserActorSuspendsAndStops(t *testing.T) {
	ctx := context.Background()
	creatorID := tid("tp-creator-user")
	actorID := tid("tp-actor-admin")

	t.Run("third-party user suspends with reason and no subscription", func(t *testing.T) {
		e := setupThirdPartyNotifyEnv(t)
		e.createUser(t, creatorID, "Creator One", "creator@example.com")
		e.createUser(t, actorID, "Admin Alice", "alice@example.com")
		e.watched.CreatedBy = creatorID
		e.watched.Phase = string(state.PhaseSuspended)

		actorCtx := contextWithIdentity(ctx, NewAuthenticatedUser(actorID, "alice@example.com", "Admin Alice", store.UserRoleAdmin, "web"))
		e.srv.notifyThirdPartyLifecycleAction(actorCtx, e.watched, api.AgentActionSuspend, "runaway loop")
		e.srv.thirdPartyStopDispatches.Wait()

		notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		require.Len(t, notifs, 1)
		assert.Equal(t, "SUSPENDED", notifs[0].Status)
		assert.Equal(t, store.CreatorFallbackSubscriptionID, notifs[0].SubscriptionID)
		assert.Equal(t, e.watched.ID, notifs[0].AgentID)
		assert.Contains(t, notifs[0].Message, "watched-agent was suspended by Admin Alice (alice@example.com) at ")
		assert.Regexp(t, rfc3339UTCRe, notifs[0].Message)
		assert.Contains(t, notifs[0].Message, ": runaway loop")
	})

	t.Run("third-party user stops without reason", func(t *testing.T) {
		e := setupThirdPartyNotifyEnv(t)
		e.createUser(t, creatorID, "Creator One", "creator@example.com")
		e.watched.CreatedBy = creatorID
		e.watched.Phase = string(state.PhaseStopped)

		actorCtx := contextWithIdentity(ctx, NewAuthenticatedUser(actorID, "bob@example.com", "Bob", store.UserRoleMember, "cli"))
		e.srv.notifyThirdPartyLifecycleAction(actorCtx, e.watched, api.AgentActionStop, "")
		e.srv.thirdPartyStopDispatches.Wait()

		notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		require.Len(t, notifs, 1)
		assert.Equal(t, "STOPPED", notifs[0].Status)
		assert.Contains(t, notifs[0].Message, "watched-agent was stopped by Bob (bob@example.com) at ")
		assert.Regexp(t, rfc3339UTCRe, notifs[0].Message)
		assert.NotContains(t, notifs[0].Message, ": ")
	})

	t.Run("creator with matching subscription gets exactly one notice after PublishAgentStatus", func(t *testing.T) {
		e := setupThirdPartyNotifyEnv(t)
		e.createUser(t, creatorID, "Creator One", "creator@example.com")
		e.watched.CreatedBy = creatorID
		e.watched.Phase = string(state.PhaseSuspended)
		require.NoError(t, e.store.UpdateAgent(ctx, e.watched))

		sub := &store.NotificationSubscription{
			ID:                api.NewUUID(),
			Scope:             store.SubscriptionScopeAgent,
			AgentID:           e.watched.ID,
			SubscriberType:    store.SubscriberTypeUser,
			SubscriberID:      creatorID,
			ProjectID:         e.project.ID,
			TriggerActivities: []string{"SUSPENDED", "STOPPED"},
			CreatedAt:         time.Now().Add(-time.Minute),
			CreatedBy:         creatorID,
		}
		require.NoError(t, e.store.CreateNotificationSubscription(ctx, sub))

		// Start the live notification dispatcher to verify handleEvent deduplicates.
		e.nd.Start()
		defer e.nd.Stop()

		actorCtx := contextWithIdentity(ctx, NewAuthenticatedUser(actorID, "alice@example.com", "Admin Alice", store.UserRoleAdmin, "web"))
		e.srv.notifyThirdPartyLifecycleAction(actorCtx, e.watched, api.AgentActionSuspend, "budget limit")
		e.pub.PublishAgentStatus(ctx, e.watched)
		e.srv.thirdPartyStopDispatches.Wait()

		waitForNotificationDispatcherDrain(t, e.store, e.pub, e.project.ID)

		notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		require.Len(t, notifs, 1, "creator with matching subscription must not receive a duplicate generic notice")
		assert.Equal(t, sub.ID, notifs[0].SubscriptionID)
		assert.Equal(t, "SUSPENDED", notifs[0].Status)
		assert.Contains(t, notifs[0].Message, "watched-agent was suspended by Admin Alice (alice@example.com) at ")
		assert.Contains(t, notifs[0].Message, ": budget limit")
	})
}

func TestThirdPartyLifecycleNotify_AgentActorAndAgentCreator(t *testing.T) {
	ctx := context.Background()
	e := setupThirdPartyNotifyEnv(t)

	// e.subscriber is the creator agent; create a third agent as the actor.
	actorAgent := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "ops-coordinator",
		Name:            "Ops Coordinator",
		Template:        "claude",
		ProjectID:       e.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: tid("broker-1"),
	}
	require.NoError(t, e.store.CreateAgent(ctx, actorAgent))

	e.watched.CreatedBy = e.subscriber.ID
	e.watched.Phase = string(state.PhaseStopped)

	agentClaims := &AgentTokenClaims{
		ProjectID: e.project.ID,
		Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
	}
	agentClaims.Subject = actorAgent.ID
	actorCtx := contextWithIdentity(ctx, &agentIdentityWrapper{agentClaims})

	e.srv.notifyThirdPartyLifecycleAction(actorCtx, e.watched, api.AgentActionStop, "task superseded")
	e.srv.thirdPartyStopDispatches.Wait()

	notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeAgent, e.subscriber.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, "STOPPED", notifs[0].Status)
	assert.Contains(t, notifs[0].Message, "watched-agent was stopped by agent:ops-coordinator at ")
	assert.Regexp(t, rfc3339UTCRe, notifs[0].Message)
	assert.Contains(t, notifs[0].Message, ": task superseded")

	calls := e.dispatcher.getCalls()
	require.Len(t, calls, 1, "creating agent must receive the dispatched notification message")
	assert.Equal(t, e.subscriber.ID, calls[0].Agent.ID)
	assert.Equal(t, notifs[0].Message, calls[0].Message)
}

func TestThirdPartyLifecycleNotify_Exclusions(t *testing.T) {
	ctx := context.Background()
	creatorID := tid("tp-excl-creator")

	t.Run("creator-initiated suspend does not emit third-party notice", func(t *testing.T) {
		e := setupThirdPartyNotifyEnv(t)
		e.createUser(t, creatorID, "Creator", "creator@example.com")
		e.watched.CreatedBy = creatorID
		e.watched.Phase = string(state.PhaseSuspended)

		creatorCtx := contextWithIdentity(ctx, NewAuthenticatedUser(creatorID, "creator@example.com", "Creator", store.UserRoleMember, "web"))
		e.srv.notifyThirdPartyLifecycleAction(creatorCtx, e.watched, api.AgentActionSuspend, "done for now")
		e.srv.thirdPartyStopDispatches.Wait()

		notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		assert.Empty(t, notifs)
	})

	t.Run("self-initiated stop does not emit third-party notice", func(t *testing.T) {
		e := setupThirdPartyNotifyEnv(t)
		e.createUser(t, creatorID, "Creator", "creator@example.com")
		e.watched.CreatedBy = creatorID
		e.watched.Phase = string(state.PhaseStopped)

		selfClaims := &AgentTokenClaims{
			ProjectID: e.project.ID,
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		}
		selfClaims.Subject = e.watched.ID
		selfCtx := contextWithIdentity(ctx, &agentIdentityWrapper{selfClaims})

		e.srv.notifyThirdPartyLifecycleAction(selfCtx, e.watched, api.AgentActionStop, "self stop")
		e.srv.thirdPartyStopDispatches.Wait()

		notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		assert.Empty(t, notifs)
	})

	t.Run("auto-suspend with no request identity does not emit third-party notice", func(t *testing.T) {
		e := setupThirdPartyNotifyEnv(t)
		e.createUser(t, creatorID, "Creator", "creator@example.com")
		e.watched.CreatedBy = creatorID
		e.watched.Phase = string(state.PhaseSuspended)

		// Background scheduler / auto-suspend runs without any identity on ctx.
		e.srv.notifyThirdPartyLifecycleAction(ctx, e.watched, api.AgentActionSuspend, "")
		e.srv.thirdPartyStopDispatches.Wait()

		notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		assert.Empty(t, notifs)
	})

	t.Run("agent without CreatedBy does not panic or notify", func(t *testing.T) {
		e := setupThirdPartyNotifyEnv(t)
		e.watched.CreatedBy = ""
		actorCtx := contextWithIdentity(ctx, NewAuthenticatedUser(tid("tp-other"), "other@example.com", "Other", store.UserRoleAdmin, "web"))
		e.srv.notifyThirdPartyLifecycleAction(actorCtx, e.watched, api.AgentActionStop, "cleanup")
		e.srv.thirdPartyStopDispatches.Wait()
	})
}

func TestThirdPartyLifecycleNotify_HTTPHandlers(t *testing.T) {
	srv, s := testServer(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	srv.events = pub
	nd := NewNotificationDispatcher(s, srv.events, srv.GetDispatcher, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)
	srv.notificationDispatcher = nd
	ctx := context.Background()

	creatorID := tid("tp-http-creator")
	adminID := tid("tp-http-admin")

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: creatorID, Email: "creator@example.com", DisplayName: "Creator Jane", Role: store.UserRoleMember, Status: store.UserStatusActive,
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "admin@example.com", DisplayName: "Admin Pat", Role: store.UserRoleAdmin, Status: store.UserStatusActive,
	}))

	project := &store.Project{
		ID:   tid("tp-http-proj"),
		Name: "TP HTTP Project",
		Slug: "tp-http-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:       tid("tp-http-broker"),
		Name:     "TP HTTP Broker",
		Slug:     "tp-http-broker",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agentToSuspend := &store.Agent{
		ID:              tid("tp-http-suspend"),
		Name:            "worker-suspend",
		Slug:            "worker-suspend",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
		Template:        "claude",
		CreatedBy:       creatorID,
		OwnerID:         creatorID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentToSuspend))

	agentToStop := &store.Agent{
		ID:              tid("tp-http-stop"),
		Name:            "worker-stop",
		Slug:            "worker-stop",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
		Template:        "claude",
		CreatedBy:       creatorID,
		OwnerID:         creatorID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentToStop))

	adminOwnAgent := &store.Agent{
		ID:              tid("tp-http-admin-own"),
		Name:            "admin-worker",
		Slug:            "admin-worker",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
		Template:        "claude",
		CreatedBy:       adminID,
		OwnerID:         adminID,
	}
	require.NoError(t, s.CreateAgent(ctx, adminOwnAgent))

	// Give creator a default SUSPENDED subscription on agentToSuspend to confirm no duplicate.
	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentToSuspend.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      creatorID,
		ProjectID:         project.ID,
		TriggerActivities: []string{"SUSPENDED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         creatorID,
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	adminIdentity := NewAuthenticatedUser(adminID, "admin@example.com", "Admin Pat", store.UserRoleAdmin, "web")

	// 1. Third-party suspend via handleAgentLifecycle with reason.
	suspendBody, _ := json.Marshal(AgentLifecycleStopRequest{Reason: "excessive API spend"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentToSuspend.ID+"/suspend", bytes.NewReader(suspendBody))
	req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
	rec := httptest.NewRecorder()
	srv.handleAgentLifecycle(rec, req, agentToSuspend.ID, api.AgentActionSuspend)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	srv.thirdPartyStopDispatches.Wait()

	// 2. Third-party stop via handleAgentLifecycle with reason.
	stopBody, _ := json.Marshal(AgentLifecycleStopRequest{Reason: "maintenance window"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentToStop.ID+"/stop", bytes.NewReader(stopBody))
	req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
	rec = httptest.NewRecorder()
	srv.handleAgentLifecycle(rec, req, agentToStop.ID, api.AgentActionStop)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	srv.thirdPartyStopDispatches.Wait()

	// 3. Creator-initiated stop of adminOwnAgent by admin -> no third-party notice.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+adminOwnAgent.ID+"/stop", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
	rec = httptest.NewRecorder()
	srv.handleAgentLifecycle(rec, req, adminOwnAgent.ID, api.AgentActionStop)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	srv.thirdPartyStopDispatches.Wait()

	waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

	creatorNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
	require.NoError(t, err)
	require.Len(t, creatorNotifs, 2, "creator should have exactly one suspend notice and one stop notice")

	byAgent := map[string]store.Notification{}
	for _, n := range creatorNotifs {
		byAgent[n.AgentID] = n
	}

	suspNotif, ok := byAgent[agentToSuspend.ID]
	require.True(t, ok)
	assert.Equal(t, "SUSPENDED", suspNotif.Status)
	assert.Equal(t, sub.ID, suspNotif.SubscriptionID)
	assert.Contains(t, suspNotif.Message, "worker-suspend was suspended by Admin Pat (admin@example.com) at ")
	assert.Contains(t, suspNotif.Message, ": excessive API spend")

	stopNotif, ok := byAgent[agentToStop.ID]
	require.True(t, ok)
	assert.Equal(t, "STOPPED", stopNotif.Status)
	assert.Equal(t, store.CreatorFallbackSubscriptionID, stopNotif.SubscriptionID)
	assert.Contains(t, stopNotif.Message, "worker-stop was stopped by Admin Pat (admin@example.com) at ")
	assert.Contains(t, stopNotif.Message, ": maintenance window")

	adminNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, adminID, false)
	require.NoError(t, err)
	assert.Empty(t, adminNotifs, "creator stopping their own agent must not emit a third-party notice")
}

func TestThirdPartyLifecycleNotify_StopAll(t *testing.T) {
	srv, s := testServer(t)
	nd := NewNotificationDispatcher(s, srv.events, srv.GetDispatcher, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)
	srv.notificationDispatcher = nd
	ctx := context.Background()

	creatorID := tid("tp-stopall-creator")
	adminID := tid("tp-stopall-admin")

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: creatorID, Email: "creator-sa@example.com", DisplayName: "Creator Sam", Role: store.UserRoleMember, Status: store.UserStatusActive,
	}))
	adminUser := &store.User{
		ID: adminID, Email: "admin-sa@example.com", DisplayName: "Admin Dana", Role: store.UserRoleAdmin, Status: store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))

	project := &store.Project{
		ID:   tid("tp-stopall-proj"),
		Name: "TP StopAll Project",
		Slug: "tp-stopall-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        adminID,
	})
	require.NoError(t, err)

	otherAgent := &store.Agent{
		ID:        tid("tp-stopall-other"),
		Name:      "other-worker",
		Slug:      "other-worker",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
		Template:  "claude",
		CreatedBy: creatorID,
		OwnerID:   creatorID,
	}
	require.NoError(t, s.CreateAgent(ctx, otherAgent))

	ownAgent := &store.Agent{
		ID:        tid("tp-stopall-own"),
		Name:      "own-worker",
		Slug:      "own-worker",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
		Template:  "claude",
		CreatedBy: adminID,
		OwnerID:   adminID,
	}
	require.NoError(t, s.CreateAgent(ctx, ownAgent))

	rec := doRequestAsUser(t, srv, adminUser, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all",
		AgentLifecycleStopRequest{Reason: "cluster drain"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var stopAllResp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stopAllResp))
	require.Equal(t, "all", stopAllResp.Scope, rec.Body.String())
	require.Equal(t, 2, stopAllResp.Stopped, rec.Body.String())
	srv.thirdPartyStopDispatches.Wait()

	creatorNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
	require.NoError(t, err)
	require.Len(t, creatorNotifs, 1)
	assert.Equal(t, "STOPPED", creatorNotifs[0].Status)
	assert.Equal(t, otherAgent.ID, creatorNotifs[0].AgentID)
	assert.Contains(t, creatorNotifs[0].Message, "other-worker was stopped by Admin Dana (admin-sa@example.com) at ")
	assert.Regexp(t, rfc3339UTCRe, creatorNotifs[0].Message)
	assert.Contains(t, creatorNotifs[0].Message, ": cluster drain")

	adminNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, adminID, false)
	require.NoError(t, err)
	assert.Empty(t, adminNotifs, "admin's own agent stopped in stop-all must not emit a third-party notice")
}

func TestThirdPartyLifecycleNotify_NoRepeatWhenAlreadyInTargetState(t *testing.T) {
	srv, s := testServer(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	srv.events = pub
	nd := NewNotificationDispatcher(s, srv.events, srv.GetDispatcher, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)
	srv.notificationDispatcher = nd
	ctx := context.Background()

	creatorID := tid("tp-noop-creator")
	adminID := tid("tp-noop-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: creatorID, Email: "creator-noop@example.com", DisplayName: "Creator", Role: store.UserRoleMember, Status: store.UserStatusActive,
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "admin-noop@example.com", DisplayName: "Admin", Role: store.UserRoleAdmin, Status: store.UserStatusActive,
	}))

	project := &store.Project{
		ID:   tid("tp-noop-proj"),
		Name: "TP Noop Project",
		Slug: "tp-noop-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	onlineBroker := &store.RuntimeBroker{
		ID:       tid("tp-noop-broker-on"),
		Name:     "TP Online Broker",
		Slug:     "tp-noop-broker-on",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9801",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, onlineBroker))

	offlineBroker := &store.RuntimeBroker{
		ID:       tid("tp-noop-broker-off"),
		Name:     "TP Offline Broker",
		Slug:     "tp-noop-broker-off",
		Status:   store.BrokerStatusOffline,
		Endpoint: "http://localhost:9802",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, offlineBroker))

	adminIdentity := NewAuthenticatedUser(adminID, "admin-noop@example.com", "Admin", store.UserRoleAdmin, "web")

	t.Run("re-stopping an already stopped agent does not send another notice", func(t *testing.T) {
		agent := &store.Agent{
			ID:              tid("tp-noop-restop"),
			Name:            "worker-restop",
			Slug:            "worker-restop",
			ProjectID:       project.ID,
			RuntimeBrokerID: onlineBroker.ID,
			Phase:           string(state.PhaseRunning),
			Template:        "claude",
			CreatedBy:       creatorID,
			OwnerID:         creatorID,
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		// First stop: running -> stopped (real transition -> 1 notice).
		body1, _ := json.Marshal(AgentLifecycleStopRequest{Reason: "first stop"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", bytes.NewReader(body1))
		req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
		rec := httptest.NewRecorder()
		srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionStop)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		// Second stop: stopped -> stopped (already in target state -> 0 new notices).
		body2, _ := json.Marshal(AgentLifecycleStopRequest{Reason: "second stop"})
		req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", bytes.NewReader(body2))
		req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
		rec = httptest.NewRecorder()
		srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionStop)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		var agentNotifs []store.Notification
		for _, n := range notifs {
			if n.AgentID == agent.ID {
				agentNotifs = append(agentNotifs, n)
			}
		}
		require.Len(t, agentNotifs, 1, "re-stopping an already-stopped agent must not send a second notice")
		assert.Contains(t, agentNotifs[0].Message, ": first stop")
	})

	t.Run("stopping a suspended agent is a real transition and still notifies", func(t *testing.T) {
		agent := &store.Agent{
			ID:              tid("tp-noop-susp-stop"),
			Name:            "worker-susp-stop",
			Slug:            "worker-susp-stop",
			ProjectID:       project.ID,
			RuntimeBrokerID: onlineBroker.ID,
			Phase:           string(state.PhaseSuspended),
			SuspendReason:   string(state.SuspendReasonUser),
			Template:        "claude",
			CreatedBy:       creatorID,
			OwnerID:         creatorID,
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		body, _ := json.Marshal(AgentLifecycleStopRequest{Reason: "teardown"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", bytes.NewReader(body))
		req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
		rec := httptest.NewRecorder()
		srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionStop)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		var agentNotifs []store.Notification
		for _, n := range notifs {
			if n.AgentID == agent.ID {
				agentNotifs = append(agentNotifs, n)
			}
		}
		require.Len(t, agentNotifs, 1, "stopping a suspended agent must still emit a third-party stop notice")
		assert.Equal(t, "STOPPED", agentNotifs[0].Status)
		assert.Contains(t, agentNotifs[0].Message, ": teardown")
	})

	t.Run("re-stopping an already stopped agent on an offline broker does not notify", func(t *testing.T) {
		agent := &store.Agent{
			ID:              tid("tp-noop-offline-restop"),
			Name:            "worker-offline-restop",
			Slug:            "worker-offline-restop",
			ProjectID:       project.ID,
			RuntimeBrokerID: offlineBroker.ID,
			Phase:           string(state.PhaseStopped),
			Template:        "claude",
			CreatedBy:       creatorID,
			OwnerID:         creatorID,
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
		rec := httptest.NewRecorder()
		srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionStop)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		for _, n := range notifs {
			assert.NotEqual(t, agent.ID, n.AgentID, "offline re-stop of an already-stopped agent must not notify")
		}
	})

	t.Run("re-suspending an already user-suspended agent does not notify", func(t *testing.T) {
		agent := &store.Agent{
			ID:              tid("tp-noop-resuspend"),
			Name:            "worker-resuspend",
			Slug:            "worker-resuspend",
			ProjectID:       project.ID,
			RuntimeBrokerID: onlineBroker.ID,
			Phase:           string(state.PhaseSuspended),
			SuspendReason:   string(state.SuspendReasonUser),
			Template:        "claude",
			CreatedBy:       creatorID,
			OwnerID:         creatorID,
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
		rec := httptest.NewRecorder()
		srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionSuspend)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		// Direct suspendAgent call on an already user-suspended agent also emits no notice.
		actorCtx := contextWithIdentity(ctx, adminIdentity)
		require.NoError(t, srv.suspendAgent(actorCtx, agent))
		srv.thirdPartyStopDispatches.Wait()

		waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		for _, n := range notifs {
			assert.NotEqual(t, agent.ID, n.AgentID, "re-suspending an already user-suspended agent must not notify")
		}
	})

	t.Run("re-stopping an already stopped managed agent does not send another notice", func(t *testing.T) {
		useManagedBackend(t, &recordingManagedBackend{})

		agent := &store.Agent{
			ID:        tid("tp-noop-managed-restop"),
			Name:      "managed-restop",
			Slug:      "managed-restop",
			ProjectID: project.ID,
			Runtime:   ManagedRuntimePrefix + "test",
			Phase:     string(state.PhaseRunning),
			Template:  "claude",
			CreatedBy: creatorID,
			OwnerID:   creatorID,
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		// First stop: running -> stopped (real transition -> 1 notice).
		body1, _ := json.Marshal(AgentLifecycleStopRequest{Reason: "first managed stop"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", bytes.NewReader(body1))
		req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
		rec := httptest.NewRecorder()
		srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionStop)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		// Second stop: stopped -> stopped (already in target state -> 0 new notices).
		body2, _ := json.Marshal(AgentLifecycleStopRequest{Reason: "second managed stop"})
		req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", bytes.NewReader(body2))
		req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
		rec = httptest.NewRecorder()
		srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionStop)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		var agentNotifs []store.Notification
		for _, n := range notifs {
			if n.AgentID == agent.ID {
				agentNotifs = append(agentNotifs, n)
			}
		}
		require.Len(t, agentNotifs, 1, "re-stopping an already-stopped managed agent must not send a second notice")
		assert.Contains(t, agentNotifs[0].Message, ": first managed stop")
	})
}

func TestThirdPartyLifecycleNotify_TerminalActivityNoStaleRefire(t *testing.T) {
	cases := []struct {
		name           string
		activity       state.Activity
		expectedStatus string
	}{
		{
			name:           "LIMITS_EXCEEDED",
			activity:       state.ActivityLimitsExceeded,
			expectedStatus: "LIMITS_EXCEEDED",
		},
		{
			name:           "CRASHED",
			activity:       state.ActivityCrashed,
			expectedStatus: "CRASHED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			pub := NewChannelEventPublisher()
			t.Cleanup(pub.Close)
			srv.events = pub
			nd := NewNotificationDispatcher(s, srv.events, srv.GetDispatcher, slog.Default())
			nd.Start()
			t.Cleanup(nd.Stop)
			srv.notificationDispatcher = nd
			ctx := context.Background()

			creatorID := tid("tp-term-creator-" + tc.name)
			adminID := tid("tp-term-admin-" + tc.name)
			require.NoError(t, s.CreateUser(ctx, &store.User{
				ID: creatorID, Email: "creator-" + tc.name + "@example.com", DisplayName: "Creator", Role: store.UserRoleMember, Status: store.UserStatusActive,
			}))
			require.NoError(t, s.CreateUser(ctx, &store.User{
				ID: adminID, Email: "admin-" + tc.name + "@example.com", DisplayName: "Admin", Role: store.UserRoleAdmin, Status: store.UserStatusActive,
			}))

			project := &store.Project{
				ID:   tid("tp-term-proj-" + tc.name),
				Name: "TP Terminal Project " + tc.name,
				Slug: "tp-term-proj-" + tc.name,
			}
			require.NoError(t, s.CreateProject(ctx, project))

			broker := &store.RuntimeBroker{
				ID:       tid("tp-term-broker-" + tc.name),
				Name:     "TP Terminal Broker",
				Slug:     "tp-term-broker-" + tc.name,
				Status:   store.BrokerStatusOnline,
				Endpoint: "http://localhost:9803",
			}
			require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

			agent := &store.Agent{
				ID:                tid("tp-term-agent-" + tc.name),
				Name:              "worker-term-" + tc.name,
				Slug:              "worker-term-" + tc.name,
				ProjectID:         project.ID,
				RuntimeBrokerID:   broker.ID,
				Phase:             string(state.PhaseRunning),
				Activity:          string(tc.activity),
				LastActivityEvent: time.Now(),
				Template:          "claude",
				CreatedBy:         creatorID,
				OwnerID:           creatorID,
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			// Custom creator subscription watching both STOPPED and the terminal activity.
			sub := &store.NotificationSubscription{
				ID:                api.NewUUID(),
				Scope:             store.SubscriptionScopeAgent,
				AgentID:           agent.ID,
				SubscriberType:    store.SubscriberTypeUser,
				SubscriberID:      creatorID,
				ProjectID:         project.ID,
				TriggerActivities: []string{"STOPPED", tc.expectedStatus},
				CreatedAt:         time.Now().Add(-time.Minute),
				CreatedBy:         creatorID,
			}
			require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

			// Deliver the initial terminal notification before the third-party stop.
			srv.events.PublishAgentStatus(ctx, agent)
			waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

			initialNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
			require.NoError(t, err)
			require.Len(t, initialNotifs, 1, "initial terminal notification should be delivered once")
			require.Equal(t, tc.expectedStatus, initialNotifs[0].Status)
			require.Equal(t, sub.ID, initialNotifs[0].SubscriptionID)

			// Third-party admin stops the agent while terminal activity is preserved on the stopped row.
			adminIdentity := NewAuthenticatedUser(adminID, "admin-"+tc.name+"@example.com", "Admin", store.UserRoleAdmin, "web")
			stopBody, _ := json.Marshal(AgentLifecycleStopRequest{Reason: "cleanup after " + tc.name})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", bytes.NewReader(stopBody))
			req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
			rec := httptest.NewRecorder()
			srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionStop)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			srv.thirdPartyStopDispatches.Wait()

			waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

			notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
			require.NoError(t, err)
			require.Len(t, notifs, 2, "must have only the original %s notice and one third-party STOPPED notice (no stale %s refire)", tc.expectedStatus, tc.expectedStatus)

			var terminalCount, stopCount int
			for _, n := range notifs {
				switch n.Status {
				case tc.expectedStatus:
					terminalCount++
					assert.Equal(t, sub.ID, n.SubscriptionID)
				case "STOPPED":
					stopCount++
					assert.Equal(t, store.CreatorFallbackSubscriptionID, n.SubscriptionID)
					assert.Contains(t, n.Message, "was stopped by Admin")
					assert.Contains(t, n.Message, ": cleanup after "+tc.name)
				default:
					t.Fatalf("unexpected notification status %q: %s", n.Status, n.Message)
				}
			}
			assert.Equal(t, 1, terminalCount, "terminal notice must not be re-fired")
			assert.Equal(t, 1, stopCount, "third-party stop notice must be delivered once")

			// Re-stopping the already-stopped agent must emit neither a second stop notice nor a stale terminal notice.
			req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
			req = req.WithContext(contextWithIdentity(req.Context(), adminIdentity))
			rec = httptest.NewRecorder()
			srv.handleAgentLifecycle(rec, req, agent.ID, api.AgentActionStop)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			srv.thirdPartyStopDispatches.Wait()

			waitForNotificationDispatcherDrain(t, s, srv.events, project.ID)

			afterRestopNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
			require.NoError(t, err)
			assert.Len(t, afterRestopNotifs, 2, "re-stop must not emit any additional notification")
		})
	}
}
