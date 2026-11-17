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

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setFastDecisionBell(t *testing.T, srv *Server, debounce time.Duration) {
	t.Helper()
	srv.decisionBell.mu.Lock()
	srv.decisionBell.debounce = debounce
	srv.decisionBell.mu.Unlock()
	t.Cleanup(func() {
		srv.stopDecisionBell()
	})
}

func waitDecisionBellIdle(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	st := &srv.decisionBell
	for {
		st.mu.Lock()
		idle := !st.started || st.stopped || st.isIdleLocked()
		st.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for decisionBell worker queue to become idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sendTopicChoicePromptAsAgent(t *testing.T, srv *Server, st store.Store, ag *store.Agent, projectID, topicID, raw string, update bool) choiceSendResponse {
	t.Helper()
	conv, err := st.GetConversationByExternalRef(context.Background(), "native", "thread:"+projectID+":"+topicID)
	require.NoError(t, err)
	body, err := json.Marshal(OutboundMessageRequest{
		Msg:             "Please decide",
		ConversationRef: "conv:" + conv.ID,
		Decisions:       decisionInputs(t, raw),
		DecisionsUpdate: update,
	})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+ag.ID+"/outbound-message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(agentCtx(r.Context(), ag))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, r, ag.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp choiceSendResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

func TestDecisionsBell_FormatMessageAndDeterministicID(t *testing.T) {
	idA1 := decisionsWaitingNotificationID("user-a")
	idA2 := decisionsWaitingNotificationID("user-a")
	idB := decisionsWaitingNotificationID("user-b")
	assert.Equal(t, idA1, idA2)
	assert.NotEqual(t, idA1, idB)

	assert.Equal(t, "No decisions are waiting on you", formatDecisionsWaitingMessage(0, 0))
	assert.Equal(t, "1 decision is waiting on you in 1 project", formatDecisionsWaitingMessage(1, 1))
	assert.Equal(t, "1 decision is waiting on you in 2 projects", formatDecisionsWaitingMessage(1, 2))
	assert.Equal(t, "3 decisions are waiting on you in 1 project", formatDecisionsWaitingMessage(3, 1))
	assert.Equal(t, "3 decisions are waiting on you in 2 projects", formatDecisionsWaitingMessage(3, 2))
}

// TestDecisionsBell_B2LifecycleAndG6Scenario verifies B1, B2, B4, AC 8, AC 9,
// and AC 13 (Gate G6):
//   - Direct and group-bound posting members get one coalesced decisions_waiting row;
//   - Non-member hub admins (AC 8), project viewers (read-only), and outsiders get none;
//   - Multi-project counts format "N decisions are waiting on you in M projects";
//   - Mark read hides the row while preserving resource_id=P across orphan GC;
//   - Answering 1 of 3 keeps A's row acknowledged (hidden) with resource_id="2" while B's updates to 2;
//   - A new prompt (N=3 > P=2) resurfaces A's row as unacknowledged;
//   - Answering all clears both A's and B's rows (acknowledged=true, resource_id="0") and publishes acknowledged=true on user.<id>.notification.
func TestDecisionsBell_B2LifecycleAndG6Scenario(t *testing.T) {
	f := newChoicesFixture(t)
	setChoicesExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 15*time.Millisecond)
	ctx := context.Background()

	// Create a non-member Hub Admin (AC 8), a read-only project viewer, and a
	// group-bound project member.
	hubAdminNonMember := &store.User{
		ID: tid("bell-hub-admin"), Email: "bell-admin@example.com", DisplayName: "Hub Admin",
		Role: store.UserRoleAdmin, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, hubAdminNonMember))
	ensureHubMembership(ctx, f.store, hubAdminNonMember.ID)

	viewerOnly := &store.User{
		ID: tid("bell-viewer"), Email: "bell-viewer@example.com", DisplayName: "Viewer Only",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, viewerOnly))
	ensureHubMembership(ctx, f.store, viewerOnly.ID)
	viewerRD, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      viewerOnly.ID,
		RoleDefinitionID: viewerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.project.ID,
		CreatedAt:        time.Now(),
	})
	require.NoError(t, err)

	groupMember := &store.User{
		ID: tid("bell-group-member"), Email: "bell-group@example.com", DisplayName: "Group Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, groupMember))
	ensureHubMembership(ctx, f.store, groupMember.ID)
	grp := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Bell Team",
		Slug:      "bell-team",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   f.alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, f.store.CreateGroup(ctx, grp))
	require.NoError(t, f.store.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    grp.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   groupMember.ID,
		Role:       store.GroupMemberRoleMember,
		AddedAt:    time.Now(),
	}))
	projMembersGroup, err := f.store.GetGroupBySlug(ctx, projectMembersGroupSlug(f.project.Slug))
	require.NoError(t, err)
	require.NoError(t, f.store.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    projMembersGroup.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   grp.ID,
		Role:       store.GroupMemberRoleMember,
		AddedAt:    time.Now(),
	}))
	memberRD, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      grp.ID,
		RoleDefinitionID: memberRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.project.ID,
		CreatedAt:        time.Now(),
	})
	require.NoError(t, err)
	agentMsgRD, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "bell-group-agent-message",
		Description: "Grants agent.message to group members in bell test",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"agent.message"},
	})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      grp.ID,
		RoleDefinitionID: agentMsgRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.project.ID,
		CreatedAt:        time.Now(),
	})
	require.NoError(t, err)

	// Allow project members to message f.agent so Alice, Bob, and groupMember can all answer.
	f.agent.Ancestry = []string{f.bob.ID}
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	aliceSub, unsubAlice := f.srv.events.Subscribe("user." + f.alice.ID + ".notification")
	defer unsubAlice()
	bobSub, unsubBob := f.srv.events.Subscribe("user." + f.bob.ID + ".notification")
	defer unsubBob()

	// Step 1: Post 3 decisions in the project topic (a 1-row prompt + a 2-row set prompt).
	p1 := sendTopicChoicePromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"d1","question":"First call?","choices":["yes","no"]}]`, false)
	p2 := sendTopicChoicePromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"d2","question":"Second call?","choices":["yes","no"]},{"id":"d3","question":"Third call?","choices":["a","b"]}]`, false)
	waitDecisionBellIdle(t, f.srv)

	for _, u := range []*store.User{f.alice, f.bob, groupMember} {
		notifs, gerr := f.store.GetNotifications(ctx, store.SubscriberTypeUser, u.ID, true)
		require.NoError(t, gerr)
		require.Len(t, notifs, 1, "user %s should have 1 unacknowledged bell row", u.Email)
		assert.Equal(t, decisionsWaitingNotificationID(u.ID), notifs[0].ID)
		assert.Equal(t, store.NotificationKindDecisionsWaiting, notifs[0].Kind)
		assert.Equal(t, store.NotificationStatusDecisionsWaiting, notifs[0].Status)
		assert.Equal(t, store.NotificationResourceDecisions, notifs[0].ResourceType)
		assert.Equal(t, "3", notifs[0].ResourceID)
		assert.Equal(t, "3 decisions are waiting on you in 1 project", notifs[0].Message)
		assert.Equal(t, uuid.Nil.String(), notifs[0].ProjectID)
		assert.Equal(t, uuid.Nil.String(), notifs[0].AgentID)
		assert.False(t, notifs[0].Acknowledged)
	}

	// Non-member hub admin (AC 8), read-only viewer, and outsider receive nothing.
	for _, excluded := range []*store.User{hubAdminNonMember, viewerOnly, f.outsider} {
		notifs, gerr := f.store.GetNotifications(ctx, store.SubscriberTypeUser, excluded.ID, false)
		require.NoError(t, gerr)
		assert.Empty(t, notifs, "excluded user %s must get no bell notification", excluded.Email)
	}

	// Step 2: Second project where only Alice is a member -> Alice sees "4 decisions ... in 2 projects".
	proj2 := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Second Project",
		Slug:      "second-project",
		OwnerID:   f.alice.ID,
		CreatedBy: f.alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, f.store.CreateProject(ctx, proj2))
	f.srv.seedProjectCreatorMembership(ctx, proj2)
	ag2 := &store.Agent{
		ID:          api.NewUUID(),
		Slug:        "proj2-bot",
		Name:        "Proj2 Bot",
		ProjectID:   proj2.ID,
		OwnerID:     f.alice.ID,
		Ancestry:    []string{f.alice.ID},
		Phase:       "running",
		MessageMode: store.MessageModeProject,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(ctx, ag2))
	topic2 := createTestDecisionTopic(t, f.srv.webChatStore, proj2.ID, "ops", f.alice.ID)
	setTopicConversationID(t, f.db, f.store, topic2, proj2.ID)
	pProj2 := sendTopicChoicePromptAsAgent(t, f.srv, f.store, ag2, proj2.ID, topic2,
		`[{"id":"p2-d1","question":"Deploy proj2?","choices":["yes","no"]}]`, false)
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.alice.ID))
	require.NoError(t, err)
	assert.Equal(t, "4", aliceRow.ResourceID)
	assert.Equal(t, "4 decisions are waiting on you in 2 projects", aliceRow.Message)
	assert.False(t, aliceRow.Acknowledged)

	// Answer proj2 decision so Alice is back to 3 in 1 project.
	rr := f.answer(t, f.alice, pProj2.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err = f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.alice.ID))
	require.NoError(t, err)
	assert.Equal(t, "3", aliceRow.ResourceID)
	assert.Equal(t, "3 decisions are waiting on you in 1 project", aliceRow.Message)
	assert.False(t, aliceRow.Acknowledged)

	// Step 3 (G6): Alice marks her notification read via HTTP API.
	ackRR := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/notifications/"+aliceRow.ID+"/ack", nil)
	require.Equal(t, http.StatusOK, ackRR.Code, "body: %s", ackRR.Body.String())

	aliceUnack, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.alice.ID, true)
	require.NoError(t, err)
	assert.Empty(t, aliceUnack, "Mark read hides the row for Alice")

	// Orphan GC must NOT purge Alice's acknowledged row while resource_id != "0".
	_, err = f.store.PurgeOrphanedNotifications(ctx)
	require.NoError(t, err)
	aliceRow, err = f.store.GetNotification(ctx, aliceRow.ID)
	require.NoError(t, err)
	assert.True(t, aliceRow.Acknowledged)
	assert.Equal(t, "3", aliceRow.ResourceID)

	// Drain prior SSE events before testing the remaining G6 steps.
	_ = collectEvents(aliceSub)
	_ = collectEvents(bobSub)

	aliceCreatedBeforeKeep := aliceRow.CreatedAt
	bobBeforeKeep, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	bobCreatedBeforeKeep := bobBeforeKeep.CreatedAt

	// Step 4 (G6): Alice answers 1 decision (p1). N drops 3 -> 2:
	// Alice stays acknowledged (hidden) with resource_id="2"; Bob stays unacknowledged and updates to 2.
	// Both keep paths leave CreatedAt unchanged.
	rr = f.answer(t, f.alice, p1.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err = f.store.GetNotification(ctx, aliceRow.ID)
	require.NoError(t, err)
	assert.True(t, aliceRow.Acknowledged, "0 < N <= P must preserve acknowledged=true")
	assert.Equal(t, "2", aliceRow.ResourceID)
	assert.Equal(t, "2 decisions are waiting on you in 1 project", aliceRow.Message)
	assert.True(t, aliceRow.CreatedAt.Equal(aliceCreatedBeforeKeep), "keep path leaves CreatedAt unchanged")

	bobRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.False(t, bobRow.Acknowledged)
	assert.Equal(t, "2", bobRow.ResourceID)
	assert.Equal(t, "2 decisions are waiting on you in 1 project", bobRow.Message)
	assert.True(t, bobRow.CreatedAt.Equal(bobCreatedBeforeKeep), "keep path leaves CreatedAt unchanged")

	// Step 5 (G6): A new 1-row prompt arrives -> N becomes 3 > P (2):
	// Alice's row resurfaces (acknowledged=false) and refreshes CreatedAt!
	time.Sleep(2 * time.Millisecond)
	p3 := sendTopicChoicePromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"d4","question":"Fourth call?","choices":["yes","no"]}]`, false)
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err = f.store.GetNotification(ctx, aliceRow.ID)
	require.NoError(t, err)
	assert.False(t, aliceRow.Acknowledged, "N > P must reset acknowledged=false so the bell resurfaces")
	assert.Equal(t, "3", aliceRow.ResourceID)
	assert.Equal(t, "3 decisions are waiting on you in 1 project", aliceRow.Message)
	assert.True(t, aliceRow.CreatedAt.After(aliceCreatedBeforeKeep), "reset-to-unacknowledged refreshes CreatedAt")

	// Step 6 (G6): Alice answers the 2-row set (p2) and the remaining 1-row prompt (p3) -> N == 0:
	// Both Alice's and Bob's rows clear (acknowledged=true, resource_id="0") and publish acknowledged=true on SSE.
	_ = collectEvents(aliceSub)
	_ = collectEvents(bobSub)

	batchRR := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/decision-prompts/"+p2.MessageID+"/answers", ChoiceSetAnswerRequest{
		IdempotencyKey: api.NewUUID(),
		Answers: []ChoiceSetAnswerItem{
			{DecisionRowID: p2.Decisions[0].RowID, ChoiceID: "1"},
			{DecisionRowID: p2.Decisions[1].RowID, ChoiceID: "1"},
		},
	})
	require.Equal(t, http.StatusOK, batchRR.Code, "body: %s", batchRR.Body.String())
	rr = f.answer(t, f.alice, p3.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	waitDecisionBellIdle(t, f.srv)

	for _, u := range []*store.User{f.alice, f.bob, groupMember} {
		row, gerr := f.store.GetNotification(ctx, decisionsWaitingNotificationID(u.ID))
		require.NoError(t, gerr)
		assert.True(t, row.Acknowledged, "N == 0 must acknowledge row for %s", u.Email)
		assert.Equal(t, "0", row.ResourceID)
		unack, lerr := f.store.GetNotifications(ctx, store.SubscriberTypeUser, u.ID, true)
		require.NoError(t, lerr)
		assert.Empty(t, unack)
	}

	aliceEvts := collectEvents(aliceSub)
	require.NotEmpty(t, aliceEvts)
	var lastAliceEvt UserNotificationEvent
	require.NoError(t, json.Unmarshal(aliceEvts[len(aliceEvts)-1].Data, &lastAliceEvt))
	assert.True(t, lastAliceEvt.Acknowledged, "clearing SSE event must carry acknowledged=true")
	assert.Equal(t, "0", lastAliceEvt.ResourceID)
}

type notificationWriteCountStore struct {
	store.Store
	fault   *storeFaultSwitch
	creates atomic.Int64
	updates atomic.Int64
}

func (s *notificationWriteCountStore) CreateNotification(ctx context.Context, notif *store.Notification) error {
	if s.fault == nil || s.fault.Active() {
		s.creates.Add(1)
	}
	return s.Store.CreateNotification(ctx, notif)
}

func (s *notificationWriteCountStore) UpdateNotificationState(ctx context.Context, id, message, resourceID string, acknowledged bool) error {
	if s.fault == nil || s.fault.Active() {
		s.updates.Add(1)
	}
	return s.Store.UpdateNotificationState(ctx, id, message, resourceID, acknowledged)
}

func (s *notificationWriteCountStore) UpdateNotificationStateIfAcknowledged(ctx context.Context, id, message, resourceID string, observedAcknowledged bool) error {
	if s.fault == nil || s.fault.Active() {
		s.updates.Add(1)
	}
	return s.Store.UpdateNotificationStateIfAcknowledged(ctx, id, message, resourceID, observedAcknowledged)
}

// TestDecisionsBell_DebounceCoalescesBurst verifies B5: a 10-row prompt and a
// rapid 10-event burst in the debounce queue coalesce into a single
// notification write per user regardless of host CPU scheduling.
func TestDecisionsBell_DebounceCoalescesBurst(t *testing.T) {
	srv, baseStore, alice, bob, project, spy, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *notificationWriteCountStore {
		return &notificationWriteCountStore{Store: inner, fault: f}
	})
	f := newChoicesFixtureOn(t, srv, spy, alice, bob, project)
	fault.Arm()
	setChoicesExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 30*time.Millisecond)

	// 1. Single 10-row prompt in Bob's DM -> 1 write.
	var ten []map[string]interface{}
	for i := 0; i < 10; i++ {
		ten = append(ten, map[string]interface{}{
			"id":       "burst-" + api.NewUUID()[:8],
			"question": "Question?",
			"choices":  []string{"yes", "no"},
		})
	}
	rawTen, err := json.Marshal(ten)
	require.NoError(t, err)
	f.sendPromptToBob(t, string(rawTen), false)
	waitDecisionBellIdle(t, f.srv)

	assert.Equal(t, int64(1), spy.creates.Load()+spy.updates.Load(), "10-row prompt must coalesce into 1 write")
	row, err := f.store.GetNotification(context.Background(), decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.Equal(t, "10", row.ResourceID)
	assert.Equal(t, "10 decisions are waiting on you in 1 project", row.Message)

	// 2. Burst of 10 separate DM decision events in the queue -> 1 additional write.
	// Seed the 10 decisions directly and hold worker dequeue until all 10
	// enqueueDecisionBellEvent calls have landed so CPU contention can never
	// split the burst across two earliest-wins windows.
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		decs, nerr := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"question":"Rapid?","choices":["y","n"]}]`))
		require.NoError(t, nerr)
		_, cerr := baseStore.CreateChoicePrompt(context.Background(), &store.ChoicePromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
				Recipient: "user:" + f.bob.ID, ThreadID: dmKey, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			Intro: "Rapid burst", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name, Decisions: decs,
		})
		require.NoError(t, cerr)
	}

	before := spy.creates.Load() + spy.updates.Load()
	holdCh := make(chan struct{})
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.testHoldCh = holdCh
	f.srv.decisionBell.mu.Unlock()

	for i := 0; i < 10; i++ {
		f.srv.enqueueDecisionBellEvent(f.project.ID, dmKey, "")
	}

	f.srv.decisionBell.mu.Lock()
	queuedUsers := len(f.srv.decisionBell.userDue)
	f.srv.decisionBell.testHoldCh = nil
	close(holdCh)
	f.srv.decisionBell.signalWakeLocked()
	f.srv.decisionBell.mu.Unlock()

	assert.Equal(t, 1, queuedUsers, "10 enqueues for the same user must coalesce into 1 pending queue entry")
	waitDecisionBellIdle(t, f.srv)

	after := spy.creates.Load() + spy.updates.Load()
	assert.Equal(t, before+1, after, "10 coalesced queue events must produce exactly 1 write")
	row, err = f.store.GetNotification(context.Background(), decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.Equal(t, "20", row.ResourceID)
}

