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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectDecisionStore_OpenPromptsAndReadableChatKeys(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "project-decision-open-prompts")
	otherProjectID := backfillTestProject(t, cs, "project-decision-other")

	agent1 := uuid.New().String()
	agent2 := uuid.New().String()
	aliceID := uuid.New().String()
	bobID := uuid.New().String()
	for _, u := range []*store.User{
		{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now()},
		{ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Status: "active", Created: time.Now()},
	} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	topicRelease := "topic-release"
	dmAlice := "dm:agent:" + agent1 + ":user:" + aliceID
	dmBob := "dm:agent:" + agent2 + ":user:" + bobID

	// 1. Multi-decision set prompt in #release where D1 has an interpreted answer
	// (still open awaiting confirm/override) and D2 is pending.
	setMsgID := uuid.New().String()
	setRes, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID:        setMsgID,
			ProjectID: projectID,
			Sender:    "agent:release-bot",
			SenderID:  agent1,
			Recipient: "group:topic",
			ThreadID:  topicRelease,
		},
		AskerAgentID:   agent1,
		AskerAgentName: "release-bot",
		Decisions: []messages.Decision{
			{Key: "D1", Question: "Cut RC?", Choices: []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}},
			{Key: "D2", Question: "Tag main?", Choices: []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}},
		},
	})
	require.NoError(t, err)
	replyMsgID := uuid.New().String()
	require.NoError(t, cs.CreateMessage(ctx, &store.Message{
		ID:        replyMsgID,
		ProjectID: projectID,
		Sender:    "user:alice@example.com",
		SenderID:  aliceID,
		Recipient: "group:topic",
		ThreadID:  topicRelease,
		Msg:       "yes to RC",
	}))
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agent1,
		SourceMessageID:      replyMsgID,
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{
			{Key: "D1", ChoiceID: "yes"},
		},
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "agent:release-bot",
			SenderID:  agent1,
			Recipient: "group:topic",
			ThreadID:  topicRelease,
			Msg:       "Interpreted D1=Yes",
		},
	})
	require.NoError(t, err)

	// 2. Settled direct-answered prompt in #release -> must NOT appear in open prompts.
	_, settledRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agent1, topicRelease, "SETTLED-1")
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    settledRowID,
		ChoiceID:         "yes",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:alice@example.com",
			Recipient: "agent:release-bot",
			Msg:       "Yes",
		},
	})
	require.NoError(t, err)

	// 3. Alice's private DM prompt, then purge its prompt row from messages!
	aliceDMMsgID, _ := seedDecisionPromptForBackfillTest(t, cs, projectID, agent1, dmAlice, "DM-ALICE")
	_, err = cs.DB().ExecContext(ctx, rebindForDialect(cs.Dialect(), "DELETE FROM messages WHERE id = ?"), aliceDMMsgID)
	require.NoError(t, err)

	// 4. Bob's private DM prompt (from agent2) + another project's prompt.
	_, _ = seedDecisionPromptForBackfillTest(t, cs, projectID, agent2, dmBob, "DM-BOB")
	_, _ = seedDecisionPromptForBackfillTest(t, cs, otherProjectID, agent1, topicRelease, "OTHER-PROJ")

	// Empty chatKeys returns nil without error.
	emptyPrompts, err := cs.ListOpenPrompts(ctx, []string{projectID}, nil)
	require.NoError(t, err)
	assert.Empty(t, emptyPrompts)

	// Bob's readable keys (topicRelease + dmBob) must NEVER return Alice's DM!
	bobPrompts, err := cs.ListOpenPrompts(ctx, []string{projectID}, []string{topicRelease, dmBob})
	require.NoError(t, err)
	require.Len(t, bobPrompts, 2)
	for _, p := range bobPrompts {
		assert.NotEqual(t, dmAlice, p.ChatKey, "Bob must never see Alice's DM prompt")
	}

	// Alice's readable keys (topicRelease + dmAlice) return both, with purged DM marked PromptExistsInMessages=false!
	alicePrompts, err := cs.ListOpenPrompts(ctx, []string{projectID}, []string{topicRelease, dmAlice})
	require.NoError(t, err)
	require.Len(t, alicePrompts, 2)
	byMsg := map[string]*store.ProjectDecisionPrompt{}
	for _, p := range alicePrompts {
		byMsg[p.MessageID] = p
	}
	require.Contains(t, byMsg, setMsgID)
	assert.True(t, byMsg[setMsgID].PromptExistsInMessages)
	require.Len(t, byMsg[setMsgID].Decisions, 2)
	assert.Equal(t, setRes.Decisions[0].RowID, byMsg[setMsgID].Decisions[0].ID)
	require.Len(t, byMsg[setMsgID].Decisions[0].Answers, 1)

	require.Contains(t, byMsg, aliceDMMsgID)
	assert.False(t, byMsg[aliceDMMsgID].PromptExistsInMessages, "purged prompt must report PromptExistsInMessages=false")

	// ListProjectDecisionAgents returns both agent1 and agent2 in projectID.
	allAgents, err := cs.ListProjectDecisionAgents(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, allAgents, 2)
}

