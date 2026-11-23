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

// searchSeed is one single-decision prompt for the Decisions search tests.
type searchSeed struct {
	projectID string
	agentID   string
	agentName string
	chatKey   string
	key       string
	question  string
	choices   []messages.Choice
}

func seedSearchDecision(t *testing.T, cs *CompositeStore, in searchSeed) (msgID, rowID string) {
	t.Helper()
	if in.choices == nil {
		in.choices = []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	}
	msgID = uuid.New().String()
	res, err := cs.CreateDecisionPrompt(context.Background(), &store.DecisionPromptInput{
		Message: &store.Message{
			ID:        msgID,
			ProjectID: in.projectID,
			Sender:    "agent:" + in.agentName,
			SenderID:  in.agentID,
			Recipient: "group:topic",
			ThreadID:  in.chatKey,
		},
		AskerAgentID:   in.agentID,
		AskerAgentName: in.agentName,
		Decisions: []messages.Decision{{
			Key:      in.key,
			Question: in.question,
			Choices:  in.choices,
		}},
	})
	require.NoError(t, err)
	require.Len(t, res.Decisions, 1)
	return msgID, res.Decisions[0].RowID
}

func answerSearchDecision(t *testing.T, cs *CompositeStore, projectID, rowID, choiceID, userID, userName string, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	ans, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    rowID,
		ChoiceID:         choiceID,
		AnsweredByUserID: userID,
		AnsweredByName:   userName,
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:" + userName,
			Recipient: "agent:asker",
			Msg:       choiceID,
		},
	})
	require.NoError(t, err)
	_, err = cs.DB().ExecContext(ctx,
		rebindForDialect(cs.Dialect(), "UPDATE decision_answers SET created_at = ? WHERE id = ?"), at, ans.ID)
	require.NoError(t, err)
	return ans.ID
}

func answerIDs(entries []*store.ProjectDecisionAnswerEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Answer.ID)
	}
	return out
}

