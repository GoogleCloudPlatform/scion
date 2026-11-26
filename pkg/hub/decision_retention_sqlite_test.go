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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

func seedOldSettledDecision(t *testing.T, ctx context.Context, cs *entadapter.CompositeStore, projID, agentID, userID, key string, age time.Duration) (msgID, decID string) {
	t.Helper()
	oldTime := time.Now().UTC().Add(-age).Truncate(time.Microsecond)
	msgID = uuid.NewString()
	res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: msgID, ProjectID: projID, ThreadID: "topic-ret-hub",
			Sender: "agent:bot", SenderID: agentID, Recipient: "user:" + userID, RecipientID: userID,
		},
		AskerAgentID:   agentID,
		AskerAgentName: "bot",
		Decisions: []messages.Decision{{
			Key:      key,
			Question: "Question " + key,
			Choices:  []messages.Choice{{ID: "1", Label: "Yes"}, {ID: "2", Label: "No"}},
		}},
	})
	require.NoError(t, err)
	decID = res.Decisions[0].RowID
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    decID,
		ChoiceID:         "1",
		AnsweredByUserID: userID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@test.com",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projID, ThreadID: "topic-ret-hub",
			Sender: "user:" + userID, SenderID: userID, Recipient: "agent:bot", RecipientID: agentID,
		},
	})
	require.NoError(t, err)

	db := cs.DB()
	uid := uuid.MustParse(decID)
	_, err = db.ExecContext(ctx, "UPDATE decisions SET created_at = ?, decided_at = ? WHERE id = ?", oldTime, oldTime, uid)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?", oldTime, uid)
	require.NoError(t, err)
	return msgID, decID
}

// TestDecisionRetentionJob_InactiveWhenOffDisabledOrNoWindow proves that the
// decision-retention handler deletes NOTHING when:
//  1. hub.decision_retention experiment is off (even if settings have enabled=true + 30d);
//  2. experiment is on, enabled=false (legal hold);
//  3. experiment is on, enabled=true, retention_days is absent (keep forever).
func TestDecisionRetentionJob_InactiveWhenOffDisabledOrNoWindow(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "ret-off", Slug: "ret-off", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	userID := uuid.NewString()
	_, decID := seedOldSettledDecision(t, ctx, cs, projID, agentID, userID, "K1", 90*24*time.Hour)

	ops := srv.GetOperationalSettings()
	handler := srv.decisionRetentionHandler()

	// 1. Experiment OFF, settings enabled + 30d -> nothing purged.
	_, err := ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30}`), "test", 0, "managed")
	require.NoError(t, err)
	handler(ctx)
	_, err = cs.GetDecision(ctx, decID)
	require.NoError(t, err, "experiment off must never purge")

	// 2. Experiment ON, enabled=false (hold) + 30d -> nothing purged.
	_, err = ops.Update(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.DecisionRetention)),
		"test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":false,"retention_days":30}`), "test", ops.SectionRevision(opsettings.DecisionRetentionSectionName), "managed")
	require.NoError(t, err)
	handler(ctx)
	_, err = cs.GetDecision(ctx, decID)
	require.NoError(t, err, "enabled=false must never purge")

	// 3. Experiment ON, enabled=true, no retention_days -> nothing purged.
	_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true}`), "test", ops.SectionRevision(opsettings.DecisionRetentionSectionName), "managed")
	require.NoError(t, err)
	handler(ctx)
	_, err = cs.GetDecision(ctx, decID)
	require.NoError(t, err, "missing retention_days must never purge")

	runs, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, runs, "inactive ticks must not write run rows")
}

// TestDecisionRetentionJob_PurgesEligibleAndWritesCountsOnlyRun proves that
// when active, the decision-retention job purges old settled prompts, keeps
// recent/unsettled ones, writes a counts-only run row, and advances the cursor.
func TestDecisionRetentionJob_PurgesEligibleAndWritesCountsOnlyRun(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	// Verify singleton registration with LockDecisionRetention.
	srv.scheduler = NewScheduler(s, nil)
	srv.registerSchedulerHandlers()
	registered := false
	srv.scheduler.mu.Lock()
	for _, h := range srv.scheduler.recurring {
		if h.Name == "decision-retention" {
			registered = true
			assert.True(t, h.Singleton, "decision-retention must be a singleton")
			break
		}
	}
	srv.scheduler.mu.Unlock()
	assert.True(t, registered, "decision-retention must be registered on the scheduler")

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "ret-on", Slug: "ret-on", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	userID := uuid.NewString()

	oldMsgID, oldDecID := seedOldSettledDecision(t, ctx, cs, projID, agentID, userID, "OLD1", 60*24*time.Hour)
	_, recentDecID := seedOldSettledDecision(t, ctx, cs, projID, agentID, userID, "NEW1", 5*24*time.Hour)

	ops := srv.GetOperationalSettings()
	_, err := ops.Update(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.DecisionRetention)),
		"test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30,"sweep":{"tick_interval_minutes":1}}`),
		"test", 0, "managed")
	require.NoError(t, err)

	srv.decisionRetentionHandler()(ctx)

	_, err = cs.GetDecision(ctx, oldDecID)
	assert.ErrorIs(t, err, store.ErrNotFound, "old settled decision must be purged")
	_, err = cs.GetMessage(ctx, oldMsgID)
	assert.NoError(t, err, "prompt chat message must remain for markdown fallback")
	_, err = cs.GetDecision(ctx, recentDecID)
	assert.NoError(t, err, "recent settled decision must remain")

	runs, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.DecisionRetentionStatusOK, runs[0].Status)
	assert.Equal(t, int64(1), runs[0].Counts[projID].Decisions)
	assert.Equal(t, int64(1), runs[0].Counts[projID].Answers)
	assert.Equal(t, 30, runs[0].Policy.RetentionDays)

	cur, err := cs.DecisionRetention().GetDecisionRetentionCursor(ctx)
	require.NoError(t, err)
	assert.Equal(t, oldDecID, cur.Position.ID)
	require.NotNil(t, cur.ResetAt)
}

