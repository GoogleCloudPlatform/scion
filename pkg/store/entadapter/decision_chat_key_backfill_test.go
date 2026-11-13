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

package entadapter

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedDecisionPromptForBackfillTest(t *testing.T, cs *CompositeStore, projectID, agentID, threadID, key string) (msgID, rowID string) {
	t.Helper()
	ctx := context.Background()
	msgID = uuid.New().String()
	res, err := cs.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID:        msgID,
			ProjectID: projectID,
			Sender:    "agent:release-bot",
			SenderID:  agentID,
			Recipient: "group:topic",
			ThreadID:  threadID,
		},
		AskerAgentID:   agentID,
		AskerAgentName: "release-bot",
		Decisions: []messages.Decision{
			{
				Key:      key,
				Question: "Ship RC?",
				Choices: []messages.Choice{
					{ID: "yes", Label: "Yes"},
					{ID: "no", Label: "No"},
				},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Decisions, 1)
	return msgID, res.Decisions[0].RowID
}

func clearDecisionChatKey(t *testing.T, cs *CompositeStore, rowID string) {
	t.Helper()
	db := cs.DB()
	require.NotNil(t, db)
	_, err := db.ExecContext(context.Background(),
		rebindForDialect(cs.Dialect(), "UPDATE decisions SET chat_key = NULL WHERE id = ?"), rowID)
	require.NoError(t, err)
}

func rawDecisionChatKey(t *testing.T, cs *CompositeStore, rowID string) sql.NullString {
	t.Helper()
	var out sql.NullString
	err := cs.DB().QueryRowContext(context.Background(),
		rebindForDialect(cs.Dialect(), "SELECT chat_key FROM decisions WHERE id = ?"), rowID).Scan(&out)
	require.NoError(t, err)
	return out
}

func TestCreateChoicePrompt_PopulatesChatKey(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-chat-key-create")
	agentID := uuid.New().String()

	_, topicRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "topic-release", "D1")
	gotTopic, err := cs.GetDecision(ctx, topicRowID)
	require.NoError(t, err)
	assert.Equal(t, "topic-release", gotTopic.ChatKey)

	_, noThreadRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "", "D2")
	gotNoThread, err := cs.GetDecision(ctx, noThreadRowID)
	require.NoError(t, err)
	assert.Empty(t, gotNoThread.ChatKey)
	assert.False(t, rawDecisionChatKey(t, cs, noThreadRowID).Valid)
}

func TestBackfillDecisionChatKeys_PopulatesPresentAndSkipsPurged(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-chat-key-backfill")
	agentID := uuid.New().String()

	// 1. Legacy row whose prompt message still exists in messages.
	_, presentRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "dm:agent:"+agentID+":user:u1", "D1")
	clearDecisionChatKey(t, cs, presentRowID)
	require.False(t, rawDecisionChatKey(t, cs, presentRowID).Valid)

	// 2. Legacy row whose prompt message was already purged by chat retention.
	purgedMsgID, purgedRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "dm:agent:"+agentID+":user:u2", "D2")
	clearDecisionChatKey(t, cs, purgedRowID)
	_, err := cs.DB().ExecContext(ctx, rebindForDialect(cs.Dialect(), "DELETE FROM messages WHERE id = ?"), purgedMsgID)
	require.NoError(t, err)

	// 3. Legacy row whose prompt message has a NULL thread_id.
	_, nullThreadRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "", "D3")
	clearDecisionChatKey(t, cs, nullThreadRowID)

	// 4. Legacy row whose prompt message has a literal empty-string thread_id ('').
	emptyThreadMsgID, emptyThreadRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "topic-to-blank", "D4")
	clearDecisionChatKey(t, cs, emptyThreadRowID)
	_, err = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "UPDATE messages SET thread_id = '' WHERE id = ?"),
		emptyThreadMsgID)
	require.NoError(t, err)

	// Clear marker if newTestCompositeStore ran Migrate at init.
	_, _ = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "DELETE FROM hub_settings WHERE section = ?"),
		decisionChatKeyBackfillMarkerSection)

	require.NoError(t, cs.BackfillDecisionChatKeys(ctx))

	gotPresent, err := cs.GetDecision(ctx, presentRowID)
	require.NoError(t, err)
	assert.Equal(t, "dm:agent:"+agentID+":user:u1", gotPresent.ChatKey)

	gotPurged, err := cs.GetDecision(ctx, purgedRowID)
	require.NoError(t, err)
	assert.Empty(t, gotPurged.ChatKey)
	assert.False(t, rawDecisionChatKey(t, cs, purgedRowID).Valid)

	assert.False(t, rawDecisionChatKey(t, cs, nullThreadRowID).Valid)
	assert.False(t, rawDecisionChatKey(t, cs, emptyThreadRowID).Valid)
}