type errAlreadyExistsOnceStore struct {
	store.Store
	fault    *storeFaultSwitch
	injected atomic.Bool
	seed     func(ctx context.Context) error
}

func (s *errAlreadyExistsOnceStore) CreateNotification(ctx context.Context, notif *store.Notification) error {
	if (s.fault == nil || s.fault.Active()) && s.injected.CompareAndSwap(false, true) {
		if err := s.seed(ctx); err != nil {
			return err
		}
		return store.ErrAlreadyExists
	}
	return s.Store.CreateNotification(ctx, notif)
}

// TestDecisionsBell_DeterministicIDConcurrentRace verifies B5 idempotence:
// concurrent recomputes and ErrAlreadyExists races on the deterministic UUIDv5
// row ID converge cleanly on the true count without failing.
func TestDecisionsBell_DeterministicIDConcurrentRace(t *testing.T) {
	srv, baseStore, alice, bob, project, raceStore, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *errAlreadyExistsOnceStore {
		return &errAlreadyExistsOnceStore{Store: inner, fault: f}
	})
	raceStore.seed = func(ctx context.Context) error {
		return baseStore.CreateNotification(ctx, &store.Notification{
			ID:             decisionsWaitingNotificationID(bob.ID),
			SubscriptionID: store.CreatorFallbackSubscriptionID,
			AgentID:        uuid.Nil.String(),
			ProjectID:      uuid.Nil.String(),
			SubscriberType: store.SubscriberTypeUser,
			SubscriberID:   bob.ID,
			Status:         store.NotificationStatusDecisionsWaiting,
			Message:        "1 decision is waiting on you in 1 project",
			Kind:           store.NotificationKindDecisionsWaiting,
			ResourceType:   store.NotificationResourceDecisions,
			ResourceID:     "1",
			Acknowledged:   true,
			CreatedAt:      time.Now().UTC(),
		})
	}
	f := newChoicesFixtureOn(t, srv, raceStore, alice, bob, project)
	fault.Arm()
	setChoicesExperiment(t, f.srv, true)

	// Seed 2 open DM decisions directly in the store (without background worker)
	// and fire concurrent recomputeDecisionBellUser calls.
	decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, twoDecisions))
	require.NoError(t, err)
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	_, err = baseStore.CreateChoicePrompt(context.Background(), &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "user:" + f.bob.ID, ThreadID: dmKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "Race test", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name, Decisions: decs,
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < len(errs); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = f.srv.recomputeDecisionBellUser(context.Background(), f.bob.ID)
		}(i)
	}
	wg.Wait()
	for i, rerr := range errs {
		assert.NoError(t, rerr, "goroutine %d", i)
	}

	row, err := baseStore.GetNotification(context.Background(), decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.Equal(t, "2", row.ResourceID)
	assert.Equal(t, "2 decisions are waiting on you in 1 project", row.Message)
	assert.False(t, row.Acknowledged)
	assert.True(t, raceStore.injected.Load(), "ErrAlreadyExists retry path must have executed")
}