// TestDecisionRetentionViews_PurgedPromptAndDanglingReplacedBy verifies that
// after a purge:
//  1. A prompt message whose decision rows were purged yields no DecisionPromptView
//     so chat falls back to the message's markdown copy.
//  2. A retained decision whose replaced_by_id row was purged degrades gracefully
//     (status=replaced, no error, ReplacedBy omitted).
func TestDecisionRetentionViews_PurgedPromptAndDanglingReplacedBy(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	cs := s.(*entadapter.CompositeStore)
	ctx := context.Background()

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "ret-view", Slug: "ret-view", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	userID := uuid.NewString()

	msgID, decID := seedOldSettledDecision(t, ctx, cs, projID, agentID, userID, "V1", 60*24*time.Hour)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	res, err := cs.DecisionRetention().PurgeDecisionMessages(ctx, []string{msgID}, map[string]time.Time{projID: cutoff})
	require.NoError(t, err)
	require.Equal(t, 1, res.PurgedMessages)

	// 1. Prompt message still exists, decisionPromptViews skips it cleanly.
	msg, err := cs.GetMessage(ctx, msgID)
	require.NoError(t, err)
	assert.Contains(t, msg.Msg, "Question V1", "markdown fallback text stays in prompt message")
	views := srv.decisionPromptViews(ctx, []*store.Message{msg})
	assert.Empty(t, views, "purged prompt must yield no DecisionPromptView so client renders markdown fallback")

	// 2. Dangling replaced_by_id on a retained decision degrades gracefully.
	dangling := &store.Decision{
		ID:           uuid.NewString(),
		ProjectID:    projID,
		MessageID:    uuid.NewString(),
		AskerAgentID: agentID,
		Key:          "OLD-KEY",
		Version:      1,
		Question:     "Old Q",
		Status:       store.DecisionStatusReplaced,
		ReplacedByID: decID, // points to the purged decision
		CreatedAt:    time.Now().UTC(),
	}
	v := srv.decisionViewFor(ctx, dangling)
	assert.Equal(t, store.DecisionStatusReplaced, v.Status)
	assert.Nil(t, v.ReplacedBy, "dangling replaced_by_id must omit ReplacedBy link without failing")
}

