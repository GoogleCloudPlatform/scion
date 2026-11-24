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
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

const decisionSettledPartialIndex = "decision_project_id_chat_key"

func decisionSettledFlag(t *testing.T, cs *CompositeStore, rowID string) bool {
	t.Helper()
	uid, err := uuid.Parse(rowID)
	require.NoError(t, err)
	row, err := cs.client.Decision.Get(context.Background(), uid)
	require.NoError(t, err)
	return row.Settled
}

func forceDecisionSettled(t *testing.T, cs *CompositeStore, settled bool, rowIDs ...string) {
	t.Helper()
	ctx := context.Background()
	if len(rowIDs) == 0 {
		_, err := cs.client.Decision.Update().SetSettled(settled).Save(ctx)
		require.NoError(t, err)
		return
	}
	uids := make([]uuid.UUID, len(rowIDs))
	for i, id := range rowIDs {
		uids[i] = uuid.MustParse(id)
	}
	_, err := cs.client.Decision.Update().Where(decision.IDIn(uids...)).SetSettled(settled).Save(ctx)
	require.NoError(t, err)
}

// TestDecisionSettled_AllWriters verifies every writer in design-scale §3.1
// maintains decisions.settled accurately and monotonically.
func TestDecisionSettled_AllWriters(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := backfillTestProject(t, cs, "proj-settled-writers")
	aliceID := uuid.NewString()
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	chatKey := uuid.NewString()
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	createPrompt := func(update bool, ds ...messages.Decision) (string, *store.DecisionPromptResult) {
		t.Helper()
		msgID := uuid.NewString()
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: msgID, ProjectID: projectID, Sender: "agent:bot", SenderID: agentID,
				Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID:   agentID,
			AskerAgentName: "bot",
			Update:         update,
			Decisions:      ds,
		})
		require.NoError(t, err)
		return msgID, res
	}

	// 1. CreateDecisionPrompt insert -> settled=false.
	_, res1 := createPrompt(false, messages.Decision{Key: "k-direct", Question: "Direct?", Choices: choices})
	rowDirect := res1.Decisions[0].RowID
	assert.False(t, decisionSettledFlag(t, cs, rowDirect), "new pending row must have settled=false")

	// 2. RecordAnswer direct on pending -> settled=true.
	_, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    rowDirect,
		ChoiceID:         "yes",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "user:" + aliceID, SenderID: aliceID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.True(t, decisionSettledFlag(t, cs, rowDirect), "direct answer on pending must set settled=true")

	// 3. CreateDecisionPrompt repost (MarkReplaced) -> replaced row settled=true, new row settled=false.
	_, resRep1 := createPrompt(false, messages.Decision{Key: "k-repost", Question: "V1?", Choices: choices})
	rowRepV1 := resRep1.Decisions[0].RowID
	assert.False(t, decisionSettledFlag(t, cs, rowRepV1))
	_, resRep2 := createPrompt(true, messages.Decision{Key: "k-repost", Question: "V2?", Choices: choices})
	rowRepV2 := resRep2.Decisions[0].RowID
	assert.True(t, decisionSettledFlag(t, cs, rowRepV1), "repost MarkReplaced must set settled=true on replaced row")
	assert.False(t, decisionSettledFlag(t, cs, rowRepV2), "new repost version must start settled=false")

	// 4. WithdrawDecisions -> settled=true; repost of terminal row (!MarkReplaced) keeps/heals settled=true.
	_, err = cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: agentID,
		Keys:         []string{"k-repost"},
		Reason:       "superseded",
	})
	require.NoError(t, err)
	assert.True(t, decisionSettledFlag(t, cs, rowRepV2), "withdraw must set settled=true")

	// Simulate older-binary withdrawn row with settled=false, then repost (!MarkReplaced) heals it to true.
	forceDecisionSettled(t, cs, false, rowRepV2)
	_, resRep3 := createPrompt(true, messages.Decision{Key: "k-repost", Question: "V3?", Choices: choices})
	assert.True(t, decisionSettledFlag(t, cs, rowRepV2), "repost of terminal row (!MarkReplaced) must set settled=true")
	assert.False(t, decisionSettledFlag(t, cs, resRep3.Decisions[0].RowID))

	// 5. RecordAnswer agent_interpreted -> settled stays false; direct confirm -> settled=true.
	_, resInterp1 := createPrompt(false, messages.Decision{Key: "k-interp-confirm", Question: "Confirm?", Choices: choices})
	rowInterpConfirm := resInterp1.Decisions[0].RowID
	srcMsg1 := uuid.NewString()
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:        rowInterpConfirm,
		ChoiceID:             "yes",
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Provenance:           store.DecisionProvenanceAgentInterpreted,
		InterpretedByAgentID: agentID,
		SourceMessageID:      srcMsg1,
		Surface:              store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "agent:bot", SenderID: agentID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.False(t, decisionSettledFlag(t, cs, rowInterpConfirm), "agent_interpreted answer must leave settled=false")

	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    rowInterpConfirm,
		ChoiceID:         "yes",
		Outcome:          store.DecisionOutcomeConfirm,
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "user:" + aliceID, SenderID: aliceID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.True(t, decisionSettledFlag(t, cs, rowInterpConfirm), "direct confirm over interpreted must set settled=true")

	// 6. RecordInterpretedAnswers -> settled stays false; direct override -> settled=true.
	_, resInterp2 := createPrompt(false, messages.Decision{Key: "k-interp-override", Question: "Override?", Choices: choices})
	rowInterpOverride := resInterp2.Decisions[0].RowID
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentID,
		SourceMessageID:      uuid.NewString(),
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{DecisionRowID: rowInterpOverride, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "agent:bot", SenderID: agentID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.False(t, decisionSettledFlag(t, cs, rowInterpOverride), "RecordInterpretedAnswers must leave settled=false")

	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    rowInterpOverride,
		ChoiceID:         "no",
		Outcome:          store.DecisionOutcomeOverride,
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "user:" + aliceID, SenderID: aliceID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.True(t, decisionSettledFlag(t, cs, rowInterpOverride), "direct override over interpreted must set settled=true")

	// 7. RecordAnswerSet on mixed prompt (pending + interpreted awaiting confirm) -> both settled=true.
	setMsgID, resSet := createPrompt(false,
		messages.Decision{Key: "k-set-1", Question: "Set 1?", Choices: choices},
		messages.Decision{Key: "k-set-2", Question: "Set 2?", Choices: choices},
	)
	rowSet1 := resSet.Decisions[0].RowID
	rowSet2 := resSet.Decisions[1].RowID
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentID,
		SourceMessageID:      uuid.NewString(),
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{DecisionRowID: rowSet2, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "agent:bot", SenderID: agentID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.False(t, decisionSettledFlag(t, cs, rowSet1))
	assert.False(t, decisionSettledFlag(t, cs, rowSet2))

	_, err = cs.RecordAnswerSet(ctx, &store.RecordAnswerSetInput{
		MessageID:        setMsgID,
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Surface:          store.DecisionSurfaceNative,
		Answers: []store.AnswerSetItem{
			{DecisionRowID: rowSet1, ChoiceID: "yes", Outcome: store.DecisionOutcomeAnswer},
			{DecisionRowID: rowSet2, ChoiceID: "yes", Outcome: store.DecisionOutcomeConfirm},
		},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "user:" + aliceID, SenderID: aliceID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.True(t, decisionSettledFlag(t, cs, rowSet1), "RecordAnswerSet pending->decided must set settled=true")
	assert.True(t, decisionSettledFlag(t, cs, rowSet2), "RecordAnswerSet confirm over interpreted must set settled=true")

	// 8. DismissDecisions on both pending and interpreted-awaiting rows -> settled=true.
	dsmMsgID, resDsm := createPrompt(false,
		messages.Decision{Key: "k-dsm-pending", Question: "Dsm 1?", Choices: choices},
		messages.Decision{Key: "k-dsm-interp", Question: "Dsm 2?", Choices: choices},
	)
	rowDsmPending := resDsm.Decisions[0].RowID
	rowDsmInterp := resDsm.Decisions[1].RowID
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentID,
		SourceMessageID:      uuid.NewString(),
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{DecisionRowID: rowDsmInterp, ChoiceID: "no"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "agent:bot", SenderID: agentID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	future := time.Now().UTC().Add(time.Hour)
	_, err = cs.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: dsmMsgID,
		User:      store.DecisionAnswerer{UserID: aliceID, Name: "Alice", Email: "alice@example.com"},
		MinAge:    time.Second,
		Now:       func() time.Time { return future },
		Surface:   store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "user:" + aliceID, SenderID: aliceID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: future,
		},
	})
	require.NoError(t, err)
	assert.True(t, decisionSettledFlag(t, cs, rowDsmPending), "dismiss pending must set settled=true")
	assert.True(t, decisionSettledFlag(t, cs, rowDsmInterp), "dismiss interpreted must set settled=true")

	// 9. Repost (MarkReplaced) of a decided-interpreted row -> v1 replaced + settled=true, v2 pending + settled=false.
	_, resInterpRep1 := createPrompt(false, messages.Decision{Key: "k-interp-replace", Question: "Interp Rep V1?", Choices: choices})
	rowInterpRepV1 := resInterpRep1.Decisions[0].RowID
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentID,
		SourceMessageID:      uuid.NewString(),
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{DecisionRowID: rowInterpRepV1, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: projectID, Sender: "agent:bot", SenderID: agentID,
			Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.False(t, decisionSettledFlag(t, cs, rowInterpRepV1), "decided-interpreted row starts settled=false")
	_, resInterpRep2 := createPrompt(true, messages.Decision{Key: "k-interp-replace", Question: "Interp Rep V2?", Choices: choices})
	rowInterpRepV2 := resInterpRep2.Decisions[0].RowID
	v1After, err := cs.GetDecision(ctx, rowInterpRepV1)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusReplaced, v1After.Status, "repost of decided-interpreted row must mark v1 replaced")
	assert.True(t, decisionSettledFlag(t, cs, rowInterpRepV1), "repost (MarkReplaced) of decided-interpreted row must set settled=true")
	assert.False(t, decisionSettledFlag(t, cs, rowInterpRepV2), "new repost version must start settled=false")
}

// legacyListOpenPromptMessageIDs is the exact pre-P1 oracle query without the
// settled=false hint.
func legacyListOpenPromptMessageIDs(t *testing.T, cs *CompositeStore, projectIDs, chatKeys []string) []string {
	t.Helper()
	projectUUIDs, err := parseProjectIDs(projectIDs)
	require.NoError(t, err)
	keys := cleanChatKeys(chatKeys)
	if len(projectUUIDs) == 0 || len(keys) == 0 {
		return nil
	}
	rows, err := cs.client.Decision.Query().
		Where(
			decision.ProjectIDIn(projectUUIDs...),
			decision.ChatKeyIn(keys...),
			decision.Or(
				decision.StatusEQ(decision.StatusPending),
				decision.And(
					decision.StatusEQ(decision.StatusDecided),
					noDirectDecisionAnswer(),
				),
			),
		).
		Select(decision.FieldMessageID).
		All(context.Background())
	require.NoError(t, err)
	seen := make(map[string]bool, len(rows))
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		id := r.MessageID.String()
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// TestDecisionSettled_ListOpenPromptsDifferential verifies that settled-filtered
// ListOpenPrompts equals the legacy exact predicate across every lifecycle
// state, before/after sweep, and with simulated older-binary rows left
// settled=false (design-scale §7 item 1).
func TestDecisionSettled_ListOpenPromptsDifferential(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	p1 := backfillTestProject(t, cs, "diff-p1")
	p2 := backfillTestProject(t, cs, "diff-p2")
	aliceID := uuid.NewString()
	bobID := uuid.NewString()
	for _, u := range []*store.User{
		{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now().UTC()},
		{ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Status: "active", Created: time.Now().UTC()},
	} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	agent1 := uuid.NewString()
	agent2 := uuid.NewString()
	topic1 := uuid.NewString()
	topic2 := uuid.NewString()
	dmAlice := "dm:agent:" + agent1 + ":user:" + aliceID
	dmBob := "dm:agent:" + agent1 + ":user:" + bobID
	choices := []messages.Choice{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}

	post := func(pid, agID, ck, key string, update bool, other bool) (string, string) {
		msgID := uuid.NewString()
		d := messages.Decision{Key: key, Question: "Q " + key, Choices: choices}
		if other {
			d.Other = &messages.OtherOption{Label: "Other"}
		}
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: msgID, ProjectID: pid, Sender: "agent:bot", SenderID: agID,
				Recipient: "chat", ThreadID: ck, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: agID, AskerAgentName: "bot", Update: update, Decisions: []messages.Decision{d},
		})
		require.NoError(t, err)
		return msgID, res.Decisions[0].RowID
	}

	// Seed every lifecycle across p1, p2, topics, DMs, and NULL chat_key.
	// Pending (open)
	post(p1, agent1, topic1, "k-pending", false, false)
	post(p1, agent1, dmAlice, "k-dm-alice-pending", false, true)
	post(p1, agent1, dmBob, "k-dm-bob-pending", false, false)
	// NULL chat_key pending (must never match any chatKey query)
	post(p1, agent1, "", "k-null-ck", false, false)

	// Decided direct (settled)
	_, rDirect := post(p1, agent1, topic1, "k-direct", false, true)
	_, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rDirect, ChoiceID: messages.ChoiceIDOther, OtherText: "custom",
		AnsweredByUserID: aliceID, AnsweredByName: "Alice", AnsweredByEmail: "alice@example.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: p1, Sender: "user:" + aliceID, SenderID: aliceID,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)

	// Interpreted awaiting confirm via RecordInterpretedAnswers (open)
	_, rInterpOpen := post(p2, agent2, topic2, "k-interp-open", false, false)
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agent2, SourceMessageID: uuid.NewString(),
		AnsweredByUserID: aliceID, AnsweredByName: "Alice", AnsweredByEmail: "alice@example.com",
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: rInterpOpen, ChoiceID: "a"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: p2, Sender: "agent:bot2", SenderID: agent2,
			Recipient: "chat", ThreadID: topic2, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)

	// Interpreted awaiting confirm via RecordAnswer(provenance=agent_interpreted) (open)
	_, rInterpSingle := post(p1, agent1, topic1, "k-interp-single", false, false)
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rInterpSingle, ChoiceID: "a",
		AnsweredByUserID: aliceID, AnsweredByName: "Alice", AnsweredByEmail: "alice@example.com",
		Provenance: store.DecisionProvenanceAgentInterpreted, Surface: store.DecisionSurfaceNative,
		InterpretedByAgentID: agent1, SourceMessageID: uuid.NewString(),
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: p1, Sender: "agent:bot", SenderID: agent1,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assert.False(t, decisionSettledFlag(t, cs, rInterpSingle), "RecordAnswer(agent_interpreted) must leave settled=false")

	// Interpreted confirmed (settled)
	_, rInterpConfirmed := post(p2, agent2, topic2, "k-interp-conf", false, false)
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agent2, SourceMessageID: uuid.NewString(),
		AnsweredByUserID: aliceID, AnsweredByName: "Alice", AnsweredByEmail: "alice@example.com",
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: rInterpConfirmed, ChoiceID: "a"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: p2, Sender: "agent:bot2", SenderID: agent2,
			Recipient: "chat", ThreadID: topic2, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rInterpConfirmed, ChoiceID: "a", Outcome: store.DecisionOutcomeConfirm,
		AnsweredByUserID: aliceID, AnsweredByName: "Alice", AnsweredByEmail: "alice@example.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: p2, Sender: "user:" + aliceID, SenderID: aliceID,
			Recipient: "chat", ThreadID: topic2, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)

	// Withdrawn + re-asked (v1 settled, v2 open)
	post(p1, agent1, topic1, "k-wd", false, false)
	_, err = cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{AskerAgentID: agent1, Keys: []string{"k-wd"}})
	require.NoError(t, err)
	post(p1, agent1, topic1, "k-wd", true, false)

	// Replaced (v1 settled, v2 open)
	post(p2, agent2, topic2, "k-rep", false, false)
	post(p2, agent2, topic2, "k-rep", true, false)

	// Dismissed (settled)
	dsmMsg, _ := post(p1, agent1, topic1, "k-dsm", false, false)
	future := time.Now().UTC().Add(time.Hour)
	_, err = cs.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: dsmMsg, User: store.DecisionAnswerer{UserID: aliceID, Name: "Alice", Email: "alice@example.com"},
		MinAge: time.Second, Now: func() time.Time { return future }, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: p1, Sender: "user:" + aliceID, SenderID: aliceID,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: future,
		},
	})
	require.NoError(t, err)

	subsets := []struct {
		name     string
		projects []string
		keys     []string
	}{
		{"all_alice", []string{p1, p2}, []string{topic1, topic2, dmAlice}},
		{"all_bob", []string{p1, p2}, []string{topic1, topic2, dmBob}},
		{"p1_only", []string{p1}, []string{topic1}},
		{"p2_only", []string{p2}, []string{topic2}},
		{"dm_alice_only", []string{p1}, []string{dmAlice}},
	}

	assertMatchesLegacy := func(phase string) {
		for _, sub := range subsets {
			gotPrompts, gerr := cs.ListOpenPrompts(ctx, sub.projects, sub.keys)
			require.NoError(t, gerr, "phase=%s subset=%s", phase, sub.name)
			gotIDs := make([]string, len(gotPrompts))
			for i, p := range gotPrompts {
				gotIDs[i] = p.MessageID
			}
			sort.Strings(gotIDs)
			wantIDs := legacyListOpenPromptMessageIDs(t, cs, sub.projects, sub.keys)
			assert.Equal(t, wantIDs, gotIDs, "phase=%s subset=%s", phase, sub.name)
		}
	}

	// Phase 1: fresh writes from new binary.
	assertMatchesLegacy("new-binary-writes")

	// Phase 2: simulate older binary leaving all rows settled=false.
	forceDecisionSettled(t, cs, false)
	pending, err := cs.HasPendingDecisionIndexWork(ctx)
	require.NoError(t, err)
	assert.True(t, pending, "older-binary settled rows must be detected as pending work")
	assertMatchesLegacy("simulated-old-binary-pre-sweep")

	// Phase 3: sweep in small pages with keyset cursor until drained.
	totalSwept := 0
	cursor := ""
	for {
		n, nextCursor, serr := cs.SweepSettledDecisionsPage(ctx, cursor, 2)
		require.NoError(t, serr)
		totalSwept += n
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	assert.Equal(t, 5, totalSwept, "5 settled rows (direct, interp-confirmed, withdrawn, replaced, dismissed) must be swept")
	pending, err = cs.HasPendingDecisionIndexWork(ctx)
	require.NoError(t, err)
	assert.False(t, pending, "no pending work after full sweep")
	assertMatchesLegacy("post-sweep")

	// Exact parity check: settled=true rows == settledDecisionPredicate() rows.
	settledHintCount, err := cs.client.Decision.Query().Where(decision.Settled(true)).Count(ctx)
	require.NoError(t, err)
	exactSettledCount, err := cs.client.Decision.Query().Where(settledDecisionPredicate()).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, exactSettledCount, settledHintCount, "settled hint count must match exact settled predicate after sweep")
}

