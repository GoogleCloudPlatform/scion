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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecisionsBell_DismissTopicAndDMLifecycleAndRightUsers verifies ptone/scion#8215
// (design-8194 4.3, 10; design-decisions-ux B2, B4):
//   - Topic dismissal enqueues bell recomputes for posting members (direct and group-bound)
//     and never for read-only viewers, non-member hub admins, or outsiders;
//   - Dismissing one prompt while others remain drops the bell count while keeping B2's
//     acknowledged state when 0 < N <= P;
//   - Dismissing the last decision in a project drops the project count and clears the bell
//     (acknowledged=true, resource_id="0") when N reaches 0, including when the asking agent is gone;
//   - DM dismissal enqueues bell recompute only for the DM user, never other project members.
func TestDecisionsBell_DismissTopicAndDMLifecycleAndRightUsers(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 15*time.Millisecond)
	ctx := context.Background()

	hubAdminNonMember := &store.User{
		ID: tid("dsm-bell-admin"), Email: "dsm-bell-admin@example.com", DisplayName: "Hub Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, hubAdminNonMember))
	ensureHubMembership(ctx, f.store, hubAdminNonMember.ID)

	viewerOnly := &store.User{
		ID: tid("dsm-bell-viewer"), Email: "dsm-bell-viewer@example.com", DisplayName: "Viewer Only",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
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
		ID: tid("dsm-bell-group"), Email: "dsm-bell-group@example.com", DisplayName: "Group Member",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, groupMember))
	ensureHubMembership(ctx, f.store, groupMember.ID)
	grp := &store.Group{
		ID:        api.NewUUID(),
		Name:      "DSM Bell Team",
		Slug:      "dsm-bell-team",
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
		Name:        "dsm-bell-group-agent-message",
		Description: "Grants agent.message to group members in dismiss bell test",
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

	f.agent.Ancestry = []string{f.bob.ID}
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	// Second project where only Alice is a member, to verify multi-project count drop on dismiss.
	proj2 := &store.Project{
		ID:        api.NewUUID(),
		Name:      "DSM Second Project",
		Slug:      "dsm-second-project",
		OwnerID:   f.alice.ID,
		CreatedBy: f.alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, f.store.CreateProject(ctx, proj2))
	f.srv.seedProjectCreatorMembership(ctx, proj2)
	ag2 := &store.Agent{
		ID:          api.NewUUID(),
		Slug:        "dsm-proj2-bot",
		Name:        "DSM Proj2 Bot",
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

	// Step 1: Ask a 2-row prompt + a 1-row prompt in f.topicID, 2 in DM with Bob, and 1 in proj2.
	pTopicSet := sendTopicDecisionPromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"DSM-T1","question":"Topic 1?","choices":["yes","no"]},{"id":"DSM-T2","question":"Topic 2?","choices":["yes","no"]}]`, false)
	pTopicSolo := sendTopicDecisionPromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"DSM-T3","question":"Topic 3?","choices":["yes","no"]}]`, false)
	pDM := f.sendPromptToBob(t,
		`[{"id":"DSM-DM1","question":"DM 1?","choices":["yes","no"]},{"id":"DSM-DM2","question":"DM 2?","choices":["yes","no"]}]`, false)
	pProj2 := sendTopicDecisionPromptAsAgent(t, f.srv, f.store, ag2, proj2.ID, topic2,
		`[{"id":"DSM-P2","question":"Proj2?","choices":["yes","no"]}]`, false)
	waitDecisionBellIdle(t, f.srv)

	f.srv.decisionClock = func() time.Time { return time.Now().UTC().Add(store.DecisionDismissMinAge + time.Minute) }

	aliceRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.alice.ID))
	require.NoError(t, err)
	assert.False(t, aliceRow.Acknowledged)
	assert.Equal(t, "4", aliceRow.ResourceID)
	assert.Equal(t, "4 decisions are waiting on you in 2 projects", aliceRow.Message)

	bobRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.False(t, bobRow.Acknowledged)
	assert.Equal(t, "5", bobRow.ResourceID)
	assert.Equal(t, "5 decisions are waiting on you in 1 project", bobRow.Message)

	groupRow, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(groupMember.ID))
	require.NoError(t, err)
	assert.False(t, groupRow.Acknowledged)
	assert.Equal(t, "3", groupRow.ResourceID)
	assert.Equal(t, "3 decisions are waiting on you in 1 project", groupRow.Message)

	for _, excluded := range []*store.User{hubAdminNonMember, viewerOnly, f.outsider} {
		notifs, gerr := f.store.GetNotifications(ctx, store.SubscriberTypeUser, excluded.ID, false)
		require.NoError(t, gerr)
		assert.Empty(t, notifs, "excluded user %s must get no bell notification", excluded.Email)
	}

	// Step 2: Alice marks her bell read; Bob dismisses the 2-row topic prompt (DSM-T1, DSM-T2).
	// Counts drop for posting members only, and Alice's acknowledged=true state is preserved (0 < N <= P).
	ackRR := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/notifications/"+aliceRow.ID+"/ack", nil)
	require.Equal(t, http.StatusOK, ackRR.Code, ackRR.Body.String())

	dsmSetOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost,
		"/api/v1/decision-prompts/"+pTopicSet.MessageID+"/dismiss", map[string]interface{}{
			"reason":  "Release already shipped",
			"surface": store.DecisionSurfaceDecisionsPage,
		}))
	require.Len(t, dsmSetOut.Dismissed, 2)
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err = f.store.GetNotification(ctx, aliceRow.ID)
	require.NoError(t, err)
	assert.True(t, aliceRow.Acknowledged, "decrease on dismiss must preserve acknowledged=true")
	assert.Equal(t, "2", aliceRow.ResourceID)
	assert.Equal(t, "2 decisions are waiting on you in 2 projects", aliceRow.Message)

	bobRow, err = f.store.GetNotification(ctx, bobRow.ID)
	require.NoError(t, err)
	assert.False(t, bobRow.Acknowledged)
	assert.Equal(t, "3", bobRow.ResourceID)
	assert.Equal(t, "3 decisions are waiting on you in 1 project", bobRow.Message)

	groupRow, err = f.store.GetNotification(ctx, groupRow.ID)
	require.NoError(t, err)
	assert.False(t, groupRow.Acknowledged)
	assert.Equal(t, "1", groupRow.ResourceID)
	assert.Equal(t, "1 decision is waiting on you in 1 project", groupRow.Message)

	for _, excluded := range []*store.User{hubAdminNonMember, viewerOnly, f.outsider} {
		notifs, gerr := f.store.GetNotifications(ctx, store.SubscriberTypeUser, excluded.ID, false)
		require.NoError(t, gerr)
		assert.Empty(t, notifs, "excluded user %s must get no bell notification after topic dismiss", excluded.Email)
	}

	// Step 3: Dismiss DSM-T3 -> groupMember clears to 0, Alice drops to 1 in 1 project, Bob drops to 2 (DM only).
	groupSub, unsubGroup := f.srv.events.Subscribe("user." + groupMember.ID + ".notification")
	defer unsubGroup()

	dsmSoloOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.alice, http.MethodPost,
		"/api/v1/decision-prompts/"+pTopicSolo.MessageID+"/dismiss", map[string]interface{}{
			"reason": "Obsolete topic question",
		}))
	require.Len(t, dsmSoloOut.Dismissed, 1)
	waitDecisionBellIdle(t, f.srv)

	groupRow, err = f.store.GetNotification(ctx, groupRow.ID)
	require.NoError(t, err)
	assert.True(t, groupRow.Acknowledged, "dismissing last decision for groupMember must clear the bell")
	assert.Equal(t, "0", groupRow.ResourceID)
	groupEvts := collectEvents(groupSub)
	require.NotEmpty(t, groupEvts)
	var lastGroupEvt UserNotificationEvent
	require.NoError(t, json.Unmarshal(groupEvts[len(groupEvts)-1].Data, &lastGroupEvt))
	assert.True(t, lastGroupEvt.Acknowledged)
	assert.Equal(t, "0", lastGroupEvt.ResourceID)

	aliceRow, err = f.store.GetNotification(ctx, aliceRow.ID)
	require.NoError(t, err)
	assert.True(t, aliceRow.Acknowledged)
	assert.Equal(t, "1", aliceRow.ResourceID)
	assert.Equal(t, "1 decision is waiting on you in 1 project", aliceRow.Message)

	bobRow, err = f.store.GetNotification(ctx, bobRow.ID)
	require.NoError(t, err)
	assert.False(t, bobRow.Acknowledged)
	assert.Equal(t, "2", bobRow.ResourceID)

	// Step 4: Bob dismisses his DM prompt -> only Bob is recomputed and cleared; Alice receives no bell event.
	aliceSub, unsubAlice := f.srv.events.Subscribe("user." + f.alice.ID + ".notification")
	defer unsubAlice()
	bobSub, unsubBob := f.srv.events.Subscribe("user." + f.bob.ID + ".notification")
	defer unsubBob()

	dsmDMOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost,
		"/api/v1/decision-prompts/"+pDM.MessageID+"/dismiss", map[string]interface{}{
			"reason": "Stale DM",
		}))
	require.Len(t, dsmDMOut.Dismissed, 2)
	waitDecisionBellIdle(t, f.srv)

	bobRow, err = f.store.GetNotification(ctx, bobRow.ID)
	require.NoError(t, err)
	assert.True(t, bobRow.Acknowledged, "DM dismiss to N=0 must clear Bob's bell")
	assert.Equal(t, "0", bobRow.ResourceID)
	bobEvts := collectEvents(bobSub)
	require.Len(t, bobEvts, 1)
	var lastBobEvt UserNotificationEvent
	require.NoError(t, json.Unmarshal(bobEvts[0].Data, &lastBobEvt))
	assert.True(t, lastBobEvt.Acknowledged)
	assert.Equal(t, "0", lastBobEvt.ResourceID)

	assert.Empty(t, collectEvents(aliceSub), "DM dismiss must not notify other project members")

	// Step 5: Hard-delete ag2 (gone asker) and dismiss DSM-P2 -> delivered=false and Alice's bell clears to 0.
	require.NoError(t, f.store.DeleteAgent(ctx, ag2.ID))
	dsmP2Out := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.alice, http.MethodPost,
		"/api/v1/decision-prompts/"+pProj2.MessageID+"/dismiss", map[string]interface{}{
			"reason": "Agent is gone",
		}))
	assert.False(t, dsmP2Out.Delivered)
	require.Len(t, dsmP2Out.Dismissed, 1)
	waitDecisionBellIdle(t, f.srv)

	aliceRow, err = f.store.GetNotification(ctx, aliceRow.ID)
	require.NoError(t, err)
	assert.True(t, aliceRow.Acknowledged)
	assert.Equal(t, "0", aliceRow.ResourceID)
}