// TestDecisionRetentionJob_MidTickHoldAndPerPageRunAudit verifies:
//  1. (O4) After page 1 commits, the run row is already persisted in the DB
//     with status=running and page 1's exact counts before page 2 begins.
//  2. (O3) Flipping enabled=false from the sleep hook between page 1 and page 2
//     stops the tick immediately so no further page is purged.
func TestDecisionRetentionJob_MidTickHoldAndPerPageRunAudit(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "ret-hold", Slug: "ret-hold", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	userID := uuid.NewString()

	const total = 60 // page_size=50 -> page 1 purges 50, page 2 would purge 10
	decIDs := make([]string, total)
	for i := 0; i < total; i++ {
		_, decIDs[i] = seedOldSettledDecision(t, ctx, cs, projID, agentID, userID, fmt.Sprintf("H%02d", i), 60*24*time.Hour)
	}

	ops := srv.GetOperationalSettings()
	_, err := ops.Update(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.DecisionRetention)),
		"test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30,"sweep":{"page_size":50,"tick_interval_minutes":1}}`),
		"test", 0, "managed")
	require.NoError(t, err)

	sw := newDecisionRetentionSweeper(cs.DecisionRetention(), true)
	sw.policy = srv.decisionRetentionPolicyAt
	sleepCalls := 0
	sw.sleep = func(ctx context.Context, _ time.Duration) error {
		sleepCalls++
		// O4: verify page 1's committed deletions are already audited in DB mid-tick.
		midRuns, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
		require.NoError(t, err)
		require.Len(t, midRuns, 1, "run row must be persisted after page 1 before sleep returns")
		assert.Equal(t, store.DecisionRetentionStatusRunning, midRuns[0].Status)
		assert.Nil(t, midRuns[0].FinishedAt)
		assert.Equal(t, int64(50), midRuns[0].Counts[projID].Decisions)
		assert.Equal(t, int64(50), midRuns[0].Counts[projID].Answers)

		// O3: set legal hold (enabled=false) mid-tick.
		_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
			json.RawMessage(`{"enabled":false,"retention_days":30,"sweep":{"page_size":50,"tick_interval_minutes":1}}`),
			"test", ops.SectionRevision(opsettings.DecisionRetentionSectionName), "managed")
		require.NoError(t, err)
		return nil
	}

	p := srv.decisionRetentionPolicyAt(time.Now())
	res, err := sw.RunTick(ctx, p)
	require.NoError(t, err)
	assert.True(t, res.Ran)
	assert.Equal(t, 1, sleepCalls)
	assert.Equal(t, 1, res.Pages, "hold set in sleep hook must stop tick before page 2")
	assert.Equal(t, 50, res.PurgedMessages)
	assert.False(t, res.ReachedCutoff)

	// Exactly 10 decisions must remain untouched.
	remaining := 0
	for _, id := range decIDs {
		if _, err := cs.GetDecision(ctx, id); err == nil {
			remaining++
		}
	}
	assert.Equal(t, 10, remaining, "page 2 rows must not be purged after mid-tick hold")

	runs, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.DecisionRetentionStatusPartial, runs[0].Status)
	assert.NotNil(t, runs[0].FinishedAt)
	assert.Equal(t, int64(50), runs[0].Counts[projID].Decisions)
}