func TestProjectsWithUnsettledDecisions(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	pEmpty := backfillTestProject(t, cs, "unsettled-empty")
	pSettled := backfillTestProject(t, cs, "unsettled-settled")
	pPending := backfillTestProject(t, cs, "unsettled-pending")
	pInterp := backfillTestProject(t, cs, "unsettled-interp")
	pOutsider := backfillTestProject(t, cs, "unsettled-outsider")

	got, err := cs.ProjectsWithUnsettledDecisions(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	_, err = cs.ProjectsWithUnsettledDecisions(ctx, []string{"not-a-uuid"})
	require.ErrorIs(t, err, store.ErrNotFound)

	aliceID := uuid.NewString()
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now().UTC(),
	}))
	agentID := uuid.NewString()
	topic := uuid.NewString()
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	// pSettled: 1 decided-direct row.
	_, rowSettled := seedSearchDecision(t, cs, searchSeed{
		projectID: pSettled, agentID: agentID, agentName: "bot", chatKey: topic, key: "k-s", question: "S?",
	})
	answerSearchDecision(t, cs, pSettled, rowSettled, "yes", aliceID, "Alice", time.Now().UTC())

	// pPending: 1 pending row.
	seedSearchDecision(t, cs, searchSeed{
		projectID: pPending, agentID: agentID, agentName: "bot", chatKey: topic, key: "k-p", question: "P?",
	})

	// pInterp: 1 interpreted-awaiting row.
	_, resI := func() (string, *store.DecisionPromptResult) {
		msgID := uuid.NewString()
		r, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: msgID, ProjectID: pInterp, Sender: "agent:bot", SenderID: agentID,
				Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: agentID, AskerAgentName: "bot",
			Decisions: []messages.Decision{{Key: "k-i", Question: "I?", Choices: choices}},
		})
		require.NoError(t, err)
		return msgID, r
	}()
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentID, SourceMessageID: uuid.NewString(),
		AnsweredByUserID: aliceID, AnsweredByName: "Alice", AnsweredByEmail: "alice@example.com",
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: resI.Decisions[0].RowID, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: pInterp, Sender: "agent:bot", SenderID: agentID,
			Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)

	// pOutsider: 1 pending row (not requested).
	seedSearchDecision(t, cs, searchSeed{
		projectID: pOutsider, agentID: agentID, agentName: "bot", chatKey: topic, key: "k-o", question: "O?",
	})

	got, err = cs.ProjectsWithUnsettledDecisions(ctx, []string{pEmpty, pSettled, pPending, pInterp, pPending})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{pPending: true, pInterp: true}, got)

	// Simulate older binary leaving pSettled row settled=false: superset includes pSettled until swept.
	forceDecisionSettled(t, cs, false, rowSettled)
	got, err = cs.ProjectsWithUnsettledDecisions(ctx, []string{pEmpty, pSettled, pPending, pInterp})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{pSettled: true, pPending: true, pInterp: true}, got)

	n, nextCursor, err := cs.SweepSettledDecisionsPage(ctx, "", 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NotEmpty(t, nextCursor)

	n2, nextCursor2, err := cs.SweepSettledDecisionsPage(ctx, nextCursor, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, n2)
	assert.Empty(t, nextCursor2)

	got, err = cs.ProjectsWithUnsettledDecisions(ctx, []string{pEmpty, pSettled, pPending, pInterp})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{pPending: true, pInterp: true}, got)
}

