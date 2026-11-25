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