func TestBackfillDecisionChatKeys_IdempotentWithAndWithoutMarker(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-chat-key-idempotent")
	agentID := uuid.New().String()

	msgID, rowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "topic-initial", "D1")
	clearDecisionChatKey(t, cs, rowID)
	_, _ = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "DELETE FROM hub_settings WHERE section = ?"),
		decisionChatKeyBackfillMarkerSection)

	require.NoError(t, cs.BackfillDecisionChatKeys(ctx))
	assert.Equal(t, "topic-initial", rawDecisionChatKey(t, cs, rowID).String)

	// Mutate messages.thread_id and re-run WITH marker and WITHOUT marker:
	// an already-populated chat_key must never be overwritten.
	_, err := cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "UPDATE messages SET thread_id = ? WHERE id = ?"),
		"topic-mutated", msgID)
	require.NoError(t, err)

	// Re-run with marker present (short-circuits).
	require.NoError(t, cs.BackfillDecisionChatKeys(ctx))
	assert.Equal(t, "topic-initial", rawDecisionChatKey(t, cs, rowID).String)

	// Re-run with marker cleared (SQL WHERE chat_key IS NULL still skips set rows).
	_, err = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "DELETE FROM hub_settings WHERE section = ?"),
		decisionChatKeyBackfillMarkerSection)
	require.NoError(t, err)
	require.NoError(t, cs.BackfillDecisionChatKeys(ctx))
	assert.Equal(t, "topic-initial", rawDecisionChatKey(t, cs, rowID).String)
}

func TestCompositeStore_Migrate_BackfillsDecisionChatKeys(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-chat-key-migrate-wiring")
	agentID := uuid.New().String()

	_, rowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "topic-migrate-wired", "D1")
	clearDecisionChatKey(t, cs, rowID)
	_, _ = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "DELETE FROM hub_settings WHERE section = ?"),
		decisionChatKeyBackfillMarkerSection)

	require.NoError(t, cs.Migrate(ctx))

	got, err := cs.GetDecision(ctx, rowID)
	require.NoError(t, err)
	assert.Equal(t, "topic-migrate-wired", got.ChatKey,
		"CompositeStore.Migrate must run BackfillDecisionChatKeys")
}

func TestRecordAnswerAndSet_SurfaceDecisionsPage(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-surface-decisions-page")
	agentID := uuid.New().String()
	userID := uuid.New().String()
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Status:      "active",
		Created:     time.Now(),
	}))

	// Single answer with surface=decisions_page.
	_, singleRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentID, "topic-1", "S1")
	ans, replayed, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    singleRowID,
		ChoiceID:         "yes",
		AnsweredByUserID: userID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceDecisionsPage,
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:alice@example.com",
			SenderID:  userID,
			Recipient: "agent:release-bot",
			Msg:       "Yes",
		},
	})
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, store.DecisionSurfaceDecisionsPage, ans.Surface)

	// Set answer with surface=decisions_page, even after the prompt row in messages is purged!
	setMsgID := uuid.New().String()
	setRes, err := cs.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID:        setMsgID,
			ProjectID: projectID,
			Sender:    "agent:release-bot",
			SenderID:  agentID,
			Recipient: "group:topic",
			ThreadID:  "topic-1",
		},
		AskerAgentID:   agentID,
		AskerAgentName: "release-bot",
		Decisions: []messages.Decision{
			{
				Key:      "B1",
				Question: "Target?",
				Choices:  []messages.Choice{{ID: "prod", Label: "Prod"}, {ID: "stage", Label: "Stage"}},
			},
			{
				Key:      "B2",
				Question: "Dry run?",
				Choices:  []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, setRes.Decisions, 2)

	// Purge prompt message from messages table before answering set!
	_, err = cs.DB().ExecContext(ctx, rebindForDialect(cs.Dialect(), "DELETE FROM messages WHERE id = ?"), setMsgID)
	require.NoError(t, err)

	batchRes, err := cs.RecordAnswerSet(ctx, &store.RecordAnswerSetInput{
		MessageID:        setMsgID,
		AnsweredByUserID: userID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Surface:          store.DecisionSurfaceDecisionsPage,
		Answers: []store.AnswerSetItem{
			{DecisionRowID: setRes.Decisions[0].RowID, ChoiceID: "prod"},
			{DecisionRowID: setRes.Decisions[1].RowID, ChoiceID: "no"},
		},
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:alice@example.com",
			SenderID:  userID,
			Recipient: "agent:release-bot",
			Msg:       "B1=Prod, B2=No",
		},
	})
	require.NoError(t, err)
	require.Len(t, batchRes.Answers, 2)
	assert.Equal(t, store.DecisionSurfaceDecisionsPage, batchRes.Answers[0].Surface)
	assert.Equal(t, store.DecisionSurfaceDecisionsPage, batchRes.Answers[1].Surface)

	// Invalid surface is rejected with ErrInvalidInput.
	_, err = cs.RecordAnswerSet(ctx, &store.RecordAnswerSetInput{
		MessageID:        setMsgID,
		AnsweredByUserID: userID,
		Surface:          "bogus_surface",
		Answers: []store.AnswerSetItem{
			{DecisionRowID: setRes.Decisions[0].RowID, ChoiceID: "prod"},
		},
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:alice@example.com",
			Recipient: "agent:release-bot",
		},
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)
}