// TestDecisionSettled_PartialIndexAutoMigrate verifies AutoMigrate creates the
// partial index (project_id, chat_key) WHERE settled = false on a fresh DB and
// restores it on an existing DB on both SQLite and Postgres.
func TestDecisionSettled_PartialIndexAutoMigrate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)

	hasIndex := func() bool {
		t.Helper()
		rows := &entsql.Rows{}
		if client.Driver().Dialect() == dialect.Postgres {
			err := client.Driver().Query(ctx,
				"SELECT indexdef FROM pg_indexes WHERE tablename = 'decisions' AND indexname = $1",
				[]any{decisionSettledPartialIndex}, rows)
			require.NoError(t, err)
			defer func() { _ = rows.Close() }()
			if !rows.Next() {
				return false
			}
			var def string
			require.NoError(t, rows.Scan(&def))
			assert.Contains(t, strings.ToLower(def), "where")
			assert.Contains(t, strings.ToLower(def), "settled")
			return true
		}
		err := client.Driver().Query(ctx,
			"SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'decisions' AND name = ?",
			[]any{decisionSettledPartialIndex}, rows)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			return false
		}
		var def string
		require.NoError(t, rows.Scan(&def))
		assert.Contains(t, strings.ToLower(def), "where")
		assert.Contains(t, strings.ToLower(def), "settled")
		return true
	}

	require.True(t, hasIndex(), "fresh migrated DB must have %s", decisionSettledPartialIndex)
	require.NoError(t, client.Driver().Exec(ctx, "DROP INDEX "+decisionSettledPartialIndex, []any{}, nil))
	require.False(t, hasIndex())
	require.NoError(t, entc.AutoMigrate(ctx, client))
	require.True(t, hasIndex(), "AutoMigrate must recreate %s on an existing DB", decisionSettledPartialIndex)
}