func TestDecisionSearch_MadeQueryArmsFiltersAndPaging(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-search-made")

	aliceID := uuid.New().String()
	bobID := uuid.New().String()
	for _, u := range []*store.User{
		{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now()},
		{ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Status: "active", Created: time.Now()},
	} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	releaseBot := uuid.New().String()
	ncX := uuid.New().String()
	topicRelease := uuid.New().String()
	topicOps := uuid.New().String()
	keys := []string{topicRelease, topicOps}
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

	// Each answer matches exactly one search arm.
	_, rowQuestion := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: ncX, agentName: "nc-x", chatKey: topicOps,
		key: "k1", question: "Run the BACKFILL on staging first?"})
	ansQuestion := answerSearchDecision(t, cs, projectID, rowQuestion, "yes", aliceID, "Alice", base)

	_, rowLabel := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: ncX, agentName: "nc-x", chatKey: topicOps,
		key: "k2", question: "Which window?", choices: []messages.Choice{{ID: "a", Label: "Overnight Window"}, {ID: "b", Label: "Lunch"}}})
	ansLabel := answerSearchDecision(t, cs, projectID, rowLabel, "a", bobID, "Bob", base.Add(1*time.Hour))

	_, rowKey := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: ncX, agentName: "nc-x", chatKey: topicOps,
		key: "migrate-db", question: "Proceed?"})
	ansKey := answerSearchDecision(t, cs, projectID, rowKey, "yes", aliceID, "Alice", base.Add(2*time.Hour))

	_, rowAgent := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: releaseBot, agentName: "Release-Bot", chatKey: topicOps,
		key: "k4", question: "Tag it?"})
	ansAgent := answerSearchDecision(t, cs, projectID, rowAgent, "no", bobID, "Bob", base.Add(3*time.Hour))

	_, rowConv := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: ncX, agentName: "nc-x", chatKey: topicRelease,
		key: "k5", question: "Proceed?"})
	ansConv := answerSearchDecision(t, cs, projectID, rowConv, "yes", aliceID, "Alice", base.Add(4*time.Hour))

	// Wildcard-looking text: only "50%_done" contains the literal "%_".
	_, rowPct := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: ncX, agentName: "nc-x", chatKey: topicOps,
		key: "k6", question: "Mark 50%_done?"})
	ansPct := answerSearchDecision(t, cs, projectID, rowPct, "yes", aliceID, "Alice", base.Add(5*time.Hour))
	_, rowNoPct := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: ncX, agentName: "nc-x", chatKey: topicOps,
		key: "k7", question: "Mark 50 percent xdone?"})
	answerSearchDecision(t, cs, projectID, rowNoPct, "yes", aliceID, "Alice", base.Add(6*time.Hour))

	list := func(f store.ProjectDecisionAnswerFilter) []string {
		t.Helper()
		entries, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, keys, f, "", 50)
		require.NoError(t, err)
		return answerIDs(entries)
	}

	t.Run("each arm alone, case-insensitive", func(t *testing.T) {
		assert.Equal(t, []string{ansQuestion}, list(store.ProjectDecisionAnswerFilter{Query: "backfill"}), "question text")
		assert.Equal(t, []string{ansLabel}, list(store.ProjectDecisionAnswerFilter{Query: "overnight WINDOW"}), "choice label")
		assert.Equal(t, []string{ansKey}, list(store.ProjectDecisionAnswerFilter{Query: "MIGRATE-db"}), "decision key")
		assert.Equal(t, []string{ansAgent}, list(store.ProjectDecisionAnswerFilter{Query: "release-bot"}), "asker agent name")
		assert.Equal(t, []string{ansConv}, list(store.ProjectDecisionAnswerFilter{Query: "zz-label-only", QueryChatKeys: []string{topicRelease}}),
			"conversation label keys")
	})

	t.Run("wildcards match literally", func(t *testing.T) {
		assert.Equal(t, []string{ansPct}, list(store.ProjectDecisionAnswerFilter{Query: "%_"}))
		assert.Equal(t, []string{ansPct}, list(store.ProjectDecisionAnswerFilter{Query: "50%"}))
		assert.Empty(t, list(store.ProjectDecisionAnswerFilter{Query: "50_p"}))
	})

	t.Run("label keys outside the readable keys never match", func(t *testing.T) {
		entries, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topicOps},
			store.ProjectDecisionAnswerFilter{Query: "zz-label-only", QueryChatKeys: []string{topicRelease}}, "", 50)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("query combined with every filter", func(t *testing.T) {
		// "proceed" matches the key and conversation rows' question.
		assert.ElementsMatch(t, []string{ansKey, ansConv}, list(store.ProjectDecisionAnswerFilter{Query: "proceed"}))
		from := base.Add(3 * time.Hour)
		to := base.Add(5 * time.Hour)
		assert.Equal(t, []string{ansConv}, list(store.ProjectDecisionAnswerFilter{
			Query:      "proceed",
			AgentID:    ncX,
			UserID:     aliceID,
			Provenance: store.ProjectDecisionProvenanceDirect,
			From:       &from,
			To:         &to,
		}))
		assert.Empty(t, list(store.ProjectDecisionAnswerFilter{Query: "proceed", UserID: bobID}))
		assert.Empty(t, list(store.ProjectDecisionAnswerFilter{Query: "proceed", AgentID: releaseBot}))
		assert.Empty(t, list(store.ProjectDecisionAnswerFilter{Query: "proceed", Provenance: store.ProjectDecisionProvenanceOverridden}))
		assert.Equal(t, []string{ansKey}, list(store.ProjectDecisionAnswerFilter{Query: "proceed", To: &from}))
	})

	t.Run("cursor paging with a query", func(t *testing.T) {
		f := store.ProjectDecisionAnswerFilter{Query: "?"}
		var got []string
		cur := ""
		for page := 0; page < 10; page++ {
			entries, next, err := cs.ListDecisionAnswers(ctx, []string{projectID}, keys, f, cur, 2)
			require.NoError(t, err)
			got = append(got, answerIDs(entries)...)
			if next == "" {
				break
			}
			cur = next
		}
		// Every question ends in "?", newest first.
		require.Len(t, got, 7)
		assert.Equal(t, ansPct, got[1])
		assert.Equal(t, ansQuestion, got[6])
	})
}

