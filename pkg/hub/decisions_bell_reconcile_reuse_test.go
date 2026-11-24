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
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

func holdDecisionBellWorkers(t *testing.T, srv *Server) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	srv.decisionBell.mu.Lock()
	srv.decisionBell.debounce = time.Millisecond
	srv.decisionBell.testHoldCh = ch
	srv.decisionBell.mu.Unlock()
	released := false
	releaseFn := func() {
		if released {
			return
		}
		released = true
		close(ch)
		srv.decisionBell.mu.Lock()
		srv.decisionBell.signalWakeLocked()
		srv.decisionBell.mu.Unlock()
	}
	t.Cleanup(func() {
		releaseFn()
		srv.stopDecisionBell()
	})
	return releaseFn
}

func decisionBellHasQueuedUser(srv *Server, userID string) (queued bool, reconcile bool) {
	srv.decisionBell.mu.Lock()
	defer srv.decisionBell.mu.Unlock()
	entry, ok := srv.decisionBell.userDue[userID]
	return ok, entry.reconcile
}

func clearDecisionBellQueuedUsers(srv *Server) {
	srv.decisionBell.mu.Lock()
	for k := range srv.decisionBell.userDue {
		delete(srv.decisionBell.userDue, k)
	}
	srv.decisionBell.mu.Unlock()
}