// TestDecisionRetentionJob_MultiPageWalkAcrossTicks verifies that a sweep
// interrupted by tick budget persists its (created_at, id) cursor and resumes
// on the next tick to purge the remaining rows (including tied created_at rows)
// while accumulating counts into the same 24h sweep run row.
func TestDecisionRetentionJob_MultiPageWalkAcrossTicks(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "ret-multi", Slug: "ret-multi", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	userID := uuid.NewString()

	const total = 65
	tiedTime := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Microsecond)
	decIDs := make([]string, total)
	db := cs.DB()
	for i := 0; i < total; i++ {
		_, decIDs[i] = seedOldSettledDecision(t, ctx, cs, projID, agentID, userID, fmt.Sprintf("M%02d", i), 60*24*time.Hour)
		// Force identical created_at across all 65 rows so the cross-tick cursor
		// must tie-break by id across the 50/15 page boundary.
		uid := uuid.MustParse(decIDs[i])
		_, err := db.ExecContext(ctx, "UPDATE decisions SET created_at = ?, decided_at = ? WHERE id = ?", tiedTime, tiedTime, uid)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?", tiedTime, uid)
		require.NoError(t, err)
	}

	ops := srv.GetOperationalSettings()
	_, err := ops.Update(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.DecisionRetention)),
		"test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30,"sweep":{"page_size":50,"tick_budget_seconds":10,"tick_interval_minutes":1}}`),
		"test", 0, "managed")
	require.NoError(t, err)

	fakeNow := time.Now().UTC()
	sw := newDecisionRetentionSweeper(cs.DecisionRetention(), true)
	sw.policy = srv.decisionRetentionPolicyAt
	sw.now = func() time.Time { return fakeNow }
	// Advance clock past tick_budget_seconds inside sleep after page 1 so Tick 1 stops with BudgetExhausted.
	sw.sleep = func(_ context.Context, _ time.Duration) error {
		fakeNow = fakeNow.Add(15 * time.Second)
		return nil
	}

	// Tick 1: purges first 50 rows and stops on budget.
	res1, err := sw.RunTick(ctx, srv.decisionRetentionPolicyAt(fakeNow))
	require.NoError(t, err)
	assert.True(t, res1.Ran)
	assert.Equal(t, 1, res1.Pages)
	assert.Equal(t, 50, res1.PurgedMessages)
	assert.True(t, res1.BudgetExhausted)
	assert.False(t, res1.ReachedCutoff)

	// Advance fake clock past tick_interval_minutes and run Tick 2.
	fakeNow = fakeNow.Add(2 * time.Minute)
	sw.sleep = func(_ context.Context, _ time.Duration) error { return nil }
	res2, err := sw.RunTick(ctx, srv.decisionRetentionPolicyAt(fakeNow))
	require.NoError(t, err)
	assert.True(t, res2.Ran)
	assert.Equal(t, 1, res2.Pages)
	assert.Equal(t, 15, res2.PurgedMessages)
	assert.True(t, res2.ReachedCutoff)

	// All 65 decisions must be purged.
	for _, id := range decIDs {
		_, err := cs.GetDecision(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "decision %s should have been purged across ticks", id)
	}

	// Same 24h sweep run row accumulates all 65 decisions/answers and finishes OK.
	runs, err := cs.DecisionRetention().ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.DecisionRetentionStatusOK, runs[0].Status)
	assert.Equal(t, int64(total), runs[0].Counts[projID].Decisions)
	assert.Equal(t, int64(total), runs[0].Counts[projID].Answers)
}

