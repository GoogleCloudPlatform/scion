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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListDecisionAnswers_IncludeExcludeFilterLists(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-filter-lists")
	projectID2 := backfillTestProject(t, cs, "decision-filter-lists-2")

	agent1 := uuid.New().String()
	agent2 := uuid.New().String()
	agent3 := uuid.New().String()

	aliceID := uuid.New().String()
	bobID := uuid.New().String()
	carolID := uuid.New().String()
	deletedUserID := uuid.New().String()
	for _, u := range []*store.User{
		{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now()},
		{ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Status: "active", Created: time.Now()},
		{ID: carolID, Email: "carol@example.com", DisplayName: "Carol", Status: "active", Created: time.Now()},
		{ID: deletedUserID, Email: "gone@example.com", DisplayName: "Gone User", Status: "active", Created: time.Now()},
	} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	topicGeneral := "topic-general"
	topicOps := "topic-ops"
	topicOtherProj := "topic-other-proj"

	createPrompt := func(pid, chatKey, agentID, agentName, key, question string) (string, string) {
		msgID := uuid.New().String()
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID:        msgID,
				ProjectID: pid,
				Sender:    "agent:" + agentName,
				SenderID:  agentID,
				Recipient: "group:topic",
				ThreadID:  chatKey,
			},
			AskerAgentID:   agentID,
			AskerAgentName: agentName,
			Decisions: []messages.Decision{
				{Key: key, Question: question, Choices: []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}},
			},
		})
		require.NoError(t, err)
		return msgID, res.Decisions[0].RowID
	}

	recordDirect := func(pid, rowID, choiceID, uid, name, email, outcome string) string {
		ans, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID:    rowID,
			ChoiceID:         choiceID,
			AnsweredByUserID: uid,
			AnsweredByName:   name,
			AnsweredByEmail:  email,
			Provenance:       store.DecisionProvenanceDirect,
			Outcome:          outcome,
			Surface:          store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID:        uuid.New().String(),
				ProjectID: pid,
				Sender:    "user:" + email,
				Recipient: "agent:asker",
				Msg:       "ans",
			},
		})
		require.NoError(t, err)
		return ans.ID
	}

	// Row 1: agent1, alice, direct, #general, "Rollout canary?"
	_, d1 := createPrompt(projectID, topicGeneral, agent1, "deploy-frontend", "D1", "Rollout canary?")
	recordDirect(projectID, d1, "yes", aliceID, "Alice", "alice@example.com", store.DecisionOutcomeAnswer)

	// Row 2: agent2, bob, confirmed (interpreted then confirmed), #ops, "Scale database?"
	_, d2 := createPrompt(projectID, topicOps, agent2, "db-ops", "D2", "Scale database?")
	replyID := uuid.New().String()
	require.NoError(t, cs.CreateMessage(ctx, &store.Message{
		ID:        replyID,
		ProjectID: projectID,
		Sender:    "user:bob@example.com",
		SenderID:  bobID,
		Recipient: "group:topic",
		ThreadID:  topicOps,
		Msg:       "yes",
	}))
	_, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agent2,
		SourceMessageID:      replyID,
		AnsweredByUserID:     bobID,
		AnsweredByName:       "Bob",
		AnsweredByEmail:      "bob@example.com",
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{Key: "D2", ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "agent:db-ops",
			SenderID:  agent2,
			Recipient: "group:topic",
			ThreadID:  topicOps,
			Msg:       "Interpreted D2=Yes",
		},
	})
	require.NoError(t, err)
	recordDirect(projectID, d2, "yes", bobID, "Bob", "bob@example.com", store.DecisionOutcomeConfirm)

	// Row 3: agent3, carol, dismissed, #general, "Clean legacy?"
	d3MsgID, d3 := createPrompt(projectID, topicGeneral, agent3, "cleanup-bot", "D3", "Clean legacy?")
	d3Row, err := cs.GetDecision(ctx, d3)
	require.NoError(t, err)
	_, err = cs.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: d3MsgID,
		RowIDs:    []string{d3},
		User: store.DecisionAnswerer{
			UserID: carolID,
			Name:   "Carol",
			Email:  "carol@example.com",
		},
		Reason:  "no longer needed",
		Surface: store.DecisionSurfaceNative,
		Now:     func() time.Time { return d3Row.CreatedAt.Add(store.DecisionDismissMinAge + time.Minute) },
		ResponseMessage: &store.Message{
			ID:        uuid.New().String(),
			ProjectID: projectID,
			Sender:    "user:carol@example.com",
			Recipient: "agent:cleanup-bot",
			Msg:       "Dismissed",
		},
	})
	require.NoError(t, err)

	// Row 4: agent1, deletedUser (user deleted -> answered_by_user_id IS NULL), direct, #ops, "Ship release?"
	_, d4 := createPrompt(projectID, topicOps, agent1, "deploy-frontend", "D4", "Ship release?")
	recordDirect(projectID, d4, "yes", deletedUserID, "Gone User", "gone@example.com", store.DecisionOutcomeAnswer)
	require.NoError(t, cs.DeleteUser(ctx, deletedUserID))

	// Row 5 in project 2 for multi-project ListDecisionAnswersForProjects parity.
	_, d5 := createPrompt(projectID2, topicOtherProj, agent2, "db-ops", "D5", "Rotate keys?")
	recordDirect(projectID2, d5, "no", aliceID, "Alice", "alice@example.com", store.DecisionOutcomeAnswer)

	keys := []string{topicGeneral, topicOps}
	entryKeys := func(entries []*store.ProjectDecisionAnswerEntry) []string {
		out := make([]string, len(entries))
		for i, e := range entries {
			out[i] = e.Decision.Key
		}
		return out
	}

	// 1. AgentIDs (IN)
	got, _, err := cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		AgentIDs: []string{agent1, agent3},
	}, "", 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D1", "D3", "D4"}, entryKeys(got))

	// 2. ExcludeAgentIDs (NOT IN)
	got, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		ExcludeAgentIDs: []string{agent1, agent3},
	}, "", 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D2"}, entryKeys(got))

	// 3. AgentID AND AgentIDs
	got, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		AgentID:  agent1,
		AgentIDs: []string{agent1, agent2},
	}, "", 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D1", "D4"}, entryKeys(got))

	// 4. UserIDs (IN)
	got, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		UserIDs: []string{aliceID, carolID},
	}, "", 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D1", "D3"}, entryKeys(got))

	// 5. ExcludeUserIDs (NOT IN) must preserve NULL answered_by_user_id (deleted user D4)!
	got, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		ExcludeUserIDs: []string{aliceID, bobID},
	}, "", 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D3", "D4"}, entryKeys(got))

	// 6. Provenances (OR)
	got, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		Provenances: []string{store.ProjectDecisionProvenanceConfirmed, store.ProjectDecisionProvenanceDismissed},
	}, "", 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D2", "D3"}, entryKeys(got))

	// 7. ExcludeProvenances (NOT)
	got, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		ExcludeProvenances: []string{store.ProjectDecisionProvenanceDirect, store.ProjectDecisionProvenanceDismissed},
	}, "", 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D2"}, entryKeys(got))

	// 8. ExcludeQueries (negated free text and negated conversation label match)
	got, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		ExcludeQueries: []store.ProjectDecisionExcludeQuery{
			{Text: "canary"},
			{Text: "ops-match", ChatKeys: []string{topicOps}},
		},
	}, "", 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D3"}, entryKeys(got))

	// 9. Multi-project ListDecisionAnswersForProjects with include/exclude lists
	gotMulti, _, err := cs.ListDecisionAnswersForProjects(
		ctx,
		[]string{projectID, projectID2},
		map[string][]string{
			projectID:  keys,
			projectID2: {topicOtherProj},
		},
		store.ProjectDecisionAnswerFilter{
			AgentIDs:           []string{agent1, agent2},
			ExcludeProvenances: []string{store.ProjectDecisionProvenanceConfirmed},
			ExcludeUserIDs:     []string{bobID},
		},
		"",
		20,
	)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"D1", "D4", "D5"}, entryKeys(gotMulti))

	// 10. Validation errors on invalid UUIDs / provenances
	_, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		AgentIDs: []string{"not-a-uuid"},
	}, "", 20)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	_, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		ExcludeAgentIDs: []string{"not-a-uuid"},
	}, "", 20)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	_, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		Provenances: []string{"bogus"},
	}, "", 20)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	_, _, err = cs.ListDecisionAnswers(ctx, []string{projectID}, keys, store.ProjectDecisionAnswerFilter{
		ExcludeProvenances: []string{""},
	}, "", 20)
	require.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestListDecisionFacets_UncappedAnswerersAndEmail(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID := backfillTestProject(t, cs, "decision-facets-600-answerers")
	agentID := uuid.New().String()
	topic := "topic-many-answerers"

	// Create 600 distinct answerers whose user IDs sort in reverse order of
	// their display names (e.g. uid "u-0599" has name "Ans 0000"), so an
	// ID-before-name cap at 500 would drop the alphabetically-first 100 names.
	const total = 600
	var lastAnswerID uuid.UUID
	for i := 0; i < total; i++ {
		uid := fmt.Sprintf("u-%04d", total-1-i)
		name := fmt.Sprintf("Ans %04d", i)
		email := fmt.Sprintf("ans%04d@example.com", i)
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID:        uuid.New().String(),
				ProjectID: projectID,
				Sender:    "agent:asker",
				SenderID:  agentID,
				Recipient: "group:topic",
				ThreadID:  topic,
			},
			AskerAgentID:   agentID,
			AskerAgentName: "asker",
			Decisions: []messages.Decision{
				{Key: fmt.Sprintf("D%d", i), Question: "Go?", Choices: []messages.Choice{{ID: "y", Label: "Yes"}}},
			},
		})
		require.NoError(t, err)
		ans, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID:    res.Decisions[0].RowID,
			ChoiceID:         "y",
			AnsweredByUserID: uid,
			AnsweredByName:   name,
			AnsweredByEmail:  email,
			Provenance:       store.DecisionProvenanceDirect,
			Outcome:          store.DecisionOutcomeAnswer,
			Surface:          store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID:        uuid.New().String(),
				ProjectID: projectID,
				Sender:    "user:" + email,
				Recipient: "agent:asker",
				Msg:       "y",
			},
		})
		require.NoError(t, err)
		lastAnswerID = uuid.MustParse(ans.ID)
	}

	// Indexed path (decision_facet_entries)
	_, answerers, _, err := cs.ListDecisionFacets(ctx, []string{projectID}, []string{topic})
	require.NoError(t, err)
	require.Len(t, answerers, total, "store must return all 600 answerers uncapped")
	assert.Equal(t, "Ans 0000", answerers[0].Name)
	assert.Equal(t, "ans0000@example.com", answerers[0].Email)
	assert.Equal(t, "u-0599", answerers[0].ID)
	assert.Equal(t, fmt.Sprintf("Ans %04d", total-1), answerers[total-1].Name)
	assert.Equal(t, fmt.Sprintf("ans%04d@example.com", total-1), answerers[total-1].Email)

	// Legacy fallback path: mark one answer facet_indexed=false and verify parity.
	_, err = cs.client.DecisionAnswer.UpdateOneID(lastAnswerID).SetFacetIndexed(false).Save(ctx)
	require.NoError(t, err)

	_, legacyAnswerers, _, err := cs.ListDecisionFacets(ctx, []string{projectID}, []string{topic})
	require.NoError(t, err)
	require.Len(t, legacyAnswerers, total, "legacy fallback must also return all 600 answerers uncapped")
	assert.Equal(t, answerers[0], legacyAnswerers[0])
	assert.Equal(t, answerers[total-1], legacyAnswerers[total-1])
}