// TestDecisionsBell_ReconcileReuse_SessionUnfiltered verifies design-scale §3.5 M6:
// session-authenticated unfiltered GET /api/v1/users/me/decisions?tab=pending
// (including limit=0, omitted tab, and sort=recent) writes the decisions_waiting
// notification inline with the exact same content as recomputeDecisionBellUser,
// preserves B2 semantics (in-sync no-op, acknowledged decrease, clear at zero),
// and does not enqueue an async recompute.
func TestDecisionsBell_ReconcileReuse_SessionUnfiltered(t *testing.T) {
	srv, _, alice, bob, project, spy, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *notificationWriteCountStore {
		return &notificationWriteCountStore{Store: inner, fault: f}
	})
	f := newDecisionsFixtureOn(t, srv, spy, alice, bob, project)
	fault.Arm()
	setDecisionsExperiment(t, f.srv, true)
	holdDecisionBellWorkers(t, f.srv)
	ctx := context.Background()

	// Seed a second project where Bob is a member so multi-project wording is exercised.
	proj2 := &store.Project{
		ID:      api.NewUUID(),
		Name:    "Bell Reuse Project 2",
		Slug:    "bell-reuse-proj-2",
		Created: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateProject(ctx, proj2))
	memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.bob.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj2.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	topic2 := createTestDecisionTopic(t, f.wcs, proj2.ID, "general", f.bob.ID)

	f.agent.Ancestry = []string{f.bob.ID}
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	// Seed 2 open decisions in project 1 and 1 open decision in project 2 directly
	// via store so no bell event is enqueued beforehand.
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	_, err = f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "chat", ThreadID: f.topicID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name,
		Decisions: []messages.Decision{
			{Key: "p1-a", Question: "P1 A?", Choices: choices},
			{Key: "p1-b", Question: "P1 B?", Choices: choices},
		},
	})
	require.NoError(t, err)
	bot2ID := api.NewUUID()
	_, err = f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: proj2.ID, Sender: "agent:bot2", SenderID: bot2ID,
			Recipient: "chat", ThreadID: topic2, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: bot2ID, AskerAgentName: "bot2",
		Decisions: []messages.Decision{
			{Key: "p2-a", Question: "P2 A?", Choices: choices},
		},
	})
	require.NoError(t, err)

	// Confirm waitingOnYouTotal agrees on (3, 2).
	wantTotal, wantProjects, err := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
	require.NoError(t, err)
	require.Equal(t, 3, wantTotal)
	require.Equal(t, 2, wantProjects)

	// 1. Tray open (limit=0) writes notification inline while workers are held,
	// without enqueuing anything into decisionBell.userDue.
	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	resp := decodeProfilePendingDecisions(t, rr)
	assert.Equal(t, 3, resp.Counts.WaitingOnYou)

	queued, _ := decisionBellHasQueuedUser(f.srv, f.bob.ID)
	assert.False(t, queued, "unfiltered session limit=0 must not enqueue async bell recompute")
	f.srv.decisionBell.mu.Lock()
	started := f.srv.decisionBell.started
	f.srv.decisionBell.mu.Unlock()
	assert.False(t, started, "bell worker pool should not even be started by inline reuse")

	row, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.Equal(t, "3", row.ResourceID)
	assert.Equal(t, formatDecisionsWaitingMessage(3, 2), row.Message)
	assert.False(t, row.Acknowledged)
	assert.Equal(t, store.NotificationKindDecisionsWaiting, row.Kind)
	assert.Equal(t, store.NotificationStatusDecisionsWaiting, row.Status)
	assert.Equal(t, store.NotificationResourceDecisions, row.ResourceType)

	// 2. Subsequent unfiltered requests (limit=0, full tab=pending, omitted tab,
	// sort=recent) are in-sync no-ops with zero writes and no async enqueue.
	writesBefore := spy.creates.Load() + spy.updates.Load()
	for _, path := range []string{
		userMeDecisionsPath + "?tab=pending&limit=0",
		userMeDecisionsPath + "?tab=pending",
		userMeDecisionsPath,
		userMeDecisionsPath + "?tab=pending&sort=recent",
	} {
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rr.Code, "path=%s body=%s", path, rr.Body.String())
		queued, _ = decisionBellHasQueuedUser(f.srv, f.bob.ID)
		assert.False(t, queued, "path=%s must not enqueue async bell recompute", path)
	}
	assert.Equal(t, writesBefore, spy.creates.Load()+spy.updates.Load(), "in-sync inline reconcile must perform zero writes")

	// 3. B2 decrease while acknowledged: mark row read at 3, withdraw 1 decision
	// in store, then open tab=pending -> keeps Acknowledged=true, updates count to 2.
	require.NoError(t, f.store.UpdateNotificationState(ctx, row.ID, row.Message, row.ResourceID, true))
	_, err = f.store.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{AskerAgentID: f.agent.ID, Keys: []string{"p1-a"}})
	require.NoError(t, err)

	rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	queued, _ = decisionBellHasQueuedUser(f.srv, f.bob.ID)
	assert.False(t, queued)
	row, err = f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.Equal(t, "2", row.ResourceID)
	assert.Equal(t, formatDecisionsWaitingMessage(2, 2), row.Message)
	assert.True(t, row.Acknowledged, "B2: decrease while acknowledged must preserve Acknowledged=true")

	// 4. Clear at zero: make row unacknowledged, withdraw remaining decisions,
	// then tray open (limit=0) -> clears row to acknowledged=true, resource_id="0".
	require.NoError(t, f.store.UpdateNotificationState(ctx, row.ID, row.Message, row.ResourceID, false))
	_, err = f.store.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{AskerAgentID: f.agent.ID, Keys: []string{"p1-b"}})
	require.NoError(t, err)
	_, err = f.store.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{AskerAgentID: bot2ID, Keys: []string{"p2-a"}})
	require.NoError(t, err)

	rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	queued, _ = decisionBellHasQueuedUser(f.srv, f.bob.ID)
	assert.False(t, queued)
	row, err = f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
	require.NoError(t, err)
	assert.Equal(t, "0", row.ResourceID)
	assert.Equal(t, decisionsWaitingClearedMessage, row.Message)
	assert.True(t, row.Acknowledged, "zero waiting decisions must clear the bell row inline")
}