func TestProjectDecisionStore_AnswersPaginationFiltersAndFacets(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "project-decision-made-log")

	agentRelease := uuid.New().String()
	agentSecretDM := uuid.New().String()
	aliceID := uuid.New().String()
	bobID := uuid.New().String()
	for _, u := range []*store.User{
		{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now()},
		{ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Status: "active", Created: time.Now()},
	} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	topicRelease := "topic-release"
	dmAlice := "dm:agent:" + agentSecretDM + ":user:" + aliceID

	// Entry 1 (interpreted) + Entry 2 (override from Yes -> No) on D9 in #release.
	_, d9RowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentRelease, topicRelease, "D9")
	sourceReplyID := uuid.New().String()
	require.NoError(t, cs.CreateMessage(ctx, &store.Message{
		ID:        sourceReplyID,
		ProjectID: projectID,
		Sender:    "user:alice@example.com",
		SenderID:  aliceID,
		Recipient: "group:topic",
		ThreadID:  topicRelease,
		Msg:       "looks good, yes",
	}))
	interpRes, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentRelease,
		SourceMessageID:      sourceReplyID,
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceSlack,
		Answers:              []store.InterpretedAnswerPair{{Key: "D9", ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "agent:release-bot",
			Recipient: "group:topic",
			ThreadID:  topicRelease,
			Msg:       "Interpreted Yes",
		},
	})
	require.NoError(t, err)
	require.Len(t, interpRes.Answers, 1)

	overrideAns, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    d9RowID,
		ChoiceID:         "no",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Provenance:       store.DecisionProvenanceDirect,
		Outcome:          store.DecisionOutcomeOverride,
		Surface:          store.DecisionSurfaceDecisionsPage,
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:alice@example.com",
			Recipient: "agent:release-bot",
			ThreadID:  topicRelease,
			Msg:       "Actually No",
		},
	})
	require.NoError(t, err)

	// Entry 3: direct answer by Bob in #release.
	_, d10RowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentRelease, topicRelease, "D10")
	directAns, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    d10RowID,
		ChoiceID:         "yes",
		AnsweredByUserID: bobID,
		AnsweredByName:   "Bob",
		AnsweredByEmail:  "bob@example.com",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:bob@example.com",
			Recipient: "agent:release-bot",
			ThreadID:  topicRelease,
			Msg:       "Yes",
		},
	})
	require.NoError(t, err)

	// Entry 4: unconfirmed interpreted answer by Alice on D11 in #release (stays in Made with AwaitingConfirmation=true).
	_, _ = seedDecisionPromptForBackfillTest(t, cs, projectID, agentRelease, topicRelease, "D11")
	unconfReplyID := uuid.New().String()
	require.NoError(t, cs.CreateMessage(ctx, &store.Message{
		ID:        unconfReplyID,
		ProjectID: projectID,
		Sender:    "user:alice@example.com",
		SenderID:  aliceID,
		Recipient: "group:topic",
		ThreadID:  topicRelease,
		Msg:       "D11 is no",
	}))
	unconfInterpRes, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentRelease,
		SourceMessageID:      unconfReplyID,
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceSlack,
		Answers:              []store.InterpretedAnswerPair{{Key: "D11", ChoiceID: "no"}},
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "agent:release-bot",
			Recipient: "group:topic",
			ThreadID:  topicRelease,
			Msg:       "Interpreted D11 No",
		},
	})
	require.NoError(t, err)
	require.Len(t, unconfInterpRes.Answers, 1)

	// Entry 5: Alice's private DM answer with agentSecretDM (must NEVER appear in Bob's list or facets!).
	_, dmRowID := seedDecisionPromptForBackfillTest(t, cs, projectID, agentSecretDM, dmAlice, "DM-SECRET")
	dmAns, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    dmRowID,
		ChoiceID:         "yes",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceDecisionsPage,
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:alice@example.com",
			Recipient: "agent:secret-bot",
			ThreadID:  dmAlice,
			Msg:       "Yes",
		},
	})
	require.NoError(t, err)

	// Force deterministic timestamps for ordering + date-range tests.
	t0 := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	t4 := time.Date(2026, 11, 13, 16, 0, 0, 0, time.UTC)
	for id, ts := range map[string]time.Time{
		unconfInterpRes.Answers[0].ID: t0,
		interpRes.Answers[0].ID:       t1,
		overrideAns.ID:                t2,
		directAns.ID:                  t3,
		dmAns.ID:                      t4,
	} {
		_, err := cs.DB().ExecContext(ctx,
			rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET created_at = ? WHERE id = ?"),
			ts, id)
		require.NoError(t, err)
	}

	// 1. Bob's facets over [topicRelease] must NOT include agentSecretDM!
	bobAgents, bobAnswerers, bobConvKeys, err := cs.ListDecisionFacets(ctx, []string{projectID}, []string{topicRelease})
	require.NoError(t, err)
	require.Len(t, bobAgents, 1)
	assert.Equal(t, agentRelease, bobAgents[0].ID)
	assert.Len(t, bobAnswerers, 2) // Alice and Bob both answered in #release
	assert.Equal(t, []string{topicRelease}, bobConvKeys)

	// 2. Paginate Bob's Made log 1 entry at a time across all 3 non-superseded entries
	// (D9's superseded interpreted answer is folded into overrideAns, not listed separately).
	var gotIDs []string
	cur := ""
	for page := 0; page < 5; page++ {
		items, next, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicRelease}, store.ProjectDecisionAnswerFilter{}, cur, 1)
		require.NoError(t, err)
		for _, it := range items {
			gotIDs = append(gotIDs, it.Answer.ID)
		}
		if next == "" {
			break
		}
		cur = next
	}
	assert.Equal(t, []string{directAns.ID, overrideAns.ID, unconfInterpRes.Answers[0].ID}, gotIDs)

	// 3. Check PreviousChoiceLabel + InterpretedAnswer on override entry, and AwaitingConfirmation + SourceExistsInMessages on unconfirmed interpreted entry.
	overriddenEntries, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicRelease}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceOverridden,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, overriddenEntries, 1)
	assert.Equal(t, overrideAns.ID, overriddenEntries[0].Answer.ID)
	assert.Equal(t, "Yes", overriddenEntries[0].PreviousChoiceLabel)
	require.NotNil(t, overriddenEntries[0].InterpretedAnswer)
	assert.Equal(t, interpRes.Answers[0].ID, overriddenEntries[0].InterpretedAnswer.ID)
	assert.Equal(t, "yes", overriddenEntries[0].InterpretedAnswer.ChoiceID)
	assert.Equal(t, "Yes", overriddenEntries[0].InterpretedAnswer.ChoiceLabel)
	assert.False(t, overriddenEntries[0].AwaitingConfirmation)
	assert.Equal(t, store.DecisionSurfaceDecisionsPage, overriddenEntries[0].Answer.Surface)

	interpEntries, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicRelease}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceInterpreted,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, interpEntries, 1, "superseded D9 interpreted row is excluded; only unconfirmed D11 remains")
	assert.Equal(t, unconfInterpRes.Answers[0].ID, interpEntries[0].Answer.ID)
	assert.True(t, interpEntries[0].AwaitingConfirmation)
	assert.Nil(t, interpEntries[0].InterpretedAnswer)
	assert.True(t, interpEntries[0].SourceExistsInMessages)

	// Purge source reply -> SourceExistsInMessages flips to false!
	_, err = cs.DB().ExecContext(ctx, rebindForDialect(cs.Dialect(), "DELETE FROM messages WHERE id = ?"), unconfReplyID)
	require.NoError(t, err)
	interpAfterPurge, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicRelease}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceInterpreted,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, interpAfterPurge, 1)
	assert.False(t, interpAfterPurge[0].SourceExistsInMessages)

	// 4. Combinable filters: user + date range + agent.
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 12, 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
	filtered, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicRelease, dmAlice}, store.ProjectDecisionAnswerFilter{
		AgentID: agentRelease,
		UserID:  bobID,
		From:    &from,
		To:      &to,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	assert.Equal(t, directAns.ID, filtered[0].Answer.ID)

	// 5. limit=0 returns empty slice and no cursor while still validating bad inputs.
	zeroItems, zeroNext, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicRelease}, store.ProjectDecisionAnswerFilter{}, "", 0)
	require.NoError(t, err)
	assert.Empty(t, zeroItems)
	assert.Empty(t, zeroNext)

	_, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicRelease}, store.ProjectDecisionAnswerFilter{
		Provenance: "bogus",
	}, "", 0)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	// 6. Equal-timestamp cursor tie-break: set all answers to identical created_at
	// and paginate with limit=1 across [topicRelease, dmAlice] -> strictly descending IDs,
	// zero duplicates or skips across the 4 non-superseded entries.
	sameTS := time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)
	nonSupersededAnsIDs := []string{unconfInterpRes.Answers[0].ID, overrideAns.ID, directAns.ID, dmAns.ID}
	for _, id := range append([]string{interpRes.Answers[0].ID}, nonSupersededAnsIDs...) {
		_, err := cs.DB().ExecContext(ctx,
			rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET created_at = ? WHERE id = ?"),
			sameTS, id)
		require.NoError(t, err)
	}

	var tieGotIDs []string
	tieCur := ""
	for page := 0; page < 6; page++ {
		items, next, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicRelease, dmAlice}, store.ProjectDecisionAnswerFilter{}, tieCur, 1)
		require.NoError(t, err)
		for _, it := range items {
			tieGotIDs = append(tieGotIDs, it.Answer.ID)
		}
		if next == "" {
			break
		}
		tieCur = next
	}
	require.Len(t, tieGotIDs, 4)
	for i := 1; i < len(tieGotIDs); i++ {
		assert.Greater(t, tieGotIDs[i-1], tieGotIDs[i], "equal created_at must tie-break by id DESC")
	}
	assert.ElementsMatch(t, nonSupersededAnsIDs, tieGotIDs)
}