type dismissBellSpyStore struct {
	store.Store
	fault      *storeFaultSwitch
	recomputes atomic.Int64
	creates    atomic.Int64
	updates    atomic.Int64
}

func (s *dismissBellSpyStore) ProjectsWithDecisions(ctx context.Context, projectIDs []string) ([]string, error) {
	if s.fault == nil || s.fault.Active() {
		s.recomputes.Add(1)
	}
	return s.Store.ProjectsWithDecisions(ctx, projectIDs)
}

func (s *dismissBellSpyStore) CreateNotification(ctx context.Context, notif *store.Notification) error {
	if s.fault == nil || s.fault.Active() {
		s.creates.Add(1)
	}
	return s.Store.CreateNotification(ctx, notif)
}

func (s *dismissBellSpyStore) UpdateNotificationState(ctx context.Context, id, message, resourceID string, acknowledged bool) error {
	if s.fault == nil || s.fault.Active() {
		s.updates.Add(1)
	}
	return s.Store.UpdateNotificationState(ctx, id, message, resourceID, acknowledged)
}

func (s *dismissBellSpyStore) UpdateNotificationStateIfAcknowledged(ctx context.Context, id, message, resourceID string, observedAcknowledged bool) error {
	if s.fault == nil || s.fault.Active() {
		s.updates.Add(1)
	}
	return s.Store.UpdateNotificationStateIfAcknowledged(ctx, id, message, resourceID, observedAcknowledged)
}