// TestDecisionsBell_B8TopicCapAndB6Reconcile verifies B8 and B6:
//   - Default cap is 500 members;
//   - Projects exceeding the member cap skip immediate topic fan-out and log project_id + member_count;
//   - DM prompts in >cap projects still update the DM user;
//   - B6 reconcile on GET /api/v1/users/me/decisions?tab=pending&limit=0 and
//     GET /api/v1/projects/{id}/decisions?tab=pending heals skipped/missed rows
//     after responding, and performs zero writes when already in sync.
func TestDecisionsBell_B8TopicCapAndB6Reconcile(t *testing.T) {
	srv, _, alice, bob, project, spy, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *notificationWriteCountStore {
		return &notificationWriteCountStore{Store: inner, fault: f}
	})
	f := newChoicesFixtureOn(t, srv, spy, alice, bob, project)
	fault.Arm()
	setChoicesExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 15*time.Millisecond)
	ctx := context.Background()

	assert.Equal(t, 500, f.srv.decisionBellTopicFanoutCap(), "default B8 cap must be 500")

	// Add a 3rd project member (carol) and lower the cap to 2 for this server.
	carol := &store.User{
		ID: tid("bell-carol"), Email: "bell-carol@example.com", DisplayName: "Carol",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, carol))
	ensureHubMembership(ctx, f.store, carol.ID)
	msgAuthzAddProjectMember(t, f.store, carol.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)
	msgAuthzGrantAgentMessage(t, f.store, carol.ID, f.project.ID)
	f.agent.Ancestry = []string{f.bob.ID}
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.maxTopicMembers = 2
	f.srv.decisionBell.mu.Unlock()

	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(origLogger)

	// Topic prompt in >cap project skips member fan-out and logs project_id + member_count.
	sendTopicChoicePromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID, oneDecision, false)
	waitDecisionBellIdle(t, f.srv)

	assert.Contains(t, logBuf.String(), "skipping topic fan-out over project member cap")
	assert.Contains(t, logBuf.String(), f.project.ID)
	assert.Contains(t, logBuf.String(), "member_count=3")

	for _, u := range []*store.User{f.alice, f.bob, carol} {
		notifs, gerr := f.store.GetNotifications(ctx, store.SubscriberTypeUser, u.ID, false)
		require.NoError(t, gerr)
		assert.Empty(t, notifs, "topic fan-out must be skipped above cap for %s", u.Email)
	}

	// DM prompt in the same >cap project STILL triggers per-user recompute for Bob.
	f.sendPromptToBob(t, `[{"id":"dm-cap","question":"DM works?","choices":["yes","no"]}]`, false)
	waitDecisionBellIdle(t, f.srv)
	bobRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.Equal(t, "2", bobRow.ResourceID, "Bob's recompute includes both the topic prompt and the DM prompt")

	// B6 Reconcile: Alice opens /api/v1/users/me/decisions?tab=pending&limit=0 -> heals Alice's bell!
	rr := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, "/api/v1/users/me/decisions?tab=pending&limit=0", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.alice.ID))
	require.NoError(t, err)
	assert.Equal(t, "1", aliceRow.ResourceID)
	assert.False(t, aliceRow.Acknowledged)

	// Calling pending reconcile again when already in sync performs zero writes.
	writesBefore := spy.creates.Load() + spy.updates.Load()
	rr = doRequestAsUser(t, f.srv, f.alice, http.MethodGet, "/api/v1/users/me/decisions?tab=pending&limit=0", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	waitDecisionBellIdle(t, f.srv)
	assert.Equal(t, writesBefore, spy.creates.Load()+spy.updates.Load(), "in-sync reconcile must do zero writes")

	// Carol opens the project Decisions page (/api/v1/projects/{id}/decisions?tab=pending) -> heals Carol's bell!
	projRR := doRequestAsUser(t, f.srv, carol, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/decisions?tab=pending", nil)
	require.Equal(t, http.StatusOK, projRR.Code, "body: %s", projRR.Body.String())
	waitDecisionBellIdle(t, f.srv)

	carolRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(carol.ID))
	require.NoError(t, err)
	assert.Equal(t, "1", carolRow.ResourceID)
	assert.False(t, carolRow.Acknowledged)
}