// TestDecisionsBell_ReconcileReuse_FallbackToAsync verifies that every excluded
// case (scoped UAT token, q/conversation/agent/project filters, project endpoint,
// bell disabled, non-200, tab=made) keeps existing reconcile behaviour.
func TestDecisionsBell_ReconcileReuse_FallbackToAsync(t *testing.T) {
	t.Run("filters_project_endpoint_non200_made_flagoff", func(t *testing.T) {
		f := newDecisionsFixture(t)
		setDecisionsExperiment(t, f.srv, true)
		release := holdDecisionBellWorkers(t, f.srv)
		ctx := context.Background()

		f.agent.Ancestry = []string{f.bob.ID}
		f.agent.MessageMode = store.MessageModeProject
		require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

		choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
		_, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
				Recipient: "chat", ThreadID: f.topicID, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name,
			Decisions: []messages.Decision{{Key: "ship", Question: "Ship release?", Choices: choices}},
		})
		require.NoError(t, err)

		// Each of these must NOT write inline while workers are held, and MUST
		// enqueue an async reconcile for f.bob.
		fallbackCases := []struct {
			name string
			path string
		}{
			{name: "filter_q", path: userMeDecisionsPath + "?tab=pending&q=Ship"},
			{name: "filter_conversation", path: userMeDecisionsPath + "?tab=pending&conversation=" + f.topicID},
			{name: "filter_agent", path: userMeDecisionsPath + "?tab=pending&agent=" + f.agent.ID},
			{name: "filter_project", path: userMeDecisionsPath + "?tab=pending&project=" + f.project.ID},
			{name: "project_endpoint", path: "/api/v1/projects/" + f.project.ID + "/decisions?tab=pending"},
		}
		for _, tc := range fallbackCases {
			t.Run(tc.name, func(t *testing.T) {
				clearDecisionBellQueuedUsers(f.srv)
				rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, tc.path, nil)
				require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

				queued, reconcile := decisionBellHasQueuedUser(f.srv, f.bob.ID)
				assert.True(t, queued, "%s must enqueue async reconcile", tc.name)
				assert.True(t, reconcile, "%s must enqueue with reconcile=true", tc.name)

				_, nerr := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
				assert.ErrorIs(t, nerr, store.ErrNotFound, "%s must not write bell notification inline", tc.name)
			})
		}

		// Non-200 (validation error) with bell enabled: neither inline write nor async enqueue.
		clearDecisionBellQueuedUsers(f.srv)
		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=-1", nil)
		require.Equal(t, http.StatusBadRequest, rr.Code)
		queued, _ := decisionBellHasQueuedUser(f.srv, f.bob.ID)
		assert.False(t, queued, "non-200 with bell enabled must not enqueue async reconcile")

		// tab=made: neither inline write nor async enqueue.
		clearDecisionBellQueuedUsers(f.srv)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=made", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		queued, _ = decisionBellHasQueuedUser(f.srv, f.bob.ID)
		assert.False(t, queued, "tab=made must not enqueue async reconcile")

		// Bell disabled (web.decisions=false -> 404): enqueues async flag-off reconcile.
		clearDecisionBellQueuedUsers(f.srv)
		setDecisionsExperiment(t, f.srv, false)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
		require.Equal(t, http.StatusNotFound, rr.Code)
		queued, reconcile := decisionBellHasQueuedUser(f.srv, f.bob.ID)
		assert.True(t, queued, "bell-disabled request must still enqueue flag-off reconcile")
		assert.True(t, reconcile)

		// Re-enable bell and release workers: one filtered request heals via async worker.
		clearDecisionBellQueuedUsers(f.srv)
		setDecisionsExperiment(t, f.srv, true)
		rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&q=Ship", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		release()
		waitDecisionBellIdle(t, f.srv)

		row, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
		require.NoError(t, err)
		assert.Equal(t, "1", row.ResourceID)
		assert.False(t, row.Acknowledged)
	})

	t.Run("scoped_uat_token_enqueues_async_and_writes_full_user_count", func(t *testing.T) {
		f := newChatFixture(t)
		setDecisionsExperiment(t, f.srv, true)
		release := holdDecisionBellWorkers(t, f.srv)
		ctx := context.Background()

		bindProjectMember(t, f.s, f.projB, f.member)
		for _, ag := range []*store.Agent{f.agentA, f.agentB} {
			ag.OwnerID = f.member
			ag.Ancestry = []string{f.member}
			ag.MessageMode = store.MessageModeProject
			require.NoError(t, f.s.UpdateAgent(ctx, ag))
		}
		seedPrompt := func(projID, chatKey, recipient, key string, agent *store.Agent) {
			decs, nerr := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+key+`","question":"`+key+`?","choices":["yes","no"]}]`))
			require.NoError(t, nerr)
			_, cerr := f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
				Message: &store.Message{
					ID: api.NewUUID(), ProjectID: projID, Sender: "agent:" + agent.Slug, SenderID: agent.ID,
					Recipient: recipient, ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
				},
				Intro: key, AskerAgentID: agent.ID, AskerAgentName: agent.Slug, Decisions: decs,
			})
			require.NoError(t, cerr)
		}
		dmKeyA := f.dm(t, f.member, "agent", f.agentA.ID)
		seedPrompt(f.projA, f.topicA, "chat", "projA-topic", f.agentA)
		seedPrompt(f.projA, dmKeyA, "user:"+f.member, "projA-dm", f.agentA)
		seedPrompt(f.projB, f.topicB, "chat", "projB-topic", f.agentB)

		// Project-bounded UAT sees only 1 decision (projA topic), not all 3 across 2 projects.
		tokProj := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
		rr := f.call(t, tokProj, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		resp := decodeProfilePendingDecisions(t, rr)
		assert.Equal(t, 1, resp.Counts.WaitingOnYou, "project-bounded UAT sees narrowed count")

		queued, reconcile := decisionBellHasQueuedUser(f.srv, f.member)
		assert.True(t, queued, "scoped token must enqueue async reconcile")
		assert.True(t, reconcile)
		_, nerr := f.s.GetNotification(ctx, decisionsWaitingNotificationID(f.member))
		assert.ErrorIs(t, nerr, store.ErrNotFound, "scoped token must not write narrowed count inline")

		// Hub-bounded UAT also enqueues async reconcile (never reuses inline).
		clearDecisionBellQueuedUsers(f.srv)
		tokHub := f.mint(t, f.member, hubBoundary(), "project:read", "inbox:read", "agent:read")
		rr = f.call(t, tokHub, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		queued, reconcile = decisionBellHasQueuedUser(f.srv, f.member)
		assert.True(t, queued, "hub-bounded UAT must enqueue async reconcile")
		assert.True(t, reconcile)

		// Release worker: async reconcile writes the user's FULL count (3 in 2 projects).
		release()
		waitDecisionBellIdle(t, f.srv)

		row, err := f.s.GetNotification(ctx, decisionsWaitingNotificationID(f.member))
		require.NoError(t, err)
		assert.Equal(t, "3", row.ResourceID, "async reconcile must record full user count, not token-narrowed count")
		assert.Equal(t, formatDecisionsWaitingMessage(3, 2), row.Message)
	})
}

