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
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectsWithDecisions_FiltersAndOrdersByLatestDecision(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	pOld := backfillTestProject(t, cs, "proj-dec-old")
	pNew := backfillTestProject(t, cs, "proj-dec-new")
	pEmpty := backfillTestProject(t, cs, "proj-dec-empty")
	pOutsider := backfillTestProject(t, cs, "proj-dec-outsider")

	empty, err := cs.ProjectsWithDecisions(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, empty)

	_, err = cs.ProjectsWithDecisions(ctx, []string{"not-a-uuid"})
	require.ErrorIs(t, err, store.ErrNotFound)

	agentID := uuid.New().String()
	topicOld := uuid.New().String()
	topicNew := uuid.New().String()
	topicOut := uuid.New().String()

	_, rowOld := seedSearchDecision(t, cs, searchSeed{
		projectID: pOld, agentID: agentID, agentName: "bot", chatKey: topicOld, key: "k-old", question: "Old?",
	})
	_, rowNew1 := seedSearchDecision(t, cs, searchSeed{
		projectID: pNew, agentID: agentID, agentName: "bot", chatKey: topicNew, key: "k-new-1", question: "New 1?",
	})
	_, rowNew2 := seedSearchDecision(t, cs, searchSeed{
		projectID: pNew, agentID: agentID, agentName: "bot", chatKey: topicNew, key: "k-new-2", question: "New 2?",
	})
	_, rowOut := seedSearchDecision(t, cs, searchSeed{
		projectID: pOutsider, agentID: agentID, agentName: "bot", chatKey: topicOut, key: "k-out", question: "Outsider?",
	})

	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	setDecisionCreatedAt := func(rowID string, at time.Time) {
		_, uerr := cs.DB().ExecContext(ctx,
			rebindForDialect(cs.Dialect(), "UPDATE decisions SET created_at = ? WHERE id = ?"), at, rowID)
		require.NoError(t, uerr)
	}
	setDecisionCreatedAt(rowOld, base.Add(-10*time.Minute))
	setDecisionCreatedAt(rowNew1, base.Add(-20*time.Minute))
	setDecisionCreatedAt(rowNew2, base.Add(-1*time.Minute))
	setDecisionCreatedAt(rowOut, base)

	got, err := cs.ProjectsWithDecisions(ctx, []string{pEmpty, pOld, pNew, pOld})
	require.NoError(t, err)
	assert.Equal(t, []string{pNew, pOld}, got, "excludes empty and non-requested projects, orders newest decision first")
}

