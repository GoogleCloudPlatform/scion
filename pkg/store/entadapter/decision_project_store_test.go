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
	setRes, err := cs.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
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
	emptyPrompts, err := cs.ListProjectOpenPrompts(ctx, projectID, nil)
	require.NoError(t, err)
	assert.Empty(t, emptyPrompts)

	// Bob's readable keys (topicRelease + dmBob) must NEVER return Alice's DM!
	bobPrompts, err := cs.ListProjectOpenPrompts(ctx, projectID, []string{topicRelease, dmBob})
	require.NoError(t, err)
	require.Len(t, bobPrompts, 2)
	for _, p := range bobPrompts {
		assert.NotEqual(t, dmAlice, p.ChatKey, "Bob must never see Alice's DM prompt")
	}

	// Alice's readable keys (topicRelease + dmAlice) return both, with purged DM marked PromptExistsInMessages=false!
	alicePrompts, err := cs.ListProjectOpenPrompts(ctx, projectID, []string{topicRelease, dmAlice})
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

	// Entry 4: Alice's private DM answer with agentSecretDM (must NEVER appear in Bob's list or facets!).
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
	t1 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	t4 := time.Date(2026, 11, 13, 16, 0, 0, 0, time.UTC)
	for id, ts := range map[string]time.Time{
		interpRes.Answers[0].ID: t1,
		overrideAns.ID:          t2,
		directAns.ID:            t3,
		dmAns.ID:                t4,
	} {
		_, err := cs.DB().ExecContext(ctx,
			rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET created_at = ? WHERE id = ?"),
			ts, id)
		require.NoError(t, err)
	}

	// 1. Bob's facets over [topicRelease] must NOT include agentSecretDM!
	bobAgents, bobAnswerers, err := cs.ListProjectDecisionFacets(ctx, projectID, []string{topicRelease})
	require.NoError(t, err)
	require.Len(t, bobAgents, 1)
	assert.Equal(t, agentRelease, bobAgents[0].ID)
	assert.Len(t, bobAnswerers, 2) // Alice and Bob both answered in #release

	// 2. Paginate Bob's Made log 1 entry at a time across all 3 entries.
	var gotIDs []string
	cur := ""
	for page := 0; page < 5; page++ {
		items, next, err := cs.ListProjectDecisionAnswers(ctx, projectID, []string{topicRelease}, store.ProjectDecisionAnswerFilter{}, cur, 1)
		require.NoError(t, err)
		for _, it := range items {
			gotIDs = append(gotIDs, it.Answer.ID)
		}
		if next == "" {
			break
		}
		cur = next
	}
	assert.Equal(t, []string{directAns.ID, overrideAns.ID, interpRes.Answers[0].ID}, gotIDs)

	// 3. Check PreviousChoiceLabel on override entry and SourceExistsInMessages before/after purge.
	overriddenEntries, _, err := cs.ListProjectDecisionAnswers(ctx, projectID, []string{topicRelease}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceOverridden,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, overriddenEntries, 1)
	assert.Equal(t, overrideAns.ID, overriddenEntries[0].Answer.ID)
	assert.Equal(t, "Yes", overriddenEntries[0].PreviousChoiceLabel)
	assert.Equal(t, store.DecisionSurfaceDecisionsPage, overriddenEntries[0].Answer.Surface)

	interpEntries, _, err := cs.ListProjectDecisionAnswers(ctx, projectID, []string{topicRelease}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceInterpreted,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, interpEntries, 1)
	assert.True(t, interpEntries[0].SourceExistsInMessages)

	// Purge source reply -> SourceExistsInMessages flips to false!
	_, err = cs.DB().ExecContext(ctx, rebindForDialect(cs.Dialect(), "DELETE FROM messages WHERE id = ?"), sourceReplyID)
	require.NoError(t, err)
	interpAfterPurge, _, err := cs.ListProjectDecisionAnswers(ctx, projectID, []string{topicRelease}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceInterpreted,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, interpAfterPurge, 1)
	assert.False(t, interpAfterPurge[0].SourceExistsInMessages)

	// 4. Combinable filters: user + date range + agent.
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 12, 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
	filtered, _, err := cs.ListProjectDecisionAnswers(ctx, projectID, []string{topicRelease, dmAlice}, store.ProjectDecisionAnswerFilter{
		AgentID: agentRelease,
		UserID:  bobID,
		From:    &from,
		To:      &to,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	assert.Equal(t, directAns.ID, filtered[0].Answer.ID)

	// 5. limit=0 returns empty slice and no cursor while still validating bad inputs.
	zeroItems, zeroNext, err := cs.ListProjectDecisionAnswers(ctx, projectID, []string{topicRelease}, store.ProjectDecisionAnswerFilter{}, "", 0)
	require.NoError(t, err)
	assert.Empty(t, zeroItems)
	assert.Empty(t, zeroNext)

	_, _, err = cs.ListProjectDecisionAnswers(ctx, projectID, []string{topicRelease}, store.ProjectDecisionAnswerFilter{
		Provenance: "bogus",
	}, "", 0)
	require.ErrorIs(t, err, store.ErrInvalidInput)
}