// TestDecisionSettled_QueryPlans pins that ListOpenPrompts (single- and
// multi-project) and ProjectsWithUnsettledDecisions use the settled=false
// partial index on SQLite and Postgres (design-scale §7 item 5).
func TestDecisionSettled_QueryPlans(t *testing.T) {
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	recClient := ent.NewClient(ent.Driver(rec))
	cs := NewCompositeStore(recClient)

	const numProjects = 5
	const rowsPerProject = 100
	projectIDs := make([]string, numProjects)
	allKeys := make([]string, 0, numProjects*2)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	baseTime := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	tx, err := base.Tx(ctx)
	require.NoError(t, err)
	for p := 0; p < numProjects; p++ {
		pid := uuid.New()
		projectIDs[p] = pid.String()
		k1 := fmt.Sprintf("topic-%d-a", p)
		k2 := fmt.Sprintf("topic-%d-b", p)
		allKeys = append(allKeys, k1, k2)
		agID := uuid.New()
		creates := make([]*ent.DecisionCreate, 0, rowsPerProject)
		for j := 0; j < rowsPerProject; j++ {
			ck := k1
			if j%2 == 1 {
				ck = k2
			}
			isOpen := j == rowsPerProject-1 && p < 2
			dc := tx.Decision.Create().
				SetProjectID(pid).
				SetChatKey(ck).
				SetMessageID(uuid.New()).
				SetPosition(0).
				SetAskerAgentID(agID).
				SetAskerAgentName("bot").
				SetKey(fmt.Sprintf("k-%d-%d", p, j)).
				SetKeyNorm(fmt.Sprintf("k-%d-%d", p, j)).
				SetKeySource(decision.KeySourceAgent).
				SetVersion(1).
				SetQuestion("Q?").
				SetChoices(choices).
				SetCreatedAt(baseTime.Add(time.Duration(p*1000+j) * time.Second))
			if isOpen {
				dc.SetStatus(decision.StatusPending).SetSettled(false)
			} else {
				dc.SetStatus(decision.StatusWithdrawn).SetSettled(true).SetWithdrawnAt(baseTime)
			}
			creates = append(creates, dc)
		}
		require.NoError(t, tx.Decision.CreateBulk(creates...).Exec(ctx))
	}
	require.NoError(t, tx.Commit())

	explain := func(t *testing.T, q recordedQuery) string {
		t.Helper()
		rows := &entsql.Rows{}
		if base.Driver().Dialect() == dialect.Postgres {
			require.NoError(t, base.Driver().Query(ctx, "EXPLAIN "+q.sql, q.args, rows))
			var lines []string
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				lines = append(lines, line)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			return strings.Join(lines, "\n")
		}
		require.NoError(t, base.Driver().Query(ctx, "EXPLAIN QUERY PLAN "+q.sql, q.args, rows))
		var lines []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			lines = append(lines, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		return strings.Join(lines, "\n")
	}

	assertUsesPartialIndex := func(t *testing.T, label string, run func()) {
		t.Helper()
		rec.mu.Lock()
		rec.queries = nil
		rec.mu.Unlock()

		run()

		rec.mu.Lock()
		queries := append([]recordedQuery(nil), rec.queries...)
		rec.mu.Unlock()
		require.NotEmpty(t, queries, "%s must emit at least one query", label)

		// The first query is the decisions lookup using settled = false.
		q := queries[0]
		plan := explain(t, q)
		t.Logf("%s (%s) SQL: %s\nPLAN:\n%s", label, base.Driver().Dialect(), q.sql, plan)
		assert.Regexp(t, `\b`+decisionSettledPartialIndex+`\b`, plan, "%s must use partial index %s; plan:\n%s", label, decisionSettledPartialIndex, plan)
		if base.Driver().Dialect() == dialect.SQLite {
			for _, line := range strings.Split(plan, "\n") {
				if strings.HasPrefix(line, "SCAN decisions") {
					assert.Regexp(t, `\b`+decisionSettledPartialIndex+`\b`, line, "%s must not full-scan decisions: %s", label, line)
				}
			}
		}
	}

	// Verify the keyset-cursor indexer page query BEFORE ANALYZE (matching
	// production SQLite where the hub never runs ANALYZE): it must seek via
	// the PK autoindex (id>?) or the partial index, never full-scan decisions.
	t.Run("SweepSettledDecisionsPage_keyset_cursor_no_analyze", func(t *testing.T) {
		rec.mu.Lock()
		rec.queries = nil
		rec.mu.Unlock()

		cursorID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
		_, err := cs.selectDecisionIDsForIndexPage(ctx, cursorID, 50, decisionSettledFalse(), settledDecisionPredicate())
		require.NoError(t, err)

		rec.mu.Lock()
		queries := append([]recordedQuery(nil), rec.queries...)
		rec.mu.Unlock()
		require.NotEmpty(t, queries)
		q := queries[0]
		plan := explain(t, q)
		t.Logf("selectDecisionIDsForIndexPage (%s, pre-ANALYZE) SQL: %s\nPLAN:\n%s", base.Driver().Dialect(), q.sql, plan)
		if base.Driver().Dialect() == dialect.SQLite {
			assert.Contains(t, plan, "SEARCH decisions USING", "keyset page query must range-seek decisions without ANALYZE; plan:\n%s", plan)
			assert.NotContains(t, plan, "SCAN decisions", "keyset page query must not full-scan decisions without ANALYZE; plan:\n%s", plan)
		} else {
			assert.Regexp(t, `Index (Only )?Scan|Bitmap Index Scan`, plan, "Postgres keyset page query must use an index scan; plan:\n%s", plan)
		}
	})

	require.NoError(t, base.Driver().Exec(ctx, "ANALYZE", []any{}, nil))

	t.Run("ListOpenPrompts_single_project", func(t *testing.T) {
		assertUsesPartialIndex(t, "ListOpenPrompts(single)", func() {
			got, err := cs.ListOpenPrompts(ctx, projectIDs[:1], allKeys[:2])
			require.NoError(t, err)
			require.Len(t, got, 1)
		})
	})

	t.Run("ListOpenPrompts_multi_project", func(t *testing.T) {
		assertUsesPartialIndex(t, "ListOpenPrompts(multi)", func() {
			got, err := cs.ListOpenPrompts(ctx, projectIDs, allKeys)
			require.NoError(t, err)
			require.Len(t, got, 2)
		})
	})

	t.Run("ProjectsWithUnsettledDecisions", func(t *testing.T) {
		assertUsesPartialIndex(t, "ProjectsWithUnsettledDecisions", func() {
			got, err := cs.ProjectsWithUnsettledDecisions(ctx, projectIDs)
			require.NoError(t, err)
			require.Len(t, got, 2)
		})
	})
}

// TestDecisionIndex_PostgresConcurrentSweepAndRepost verifies that two
// concurrent settled sweepers over overlapping pages plus a concurrent repost
// of terminal keys (which takes FOR UPDATE locks in loadKeyStates) never
// deadlock (SQLSTATE 40P01) on Postgres and converge to exact settled parity.
func TestDecisionIndex_PostgresConcurrentSweepAndRepost(t *testing.T) {
	cs := newTestCompositeStore(t)
	if !cs.isPostgres() {
		t.Skip("Postgres-only concurrency/deadlock test (requires FOR UPDATE SKIP LOCKED)")
	}
	ctx := context.Background()

	projectID := backfillTestProject(t, cs, "pg-conc-sweep")
	agentID := uuid.NewString()
	chatKey := uuid.NewString()
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	baseTime := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

	// Seed 240 withdrawn decisions forced to settled=false (overlapping sweep pages).
	pUUID := uuid.MustParse(projectID)
	aUUID := uuid.MustParse(agentID)
	creates := make([]*ent.DecisionCreate, 0, 240)
	for i := 0; i < 240; i++ {
		key := fmt.Sprintf("bulk-%04d", i)
		creates = append(creates, cs.client.Decision.Create().
			SetProjectID(pUUID).
			SetChatKey(chatKey).
			SetMessageID(uuid.New()).
			SetPosition(0).
			SetAskerAgentID(aUUID).
			SetAskerAgentName("bot").
			SetKey(key).
			SetKeyNorm(key).
			SetKeySource(decision.KeySourceAgent).
			SetVersion(1).
			SetQuestion("Bulk?").
			SetChoices(choices).
			SetStatus(decision.StatusWithdrawn).
			SetSettled(false).
			SetCreatedAt(baseTime.Add(time.Duration(i)*time.Second)).
			SetWithdrawnAt(baseTime))
	}
	require.NoError(t, cs.client.Decision.CreateBulk(creates...).Exec(ctx))

	// Seed 12 terminal (withdrawn) keys that a concurrent writer will repost
	// with Update: true (loadKeyStates FOR UPDATE) while sweepers are running.
	repostKeys := make([]string, 12)
	for i := range repostKeys {
		k := fmt.Sprintf("term-repost-%02d", i)
		repostKeys[i] = k
		_, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: uuid.NewString(), ProjectID: projectID, Sender: "agent:bot", SenderID: agentID,
				Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: agentID, AskerAgentName: "bot",
			Decisions: []messages.Decision{{Key: k, Question: "V1?", Choices: choices}},
		})
		require.NoError(t, err)
		_, err = cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
			AskerAgentID: agentID, Keys: []string{k},
		})
		require.NoError(t, err)
	}
	// Reset all rows to settled=false to simulate older-binary state before rollout.
	forceDecisionSettled(t, cs, false)

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	// Two concurrent sweepers draining overlapping pages of 25 rows using
	// keyset cursors (continuing across short SKIP LOCKED pages until empty).
	for s := 0; s < 2; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cursor := ""
			for {
				_, nextCursor, err := cs.SweepSettledDecisionsPage(ctx, cursor, 25)
				if err != nil {
					errCh <- fmt.Errorf("sweeper error: %w", err)
					return
				}
				if nextCursor == "" {
					return
				}
				cursor = nextCursor
			}
		}()
	}

	// One concurrent writer reposting all 12 terminal keys with Update: true.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, k := range repostKeys {
			_, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
				Message: &store.Message{
					ID: uuid.NewString(), ProjectID: projectID, Sender: "agent:bot", SenderID: agentID,
					Recipient: "chat", ThreadID: chatKey, Channel: "web", CreatedAt: time.Now().UTC(),
				},
				AskerAgentID: agentID, AskerAgentName: "bot", Update: true,
				Decisions: []messages.Decision{{Key: k, Question: "V2?", Choices: choices}},
			})
			if err != nil {
				errCh <- fmt.Errorf("repost %s error: %w", k, err)
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	// Drain any row skipped via SKIP LOCKED while the repost held its lock
	// (fresh pass from cursor "").
	cursor := ""
	for {
		_, nextCursor, err := cs.SweepSettledDecisionsPage(ctx, cursor, 50)
		require.NoError(t, err)
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	pending, err := cs.HasPendingDecisionIndexWork(ctx)
	require.NoError(t, err)
	assert.False(t, pending, "no pending settled work after concurrent sweep + repost")

	settledHint, err := cs.client.Decision.Query().Where(decision.Settled(true)).Count(ctx)
	require.NoError(t, err)
	settledExact, err := cs.client.Decision.Query().Where(settledDecisionPredicate()).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, settledExact, settledHint, "final settled hint count must equal exact settled predicate")
	assert.Equal(t, 240+len(repostKeys), settledHint, "all 240 bulk + 12 terminal v1 rows must be settled")
}