func TestListDecisionAnswersPerProject_PerProjectLimitFiltersAndEnrichment(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	pBusy := backfillTestProject(t, cs, "proj-busy")
	pSmall := backfillTestProject(t, cs, "proj-small")
	pNone := backfillTestProject(t, cs, "proj-none")

	aliceID := uuid.New().String()
	bobID := uuid.New().String()
	for _, u := range []*store.User{
		{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now()},
		{ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Status: "active", Created: time.Now()},
	} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	agentA := uuid.New().String()
	agentB := uuid.New().String()
	topicBusy := uuid.New().String()
	topicSmall := uuid.New().String()
	dmSecret := "dm:agent:" + agentA + ":user:" + bobID

	base := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)

	// Seed 7 answers in pBusy (topicBusy), plus 1 secret DM answer in pBusy that Alice cannot read.
	var busyAnsIDs []string
	for i := 0; i < 7; i++ {
		_, rowID := seedSearchDecision(t, cs, searchSeed{
			projectID: pBusy, agentID: agentA, agentName: "alpha-bot", chatKey: topicBusy,
			key: fmt.Sprintf("busy-%d", i), question: fmt.Sprintf("Deploy service %d?", i),
		})
		ansID := answerSearchDecision(t, cs, pBusy, rowID, "yes", aliceID, "Alice", base.Add(time.Duration(i)*time.Minute))
		busyAnsIDs = append(busyAnsIDs, ansID)
	}
	_, dmRow := seedSearchDecision(t, cs, searchSeed{
		projectID: pBusy, agentID: agentA, agentName: "alpha-bot", chatKey: dmSecret,
		key: "secret-dm", question: "Deploy secret DM?",
	})
	_ = answerSearchDecision(t, cs, pBusy, dmRow, "yes", bobID, "Bob", base.Add(10*time.Minute))

	// Seed 2 answers in pSmall: one interpreted + overridden so enrichment is exercised.
	_, smallRow1 := seedSearchDecision(t, cs, searchSeed{
		projectID: pSmall, agentID: agentB, agentName: "beta-bot", chatKey: topicSmall,
		key: "small-1", question: "Keep cache warm?",
		choices: []messages.Choice{{ID: "yes", Label: "Warm"}, {ID: "no", Label: "Cold"}},
	})
	srcMsgID := uuid.New().String()
	require.NoError(t, cs.CreateMessage(ctx, &store.Message{
		ID: srcMsgID, ProjectID: pSmall, Sender: "user:" + aliceID, SenderID: aliceID,
		Recipient: "group:topic", ThreadID: topicSmall, Msg: "warm please", CreatedAt: base.Add(-5 * time.Minute),
	}))
	interpRes, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentB,
		SourceMessageID:      srcMsgID,
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{DecisionRowID: smallRow1, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: uuid.New().String(), ProjectID: pSmall, Sender: "agent:beta-bot", SenderID: agentB,
			Recipient: "group:topic", ThreadID: topicSmall, Msg: "recorded", CreatedAt: base.Add(-4 * time.Minute),
		},
		FormatMessage: func(_ []store.InterpretedFormattedItem) string { return "recorded" },
	})
	require.NoError(t, err)
	require.Len(t, interpRes.Answers, 1)
	interpAnsID := interpRes.Answers[0].ID
	_, err = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET created_at = ? WHERE id = ?"),
		base.Add(1*time.Minute), interpAnsID)
	require.NoError(t, err)

	overrideAns, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    smallRow1,
		ChoiceID:         "no",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		Provenance:       store.DecisionProvenanceDirect,
		Outcome:          store.DecisionOutcomeOverride,
		Surface:          store.DecisionSurfaceDecisionsPage,
		ResponseMessage: &store.Message{
			ID: uuid.New().String(), ProjectID: pSmall, Sender: "user:Alice",
			Recipient: "agent:beta-bot", Msg: "no",
		},
	})
	require.NoError(t, err)
	overrideAnsID := overrideAns.ID
	_, err = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET created_at = ? WHERE id = ?"),
		base.Add(2*time.Minute), overrideAnsID)
	require.NoError(t, err)

	projects := []string{pBusy, pSmall, pNone}
	aliceKeys := []string{topicBusy, topicSmall}

	// perProject=6 (5 + 1 sentinel for "more"): pBusy returns newest 6 of 7 (excluding secret DM),
	// pSmall returns both 2 answers with PreviousChoiceLabel populated, pNone is omitted.
	byProj, err := cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{}, 6)
	require.NoError(t, err)
	require.Len(t, byProj, 2)
	assert.NotContains(t, byProj, pNone)

	require.Len(t, byProj[pBusy], 6)
	assert.Equal(t, []string{
		busyAnsIDs[6], busyAnsIDs[5], busyAnsIDs[4], busyAnsIDs[3], busyAnsIDs[2], busyAnsIDs[1],
	}, answerIDs(byProj[pBusy]))

	require.Len(t, byProj[pSmall], 2)
	assert.Equal(t, []string{overrideAnsID, interpAnsID}, answerIDs(byProj[pSmall]))
	assert.Equal(t, "Warm", byProj[pSmall][0].PreviousChoiceLabel)
	assert.True(t, byProj[pSmall][0].PromptExistsInMessages)
	assert.True(t, byProj[pSmall][1].SourceExistsInMessages)

	// Filter by Provenance=overridden + Query.
	filtered, err := cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceOverridden,
		Query:      "cache",
	}, 6)
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	require.Len(t, filtered[pSmall], 1)
	assert.Equal(t, overrideAnsID, filtered[pSmall][0].Answer.ID)

	// Zero / empty cases and validation errors.
	none, err := cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{}, 0)
	require.NoError(t, err)
	assert.Nil(t, none)

	_, err = cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{}, -1)
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{}, store.MaxDecisionListLimit+1)
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{AgentID: "bad"}, 6)
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{Provenance: "bad"}, 6)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	// Large key slice exceeding maxDecisionAnswerKeyBatch (40,000 keys > SQLite's 32,766 limit)
	// chunks safely and still caps each project at perProject.
	hugeKeys := make([]string, 0, 40000)
	hugeKeys = append(hugeKeys, topicBusy)
	for i := 0; i < 39998; i++ {
		hugeKeys = append(hugeKeys, fmt.Sprintf("unused-topic-%05d", i))
	}
	hugeKeys = append(hugeKeys, topicSmall)
	byProjHuge, err := cs.ListDecisionAnswersPerProject(ctx, projects, hugeKeys, store.ProjectDecisionAnswerFilter{}, 6)
	require.NoError(t, err)
	require.Len(t, byProjHuge[pBusy], 6)
	require.Len(t, byProjHuge[pSmall], 2)
}

func TestRankedDecisionAnswersSelector_PostgresAndSQLitePlaceholders(t *testing.T) {
	p1, p2 := uuid.New(), uuid.New()
	agentID := uuid.New()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	filter := store.ProjectDecisionAnswerFilter{
		Query:         "deploy",
		QueryChatKeys: []string{"topic-1"},
		AgentID:       agentID.String(),
		UserID:        "user-1",
		Provenance:    store.ProjectDecisionProvenanceDirect,
		From:          &now,
	}
	agentUUID, provPred, err := validateDecisionAnswerFilter(filter)
	require.NoError(t, err)
	preds := decisionAnswerFilterPredicates(filter, agentUUID, provPred)

	for _, d := range []string{dialect.Postgres, dialect.SQLite} {
		b := entsql.Dialect(d)
		outerTable := b.Table(decisionanswer.Table)
		ranked := rankedDecisionAnswersSelector(d, []uuid.UUID{p1, p2}, []string{"topic-1", "topic-2"}, preds, 6)
		outer := b.Select(outerTable.C(decisionanswer.FieldID)).
			From(outerTable).
			Where(entsql.In(outerTable.C(decisionanswer.FieldID), ranked))
		sqlStr, args := outer.Query()
		assert.Contains(t, sqlStr, "ROW_NUMBER() OVER (PARTITION BY")
		assert.NotEmpty(t, args)
		if d == dialect.Postgres {
			assert.Contains(t, sqlStr, fmt.Sprintf("$%d", len(args)), "highest placeholder must match arg count")
			assert.NotContains(t, sqlStr, fmt.Sprintf("$%d", len(args)+1))
			assert.NotContains(t, sqlStr, "?")
		} else {
			assert.Contains(t, sqlStr, "?")
			assert.NotContains(t, sqlStr, "$1")
		}
	}
}