func (s *dismissBellSpyStore) reset() {
	s.recomputes.Store(0)
	s.creates.Store(0)
	s.updates.Store(0)
}

func (s *dismissBellSpyStore) writes() int64 {
	return s.creates.Load() + s.updates.Load()
}

// TestDecisionsBell_DismissOncePerCallAndNoOpOnFailuresAndRepeat verifies ptone/scion#8215:
//   - Failed dismissals (409 too_recent, 409 dismiss_changed, 400 validation, 403 users_only/read-only,
//     404 non-participant DM admin) enqueue nothing and write nothing;
//   - Multi-row DM dismissal coalesces into 1 userDue entry and 1 recompute (the DM user only)
//     while emitting chat.decision.updated per dismissed row;
//   - Multi-row topic dismissal enqueues 1 topicDue entry and 0 userDue entries (even when a
//     non-member hub admin performs the dismissal), emits chat.decision.updated per row, and
//     recomputes only the project's posting members;
//   - Repeating a dismissal when all rows are already dismissed (alreadyDismissed) enqueues nothing.
func TestDecisionsBell_DismissOncePerCallAndNoOpOnFailuresAndRepeat(t *testing.T) {
	srv, _, alice, bob, project, spy, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *dismissBellSpyStore {
		return &dismissBellSpyStore{Store: inner, fault: f}
	})
	f := newDecisionsFixtureOn(t, srv, spy, alice, bob, project)
	setDecisionsExperiment(t, f.srv, true)
	setFastDecisionBell(t, f.srv, 10*time.Millisecond)
	ctx := context.Background()

	f.agent.Ancestry = []string{f.bob.ID}
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	hubAdminNonMember := &store.User{
		ID: tid("dsm-spy-admin"), Email: "dsm-spy-admin@example.com", DisplayName: "NonMember Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, hubAdminNonMember))
	ensureHubMembership(ctx, f.store, hubAdminNonMember.ID)

	viewerOnly := &store.User{
		ID: tid("dsm-spy-viewer"), Email: "dsm-spy-viewer@example.com", DisplayName: "Viewer Only",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
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

	pTopic := sendTopicDecisionPromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"DT1","question":"DT1?","choices":["yes","no"]},{"id":"DT2","question":"DT2?","choices":["yes","no"]}]`, false)
	pDM := f.sendPromptToBob(t,
		`[{"id":"DDM1","question":"DDM1?","choices":["yes","no"]},{"id":"DDM2","question":"DDM2?","choices":["yes","no"]}]`, false)
	pAns := f.sendPromptToBob(t, `[{"id":"DANS1","question":"Answered?","choices":["yes","no"]}]`, false)
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, pAns.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)
	waitDecisionBellIdle(t, f.srv)

	dTopic0, err := f.store.GetDecision(ctx, pTopic.Decisions[0].RowID)
	require.NoError(t, err)

	setFastDecisionBell(t, f.srv, 25*time.Millisecond)
	fault.Arm()
	spy.reset()

	topicDismissPath := "/api/v1/decision-prompts/" + pTopic.MessageID + "/dismiss"
	dmDismissPath := "/api/v1/decision-prompts/" + pDM.MessageID + "/dismiss"
	ansDismissPath := "/api/v1/decision-prompts/" + pAns.MessageID + "/dismiss"

	// 1. Failed dismissals must enqueue zero items and perform zero recomputes/writes.
	failHoldCh := make(chan struct{})
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.testHoldCh = failHoldCh
	f.srv.decisionBell.mu.Unlock()

	// 409 too_recent on topic and DM prompts.
	f.srv.decisionClock = func() time.Time { return dTopic0.CreatedAt.Add(4*time.Minute + 59*time.Second) }
	assert.Equal(t, http.StatusConflict, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, topicDismissPath, nil).Code)
	assert.Equal(t, http.StatusConflict, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, dmDismissPath, nil).Code)

	// Advance clock past minimum age for the remaining failure checks.
	f.srv.decisionClock = func() time.Time { return time.Now().UTC().Add(store.DecisionDismissMinAge + time.Minute) }

	// 400 invalid surface.
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, topicDismissPath, map[string]interface{}{
		"surface": "slack",
	}).Code)
	// 403 read-only viewer on topic.
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, f.srv, viewerOnly, http.MethodPost, topicDismissPath, nil).Code)
	// 403 agent caller.
	agentReq := httptest.NewRequest(http.MethodPost, topicDismissPath, strings.NewReader(`{}`))
	agentReq.Header.Set("Content-Type", "application/json")
	agentRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(agentRec, agentReq.WithContext(agentCtx(agentReq.Context(), f.agent)))
	assert.Equal(t, http.StatusForbidden, agentRec.Code)
	// 404 non-participant/non-member admin on DM and topic prompts.
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, hubAdminNonMember, http.MethodPost, dmDismissPath, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, hubAdminNonMember, http.MethodPost, topicDismissPath, nil).Code)
	// 409 dismiss_changed on already-answered prompt.
	changedRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, ansDismissPath, nil)
	require.Equal(t, http.StatusConflict, changedRR.Code, changedRR.Body.String())
	assert.Equal(t, ErrCodeDismissChanged, errorCode(t, changedRR))

	f.srv.decisionBell.mu.Lock()
	failQueuedUsers := len(f.srv.decisionBell.userDue)
	failQueuedTopics := len(f.srv.decisionBell.topicDue)
	f.srv.decisionBell.testHoldCh = nil
	close(failHoldCh)
	f.srv.decisionBell.signalWakeLocked()
	f.srv.decisionBell.mu.Unlock()

	assert.Zero(t, failQueuedUsers, "failed dismissals must not enqueue any user")
	assert.Zero(t, failQueuedTopics, "failed dismissals must not enqueue any topic")
	waitDecisionBellIdle(t, f.srv)
	assert.Zero(t, spy.recomputes.Load(), "failed dismissals must trigger zero recomputes")
	assert.Zero(t, spy.writes(), "failed dismissals must trigger zero notification writes")

	// 2. Multi-row DM dismissal (DDM1 + DDM2) coalesces into 1 userDue entry and 1 recompute (Bob only),
	//    while still emitting 2 chat.decision.updated events.
	projUpdatedCh, unsubProj := f.srv.events.Subscribe("project." + f.project.ID + ".chat.decision.updated")
	defer unsubProj()
	bobUpdatedCh, unsubBob := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.decision.updated")
	defer unsubBob()

	dmHoldCh := make(chan struct{})
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.testHoldCh = dmHoldCh
	f.srv.decisionBell.mu.Unlock()

	dmOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, dmDismissPath, map[string]interface{}{
		"reason": "DM batch stale",
	}))
	require.Len(t, dmOut.Dismissed, 2)
	assert.Len(t, collectEvents(bobUpdatedCh), 2, "each newly dismissed DM row emits chat.decision.updated")

	f.srv.decisionBell.mu.Lock()
	dmEnqueuedUsers := len(f.srv.decisionBell.userDue)
	_, bobQueued := f.srv.decisionBell.userDue[f.bob.ID]
	dmEnqueuedTopics := len(f.srv.decisionBell.topicDue)
	f.srv.decisionBell.testHoldCh = nil
	close(dmHoldCh)
	f.srv.decisionBell.signalWakeLocked()
	f.srv.decisionBell.mu.Unlock()

	assert.Equal(t, 1, dmEnqueuedUsers, "DDM1 and DDM2 coalesce into a single userDue entry for Bob")
	assert.True(t, bobQueued)
	assert.Zero(t, dmEnqueuedTopics)

	waitDecisionBellIdle(t, f.srv)
	assert.Equal(t, int64(1), spy.recomputes.Load(), "2 dismissed DM rows coalesce into 1 recompute for Bob")

	// 3. Multi-row topic dismissal (DT1 + DT2) performed by Bob enqueues 1 topicDue entry
	//    and 0 userDue entries, emits 2 SSE events, recomputes only the 2 posting members
	//    (Alice and Bob), and never notifies excluded users.
	spy.reset()
	topicHoldCh := make(chan struct{})
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.testHoldCh = topicHoldCh
	f.srv.decisionBell.mu.Unlock()

	topicOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, topicDismissPath, map[string]interface{}{
		"reason": "Topic batch stale",
	}))
	require.Len(t, topicOut.Dismissed, 2)
	assert.Empty(t, topicOut.AlreadyDismissed)
	assert.Len(t, collectEvents(projUpdatedCh), 2, "each newly dismissed topic row emits chat.decision.updated")

	f.srv.decisionBell.mu.Lock()
	topicEnqueuedUsers := len(f.srv.decisionBell.userDue)
	topicEnqueuedTopics := len(f.srv.decisionBell.topicDue)
	f.srv.decisionBell.testHoldCh = nil
	close(topicHoldCh)
	f.srv.decisionBell.signalWakeLocked()
	f.srv.decisionBell.mu.Unlock()

	assert.Zero(t, topicEnqueuedUsers, "pending topic dismissal must enqueue topicDue only, not userDue")
	assert.Equal(t, 1, topicEnqueuedTopics, "DT1 and DT2 coalesce into 1 topicDue entry")

	waitDecisionBellIdle(t, f.srv)
	assert.Equal(t, int64(2), spy.recomputes.Load(), "topic dismissal recomputes only the 2 posting project members")

	for _, u := range []*store.User{f.alice, f.bob} {
		row, gerr := f.store.GetNotification(ctx, decisionsWaitingNotificationID(u.ID))
		require.NoError(t, gerr)
		assert.True(t, row.Acknowledged)
		assert.Equal(t, "0", row.ResourceID)
	}
	adminNotifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, hubAdminNonMember.ID, false)
	require.NoError(t, err)
	assert.Empty(t, adminNotifs, "non-member hub admin must get no bell notification")

	// 3b. Above the B8 project member cap, pending topic dismissal enqueues topicDue only
	//     and skips immediate fan-out (0 recomputes), rather than leaking the dismissing
	//     user into userDue.
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.maxTopicMembers = 1
	f.srv.decisionBell.mu.Unlock()
	pCap := sendTopicDecisionPromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"DCAP1","question":"Above cap?","choices":["yes","no"]}]`, false)
	waitDecisionBellIdle(t, f.srv)
	f.srv.decisionClock = func() time.Time { return time.Now().UTC().Add(2*store.DecisionDismissMinAge + time.Minute) }

	spy.reset()
	capOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+pCap.MessageID+"/dismiss", nil))
	require.Len(t, capOut.Dismissed, 1)
	waitDecisionBellIdle(t, f.srv)
	assert.Zero(t, spy.recomputes.Load(), "pending topic dismissal above B8 member cap must skip immediate recompute")

	// Conversely, when a newly dismissed row carries a prior agent_interpreted answer
	// naming Bob (design-8194 4.3 item 4), enqueueDecisionBellDismiss still enqueues
	// the named user directly even above the B8 topic fan-out cap.
	spy.reset()
	f.srv.enqueueDecisionBellDismiss(f.project.ID, f.topicID, []*store.Decision{{
		ID:        api.NewUUID(),
		ProjectID: f.project.ID,
		ChatKey:   f.topicID,
		Status:    store.DecisionStatusDismissed,
		Answers: []*store.DecisionAnswer{
			{Provenance: store.DecisionProvenanceAgentInterpreted, AnsweredByUserID: f.bob.ID},
			{Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeDismiss, AnsweredByUserID: f.bob.ID},
		},
	}})
	waitDecisionBellIdle(t, f.srv)
	assert.Equal(t, int64(1), spy.recomputes.Load(), "dismissed interpreted row must enqueue its named user even above B8 cap")

	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.maxTopicMembers = 0
	f.srv.decisionBell.mu.Unlock()

	// 4. Repeat dismissal when all rows are already dismissed (alreadyDismissed) enqueues nothing.
	spy.reset()
	repeatHoldCh := make(chan struct{})
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.testHoldCh = repeatHoldCh
	f.srv.decisionBell.mu.Unlock()

	repeatTopicOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, topicDismissPath, map[string]interface{}{
		"reason": "Ignored retry",
	}))
	assert.Empty(t, repeatTopicOut.Dismissed)
	require.Len(t, repeatTopicOut.AlreadyDismissed, 2)

	repeatDMOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, dmDismissPath, map[string]interface{}{
		"reason": "Ignored retry",
	}))
	assert.Empty(t, repeatDMOut.Dismissed)
	require.Len(t, repeatDMOut.AlreadyDismissed, 2)

	f.srv.decisionBell.mu.Lock()
	repeatQueuedUsers := len(f.srv.decisionBell.userDue)
	repeatQueuedTopics := len(f.srv.decisionBell.topicDue)
	f.srv.decisionBell.testHoldCh = nil
	close(repeatHoldCh)
	f.srv.decisionBell.signalWakeLocked()
	f.srv.decisionBell.mu.Unlock()

	assert.Zero(t, repeatQueuedUsers, "idempotent repeat must not enqueue any user")
	assert.Zero(t, repeatQueuedTopics, "idempotent repeat must not enqueue any topic")
	waitDecisionBellIdle(t, f.srv)
	assert.Zero(t, spy.recomputes.Load(), "idempotent repeat must trigger zero recomputes")
	assert.Zero(t, spy.writes(), "idempotent repeat must trigger zero notification writes")

	// 5. P2 subset + idempotencyKey integration:
	//    - Dismissing only a pending row of a set (leaving an interpreted sibling untouched)
	//      enqueues only for the newly dismissed row (topicDue only, not the untouched
	//      interpreted sibling's named user);
	//    - Dismissing the interpreted row with idempotencyKey enqueues its named user once,
	//      and replaying the same idempotencyKey enqueues nothing.
	pSub := sendTopicDecisionPromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID, `[
		{"id":"DSUB1","question":"Interpreted row?","choices":["yes","no"]},
		{"id":"DSUB2","question":"Pending row?","choices":["yes","no"]}
	]`, false)
	recordTestInterpretedForUser(t, f.store, f.project.ID, f.topicID, f.agent, f.alice, pSub.Decisions[0].RowID, "1")
	waitDecisionBellIdle(t, f.srv)
	f.srv.decisionClock = func() time.Time { return time.Now().UTC().Add(3*store.DecisionDismissMinAge + time.Minute) }

	spy.reset()
	subPendingHold := make(chan struct{})
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.testHoldCh = subPendingHold
	f.srv.decisionBell.mu.Unlock()

	subPendingOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+pSub.MessageID+"/dismiss", map[string]interface{}{
		"decisionIds": []string{pSub.Decisions[1].RowID},
		"reason":      "Only DSUB2 stale",
	}))
	require.Len(t, subPendingOut.Dismissed, 1)

	f.srv.decisionBell.mu.Lock()
	subPendingUsers := len(f.srv.decisionBell.userDue)
	subPendingTopics := len(f.srv.decisionBell.topicDue)
	f.srv.decisionBell.testHoldCh = nil
	close(subPendingHold)
	f.srv.decisionBell.signalWakeLocked()
	f.srv.decisionBell.mu.Unlock()

	assert.Zero(t, subPendingUsers, "subset dismiss of pending DSUB2 must not enqueue untouched interpreted DSUB1's named user into userDue")
	assert.Equal(t, 1, subPendingTopics)
	waitDecisionBellIdle(t, f.srv)

	// Alice dismisses interpreted DSUB1 with an idempotencyKey -> enqueues Alice in userDue;
	// replaying the same idempotencyKey enqueues nothing.
	spy.reset()
	subInterpHold := make(chan struct{})
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.testHoldCh = subInterpHold
	f.srv.decisionBell.mu.Unlock()

	subInterpOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/decision-prompts/"+pSub.MessageID+"/dismiss", map[string]interface{}{
		"decisionIds":    []string{pSub.Decisions[0].RowID},
		"reason":         "Dismiss interpreted DSUB1",
		"idempotencyKey": "bell-dsm-idem-1",
	}))
	require.Len(t, subInterpOut.Dismissed, 1)

	f.srv.decisionBell.mu.Lock()
	_, aliceQueued := f.srv.decisionBell.userDue[f.alice.ID]
	f.srv.decisionBell.testHoldCh = nil
	close(subInterpHold)
	f.srv.decisionBell.signalWakeLocked()
	f.srv.decisionBell.mu.Unlock()
	assert.True(t, aliceQueued, "newly dismissed interpreted row enqueues named user")
	waitDecisionBellIdle(t, f.srv)

	spy.reset()
	idemReplayHold := make(chan struct{})
	f.srv.decisionBell.mu.Lock()
	f.srv.decisionBell.testHoldCh = idemReplayHold
	f.srv.decisionBell.mu.Unlock()

	idemReplayOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/decision-prompts/"+pSub.MessageID+"/dismiss", map[string]interface{}{
		"decisionIds":    []string{pSub.Decisions[0].RowID},
		"reason":         "Dismiss interpreted DSUB1",
		"idempotencyKey": "bell-dsm-idem-1",
	}))
	assert.Equal(t, subInterpOut.ResponseMessageID, idemReplayOut.ResponseMessageID)

	f.srv.decisionBell.mu.Lock()
	idemQueuedUsers := len(f.srv.decisionBell.userDue)
	idemQueuedTopics := len(f.srv.decisionBell.topicDue)
	f.srv.decisionBell.testHoldCh = nil
	close(idemReplayHold)
	f.srv.decisionBell.signalWakeLocked()
	f.srv.decisionBell.mu.Unlock()

	assert.Zero(t, idemQueuedUsers, "idempotencyKey replay must not enqueue any user")
	assert.Zero(t, idemQueuedTopics, "idempotencyKey replay must not enqueue any topic")
	waitDecisionBellIdle(t, f.srv)
	assert.Zero(t, spy.recomputes.Load(), "idempotencyKey replay must trigger zero recomputes")
}