// TestDecisionRetentionJob_CursorDoesNotResetBeforeReachingCutoff verifies that
// a backlog of ineligible old rows (e.g. settled rows paired with an open
// sibling) larger than one day's scan budget does not starve newer eligible
// rows: the cursor does not reset after 24h until the walk has actually reached
// the cutoff, and resets on the day after reaching the cutoff.
func TestDecisionRetentionJob_CursorDoesNotResetBeforeReachingCutoff(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	cs := s.(*entadapter.CompositeStore)
	srv.SetDecisionRetentionStore(cs.DecisionRetention())
	ctx := context.Background()

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projID, Name: "ret-backlog", Slug: "ret-backlog", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	userID := uuid.NewString()

	baseNow := time.Now().UTC().Truncate(time.Microsecond)
	tBacklog := baseNow.Add(-70 * 24 * time.Hour)
	tEligible := baseNow.Add(-60 * 24 * time.Hour)
	db := cs.DB()

	// Seed 60 ineligible old settled rows (each sharing its message with a
	// pending sibling) so page 1 (size 50) scans only ineligible rows. Use a
	// fresh asker agent ID per iteration so we stay under the per-agent
	// pending-decisions cap (50).
	const backlogMsgs = 60
	for i := 0; i < backlogMsgs; i++ {
		backlogAgentID := uuid.NewString()
		msgID := uuid.NewString()
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: msgID, ProjectID: projID, ThreadID: "topic-ret-backlog",
				Sender: "agent:bot", SenderID: backlogAgentID, Recipient: "user:" + userID, RecipientID: userID,
			},
			AskerAgentID:   backlogAgentID,
			AskerAgentName: "bot",
			Decisions: []messages.Decision{
				{Key: fmt.Sprintf("B%02d-S", i), Question: "Settled", Choices: []messages.Choice{{ID: "1", Label: "Yes"}}},
				{Key: fmt.Sprintf("B%02d-P", i), Question: "Pending", Choices: []messages.Choice{{ID: "1", Label: "Yes"}}},
			},
		})
		require.NoError(t, err)
		settledID := res.Decisions[0].RowID
		pendingID := res.Decisions[1].RowID
		_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID: settledID, ChoiceID: "1",
			AnsweredByUserID: userID, AnsweredByName: "Alice", AnsweredByEmail: "alice@test.com",
			Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: projID, ThreadID: "topic-ret-backlog",
				Sender: "user:" + userID, SenderID: userID, Recipient: "agent:bot", RecipientID: backlogAgentID,
			},
		})
		require.NoError(t, err)
		ts := tBacklog.Add(time.Duration(i) * time.Second)
		for _, id := range []string{settledID, pendingID} {
			uid := uuid.MustParse(id)
			_, err = db.ExecContext(ctx, "UPDATE decisions SET created_at = ? WHERE id = ?", ts, uid)
			require.NoError(t, err)
		}
		uidS := uuid.MustParse(settledID)
		_, err = db.ExecContext(ctx, "UPDATE decisions SET decided_at = ? WHERE id = ?", ts, uidS)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?", ts, uidS)
		require.NoError(t, err)
	}

	// Seed 15 eligible old settled rows after the backlog.
	const eligibleCount = 15
	eligibleDecIDs := make([]string, eligibleCount)
	for i := 0; i < eligibleCount; i++ {
		_, eligibleDecIDs[i] = seedOldSettledDecision(t, ctx, cs, projID, agentID, userID, fmt.Sprintf("E%02d", i), 60*24*time.Hour)
		ts := tEligible.Add(time.Duration(i) * time.Second)
		uid := uuid.MustParse(eligibleDecIDs[i])
		_, err := db.ExecContext(ctx, "UPDATE decisions SET created_at = ?, decided_at = ? WHERE id = ?", ts, ts, uid)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?", ts, uid)
		require.NoError(t, err)
	}

	ops := srv.GetOperationalSettings()
	_, err := ops.Update(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.DecisionRetention)),
		"test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, opsettings.DecisionRetentionSectionName,
		json.RawMessage(`{"enabled":true,"retention_days":30,"sweep":{"page_size":50,"tick_budget_seconds":10,"tick_interval_minutes":1}}`),
		"test", 0, "managed")
	require.NoError(t, err)

	fakeNow := baseNow
	sw := newDecisionRetentionSweeper(cs.DecisionRetention(), true)
	sw.policy = srv.decisionRetentionPolicyAt
	sw.now = func() time.Time { return fakeNow }
	// Exhaust the tick budget after 1 page (50 rows) so Day 1 cannot finish the backlog.
	sw.sleep = func(_ context.Context, _ time.Duration) error {
		fakeNow = fakeNow.Add(15 * time.Second)
		return nil
	}

	// Day 1: scans first 50 ineligible backlog rows and stops on budget.
	res1, err := sw.RunTick(ctx, srv.decisionRetentionPolicyAt(fakeNow))
	require.NoError(t, err)
	assert.Equal(t, 1, res1.Pages)
	assert.Equal(t, 0, res1.PurgedMessages)
	assert.Equal(t, 50, res1.SkippedMessages)
	assert.True(t, res1.BudgetExhausted)
	assert.False(t, res1.ReachedCutoff)

	cur1, err := cs.DecisionRetention().GetDecisionRetentionCursor(ctx)
	require.NoError(t, err)
	assert.False(t, cur1.Position.IsStart())
	assert.False(t, cur1.ReachedCutoff)
	require.NotNil(t, cur1.ResetAt)
	day1ResetAt := *cur1.ResetAt

	// Day 2 (+25h): even though >24h elapsed, cursor MUST NOT reset because
	// ReachedCutoff is false. It resumes past the first 50 backlog rows, scans
	// the remaining 10 ineligible + 15 eligible rows, purges all 15 eligible
	// rows, and reaches the cutoff.
	fakeNow = baseNow.Add(25 * time.Hour)
	res2, err := sw.RunTick(ctx, srv.decisionRetentionPolicyAt(fakeNow))
	require.NoError(t, err)
	assert.Equal(t, 1, res2.Pages)
	assert.Equal(t, eligibleCount, res2.PurgedMessages, "eligible rows behind backlog must not be starved")
	assert.Equal(t, 10, res2.SkippedMessages)
	assert.True(t, res2.ReachedCutoff)

	for _, id := range eligibleDecIDs {
		_, err := cs.GetDecision(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}
	cur2, err := cs.DecisionRetention().GetDecisionRetentionCursor(ctx)
	require.NoError(t, err)
	assert.True(t, cur2.ReachedCutoff)
	assert.True(t, cur2.ResetAt.Equal(day1ResetAt), "ResetAt must not advance before reset occurs")

	// Day 3 (+50h): now ReachedCutoff is true AND >24h elapsed since day1ResetAt,
	// so the cursor resets to the start of the walk and revisits the backlog.
	fakeNow = baseNow.Add(50 * time.Hour)
	res3, err := sw.RunTick(ctx, srv.decisionRetentionPolicyAt(fakeNow))
	require.NoError(t, err)
	assert.Equal(t, 1, res3.Pages)
	assert.Equal(t, 50, res3.SkippedMessages, "after reset, walk revisits backlog from start")
	assert.False(t, res3.ReachedCutoff)

	cur3, err := cs.DecisionRetention().GetDecisionRetentionCursor(ctx)
	require.NoError(t, err)
	require.NotNil(t, cur3.ResetAt)
	assert.True(t, cur3.ResetAt.After(day1ResetAt), "ResetAt must advance when cursor resets")
	assert.False(t, cur3.ReachedCutoff)
}