// TestDecisionsBell_NoExternalChannelOrBrokerDispatch verifies B1 & §9:
// decisions_waiting never fans out to ChannelRegistry, MessageBrokerProxy,
// AgentDispatcher, or broadcast/project notification subjects.
func TestDecisionsBell_NoExternalChannelOrBrokerDispatch(t *testing.T) {
	f := newChoicesFixture(t)
	setChoicesExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 15*time.Millisecond)

	bus := &spyBrokerBus{}
	f.srv.SetMessageBrokerProxy(NewMessageBrokerProxy(bus, f.store, f.srv.events,
		func() AgentDispatcher { return f.dispatcher }, slog.Default()))
	ch := &summaryRecordingChannel{}
	reg := &ChannelRegistry{entries: []channelEntry{{channel: ch, config: ChannelConfig{Type: "recording"}}}, log: slog.Default()}
	f.srv.SetChannelRegistry(reg)

	broadcastSub, unsubB := f.srv.events.Subscribe("notification.>")
	defer unsubB()
	projNotifSub, unsubP := f.srv.events.Subscribe("project." + f.project.ID + ".notification")
	defer unsubP()
	bobNotifSub, unsubU := f.srv.events.Subscribe("user." + f.bob.ID + ".notification")
	defer unsubU()

	// Trigger bell write directly through recompute after seeding a decision in store
	// so broker/dispatcher activity reflects ONLY the bell writer.
	decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, oneDecision))
	require.NoError(t, err)
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	_, err = f.store.CreateChoicePrompt(context.Background(), &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "user:" + f.bob.ID, ThreadID: dmKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "No external dispatch", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name, Decisions: decs,
	})
	require.NoError(t, err)

	beforeDispatchCalls := len(f.dispatcher.getMessages())
	f.srv.enqueueDecisionBellEvent(f.project.ID, dmKey, f.bob.ID)
	waitDecisionBellIdle(t, f.srv)

	bobEvts := collectEvents(bobNotifSub)
	require.Len(t, bobEvts, 1, "user.<id>.notification must receive the bell update")
	assert.Empty(t, collectEvents(broadcastSub), "notification.> must receive nothing")
	assert.Empty(t, collectEvents(projNotifSub), "project.<id>.notification must receive nothing")
	assert.Zero(t, ch.count(), "ChannelRegistry must never be called for decisions_waiting")
	assert.Empty(t, bus.getEvents(), "MessageBrokerProxy must never be called for decisions_waiting")
	assert.Len(t, f.dispatcher.getMessages(), beforeDispatchCalls, "AgentDispatcher must never be called for decisions_waiting")
}