// TestDecisionsBell_WaitingOnYouTotalPinsDismissedExclusion pins waitingOnYouTotal
// (the count query backing the bell): dismissed rows carry an outcome=dismiss
// ($dismissed) answer row that EffectiveAnswer() skips, and status=dismissed is
// terminal, so dismissed rows drop out of standalone prompts, partially dismissed
// multi-question sets, DM prompts, and --update replacements without double-counting.
func TestDecisionsBell_WaitingOnYouTotalPinsDismissedExclusion(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsExperiment(t, f.srv, true)
	ctx := context.Background()

	f.agent.Ancestry = []string{f.bob.ID}
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	setPrompt := sendTopicDecisionPromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID, `[
		{"id":"PIN-DSM-1","question":"Q1?","choices":["yes","no"]},
		{"id":"PIN-DSM-2","question":"Q2?","choices":["yes","no"]},
		{"id":"PIN-DSM-3","question":"Q3?","choices":["yes","no"]}
	]`, false)
	soloPrompt := sendTopicDecisionPromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"PIN-DSM-SOLO","question":"QSolo?","choices":["yes","no"]}]`, false)
	dmPrompt := f.sendPromptToBob(t, `[{"id":"PIN-DSM-DM","question":"QDM?","choices":["yes","no"]}]`, false)

	totalA, projA, err := f.srv.waitingOnYouTotal(ctx, f.alice.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, totalA)
	assert.Equal(t, 1, projA)

	totalB, projB, err := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 5, totalB)
	assert.Equal(t, 1, projB)

	// Record an agent_interpreted answer on PIN-DSM-2 naming Bob: still awaits Bob's confirm, not Alice's.
	recordTestInterpretedForUser(t, f.store, f.project.ID, f.topicID, f.agent, f.bob, setPrompt.Decisions[1].RowID, "1")

	totalA, projA, err = f.srv.waitingOnYouTotal(ctx, f.alice.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, totalA)
	assert.Equal(t, 1, projA)

	totalB, projB, err = f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 5, totalB)
	assert.Equal(t, 1, projB)

	futureNow := func() time.Time { return time.Now().UTC().Add(store.DecisionDismissMinAge + time.Minute) }
	dismissPromptAs := func(msgID, chatKey string, user *store.User) *store.DismissDecisionsResult {
		res, derr := f.store.DismissDecisions(ctx, &store.DismissDecisionsInput{
			MessageID: msgID,
			User: store.DecisionAnswerer{
				UserID: user.ID,
				Name:   user.DisplayName,
				Email:  user.Email,
			},
			Reason:  "Stale",
			Surface: store.DecisionSurfaceDecisionsPage,
			ResponseMessage: &store.Message{
				ID:        api.NewUUID(),
				ProjectID: f.project.ID,
				Sender:    "user:" + user.ID,
				SenderID:  user.ID,
				Recipient: "agent:" + f.agent.Slug,
				ThreadID:  chatKey,
				Type:      messages.TypeDecisionResponse,
				Channel:   "web",
				Msg:       "dismissed",
				CreatedAt: futureNow(),
			},
			Now: futureNow,
		})
		require.NoError(t, derr)
		return res
	}

	// Dismiss standalone PIN-DSM-SOLO: prompt drops out of ListOpenPrompts completely.
	soloRes := dismissPromptAs(soloPrompt.MessageID, f.topicID, f.alice)
	require.Len(t, soloRes.Dismissed, 1)
	assert.Nil(t, soloRes.Dismissed[0].EffectiveAnswer(), "dismissed row must have nil EffectiveAnswer")
	require.NotNil(t, soloRes.Dismissed[0].Dismissal(), "dismissed row must expose Dismissal()")

	totalA, projA, err = f.srv.waitingOnYouTotal(ctx, f.alice.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, totalA)
	assert.Equal(t, 1, projA)

	totalB, projB, err = f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, totalB)
	assert.Equal(t, 1, projB)

	// Dismiss the open pending rows waiting on Alice in setPrompt (PIN-DSM-1 and PIN-DSM-3):
	// PIN-DSM-2 (interpreted answer naming Bob) stays open on setPrompt, so ListOpenPrompts
	// still returns setPrompt while classifyProjectOpenPrompts excludes the dismissed siblings.
	setRes := dismissPromptAs(setPrompt.MessageID, f.topicID, f.alice)
	require.Len(t, setRes.Dismissed, 2)

	totalA, projA, err = f.srv.waitingOnYouTotal(ctx, f.alice.ID)
	require.NoError(t, err)
	assert.Zero(t, totalA, "dismissed siblings PIN-DSM-1/3 excluded and PIN-DSM-2 waits on Bob")
	assert.Zero(t, projA)

	totalB, projB, err = f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, totalB, "dismissed siblings PIN-DSM-1/3 excluded while PIN-DSM-2 and PIN-DSM-DM remain")
	assert.Equal(t, 1, projB)

	// Bob dismisses PIN-DSM-DM: Bob keeps only PIN-DSM-2 (awaiting confirm).
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	dmRes := dismissPromptAs(dmPrompt.MessageID, dmKey, f.bob)
	require.Len(t, dmRes.Dismissed, 1)

	totalB, projB, err = f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, totalB)
	assert.Equal(t, 1, projB)

	// Re-asking PIN-DSM-1 via --update creates pending v2: counted once, dismissed v1 remains excluded.
	sendTopicDecisionPromptAsAgent(t, f.srv, f.store, f.agent, f.project.ID, f.topicID,
		`[{"id":"PIN-DSM-1","question":"Q1 v2?","choices":["yes","no"]}]`, true)

	totalA, projA, err = f.srv.waitingOnYouTotal(ctx, f.alice.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, totalA)
	assert.Equal(t, 1, projA)

	totalB, projB, err = f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, totalB)
	assert.Equal(t, 1, projB)
}