func TestDecisionSearch_MultiProjectMethods(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projA := backfillTestProject(t, cs, "decision-multi-a")
	projB := backfillTestProject(t, cs, "decision-multi-b")
	projC := backfillTestProject(t, cs, "decision-multi-c")

	aliceID := uuid.New().String()
	require.NoError(t, cs.CreateUser(ctx, &store.User{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now()}))

	botA := uuid.New().String()
	botB := uuid.New().String()
	botC := uuid.New().String()
	topicA := uuid.New().String()
	topicB := uuid.New().String()
	topicC := uuid.New().String()
	topicEmpty := uuid.New().String()
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

	msgA, rowA := seedSearchDecision(t, cs, searchSeed{projectID: projA, agentID: botA, agentName: "bot-a", chatKey: topicA, key: "a1", question: "A?"})
	msgB, rowB := seedSearchDecision(t, cs, searchSeed{projectID: projB, agentID: botB, agentName: "bot-b", chatKey: topicB, key: "b1", question: "B?"})
	_, rowC := seedSearchDecision(t, cs, searchSeed{projectID: projC, agentID: botC, agentName: "bot-c", chatKey: topicC, key: "c1", question: "C?"})
	ansA := answerSearchDecision(t, cs, projA, rowA, "yes", aliceID, "Alice", base)
	ansB := answerSearchDecision(t, cs, projB, rowB, "yes", aliceID, "Alice", base.Add(time.Hour))
	answerSearchDecision(t, cs, projC, rowC, "yes", aliceID, "Alice", base.Add(2*time.Hour))
	// One more open prompt per listed project.
	openA, _ := seedSearchDecision(t, cs, searchSeed{projectID: projA, agentID: botA, agentName: "bot-a", chatKey: topicA, key: "a2", question: "A2?"})
	openB, _ := seedSearchDecision(t, cs, searchSeed{projectID: projB, agentID: botB, agentName: "bot-b", chatKey: topicB, key: "b2", question: "B2?"})
	seedSearchDecision(t, cs, searchSeed{projectID: projC, agentID: botC, agentName: "bot-c", chatKey: topicC, key: "c2", question: "C2?"})
	_ = msgA
	_ = msgB

	projects := []string{projA, projB}
	// topicC is in the key union but projC is not listed: it is excluded.
	keys := []string{topicA, topicB, topicC, topicEmpty}

	prompts, err := cs.ListOpenPrompts(ctx, projects, keys)
	require.NoError(t, err)
	gotPrompts := map[string]string{}
	for _, p := range prompts {
		gotPrompts[p.MessageID] = p.ProjectID
	}
	assert.Equal(t, map[string]string{openA: projA, openB: projB}, gotPrompts)

	entries, _, err := cs.ListDecisionAnswers(ctx, projects, keys, store.ProjectDecisionAnswerFilter{}, "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{ansB, ansA}, answerIDs(entries))

	agents, answerers, convKeys, err := cs.ListDecisionFacets(ctx, projects, keys)
	require.NoError(t, err)
	var agentIDs []string
	for _, a := range agents {
		agentIDs = append(agentIDs, a.ID)
	}
	assert.ElementsMatch(t, []string{botA, botB}, agentIDs)
	require.Len(t, answerers, 1)
	assert.Equal(t, aliceID, answerers[0].ID)
	assert.ElementsMatch(t, []string{topicA, topicB}, convKeys, "only keys with decisions, never topicEmpty or another project's key")

	// Empty project list returns nothing without error.
	none, err := cs.ListOpenPrompts(ctx, nil, keys)
	require.NoError(t, err)
	assert.Empty(t, none)
	noEntries, _, err := cs.ListDecisionAnswers(ctx, nil, keys, store.ProjectDecisionAnswerFilter{}, "", 50)
	require.NoError(t, err)
	assert.Empty(t, noEntries)
}

func TestDecisionSearch_AgentFacetUsesNewestSnapshot(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-facet-snapshot")
	bot := uuid.New().String()
	topic := uuid.New().String()

	_, oldRow := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: bot, agentName: "zeta-old", chatKey: topic, key: "k1", question: "Old?"})
	_, newRow := seedSearchDecision(t, cs, searchSeed{projectID: projectID, agentID: bot, agentName: "alpha-new", chatKey: topic, key: "k2", question: "New?"})
	for row, ts := range map[string]time.Time{
		oldRow: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		newRow: time.Date(2026, 6, 1, 0, 0, 0, 500, time.UTC),
	} {
		_, err := cs.DB().ExecContext(ctx, rebindForDialect(cs.Dialect(), "UPDATE decisions SET created_at = ? WHERE id = ?"), ts, row)
		require.NoError(t, err)
	}

	agents, _, _, err := cs.ListDecisionFacets(ctx, []string{projectID}, []string{topic})
	require.NoError(t, err)
	require.Len(t, agents, 1)
	assert.Equal(t, "alpha-new", agents[0].Name, "the newest decision's snapshot wins, not the lexical maximum")
}