// TestDecisionsBell_ExperimentMatrixAndFlagOffReconcile verifies:
//   - When web.decisions is off, the bell writer is a no-op;
//   - Rows written while web.decisions was on are acknowledged on the next reconcile when it is off.
func TestDecisionsBell_ExperimentMatrixAndFlagOffReconcile(t *testing.T) {
	f := newChoicesFixture(t)
	setFastDecisionBell(t, f.srv, 15*time.Millisecond)
	ctx := context.Background()

	// Seed a pending decision for Bob.
	_, res := f.topicPrompt(t, oneDecision, false)
	require.Len(t, res.Decisions, 1)

	setChoicesExperiment(t, f.srv, false)
	f.srv.enqueueDecisionBellEvent(f.project.ID, f.topicID, f.bob.ID)
	require.NoError(t, f.srv.recomputeDecisionBellUser(ctx, f.bob.ID))
	waitDecisionBellIdle(t, f.srv)
	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.bob.ID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs, "no bell writes allowed when web.decisions is off")

	// Enable web.decisions -> write unacknowledged rows for Alice and Bob.
	setChoicesExperiment(t, f.srv, true)
	require.NoError(t, f.srv.recomputeDecisionBellUser(ctx, f.bob.ID))
	require.NoError(t, f.srv.recomputeDecisionBellUser(ctx, f.alice.ID))

	bobRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.False(t, bobRow.Acknowledged)

	// Turn off web.decisions -> reconcile via /users/me/decisions acknowledges Bob's row.
	setChoicesExperiment(t, f.srv, false)
	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/users/me/decisions?tab=pending&limit=0", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	waitDecisionBellIdle(t, f.srv)

	bobRow, err = f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.True(t, bobRow.Acknowledged, "flag-off reconcile on /users/me/decisions must acknowledge stale row")
	assert.Equal(t, "0", bobRow.ResourceID)

	// Reconcile via GET /api/v1/notifications when flag is off also acknowledges Alice's stale row.
	notifRR := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, "/api/v1/notifications", nil)
	require.Equal(t, http.StatusOK, notifRR.Code)
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.alice.ID))
	require.NoError(t, err)
	assert.True(t, aliceRow.Acknowledged, "flag-off reconcile on /notifications must acknowledge stale row")
	assert.Equal(t, "0", aliceRow.ResourceID)
}