// TestDecisionsQueryCount_TrayOpenReconcileReuse verifies design-scale §3.5 M6 / §8 P5:
// a full tray open (GET /users/me/decisions?tab=pending&limit=0 + bell reconcile)
// reuses the handler's counts inline without running waitingOnYouTotal, cutting
// total tray-open SQL queries roughly in half compared to P1 head while remaining
// constant in project count (5 vs 40) and history depth (10 vs 300).
func TestDecisionsQueryCount_TrayOpenReconcileReuse(t *testing.T) {
	measure := func(t *testing.T, numProjects, settledPerProject, openProjects int) (trayOpenQueries, bellOnlyQueries int64) {
		t.Helper()
		f, cs, counter := newCountingDecisionsFixture(t)
		setDecisionsExperiment(t, f.srv, true)
		setFastDecisionBell(t, f.srv, time.Millisecond)
		seedSettledAndOpenHistoryForQueryCount(t, f, cs, numProjects, settledPerProject, openProjects)
		ctx := context.Background()

		// Full tray open: HTTP limit=0 + bell reconcile (waitDecisionBellIdle).
		counter.Reset(true)
		rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
		waitDecisionBellIdle(t, f.srv)
		trayOpenQueries = counter.Count()
		counter.Reset(false)

		resp := decodeProfilePendingDecisions(t, rr)
		assert.Equal(t, openProjects, resp.Counts.WaitingOnYou)
		f.srv.decisionBell.mu.Lock()
		started := f.srv.decisionBell.started
		f.srv.decisionBell.mu.Unlock()
		assert.False(t, started, "tray open must not start/enqueue async bell worker")

		if openProjects > 0 {
			row, err := f.store.GetNotification(ctx, decisionsWaitingNotificationID(f.bob.ID))
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("%d", openProjects), row.ResourceID)
		}

		// Measure standalone waitingOnYouTotal as the cost M6 avoids per tray open.
		counter.Reset(true)
		total, projs, err := f.srv.waitingOnYouTotal(ctx, f.bob.ID)
		bellOnlyQueries = counter.Count()
		counter.Reset(false)
		require.NoError(t, err)
		assert.Equal(t, openProjects, total)
		assert.Equal(t, openProjects, projs)

		t.Logf("projects=%d history=%d open=%d -> trayOpen(limit=0+reconcile)=%d queries (saved waitingOnYouTotal=%d queries; P1 total would be ~%d)",
			numProjects, settledPerProject, openProjects, trayOpenQueries, bellOnlyQueries, trayOpenQueries+bellOnlyQueries)
		return trayOpenQueries, bellOnlyQueries
	}

	t.Run("quiet_user", func(t *testing.T) {
		q5p10h, bell5p10h := measure(t, 5, 10, 0)
		q40p10h, bell40p10h := measure(t, 40, 10, 0)
		q5p300h, bell5p300h := measure(t, 5, 300, 0)

		assert.Equal(t, q5p10h, q40p10h, "tray-open query count must be constant across 5 vs 40 projects")
		assert.Equal(t, q5p10h, q5p300h, "tray-open query count must be constant across 10 vs 300 history rows")
		assert.Equal(t, bell5p10h, bell40p10h)
		assert.Equal(t, bell5p10h, bell5p300h)
		// Reusing counts avoids the entire waitingOnYouTotal roll-up (~half of P1's total).
		assert.Less(t, q40p10h*10, (q40p10h+bell40p10h)*6, "tray open with M6 must be under 60%% of tray-open + waitingOnYouTotal")
	})

	t.Run("one_open_project", func(t *testing.T) {
		q5p10h, bell5p10h := measure(t, 5, 10, 1)
		q40p10h, bell40p10h := measure(t, 40, 10, 1)
		q5p300h, bell5p300h := measure(t, 5, 300, 1)

		assert.Equal(t, q5p10h, q40p10h, "tray-open query count with 1 open must be constant across 5 vs 40 projects")
		assert.Equal(t, q5p10h, q5p300h, "tray-open query count with 1 open must be constant across 10 vs 300 history rows")
		assert.Equal(t, bell5p10h, bell40p10h)
		assert.Equal(t, bell5p10h, bell5p300h)
		assert.Less(t, q40p10h*10, (q40p10h+bell40p10h)*6, "tray open with M6 must be under 60%% of tray-open + waitingOnYouTotal")
	})
}