func TestDecisionAnswers_AskerAgentIndexExists(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	const name = "decisionanswer_project_id_asker_agent_id_created_at"
	var query string
	switch cs.Dialect() {
	case "postgres":
		query = "SELECT COUNT(*) FROM pg_indexes WHERE tablename = 'decision_answers' AND indexname = $1"
	default:
		query = "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'decision_answers' AND name = ?"
	}
	var n int
	require.NoError(t, cs.DB().QueryRowContext(ctx, query, name).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestDecisionSearch_MadeOtherTextAndPreviousOtherText(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-search-other")

	aliceID := uuid.New().String()
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID:          aliceID,
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Status:      "active",
		Created:     time.Now(),
	}))

	botID := uuid.New().String()
	topic := uuid.New().String()
	promptMsgID := uuid.New().String()
	promptRes, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID:        promptMsgID,
			ProjectID: projectID,
			Sender:    "agent:release-bot",
			SenderID:  botID,
			Recipient: "group:topic",
			ThreadID:  topic,
		},
		AskerAgentID:   botID,
		AskerAgentName: "release-bot",
		Decisions: []messages.Decision{{
			Key:      "region",
			Question: "Which region?",
			Choices:  []messages.Choice{{ID: "1", Label: "us-central1"}},
			Other:    &messages.OtherOption{Label: "Another region"},
		}},
	})
	require.NoError(t, err)
	rowID := promptRes.Decisions[0].RowID

	srcMsgID := uuid.New().String()
	require.NoError(t, cs.CreateMessage(ctx, &store.Message{
		ID:        srcMsgID,
		ProjectID: projectID,
		ThreadID:  topic,
		Sender:    "user:" + aliceID,
		SenderID:  aliceID,
		Recipient: "group:topic",
		Msg:       "use europe-west4",
	}))

	// Interpreted Other answer, then direct override to a different Other text.
	interpRes, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: botID,
		SourceMessageID:      srcMsgID,
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceSlack,
		Answers:              []store.InterpretedAnswerPair{{Key: "region", ChoiceID: messages.ChoiceIDOther, OtherText: "europe-west4"}},
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			ThreadID:  topic,
			Sender:    "agent:release-bot",
			Recipient: "group:topic",
			Msg:       "interpreted",
		},
	})
	require.NoError(t, err)
	interpAnsID := interpRes.Answers[0].ID

	overAns, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    rowID,
		ChoiceID:         messages.ChoiceIDOther,
		OtherText:        "asia-southeast1 canary",
		AnsweredByUserID: aliceID,
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Provenance:       store.DecisionProvenanceDirect,
		Outcome:          store.DecisionOutcomeOverride,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			ThreadID:  topic,
			Sender:    "user:Alice",
			Recipient: "agent:release-bot",
			Msg:       "override",
		},
	})
	require.NoError(t, err)

	// Search by word in override's other_text returns the folded override entry
	// with PreviousChoiceLabel, PreviousOtherText, and InterpretedAnswer.
	entries, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topic}, store.ProjectDecisionAnswerFilter{
		Query: "SOUTHEAST1",
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, overAns.ID, entries[0].Answer.ID)
	assert.Equal(t, "asia-southeast1 canary", entries[0].Answer.OtherText)
	assert.Equal(t, "Another region", entries[0].PreviousChoiceLabel)
	assert.Equal(t, "europe-west4", entries[0].PreviousOtherText)
	require.NotNil(t, entries[0].InterpretedAnswer)
	assert.Equal(t, interpAnsID, entries[0].InterpretedAnswer.ID)
	assert.Equal(t, "europe-west4", entries[0].InterpretedAnswer.OtherText)

	// Search by word in a superseded interpreted entry's other_text returns no
	// top-level entry after the Made fold (design-8443 §4.2, D2).
	supersededEntries, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topic}, store.ProjectDecisionAnswerFilter{
		Query: "EUROPE-WEST4",
	}, "", 50)
	require.NoError(t, err)
	assert.Empty(t, supersededEntries)

	// Search by word in an unconfirmed interpreted entry's other_text still finds it.
	unconfPrompt, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "agent:release-bot",
			SenderID:  botID,
			Recipient: "group:topic",
			ThreadID:  topic,
		},
		AskerAgentID:   botID,
		AskerAgentName: "release-bot",
		Decisions: []messages.Decision{{
			Key:      "region-unconf",
			Question: "Which fallback region?",
			Choices:  []messages.Choice{{ID: "1", Label: "us-central1"}},
			Other:    &messages.OtherOption{Label: "Another region"},
		}},
	})
	require.NoError(t, err)
	require.Len(t, unconfPrompt.Decisions, 1)

	unconfRes, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: botID,
		SourceMessageID:      srcMsgID,
		AnsweredByUserID:     aliceID,
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceSlack,
		Answers:              []store.InterpretedAnswerPair{{Key: "region-unconf", ChoiceID: messages.ChoiceIDOther, OtherText: "me-west1 fallback"}},
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			ThreadID:  topic,
			Sender:    "agent:release-bot",
			Recipient: "group:topic",
			Msg:       "interpreted unconfirmed",
		},
	})
	require.NoError(t, err)
	unconfAnsID := unconfRes.Answers[0].ID

	interpEntries, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, []string{topic}, store.ProjectDecisionAnswerFilter{
		Query: "ME-WEST1",
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, interpEntries, 1)
	assert.Equal(t, unconfAnsID, interpEntries[0].Answer.ID)
	assert.Equal(t, "me-west1 fallback", interpEntries[0].Answer.OtherText)
	assert.True(t, interpEntries[0].AwaitingConfirmation)
	assert.Empty(t, interpEntries[0].PreviousOtherText)
}