// TestDecisionsBell_InterpretedAndReplacedLifecycle verifies B4 triggers on
// agent-recorded interpreted answers and --update replacements.
func TestDecisionsBell_InterpretedAndReplacedLifecycle(t *testing.T) {
	f := newChoicesFixture(t)
	setChoicesExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 15*time.Millisecond)
	ctx := context.Background()

	f.agent.Ancestry = []string{f.bob.ID}
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	// 1. Topic prompt -> both Alice and Bob have 1 waiting decision.
	p1 := sendTopicChoicePromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"interp-key","question":"Ship?","choices":["yes","no"]}]`, false)
	waitDecisionBellIdle(t, f.srv)

	for _, u := range []*store.User{f.alice, f.bob} {
		row, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(u.ID))
		require.NoError(t, err)
		assert.False(t, row.Acknowledged)
		assert.Equal(t, "1", row.ResourceID)
	}

	// 2. Bob posts a message in the topic, and the agent records an interpreted answer naming Bob
	// via POST /api/v1/agent/decisions/record-interpreted:
	// Alice moves to 0 waiting (other_user_confirm -> cleared!), while Bob stays at 1 waiting (needs confirm).
	conv, err := f.store.GetConversationByExternalRef(ctx, "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	srcID := api.NewUUID()
	require.NoError(t, f.store.CreateMessage(ctx, &store.Message{
		ID:             srcID,
		ProjectID:      f.project.ID,
		ConversationID: conv.ID,
		Sender:         "user:" + f.bob.ID,
		SenderID:       f.bob.ID,
		Recipient:      "chat",
		ThreadID:       f.topicID,
		Type:           messages.TypeChat,
		Channel:        "web",
		Msg:            "yes please",
		CreatedAt:      time.Now().UTC(),
	}))
	recRR := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: srcID,
		Answers: []InterpretedAnswerInput{
			{Key: "interp-key", ChoiceID: "1"},
		},
	})
	require.Equal(t, http.StatusOK, recRR.Code, "body: %s", recRR.Body.String())
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.alice.ID))
	require.NoError(t, err)
	assert.True(t, aliceRow.Acknowledged, "Alice's row clears when Bob is recorded as the interpreted answerer")
	assert.Equal(t, "0", aliceRow.ResourceID)

	bobRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.False(t, bobRow.Acknowledged, "Bob still needs to confirm the interpreted answer")
	assert.Equal(t, "1", bobRow.ResourceID)

	// 3. Agent replaces interp-key (--update) -> new open version resurfaces Alice's row!
	_ = p1
	sendTopicChoicePromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"interp-key","question":"Ship v2?","choices":["yes","no"]}]`, true)
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err = f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.alice.ID))
	require.NoError(t, err)
	assert.False(t, aliceRow.Acknowledged, "replaced open decision resurfaces Alice's bell")
	assert.Equal(t, "1", aliceRow.ResourceID)
}