func TestProjectDecisionStore_MadeFoldConfirmDismissReplaceAndFacets(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "project-decision-made-fold")

	agentID := uuid.New().String()
	aliceID := uuid.New().String()
	bobID := uuid.New().String()
	charlieID := uuid.New().String()
	for _, u := range []*store.User{
		{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now().UTC()},
		{ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Status: "active", Created: time.Now().UTC()},
		{ID: charlieID, Email: "charlie@example.com", DisplayName: "Charlie", Status: "active", Created: time.Now().UTC()},
	} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	topic := "topic-fold"
	base := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	seedPrompt := func(key, question, defaultChoice string, update bool) (string, string) {
		msgID := uuid.New().String()
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID:        msgID,
				ProjectID: projectID,
				Sender:    "agent:fold-bot",
				SenderID:  agentID,
				Recipient: "group:topic",
				ThreadID:  topic,
				CreatedAt: base.Add(-10 * time.Minute),
			},
			AskerAgentID:   agentID,
			AskerAgentName: "fold-bot",
			Update:         update,
			Decisions: []messages.Decision{{
				Key:             key,
				Question:        question,
				DefaultChoiceID: defaultChoice,
				Choices: []messages.Choice{
					{ID: "pg", Label: "Postgres", Description: "Managed Cloud SQL"},
					{ID: "sqlite", Label: "SQLite", Description: "Embedded single-node"},
				},
			}},
		})
		require.NoError(t, err)
		_, err = cs.DB().ExecContext(ctx,
			rebindForDialect(cs.Dialect(), "UPDATE decisions SET created_at = ? WHERE id = ?"),
			base.Add(-10*time.Minute), res.Decisions[0].RowID)
		require.NoError(t, err)
		return msgID, res.Decisions[0].RowID
	}

	recordInterpreted := func(key, choiceID, userID, userName string) *store.DecisionAnswer {
		replyID := uuid.New().String()
		require.NoError(t, cs.CreateMessage(ctx, &store.Message{
			ID: replyID, ProjectID: projectID, Sender: "user:" + userID, SenderID: userID,
			Recipient: "group:topic", ThreadID: topic, Msg: choiceID, CreatedAt: base.Add(-8 * time.Minute),
		}))
		res, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
			InterpretedByAgentID: agentID,
			SourceMessageID:      replyID,
			AnsweredByUserID:     userID,
			AnsweredByName:       userName,
			Surface:              store.DecisionSurfaceNative,
			Answers:              []store.InterpretedAnswerPair{{Key: key, ChoiceID: choiceID}},
			ResponseMessage: &store.Message{
				ID: uuid.New().String(), ProjectID: projectID, Sender: "agent:fold-bot", SenderID: agentID,
				Recipient: "group:topic", ThreadID: topic, Msg: "interpreted", CreatedAt: base.Add(-7 * time.Minute),
			},
		})
		require.NoError(t, err)
		require.Len(t, res.Answers, 1)
		return res.Answers[0]
	}

	// (a) Plain direct answer on K-PLAIN.
	_, plainRowID := seedPrompt("K-PLAIN", "Which DB for prod?", "pg", false)
	plainAns, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    plainRowID,
		ChoiceID:         "pg",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceDecisionsPage,
		ResponseMessage: &store.Message{
			ID: uuid.New().String(), ProjectID: projectID, Sender: "user:alice", Recipient: "agent:fold-bot", Msg: "pg",
		},
	})
	require.NoError(t, err)

	// (b) Interpreted answer later confirmed by Alice on K-CONF.
	_, confRowID := seedPrompt("K-CONF", "Confirm Postgres?", "pg", false)
	confInterp := recordInterpreted("K-CONF", "pg", aliceID, "Alice")
	confAns, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    confRowID,
		ChoiceID:         "pg",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		Provenance:       store.DecisionProvenanceDirect,
		Outcome:          store.DecisionOutcomeConfirm,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.New().String(), ProjectID: projectID, Sender: "user:alice", Recipient: "agent:fold-bot", Msg: "confirm pg",
		},
	})
	require.NoError(t, err)

	// (c) Interpreted answer later dismissed by Bob on K-DSM.
	dsmMsgID, dsmRowID := seedPrompt("K-DSM", "Dismiss stale choice?", "pg", false)
	dsmInterp := recordInterpreted("K-DSM", "sqlite", bobID, "Bob")
	dsmRes, err := cs.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: dsmMsgID,
		RowIDs:    []string{dsmRowID},
		User:      store.DecisionAnswerer{UserID: bobID, Name: "Bob", Email: "bob@example.com"},
		Reason:    "Superseded by architecture review",
		Surface:   store.DecisionSurfaceDecisionsPage,
		Now:       func() time.Time { return base },
		ResponseMessage: &store.Message{
			ID: uuid.New().String(), ProjectID: projectID, Sender: "user:bob", Recipient: "agent:fold-bot", ThreadID: topic, Msg: "dismissed",
		},
	})
	require.NoError(t, err)
	require.Len(t, dsmRes.Dismissed, 1)

	// (d) Unconfirmed interpreted answer on active K-UNCONF (awaiting confirmation).
	_, _ = seedPrompt("K-UNCONF", "Still waiting confirmation?", "pg", false)
	unconfInterp := recordInterpreted("K-UNCONF", "sqlite", aliceID, "Alice")

	// (e) Unconfirmed interpreted answer on K-REPL v1 that is then replaced by --update (v2 answered directly).
	_, replV1RowID := seedPrompt("K-REPL", "Replaced v1?", "pg", false)
	replV1Interp := recordInterpreted("K-REPL", "pg", aliceID, "Alice")
	_, replV2RowID := seedPrompt("K-REPL", "Replaced v2?", "sqlite", true)
	replV2Ans, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    replV2RowID,
		ChoiceID:         "sqlite",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.New().String(), ProjectID: projectID, Sender: "user:alice", Recipient: "agent:fold-bot", Msg: "sqlite",
		},
	})
	require.NoError(t, err)

	// (f) Facet exclusion check: Charlie only appears on a superseded agent_interpreted row
	// whose direct sibling had its user cleared (simulating user deletion or divergence on the direct row).
	_, facetRowID := seedPrompt("K-FACET", "Facet check?", "pg", false)
	_ = recordInterpreted("K-FACET", "pg", charlieID, "Charlie")
	facetDirectAns, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    facetRowID,
		ChoiceID:         "pg",
		AnsweredByUserID: charlieID,
		AnsweredByName:   "Charlie",
		Provenance:       store.DecisionProvenanceDirect,
		Outcome:          store.DecisionOutcomeConfirm,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.New().String(), ProjectID: projectID, Sender: "user:charlie", Recipient: "agent:fold-bot", Msg: "confirm",
		},
	})
	require.NoError(t, err)
	_, err = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET answered_by_user_id = NULL WHERE id = ?"),
		facetDirectAns.ID)
	require.NoError(t, err)

	// Verify Made list contains exactly 1 entry per decision version (7 versions total:
	// K-PLAIN, K-CONF, K-DSM, K-UNCONF, K-REPL v1, K-REPL v2, K-FACET) and ZERO superseded interpreted rows!
	entries, next, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topic}, store.ProjectDecisionAnswerFilter{}, "", 50)
	require.NoError(t, err)
	assert.Empty(t, next)
	require.Len(t, entries, 7)

	byAnswerID := make(map[string]*store.ProjectDecisionAnswerEntry, len(entries))
	for _, e := range entries {
		byAnswerID[e.Answer.ID] = e
		// Loaded Decision carries Choices, DefaultChoiceID, Status, ReplacedByID.
		require.Len(t, e.Decision.Choices, 2)
		assert.Equal(t, "Managed Cloud SQL", e.Decision.Choices[0].Description)
	}

	// K-PLAIN: direct answer, no InterpretedAnswer, not awaiting confirmation.
	require.Contains(t, byAnswerID, plainAns.ID)
	assert.Nil(t, byAnswerID[plainAns.ID].InterpretedAnswer)
	assert.False(t, byAnswerID[plainAns.ID].AwaitingConfirmation)
	assert.Equal(t, "pg", byAnswerID[plainAns.ID].Decision.DefaultChoiceID)
	assert.Equal(t, store.DecisionStatusDecided, byAnswerID[plainAns.ID].Decision.Status)

	// K-CONF: direct confirm with folded InterpretedAnswer; K-CONF's raw interpreted answer ID is NOT listed!
	assert.NotContains(t, byAnswerID, confInterp.ID)
	require.Contains(t, byAnswerID, confAns.ID)
	require.NotNil(t, byAnswerID[confAns.ID].InterpretedAnswer)
	assert.Equal(t, confInterp.ID, byAnswerID[confAns.ID].InterpretedAnswer.ID)
	assert.Equal(t, "pg", byAnswerID[confAns.ID].InterpretedAnswer.ChoiceID)
	assert.Equal(t, "Postgres", byAnswerID[confAns.ID].InterpretedAnswer.ChoiceLabel)
	assert.True(t, byAnswerID[confAns.ID].InterpretedAnswer.MatchedDefault)
	assert.False(t, byAnswerID[confAns.ID].AwaitingConfirmation)

	// K-DSM: direct dismiss with folded InterpretedAnswer; K-DSM's raw interpreted answer ID is NOT listed!
	dsmAns := dsmRes.Dismissed[0].Dismissal()
	require.NotNil(t, dsmAns)
	assert.NotContains(t, byAnswerID, dsmInterp.ID)
	require.Contains(t, byAnswerID, dsmAns.ID)
	assert.Equal(t, store.DecisionOutcomeDismiss, byAnswerID[dsmAns.ID].Answer.Outcome)
	assert.Equal(t, "$dismissed", byAnswerID[dsmAns.ID].Answer.ChoiceID)
	require.NotNil(t, byAnswerID[dsmAns.ID].InterpretedAnswer)
	assert.Equal(t, dsmInterp.ID, byAnswerID[dsmAns.ID].InterpretedAnswer.ID)
	assert.Equal(t, "sqlite", byAnswerID[dsmAns.ID].InterpretedAnswer.ChoiceID)
	assert.Equal(t, "SQLite", byAnswerID[dsmAns.ID].InterpretedAnswer.ChoiceLabel)
	assert.False(t, byAnswerID[dsmAns.ID].AwaitingConfirmation)
	assert.Equal(t, store.DecisionStatusDismissed, byAnswerID[dsmAns.ID].Decision.Status)

	// K-UNCONF: active unconfirmed interpreted entry -> AwaitingConfirmation = true.
	require.Contains(t, byAnswerID, unconfInterp.ID)
	assert.Nil(t, byAnswerID[unconfInterp.ID].InterpretedAnswer)
	assert.True(t, byAnswerID[unconfInterp.ID].AwaitingConfirmation)

	// K-REPL v1 & v2: both versions keep their own entry; v1 has ReplacedByID = v2 and AwaitingConfirmation = false!
	require.Contains(t, byAnswerID, replV1Interp.ID)
	assert.Equal(t, replV1RowID, byAnswerID[replV1Interp.ID].Decision.ID)
	assert.Equal(t, store.DecisionStatusReplaced, byAnswerID[replV1Interp.ID].Decision.Status)
	assert.Equal(t, replV2RowID, byAnswerID[replV1Interp.ID].Decision.ReplacedByID)
	assert.False(t, byAnswerID[replV1Interp.ID].AwaitingConfirmation, "replaced v1 can no longer be confirmed")
	require.Contains(t, byAnswerID, replV2Ans.ID)
	assert.Equal(t, 2, byAnswerID[replV2Ans.ID].Decision.Version)

	// Provenance = dismissed keeps working and carries InterpretedAnswer!
	dsmFiltered, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topic}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceDismissed,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, dsmFiltered, 1)
	assert.Equal(t, dsmAns.ID, dsmFiltered[0].Answer.ID)
	require.NotNil(t, dsmFiltered[0].InterpretedAnswer)
	assert.Equal(t, dsmInterp.ID, dsmFiltered[0].InterpretedAnswer.ID)

	// Provenance = interpreted returns ONLY the 2 un-superseded interpreted entries (K-UNCONF and K-REPL v1).
	interpFiltered, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topic}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceInterpreted,
	}, "", 50)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{unconfInterp.ID, replV1Interp.ID}, answerIDs(interpFiltered))

	// ListDecisionFacets excludes superseded interpreted rows from answerers facet (Charlie is absent).
	_, facetAnswerers, _, err := cs.ListDecisionFacets(ctx, []string{projectID}, []string{topic})
	require.NoError(t, err)
	answererIDs := make([]string, 0, len(facetAnswerers))
	for _, a := range facetAnswerers {
		answererIDs = append(answererIDs, a.ID)
	}
	assert.ElementsMatch(t, []string{aliceID, bobID}, answererIDs)
}