// TestDecisionRetentionJob_ReconcileInterruptedRuns verifies that startup
// reconciliation marks running rows started before server startTime as failed
// (preserving committed counts), leaves rows started after startTime alone,
// and is idempotent.
func TestDecisionRetentionJob_ReconcileInterruptedRuns(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	cs := s.(*entadapter.CompositeStore)
	st := cs.DecisionRetention()
	srv.SetDecisionRetentionStore(st)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	srv.mu.Lock()
	srv.startTime = now
	srv.mu.Unlock()

	projID := uuid.NewString()
	interrupted := &store.DecisionRetentionRun{
		Trigger:   store.DecisionRetentionTriggerSweep,
		StartedAt: now.Add(-5 * time.Minute),
		Status:    store.DecisionRetentionStatusRunning,
		Policy:    store.DecisionRetentionPolicy{RetentionDays: 30, Cutoff: now.Add(-30 * 24 * time.Hour)},
		Counts:    map[string]store.DecisionRetentionCounts{projID: {Decisions: 50, Answers: 50}},
	}
	require.NoError(t, st.SaveDecisionRetentionRun(ctx, interrupted))

	live := &store.DecisionRetentionRun{
		Trigger:   store.DecisionRetentionTriggerSweep,
		StartedAt: now.Add(time.Minute),
		Status:    store.DecisionRetentionStatusRunning,
		Policy:    store.DecisionRetentionPolicy{RetentionDays: 30, Cutoff: now.Add(-30 * 24 * time.Hour)},
		Counts:    map[string]store.DecisionRetentionCounts{},
	}
	require.NoError(t, st.SaveDecisionRetentionRun(ctx, live))

	assert.Equal(t, 1, srv.ReconcileInterruptedDecisionRetentionRuns(ctx))
	assert.Equal(t, 0, srv.ReconcileInterruptedDecisionRetentionRuns(ctx), "reconcile must be idempotent")

	runs, err := st.ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, runs, 2)
	byID := map[string]store.DecisionRetentionRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	gotInt := byID[interrupted.ID]
	assert.Equal(t, store.DecisionRetentionStatusFailed, gotInt.Status)
	assert.Equal(t, decisionRetentionInterruptedError, gotInt.Error)
	assert.NotNil(t, gotInt.FinishedAt)
	assert.Equal(t, int64(50), gotInt.Counts[projID].Decisions, "committed counts must be preserved")

	gotLive := byID[live.ID]
	assert.Equal(t, store.DecisionRetentionStatusRunning, gotLive.Status)
	assert.Nil(t, gotLive.FinishedAt)
}