type interleaveAckOnKeepWriteStore struct {
	store.Store
	fault         *storeFaultSwitch
	interleaved   atomic.Bool
	conflicts     atomic.Int64
	onBeforeWrite func()
}

func (s *interleaveAckOnKeepWriteStore) UpdateNotificationStateIfAcknowledged(ctx context.Context, id, message, resourceID string, observedAcknowledged bool) error {
	if (s.fault == nil || s.fault.Active()) && !observedAcknowledged && s.onBeforeWrite != nil && s.interleaved.CompareAndSwap(false, true) {
		s.onBeforeWrite()
	}
	err := s.Store.UpdateNotificationStateIfAcknowledged(ctx, id, message, resourceID, observedAcknowledged)
	if err != nil {
		s.conflicts.Add(1)
	}
	return err
}

// TestDecisionsBell_ConcurrentMarkReadNotUndoneByDecrease verifies that when a
// user clicks Mark read (POST /api/v1/notifications/{id}/ack) between the
// recompute worker's GetNotification(acknowledged=false, P=3) and its keep-path
// write (N=2), the conditional update detects the concurrent acknowledgement,
// retries, preserves acknowledged=true with resource_id="2", and never
// publishes an unacknowledged row on user.<id>.notification.
func TestDecisionsBell_ConcurrentMarkReadNotUndoneByDecrease(t *testing.T) {
	srv, _, alice, bob, project, hookStore, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *interleaveAckOnKeepWriteStore {
		return &interleaveAckOnKeepWriteStore{Store: inner, fault: f}
	})
	f := newChoicesFixtureOn(t, srv, hookStore, alice, bob, project)
	setChoicesExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 15*time.Millisecond)
	ctx := context.Background()

	// Seed 3 waiting DM decisions for Bob (a 1-row prompt + a 2-row prompt) so
	// Bob starts with P=3, acknowledged=false and can answer 1 decision (3 -> 2).
	p1 := f.sendPromptToBob(t, `[{"id":"race-1","question":"Q1?","choices":["yes","no"]}]`, false)
	f.sendPromptToBob(t, `[
		{"id":"race-2","question":"Q2?","choices":["yes","no"]},
		{"id":"race-3","question":"Q3?","choices":["yes","no"]}
	]`, false)
	waitDecisionBellIdle(t, f.srv)

	notifID := decisionsWaitingNotificationID(f.bob.ID)
	row, err := f.store.GetNotification(ctx, notifID)
	require.NoError(t, err)
	require.False(t, row.Acknowledged)
	require.Equal(t, "3", row.ResourceID)

	bobSub, unsubBob := f.srv.events.Subscribe("user." + f.bob.ID + ".notification")
	defer unsubBob()

	// Arm the hook so that right after GetNotification reads acknowledged=false
	// on the 3 -> 2 decrease recompute, Bob's HTTP Mark read executes before
	// UpdateNotificationStateIfAcknowledged runs.
	hookStore.onBeforeWrite = func() {
		ackRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/notifications/"+notifID+"/ack", nil)
		require.Equal(t, http.StatusOK, ackRR.Code, "body: %s", ackRR.Body.String())
	}
	fault.Arm()

	rr := f.answer(t, f.bob, p1.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	waitDecisionBellIdle(t, f.srv)

	assert.True(t, hookStore.interleaved.Load(), "interleaved Mark read hook must have fired")
	assert.Equal(t, int64(1), hookStore.conflicts.Load(), "first keep-state write must conflict and retry")

	row, err = f.store.GetNotification(ctx, notifID)
	require.NoError(t, err)
	assert.True(t, row.Acknowledged, "concurrent Mark read must NOT be undone by a decrease recompute")
	assert.Equal(t, "2", row.ResourceID)
	assert.Equal(t, "2 decisions are waiting on you in 1 project", row.Message)

	evts := collectEvents(bobSub)
	require.NotEmpty(t, evts, "retry must publish the updated acknowledged state")
	for i, raw := range evts {
		var evt UserNotificationEvent
		require.NoError(t, json.Unmarshal(raw.Data, &evt), "event %d", i)
		assert.True(t, evt.Acknowledged, "event %d must not publish an unacknowledged row after Mark read", i)
	}
}

type panicTopicBindingsStore struct {
	store.Store
	fault     *storeFaultSwitch
	panicked  atomic.Bool
	projectID string
}

func (s *panicTopicBindingsStore) ListRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) ([]*store.RoleBinding, error) {
	if (s.fault == nil || s.fault.Active()) && scopeType == store.RoleScopeProject && scopeID == s.projectID && s.panicked.CompareAndSwap(false, true) {
		panic("injected topic expansion panic")
	}
	return s.Store.ListRoleBindingsForScope(ctx, scopeType, scopeID)
}