// TestDecisionIndex_LinearBackfillPerPageCost verifies that sweeping a large
// backlog with the keyset cursor maintains flat (non-quadratic) per-page cost
// from the first pages to the last pages on SQLite without ANALYZE.
func TestDecisionIndex_LinearBackfillPerPageCost(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := backfillTestProject(t, cs, "linear-backfill")
	pUUID := uuid.MustParse(projectID)
	aUUID := uuid.New()
	chatKey := uuid.NewString()
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	baseTime := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

	const totalRows = 10000
	const batchSize = 500
	for off := 0; off < totalRows; off += batchSize {
		creates := make([]*ent.DecisionCreate, 0, batchSize)
		for j := 0; j < batchSize; j++ {
			k := fmt.Sprintf("lin-%05d", off+j)
			creates = append(creates, cs.client.Decision.Create().
				SetProjectID(pUUID).
				SetChatKey(chatKey).
				SetMessageID(uuid.New()).
				SetPosition(0).
				SetAskerAgentID(aUUID).
				SetAskerAgentName("bot").
				SetKey(k).
				SetKeyNorm(k).
				SetKeySource(decision.KeySourceAgent).
				SetVersion(1).
				SetQuestion("Linear?").
				SetChoices(choices).
				SetStatus(decision.StatusWithdrawn).
				SetSettled(false).
				SetCreatedAt(baseTime.Add(time.Duration(off+j)*time.Second)).
				SetWithdrawnAt(baseTime))
		}
		require.NoError(t, cs.client.Decision.CreateBulk(creates...).Exec(ctx))
	}

	var pageDurs []time.Duration
	totalSwept := 0
	cursor := ""
	for {
		start := time.Now()
		n, nextCursor, err := cs.SweepSettledDecisionsPage(ctx, cursor, DefaultDecisionIndexPageSize)
		dur := time.Since(start)
		require.NoError(t, err)
		if nextCursor == "" {
			break
		}
		pageDurs = append(pageDurs, dur)
		totalSwept += n
		cursor = nextCursor
	}

	require.Equal(t, totalRows, totalSwept)
	require.Equal(t, totalRows/DefaultDecisionIndexPageSize, len(pageDurs))

	median := func(ds []time.Duration) time.Duration {
		cp := append([]time.Duration(nil), ds...)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		return cp[len(cp)/2]
	}
	firstMed := median(pageDurs[:5])
	lastMed := median(pageDurs[len(pageDurs)-5:])
	worst := pageDurs[0]
	for _, d := range pageDurs[1:] {
		if d > worst {
			worst = d
		}
	}
	t.Logf("[%s] 10k backfill: %d pages, first-5 med=%v, last-5 med=%v, worst=%v",
		cs.Dialect(), len(pageDurs), firstMed, lastMed, worst)
	assert.LessOrEqual(t, lastMed, firstMed*3+15*time.Millisecond,
		"last-5 median page duration (%v) must stay linear vs first-5 (%v)", lastMed, firstMed)
}