// TestDecisionsScale_TrayOpen benchmarks a complete tray open
// (GET /api/v1/users/me/decisions?tab=pending&limit=0 + bell reconcile) at
// scales R (20p/10k) and S (200p/200k) on SQLite (default) or PostgreSQL
// (when SCION_TEST_POSTGRES_DSN is set). Skipped unless SCION_DECISIONS_SCALE=1.
func TestDecisionsScale_TrayOpen(t *testing.T) {
	if os.Getenv("SCION_DECISIONS_SCALE") != "1" {
		t.Skip("set SCION_DECISIONS_SCALE=1 to run hub-level tray-open scale timings")
	}

	pgDSN := os.Getenv("SCION_TEST_POSTGRES_DSN")
	dialectName := "sqlite"
	if pgDSN != "" {
		dialectName = "postgres"
	}

	scenarios := []struct {
		name           string
		numProjects    int
		rowsPerProject int
		openProjects   int
	}{
		{name: "R_20p_10k_quiet", numProjects: 20, rowsPerProject: 500, openProjects: 0},
		{name: "R_20p_10k_1open", numProjects: 20, rowsPerProject: 500, openProjects: 1},
		{name: "S_200p_200k_quiet", numProjects: 200, rowsPerProject: 1000, openProjects: 0},
		{name: "S_200p_200k_1open", numProjects: 200, rowsPerProject: 1000, openProjects: 1},
	}

	for _, sc := range scenarios {
		t.Run(dialectName+"_"+sc.name, func(t *testing.T) {
			var (
				f       *decisionsFixture
				cs      *entadapter.CompositeStore
				counter *bellQueryCountingDriver
			)
			ctx := context.Background()
			if pgDSN != "" {
				// Use an isolated schema per subtest on the configured Postgres database.
				schemaName := fmt.Sprintf("p5_bench_%d_%s", time.Now().UnixNano(), strings.ToLower(sc.name))
				adminClient, err := entc.OpenPostgres(pgDSN, entc.PoolConfig{})
				require.NoError(t, err)
				adminDB := entadapter.NewCompositeStore(adminClient).DB()
				_, err = adminDB.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA "%s"`, schemaName))
				require.NoError(t, err)
				t.Cleanup(func() {
					_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE`, schemaName))
					_ = adminClient.Close()
				})
				sep := "?"
				for i := 0; i < len(pgDSN); i++ {
					if pgDSN[i] == '?' {
						sep = "&"
						break
					}
				}
				subDSN := pgDSN + sep + "search_path=" + schemaName
				base, err := entc.OpenPostgres(subDSN, entc.PoolConfig{})
				require.NoError(t, err)
				t.Cleanup(func() { _ = base.Close() })
				require.NoError(t, entc.AutoMigrate(ctx, base))
				counter = newBellQueryCountingDriver(base.Driver())
				cs = entadapter.NewCompositeStore(ent.NewClient(ent.Driver(counter)))
				srv, s := testServerOnMigratedStore(t, cs)
				alice, bob, project := setupDemoPolicyOn(t, srv, s)
				f = newDecisionsFixtureOn(t, srv, s, alice, bob, project)
			} else {
				f, cs, counter = newCountingDecisionsFixture(t)
			}

			setDecisionsExperiment(t, f.srv, true)
			setFastDecisionBell(t, f.srv, time.Millisecond)

			memberRole, err := f.store.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
			require.NoError(t, err)

			base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
			choicesJSON := `[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]`
			detUUID := func(tag byte, p, j int) string {
				var u uuid.UUID
				u[0] = tag
				binary.BigEndian.PutUint32(u[4:8], uint32(p+1))
				binary.BigEndian.PutUint32(u[8:12], uint32(j+1))
				u[6] = (u[6] & 0x0f) | 0x40
				u[8] = (u[8] & 0x3f) | 0x80
				return u.String()
			}

			seedStart := time.Now()
			type projSeed struct {
				projID string
				tpID   string
				botID  string
				pIdx   int
			}
			projSeeds := make([]projSeed, sc.numProjects)
			var openSeeds []projSeed

			for p := 0; p < sc.numProjects; p++ {
				projID := detUUID(0x01, p, 0)
				require.NoError(t, f.store.CreateProject(ctx, &store.Project{
					ID: projID, Name: fmt.Sprintf("p5-%s-p-%03d", sc.name, p), Slug: fmt.Sprintf("p5-%s-p-%03d", sc.name, p), Created: base,
				}))
				_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.bob.ID,
					ScopeType: store.RoleScopeProject, ScopeID: projID, CreatedBy: "test",
				})
				require.NoError(t, err)
				tpID := createTestDecisionTopic(t, f.wcs, projID, "general", f.bob.ID)
				botID := detUUID(0x10, p, 0)
				ps := projSeed{projID: projID, tpID: tpID, botID: botID, pIdx: p}
				projSeeds[p] = ps
				if p < sc.openProjects {
					openSeeds = append(openSeeds, ps)
				}
			}

			tx, err := cs.DB().BeginTx(ctx, nil)
			require.NoError(t, err)
			decSQL := `INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, created_at, decided_at, withdrawn_at, dismissed_at)
				 VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, 'agent', 1, ?, ?, false, 0, ?, ?, ?, ?, ?, ?)`
			ansSQL := `INSERT INTO decision_answers (id, decision_row_id, project_id, asker_agent_id, key, version, question_text, choice_id, choice_label, answered_by_user_id, answered_by_name, answered_by_email, provenance, outcome, surface, response_message_id, matched_default, created_at)
				 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, 'Bob', 'bob@test.com', 'direct', ?, 'decisions_page', ?, false, ?)`
			if pgDSN != "" {
				decSQL = `INSERT INTO decisions (id, project_id, chat_key, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, allow_other, intro_length, status, settled, created_at, decided_at, withdrawn_at, dismissed_at)
				 VALUES ($1, $2, $3, $4, 0, $5, $6, $7, $8, 'agent', 1, $9, $10, false, 0, $11, $12, $13, $14, $15, $16)`
				ansSQL = `INSERT INTO decision_answers (id, decision_row_id, project_id, asker_agent_id, key, version, question_text, choice_id, choice_label, answered_by_user_id, answered_by_name, answered_by_email, provenance, outcome, surface, response_message_id, matched_default, created_at)
				 VALUES ($1, $2, $3, $4, $5, 1, $6, $7, $8, $9, 'Bob', 'bob@test.com', 'direct', $10, 'decisions_page', $11, false, $12)`
			}
			stmtDec, err := tx.PrepareContext(ctx, decSQL)
			require.NoError(t, err)
			stmtAns, err := tx.PrepareContext(ctx, ansSQL)
			require.NoError(t, err)

			for p, ps := range projSeeds {
				projID, tpID, botID := ps.projID, ps.tpID, ps.botID
				for j := 0; j < sc.rowsPerProject; j++ {
					decID := detUUID(0x20, p, j)
					msgID := detUUID(0x40, p, j)
					key := fmt.Sprintf("k-%03d-%06d", p, j)
					ts := base.Add(time.Duration(p*100000+j) * time.Second)
					qText := fmt.Sprintf("Deploy %d?", j)
					switch {
					case j%20 == 1:
						_, err = stmtDec.ExecContext(ctx, decID, projID, tpID, msgID, botID, "bot", key, key, qText, choicesJSON, "withdrawn", true, ts, nil, ts, nil)
						require.NoError(t, err)
					case j%20 == 2:
						_, err = stmtDec.ExecContext(ctx, decID, projID, tpID, msgID, botID, "bot", key, key, qText, choicesJSON, "dismissed", true, ts, nil, nil, ts)
						require.NoError(t, err)
						_, err = stmtAns.ExecContext(ctx, detUUID(0x30, p, j), decID, projID, botID, key, qText, "$dismissed", "Dismissed", f.bob.ID, "dismiss", detUUID(0x50, p, j), ts)
						require.NoError(t, err)
					default:
						_, err = stmtDec.ExecContext(ctx, decID, projID, tpID, msgID, botID, "bot", key, key, qText, choicesJSON, "decided", true, ts, ts, nil, nil)
						require.NoError(t, err)
						_, err = stmtAns.ExecContext(ctx, detUUID(0x30, p, j), decID, projID, botID, key, qText, "yes", "Yes", f.bob.ID, "answer", detUUID(0x50, p, j), ts)
						require.NoError(t, err)
					}
				}
			}
			require.NoError(t, stmtDec.Close())
			require.NoError(t, stmtAns.Close())
			require.NoError(t, tx.Commit())

			choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
			for _, osd := range openSeeds {
				_, err = f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
					Message: &store.Message{
						ID: api.NewUUID(), ProjectID: osd.projID, Sender: "agent:bot", SenderID: osd.botID,
						Recipient: "chat", ThreadID: osd.tpID, Channel: "web", CreatedAt: base.Add(24 * time.Hour),
					},
					AskerAgentID: osd.botID, AskerAgentName: "bot",
					Decisions: []messages.Decision{{Key: fmt.Sprintf("open-%03d", osd.pIdx), Question: "Ship?", Choices: choices}},
				})
				require.NoError(t, err)
			}
			_, err = cs.DB().ExecContext(ctx, "ANALYZE")
			require.NoError(t, err)
			t.Logf("[%s/%s] seeded %d projects x %d rows (+ %d open) in %v", dialectName, sc.name, sc.numProjects, sc.rowsPerProject, sc.openProjects, time.Since(seedStart))

			// Warm run + 3 measured full tray opens (limit=0 + reconcile).
			rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
			require.Equal(t, http.StatusOK, rr.Code)
			waitDecisionBellIdle(t, f.srv)

			var totalDur time.Duration
			var lastQueries int64
			const runs = 3
			for i := 0; i < runs; i++ {
				counter.Reset(true)
				t0 := time.Now()
				rr = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, userMeDecisionsPath+"?tab=pending&limit=0", nil)
				waitDecisionBellIdle(t, f.srv)
				totalDur += time.Since(t0)
				lastQueries = counter.Count()
				counter.Reset(false)
				resp := decodeProfilePendingDecisions(t, rr)
				require.Equal(t, sc.openProjects, resp.Counts.WaitingOnYou)
			}

			t.Logf("[%s/%s] TRAY OPEN (limit=0 + reconcile, avg of %d): %v (%d queries)",
				dialectName, sc.name, runs, totalDur/runs, lastQueries)
		})
	}
}