// TestDecisionsBell_TopicWorkerPanicRecovery verifies that a panic inside
// expandDecisionBellTopic is recovered, clears inFlightTopics, and leaves the
// worker pool healthy for subsequent topic events.
func TestDecisionsBell_TopicWorkerPanicRecovery(t *testing.T) {
	srv, _, alice, bob, project, panicStore, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *panicTopicBindingsStore {
		return &panicTopicBindingsStore{Store: inner, fault: f}
	})
	panicStore.projectID = project.ID
	f := newChoicesFixtureOn(t, srv, panicStore, alice, bob, project)
	setChoicesExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 15*time.Millisecond)

	f.agent.Ancestry = []string{f.bob.ID}
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(context.Background(), f.agent))

	fault.Arm()
	sendTopicChoicePromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID, oneDecision, false)
	waitDecisionBellIdle(t, f.srv)

	assert.True(t, panicStore.panicked.Load(), "injected panic must have fired")
	f.srv.decisionBell.mu.Lock()
	inFlightTopics := len(f.srv.decisionBell.inFlightTopics)
	f.srv.decisionBell.mu.Unlock()
	assert.Zero(t, inFlightTopics, "inFlightTopics must be cleared after topic worker panic")

	// A subsequent topic event on the same topic still expands and delivers normally.
	sendTopicChoicePromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"after-panic","question":"Recovered?","choices":["yes","no"]}]`, false)
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err := f.store.GetNotification(context.Background(), decisionsWaitingNotificationID(f.alice.ID))
	require.NoError(t, err)
	assert.Equal(t, "2", aliceRow.ResourceID)
	assert.False(t, aliceRow.Acknowledged)
}

type batchUserExpansionSpyStore struct {
	store.Store
	fault                   *storeFaultSwitch
	mu                      sync.Mutex
	getUserByID             map[string]int
	checkUserAccountGetUser atomic.Int64
	getUsersByIDs           atomic.Int64
	failBatch               atomic.Bool
}

func (s *batchUserExpansionSpyStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.fault == nil || s.fault.Active() {
		var pcs [16]uintptr
		n := runtime.Callers(2, pcs[:])
		frames := runtime.CallersFrames(pcs[:n])
		for {
			fr, more := frames.Next()
			if strings.HasSuffix(fr.Function, ".checkUserAccount") {
				s.checkUserAccountGetUser.Add(1)
				break
			}
			if !more {
				break
			}
		}
		s.mu.Lock()
		if s.getUserByID == nil {
			s.getUserByID = make(map[string]int)
		}
		s.getUserByID[id]++
		s.mu.Unlock()
	}
	return s.Store.GetUser(ctx, id)
}

func (s *batchUserExpansionSpyStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*store.User, error) {
	if s.fault == nil || s.fault.Active() {
		s.getUsersByIDs.Add(1)
		if s.failBatch.Load() {
			return nil, errors.New("injected batch user lookup failure")
		}
	}
	return s.Store.GetUsersByIDs(ctx, ids)
}

// TestDecisionsBell_TopicExpansionBatchesUserAccountLookups verifies that
// expandDecisionBellTopic batch-loads candidate user rows once via GetUsersByIDs
// (0 per-member GetUser calls from checkUserAccount), still excludes suspended
// and read-only members before membership evaluation, and fails closed when
// GetUsersByIDs errors.
func TestDecisionsBell_TopicExpansionBatchesUserAccountLookups(t *testing.T) {
	srv, rawStore, alice, bob, project, spy, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *batchUserExpansionSpyStore {
		return &batchUserExpansionSpyStore{Store: inner, fault: f}
	})
	f := newChoicesFixtureOn(t, srv, spy, alice, bob, project)
	setChoicesExperiment(t, f.srv, true)
	ctx := context.Background()

	isolatedProj := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Isolated Bell Project",
		Slug:      "isolated-bell-project",
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, rawStore.CreateProject(ctx, isolatedProj))

	f.srv.mu.RLock()
	wcs := f.srv.webChatStore
	f.srv.mu.RUnlock()
	require.NotNil(t, wcs)
	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: isolatedProj.ID,
		Name:      "general",
		CreatedBy: alice.ID,
		CreatedAt: time.Now().UTC(),
	}))

	memberRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	viewerRD, err := rawStore.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
	require.NoError(t, err)

	addRole := func(u *store.User, rdID string) {
		require.NoError(t, rawStore.CreateUser(ctx, u))
		_, berr := rawStore.CreateRoleBinding(ctx, &store.RoleBinding{
			ID:               api.NewUUID(),
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      u.ID,
			RoleDefinitionID: rdID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          isolatedProj.ID,
			CreatedAt:        time.Now(),
		})
		require.NoError(t, berr)
	}

	m1 := &store.User{ID: api.NewUUID(), Email: "m1-batch@example.com", DisplayName: "M1", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	m2 := &store.User{ID: api.NewUUID(), Email: "m2-batch@example.com", DisplayName: "M2", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	suspended := &store.User{ID: api.NewUUID(), Email: "susp-batch@example.com", DisplayName: "Susp", Role: store.UserRoleMember, Status: store.UserStatusSuspended, Created: time.Now()}
	readOnly := &store.User{ID: api.NewUUID(), Email: "ro-batch@example.com", DisplayName: "RO", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}

	addRole(m1, memberRD.ID)
	addRole(m2, memberRD.ID)
	addRole(suspended, memberRD.ID)
	addRole(readOnly, viewerRD.ID)

	fault.Arm()
	got := f.srv.expandDecisionBellTopic(ctx, isolatedProj.ID, topicID)
	assert.ElementsMatch(t, []string{m1.ID, m2.ID}, got, "only active posting members may be returned; suspended and viewer excluded")
	assert.Equal(t, int64(1), spy.getUsersByIDs.Load(), "candidate users must be fetched in one GetUsersByIDs batch")
	assert.Zero(t, spy.checkUserAccountGetUser.Load(), "checkUserAccount must read from the GetUsersByIDs map with zero GetUser calls")
	spy.mu.Lock()
	suspGetUsers := spy.getUserByID[suspended.ID]
	spy.mu.Unlock()
	assert.Zero(t, suspGetUsers, "suspended user must be rejected from batched user map before CheckEffectiveMembership")

	// Injecting a GetUsersByIDs failure fails closed (returns nil).
	spy.getUsersByIDs.Store(0)
	spy.checkUserAccountGetUser.Store(0)
	spy.failBatch.Store(true)
	assert.Nil(t, f.srv.expandDecisionBellTopic(ctx, isolatedProj.ID, topicID))
	assert.Equal(t, int64(1), spy.getUsersByIDs.Load())
	assert.Zero(t, spy.checkUserAccountGetUser.Load())
}
