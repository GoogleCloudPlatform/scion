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
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
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

	// Seed 2 decisions in pSmall: small-1 interpreted + overridden (folds into 1 entry),
	// and small-2 unconfirmed interpreted (stays as 1 entry with AwaitingConfirmation=true).
	_, smallRow1 := seedSearchDecision(t, cs, searchSeed{
		projectID: pSmall, agentID: agentB, agentName: "beta-bot", chatKey: topicSmall,
		key: "small-1", question: "Keep cache warm?",
		choices: []messages.Choice{{ID: "yes", Label: "Warm"}, {ID: "no", Label: "Cold"}},
	})
	_, smallRow2 := seedSearchDecision(t, cs, searchSeed{
		projectID: pSmall, agentID: agentB, agentName: "beta-bot", chatKey: topicSmall,
		key: "small-2", question: "Pin image digest?",
		choices: []messages.Choice{{ID: "yes", Label: "Pin"}, {ID: "no", Label: "Float"}},
	})
	srcMsgID := uuid.New().String()
	require.NoError(t, cs.CreateMessage(ctx, &store.Message{
		ID: srcMsgID, ProjectID: pSmall, Sender: "user:" + aliceID, SenderID: aliceID,
		Recipient: "group:topic", ThreadID: topicSmall, Msg: "warm and pin please", CreatedAt: base.Add(-5 * time.Minute),
	}))
	interpRes, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentB,
		SourceMessageID:      srcMsgID,
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		Surface:              store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{
			{DecisionRowID: smallRow1, ChoiceID: "yes"},
			{DecisionRowID: smallRow2, ChoiceID: "yes"},
		},
		ResponseMessage: &store.Message{
			ID: uuid.New().String(), ProjectID: pSmall, Sender: "agent:beta-bot", SenderID: agentB,
			Recipient: "group:topic", ThreadID: topicSmall, Msg: "recorded", CreatedAt: base.Add(-4 * time.Minute),
		},
		FormatMessage: func(_ []store.InterpretedFormattedItem) string { return "recorded" },
	})
	require.NoError(t, err)
	require.Len(t, interpRes.Answers, 2)
	var interpSmall1ID, interpSmall2ID string
	for _, a := range interpRes.Answers {
		switch a.DecisionRowID {
		case smallRow1:
			interpSmall1ID = a.ID
		case smallRow2:
			interpSmall2ID = a.ID
		}
	}
	require.NotEmpty(t, interpSmall1ID)
	require.NotEmpty(t, interpSmall2ID)
	_, err = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET created_at = ? WHERE id = ?"),
		base.Add(30*time.Second), interpSmall2ID)
	require.NoError(t, err)
	_, err = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET created_at = ? WHERE id = ?"),
		base.Add(1*time.Minute), interpSmall1ID)
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
	aliceKeys := map[string][]string{
		pBusy:  {topicBusy},
		pSmall: {topicSmall},
	}

	// perProject=6 (5 + 1 sentinel for "more"): pBusy returns newest 6 of 7 (excluding secret DM),
	// pSmall returns 2 non-superseded entries (overrideAnsID with folded interpSmall1ID, plus unconfirmed interpSmall2ID), pNone is omitted.
	byProj, err := cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{}, 6)
	require.NoError(t, err)
	require.Len(t, byProj, 2)
	assert.NotContains(t, byProj, pNone)

	require.Len(t, byProj[pBusy], 6)
	assert.Equal(t, []string{
		busyAnsIDs[6], busyAnsIDs[5], busyAnsIDs[4], busyAnsIDs[3], busyAnsIDs[2], busyAnsIDs[1],
	}, answerIDs(byProj[pBusy]))

	require.Len(t, byProj[pSmall], 2)
	assert.Equal(t, []string{overrideAnsID, interpSmall2ID}, answerIDs(byProj[pSmall]))
	assert.Equal(t, "Warm", byProj[pSmall][0].PreviousChoiceLabel)
	require.NotNil(t, byProj[pSmall][0].InterpretedAnswer)
	assert.Equal(t, interpSmall1ID, byProj[pSmall][0].InterpretedAnswer.ID)
	assert.False(t, byProj[pSmall][0].AwaitingConfirmation)
	assert.True(t, byProj[pSmall][0].PromptExistsInMessages)
	assert.True(t, byProj[pSmall][1].AwaitingConfirmation)
	assert.Nil(t, byProj[pSmall][1].InterpretedAnswer)
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

	// QueryChatKeys (conversation label match) scopes per project without matching other projects.
	byConvLabel, err := cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{
		Query:         "nomatch-text-only-conv-label",
		QueryChatKeys: []string{topicSmall},
	}, 6)
	require.NoError(t, err)
	require.Len(t, byConvLabel, 1)
	require.Len(t, byConvLabel[pSmall], 2)

	// ListDecisionFacets carries each asking agent's ProjectID and leaves answerers without ProjectID.
	facetAgents, facetAnswerers, _, err := cs.ListDecisionFacets(ctx, projects, []string{topicBusy, topicSmall})
	require.NoError(t, err)
	agentProjects := make(map[string]string, len(facetAgents))
	for _, a := range facetAgents {
		agentProjects[a.ID] = a.ProjectID
	}
	assert.Equal(t, map[string]string{
		agentA: pBusy,
		agentB: pSmall,
	}, agentProjects)
	require.NotEmpty(t, facetAnswerers)
	for _, ans := range facetAnswerers {
		assert.Empty(t, ans.ProjectID, "answerers span projects and must not carry ProjectID")
	}

	// Zero / empty cases and validation errors.
	none, err := cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{}, 0)
	require.NoError(t, err)
	assert.Nil(t, none)
	emptyMap, err := cs.ListDecisionAnswersPerProject(ctx, projects, nil, store.ProjectDecisionAnswerFilter{}, 6)
	require.NoError(t, err)
	assert.Nil(t, emptyMap)

	_, err = cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{}, -1)
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{}, store.MaxDecisionListLimit+1)
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{AgentID: "bad"}, 6)
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeys, store.ProjectDecisionAnswerFilter{Provenance: "bad"}, 6)
	require.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestListDecisionAnswersPerProject_HistoryScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100k-answer history scale test in short mode")
	}
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	const (
		numProjects       = 200
		topicsPerProject  = 5
		answersPerProject = 500 // 200 * 500 = 100,000 decided rows + 100,000 direct answers
		perProjectLimit   = 6
	)

	projectIDs := make([]string, numProjects)
	projectUUIDs := make([]uuid.UUID, numProjects)
	keysByProject := make(map[string][]string, numProjects)
	expectedTopIDs := make(map[string][]string, numProjects)

	for p := 0; p < numProjects; p++ {
		pid := backfillTestProject(t, cs, fmt.Sprintf("hist-proj-%03d", p))
		projectIDs[p] = pid
		projectUUIDs[p] = uuid.MustParse(pid)
		topics := make([]string, topicsPerProject)
		for tp := 0; tp < topicsPerProject; tp++ {
			topics[tp] = uuid.NewString()
		}
		keysByProject[pid] = topics
	}

	userID := uuid.NewString()
	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	histUUID := func(tag byte, p, j int) uuid.UUID {
		var u uuid.UUID
		u[0] = tag
		binary.BigEndian.PutUint32(u[4:8], uint32(p+1))
		binary.BigEndian.PutUint32(u[8:12], uint32(j+1))
		u[6] = (u[6] & 0x0f) | 0x40
		u[8] = (u[8] & 0x3f) | 0x80
		return u
	}

	seedStart := time.Now()
	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	for p := 0; p < numProjects; p++ {
		pUUID := projectUUIDs[p]
		pid := projectIDs[p]
		topics := keysByProject[pid]
		agentID := histUUID(0x10, p, 0)

		decCreates := make([]*ent.DecisionCreate, answersPerProject)
		ansCreates := make([]*ent.DecisionAnswerCreate, answersPerProject)
		ansIDs := make([]string, answersPerProject)

		for j := 0; j < answersPerProject; j++ {
			decID := histUUID(0x20, p, j)
			ansID := histUUID(0x30, p, j)
			msgID := histUUID(0x40, p, j)
			respMsgID := histUUID(0x50, p, j)
			chatKey := topics[j%topicsPerProject]
			key := fmt.Sprintf("h-%03d-%04d", p, j)
			ts := base.Add(time.Duration(p*1000+j) * time.Second)

			decCreates[j] = tx.Decision.Create().
				SetID(decID).
				SetProjectID(pUUID).
				SetChatKey(chatKey).
				SetMessageID(msgID).
				SetPosition(0).
				SetAskerAgentID(agentID).
				SetAskerAgentName("hist-bot").
				SetKey(key).
				SetKeyNorm(key).
				SetKeySource(decision.KeySourceAgent).
				SetVersion(1).
				SetQuestion("History question?").
				SetChoices(choices).
				SetStatus(decision.StatusDecided).
				SetCreatedAt(ts).
				SetDecidedAt(ts)

			ansCreates[j] = tx.DecisionAnswer.Create().
				SetID(ansID).
				SetDecisionRowID(decID).
				SetProjectID(pUUID).
				SetAskerAgentID(agentID).
				SetKey(key).
				SetVersion(1).
				SetQuestionText("History question?").
				SetChoiceID("yes").
				SetChoiceLabel("Yes").
				SetAnsweredByUserID(userID).
				SetAnsweredByName("Alice").
				SetProvenance(decisionanswer.ProvenanceDirect).
				SetOutcome(decisionanswer.OutcomeAnswer).
				SetSurface(decisionanswer.SurfaceDecisionsPage).
				SetResponseMessageID(respMsgID).
				SetCreatedAt(ts)

			ansIDs[j] = ansID.String()
		}
		require.NoError(t, tx.Decision.CreateBulk(decCreates...).Exec(ctx))
		require.NoError(t, tx.DecisionAnswer.CreateBulk(ansCreates...).Exec(ctx))

		top := make([]string, perProjectLimit)
		for k := 0; k < perProjectLimit; k++ {
			top[k] = ansIDs[answersPerProject-1-k]
		}
		expectedTopIDs[pid] = top
	}
	require.NoError(t, tx.Commit())
	t.Logf("seeded %d decisions + %d answers across %d projects in %v",
		numProjects*answersPerProject, numProjects*answersPerProject, numProjects, time.Since(seedStart))

	queryStart := time.Now()
	byProj, err := cs.ListDecisionAnswersPerProject(ctx, projectIDs, keysByProject, store.ProjectDecisionAnswerFilter{}, perProjectLimit)
	queryElapsed := time.Since(queryStart)
	require.NoError(t, err)
	t.Logf("100k answers (%d projects x %d answers): ListDecisionAnswersPerProject(..., %d) took %v",
		numProjects, answersPerProject, perProjectLimit, queryElapsed)

	require.Len(t, byProj, numProjects)
	for _, pid := range projectIDs {
		require.Len(t, byProj[pid], perProjectLimit)
		assert.Equal(t, expectedTopIDs[pid], answerIDs(byProj[pid]))
	}
}
