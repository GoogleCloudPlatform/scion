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
	"fmt"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// legacyListDecisionAnswers is the pre-P3 global-ORDER-BY single query, kept
// in tests as the differential oracle for M3 (design-scale §7 item 1).
func legacyListDecisionAnswers(
	ctx context.Context,
	cs *CompositeStore,
	projectIDs []string,
	chatKeys []string,
	filter store.ProjectDecisionAnswerFilter,
	cursor string,
	limit int,
) ([]*store.ProjectDecisionAnswerEntry, string, error) {
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, "", err
	}
	agentUUID, provPred, err := validateDecisionAnswerFilter(filter)
	if err != nil {
		return nil, "", err
	}
	var cur decisionListCursor
	if cursor != "" {
		cur, err = decodeDecisionListCursor(cursor)
		if err != nil {
			return nil, "", err
		}
	}
	keys := cleanChatKeys(chatKeys)
	if limit == 0 || len(keys) == 0 || len(projectUUIDs) == 0 {
		return nil, "", nil
	}
	q := cs.client.DecisionAnswer.Query().
		Where(
			decisionanswer.ProjectIDIn(projectUUIDs...),
			decisionAnswerInChatKeys(keys),
			notSupersededInterpretedAnswer(),
		)
	q = applyDecisionAnswerFilter(q, filter, agentUUID, provPred)
	if cursor != "" {
		q = q.Where(decisionAnswerCursorPredicate(cur))
	}
	rows, err := q.Order(ent.Desc(decisionanswer.FieldCreatedAt), ent.Desc(decisionanswer.FieldID)).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, "", mapError(err)
	}
	var nextCursor string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		nextCursor, err = encodeDecisionListCursor(last.CreatedAt, last.ID)
		if err != nil {
			return nil, "", err
		}
	}
	if len(rows) == 0 {
		return nil, "", nil
	}
	entries, err := cs.enrichDecisionAnswerRows(ctx, rows)
	return entries, nextCursor, err
}

// legacyListDecisionAnswersPerProject is the pre-P3 sequential per-project
// loop, kept in tests as the differential oracle for M3.
func legacyListDecisionAnswersPerProject(
	ctx context.Context,
	cs *CompositeStore,
	projectIDs []string,
	chatKeysByProject map[string][]string,
	filter store.ProjectDecisionAnswerFilter,
	perProject int,
) (map[string][]*store.ProjectDecisionAnswerEntry, error) {
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, err
	}
	agentUUID, provPred, err := validateDecisionAnswerFilter(filter)
	if err != nil {
		return nil, err
	}
	if perProject == 0 || len(projectUUIDs) == 0 || len(chatKeysByProject) == 0 {
		return nil, nil
	}
	cleanedQueryKeys := cleanChatKeys(filter.QueryChatKeys)
	var allRows []*ent.DecisionAnswer
	for _, pUUID := range projectUUIDs {
		pid := pUUID.String()
		projKeys := cleanChatKeys(chatKeysByProject[pid])
		if len(projKeys) == 0 {
			continue
		}
		projFilter := filter
		if filter.Query != "" && len(cleanedQueryKeys) > 0 {
			projKeySet := make(map[string]bool, len(projKeys))
			for _, k := range projKeys {
				projKeySet[k] = true
			}
			var qKeys []string
			for _, qk := range cleanedQueryKeys {
				if projKeySet[qk] {
					qKeys = append(qKeys, qk)
				}
			}
			projFilter.QueryChatKeys = qKeys
		}
		q := cs.client.DecisionAnswer.Query().
			Where(
				decisionanswer.ProjectIDEQ(pUUID),
				decisionAnswerInChatKeys(projKeys),
				notSupersededInterpretedAnswer(),
			)
		q = applyDecisionAnswerFilter(q, projFilter, agentUUID, provPred)
		rows, err := q.Order(ent.Desc(decisionanswer.FieldCreatedAt), ent.Desc(decisionanswer.FieldID)).
			Limit(perProject).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		allRows = append(allRows, rows...)
	}
	if len(allRows) == 0 {
		return nil, nil
	}
	entries, err := cs.enrichDecisionAnswerRows(ctx, allRows)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]*store.ProjectDecisionAnswerEntry, len(projectUUIDs))
	for _, e := range entries {
		out[e.Decision.ProjectID] = append(out[e.Decision.ProjectID], e)
	}
	return out, nil
}

func assertAnswerEntriesEqual(t *testing.T, want, got []*store.ProjectDecisionAnswerEntry, msg string) {
	t.Helper()
	require.Len(t, got, len(want), "%s: entry count mismatch", msg)
	for i := range want {
		w, g := want[i], got[i]
		assert.Equal(t, w.Answer.ID, g.Answer.ID, "%s[%d] answer ID", msg, i)
		assert.Equal(t, w.Decision.ID, g.Decision.ID, "%s[%d] decision ID", msg, i)
		assert.Equal(t, w.Decision.ProjectID, g.Decision.ProjectID, "%s[%d] project ID", msg, i)
		assert.Equal(t, w.Answer.ChoiceID, g.Answer.ChoiceID, "%s[%d] choice ID", msg, i)
		assert.Equal(t, w.Answer.ChoiceLabel, g.Answer.ChoiceLabel, "%s[%d] choice label", msg, i)
		assert.Equal(t, w.Answer.OtherText, g.Answer.OtherText, "%s[%d] other text", msg, i)
		assert.Equal(t, w.Answer.Provenance, g.Answer.Provenance, "%s[%d] provenance", msg, i)
		assert.Equal(t, w.Answer.Outcome, g.Answer.Outcome, "%s[%d] outcome", msg, i)
		assert.Equal(t, w.Answer.DismissReason, g.Answer.DismissReason, "%s[%d] dismiss reason", msg, i)
		assert.Equal(t, w.AwaitingConfirmation, g.AwaitingConfirmation, "%s[%d] awaiting confirm", msg, i)
		assert.Equal(t, w.PreviousChoiceLabel, g.PreviousChoiceLabel, "%s[%d] prev choice label", msg, i)
		assert.Equal(t, w.PreviousOtherText, g.PreviousOtherText, "%s[%d] prev other text", msg, i)
		assert.Equal(t, w.PromptExistsInMessages, g.PromptExistsInMessages, "%s[%d] prompt exists", msg, i)
		assert.Equal(t, w.SourceExistsInMessages, g.SourceExistsInMessages, "%s[%d] source exists", msg, i)
		assert.True(t, w.Answer.CreatedAt.Equal(g.Answer.CreatedAt), "%s[%d] created_at", msg, i)
		if w.InterpretedAnswer != nil {
			require.NotNil(t, g.InterpretedAnswer, "%s[%d] interpreted sibling", msg, i)
			assert.Equal(t, w.InterpretedAnswer.ID, g.InterpretedAnswer.ID, "%s[%d] interpreted sibling ID", msg, i)
		} else {
			assert.Nil(t, g.InterpretedAnswer, "%s[%d] interpreted sibling", msg, i)
		}
	}
}

func TestListProjectDecisionAgentsForProjects_ParityAndChunking(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	// Empty input returns nil without error.
	empty, err := cs.ListProjectDecisionAgentsForProjects(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, empty)

	// Invalid UUID fails cleanly.
	_, err = cs.ListProjectDecisionAgentsForProjects(ctx, []string{"not-a-uuid"})
	require.Error(t, err)

	// Seed 515 projects (exceeding chunk size 500) so chunking is exercised.
	const numProjects = 515
	projectIDs := make([]string, numProjects)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	for p := 0; p < numProjects; p++ {
		pid := uuid.New()
		projectIDs[p] = pid.String()
		if p == numProjects-1 {
			// Leave the last project with zero decisions to verify omission.
			continue
		}
		agA := uuid.New()
		agB := uuid.New()
		creates := []*ent.DecisionCreate{
			tx.Decision.Create().
				SetProjectID(pid).SetChatKey("t-" + pid.String()).SetMessageID(uuid.New()).SetPosition(0).
				SetAskerAgentID(agA).SetAskerAgentName("alpha").
				SetKey("k1").SetKeyNorm("k1").SetKeySource(decision.KeySourceAgent).SetVersion(1).
				SetQuestion("Q1?").SetChoices(choices).SetStatus(decision.StatusPending).SetCreatedAt(base),
		}
		if p%3 == 0 {
			creates = append(creates, tx.Decision.Create().
				SetProjectID(pid).SetChatKey("t-"+pid.String()).SetMessageID(uuid.New()).SetPosition(0).
				SetAskerAgentID(agB).SetAskerAgentName("beta").
				SetKey("k2").SetKeyNorm("k2").SetKeySource(decision.KeySourceAgent).SetVersion(1).
				SetQuestion("Q2?").SetChoices(choices).SetStatus(decision.StatusPending).SetCreatedAt(base.Add(time.Minute)))
		}
		require.NoError(t, tx.Decision.CreateBulk(creates...).Exec(ctx))
	}
	require.NoError(t, tx.Commit())

	// Include a duplicate project ID in the request to verify deduplication.
	reqIDs := append([]string{projectIDs[0]}, projectIDs...)
	got, err := cs.ListProjectDecisionAgentsForProjects(ctx, reqIDs)
	require.NoError(t, err)
	assert.Len(t, got, numProjects-1, "project with zero decisions must be omitted")
	assert.NotContains(t, got, projectIDs[numProjects-1])

	// Spot-check parity with single-project ListProjectDecisionAgents across chunk boundary.
	for _, idx := range []int{0, 1, 3, 499, 500, 501, numProjects - 2} {
		pid := projectIDs[idx]
		want, err := cs.ListProjectDecisionAgents(ctx, pid)
		require.NoError(t, err)
		assert.Equal(t, want, got[pid], "project index %d", idx)
	}
}

// TestDecisionAnswersP3_Differential verifies that multi-project
// ListDecisionAnswers and ListDecisionAnswersPerProject produce identical
// results and cursor pagination to the legacy implementations across a full
// lifecycle fixture including an unreadable-DM-heavy project (design-scale §7 item 1).
func TestDecisionAnswersP3_Differential(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	pDMHeavy := backfillTestProject(t, cs, "p3-diff-dm-heavy")
	pLifecycle := backfillTestProject(t, cs, "p3-diff-lifecycle")
	pTies := backfillTestProject(t, cs, "p3-diff-ties")
	pEmpty := backfillTestProject(t, cs, "p3-diff-empty")
	projects := []string{pDMHeavy, pLifecycle, pTies, pEmpty}

	aliceID := uuid.NewString()
	bobID := uuid.NewString()
	deletedUserID := uuid.NewString()
	for _, u := range []*store.User{
		{ID: aliceID, Email: "alice@example.com", DisplayName: "Alice", Status: "active", Created: time.Now()},
		{ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Status: "active", Created: time.Now()},
		{ID: deletedUserID, Email: "del@example.com", DisplayName: "Deleted User", Status: "active", Created: time.Now()},
	} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	ag0 := uuid.NewString()
	ag1 := uuid.NewString()
	ag2 := uuid.NewString()

	topicDMHeavy := uuid.NewString()
	topicLifecycle := uuid.NewString()
	topicTies := uuid.NewString()
	dmAlice0 := "dm:agent:" + ag0 + ":user:" + aliceID
	dmBob0 := "dm:agent:" + ag0 + ":user:" + bobID
	dmAlice1 := "dm:agent:" + ag1 + ":user:" + aliceID

	base := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	// Helper to insert a decision + optional answer directly so timestamps and
	// edge cases (tie timestamps, NULL chat_key, cleared user, interpreted/override/dismiss)
	// are exact.
	insertDec := func(pid, chatKey, agID, agName, key, question string, status decision.Status, settled bool, ts time.Time) uuid.UUID {
		t.Helper()
		decID := uuid.New()
		dc := cs.client.Decision.Create().
			SetID(decID).
			SetProjectID(uuid.MustParse(pid)).
			SetMessageID(uuid.New()).
			SetPosition(0).
			SetAskerAgentID(uuid.MustParse(agID)).
			SetAskerAgentName(agName).
			SetKey(key).SetKeyNorm(key).SetKeySource(decision.KeySourceAgent).SetVersion(1).
			SetQuestion(question).SetChoices(choices).
			SetStatus(status).SetSettled(settled).SetCreatedAt(ts)
		if chatKey != "" {
			dc.SetChatKey(chatKey)
		}
		_, err := dc.Save(ctx)
		require.NoError(t, err)
		return decID
	}
	insertAns := func(decID uuid.UUID, pid, agID, key, qText, choiceID, choiceLabel, otherText, dismissReason, userID, userName string, prov decisionanswer.Provenance, outcome decisionanswer.Outcome, ts time.Time) uuid.UUID {
		t.Helper()
		ansID := uuid.New()
		ac := cs.client.DecisionAnswer.Create().
			SetID(ansID).
			SetDecisionRowID(decID).
			SetProjectID(uuid.MustParse(pid)).
			SetAskerAgentID(uuid.MustParse(agID)).
			SetKey(key).SetVersion(1).SetQuestionText(qText).
			SetChoiceID(choiceID).SetChoiceLabel(choiceLabel).
			SetAnsweredByName(userName).SetAnsweredByEmail(strings.ToLower(userName) + "@example.com").
			SetProvenance(prov).SetOutcome(outcome).
			SetSurface(decisionanswer.SurfaceDecisionsPage).
			SetResponseMessageID(uuid.New()).
			SetCreatedAt(ts)
		if userID != "" {
			ac.SetAnsweredByUserID(userID)
		}
		if otherText != "" {
			ac.SetOtherText(otherText)
		}
		if dismissReason != "" {
			ac.SetDismissReason(dismissReason)
		}
		if prov == decisionanswer.ProvenanceAgentInterpreted {
			ac.SetInterpretedByAgentID(uuid.MustParse(agID)).SetSourceMessageID(uuid.New())
		}
		_, err := ac.Save(ctx)
		require.NoError(t, err)
		return ansID
	}

	// 1. DM-heavy project: 30 unreadable Bob-DM answers newer than 3 readable Alice rows.
	for i := 0; i < 30; i++ {
		ts := base.Add(time.Duration(100+i) * time.Minute)
		d := insertDec(pDMHeavy, dmBob0, ag0, "agent-dm", fmt.Sprintf("bob-dm-%d", i), "Secret Bob Q?", decision.StatusDecided, true, ts)
		insertAns(d, pDMHeavy, ag0, fmt.Sprintf("bob-dm-%d", i), "Secret Bob Q?", "yes", "Yes", "", "", bobID, "Bob", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeAnswer, ts)
	}
	for i := 0; i < 3; i++ {
		ts := base.Add(time.Duration(10+i) * time.Minute)
		ck := topicDMHeavy
		if i == 2 {
			ck = dmAlice0
		}
		d := insertDec(pDMHeavy, ck, ag0, "agent-dm", fmt.Sprintf("alice-vis-%d", i), fmt.Sprintf("Visible deploy %d?", i), decision.StatusDecided, true, ts)
		insertAns(d, pDMHeavy, ag0, fmt.Sprintf("alice-vis-%d", i), fmt.Sprintf("Visible deploy %d?", i), "yes", "Yes", "", "", aliceID, "Alice", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeAnswer, ts)
	}

	// 2. Lifecycle project: direct, $other, interpreted awaiting, confirmed, overridden,
	//    dismissed (incl. superseding interpreted), withdrawn, replaced, NULL chat_key, deleted user.
	dDirect := insertDec(pLifecycle, topicLifecycle, ag1, "agent-life", "direct-1", "Ship canary?", decision.StatusDecided, true, base.Add(20*time.Minute))
	insertAns(dDirect, pLifecycle, ag1, "direct-1", "Ship canary?", "yes", "Yes", "", "", aliceID, "Alice", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeAnswer, base.Add(21*time.Minute))

	dOther := insertDec(pLifecycle, dmAlice1, ag1, "agent-life", "other-1", "Custom region?", decision.StatusDecided, true, base.Add(22*time.Minute))
	insertAns(dOther, pLifecycle, ag1, "other-1", "Custom region?", "$other", "Other", "us-west4", "", bobID, "Bob", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeAnswer, base.Add(23*time.Minute))

	dInterp := insertDec(pLifecycle, topicLifecycle, ag1, "agent-life", "interp-1", "Awaiting confirm?", decision.StatusDecided, false, base.Add(24*time.Minute))
	insertAns(dInterp, pLifecycle, ag1, "interp-1", "Awaiting confirm?", "yes", "Yes", "", "", aliceID, "Alice", decisionanswer.ProvenanceAgentInterpreted, decisionanswer.OutcomeAnswer, base.Add(25*time.Minute))

	dConfirm := insertDec(pLifecycle, topicLifecycle, ag1, "agent-life", "confirm-1", "Confirmed deploy?", decision.StatusDecided, true, base.Add(26*time.Minute))
	insertAns(dConfirm, pLifecycle, ag1, "confirm-1", "Confirmed deploy?", "yes", "Yes", "", "", bobID, "Bob", decisionanswer.ProvenanceAgentInterpreted, decisionanswer.OutcomeAnswer, base.Add(27*time.Minute))
	insertAns(dConfirm, pLifecycle, ag1, "confirm-1", "Confirmed deploy?", "yes", "Yes", "", "", aliceID, "Alice", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeConfirm, base.Add(28*time.Minute))

	dOverride := insertDec(pLifecycle, topicLifecycle, ag1, "agent-life", "override-1", "Overridden deploy?", decision.StatusDecided, true, base.Add(29*time.Minute))
	insertAns(dOverride, pLifecycle, ag1, "override-1", "Overridden deploy?", "$other", "Other", "eu-west1", "", bobID, "Bob", decisionanswer.ProvenanceAgentInterpreted, decisionanswer.OutcomeAnswer, base.Add(30*time.Minute))
	insertAns(dOverride, pLifecycle, ag1, "override-1", "Overridden deploy?", "no", "No", "", "", aliceID, "Alice", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeOverride, base.Add(31*time.Minute))

	dDismiss := insertDec(pLifecycle, topicLifecycle, ag1, "agent-life", "dismiss-1", "Dismissed prompt?", decision.StatusDismissed, true, base.Add(32*time.Minute))
	insertAns(dDismiss, pLifecycle, ag1, "dismiss-1", "Dismissed prompt?", "yes", "Yes", "", "", bobID, "Bob", decisionanswer.ProvenanceAgentInterpreted, decisionanswer.OutcomeAnswer, base.Add(33*time.Minute))
	insertAns(dDismiss, pLifecycle, ag1, "dismiss-1", "Dismissed prompt?", "$dismissed", "Dismissed", "", "no longer needed", aliceID, "Alice", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeDismiss, base.Add(34*time.Minute))

	// Withdrawn and pending rows (no answers).
	insertDec(pLifecycle, topicLifecycle, ag1, "agent-life", "withdrawn-1", "Withdrawn?", decision.StatusWithdrawn, true, base.Add(35*time.Minute))
	insertDec(pLifecycle, topicLifecycle, ag1, "agent-life", "pending-1", "Pending?", decision.StatusPending, false, base.Add(36*time.Minute))

	// NULL chat_key row (must never leak into any readable key query).
	dNullKey := insertDec(pLifecycle, "", ag1, "agent-life", "null-ck", "Null chat key?", decision.StatusDecided, true, base.Add(37*time.Minute))
	insertAns(dNullKey, pLifecycle, ag1, "null-ck", "Null chat key?", "yes", "Yes", "", "", aliceID, "Alice", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeAnswer, base.Add(38*time.Minute))

	// Deleted user answer (answered_by_user_id cleared, snapshot retained).
	dDelUser := insertDec(pLifecycle, topicLifecycle, ag1, "agent-life", "del-user", "Deleted user Q?", decision.StatusDecided, true, base.Add(39*time.Minute))
	insertAns(dDelUser, pLifecycle, ag1, "del-user", "Deleted user Q?", "yes", "Yes", "", "", "", "Deleted User", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeAnswer, base.Add(40*time.Minute))

	// 3. Tie timestamps across projects (pTies + pLifecycle share exact created_at).
	tieTS := base.Add(50 * time.Minute)
	for i := 0; i < 4; i++ {
		pid := pTies
		ck := topicTies
		ag := ag2
		if i%2 == 1 {
			pid = pLifecycle
			ck = topicLifecycle
			ag = ag1
		}
		d := insertDec(pid, ck, ag, "agent-tie", fmt.Sprintf("tie-%d", i), "Tie Q?", decision.StatusDecided, true, tieTS)
		insertAns(d, pid, ag, fmt.Sprintf("tie-%d", i), "Tie Q?", "yes", "Yes", "", "", aliceID, "Alice", decisionanswer.ProvenanceDirect, decisionanswer.OutcomeAnswer, tieTS)
	}

	aliceAllKeys := []string{topicDMHeavy, dmAlice0, topicLifecycle, dmAlice1, topicTies}
	aliceKeysByProj := map[string][]string{
		pDMHeavy:   {topicDMHeavy, dmAlice0},
		pLifecycle: {topicLifecycle, dmAlice1},
		pTies:      {topicTies},
		pEmpty:     nil,
	}

	// Differential pagination over ListDecisionAnswers and ListDecisionAnswersForProjects.
	t.Run("ListDecisionAnswers_pagination_unfiltered", func(t *testing.T) {
		curNew, curProj, curOld := "", "", ""
		for page := 0; page < 20; page++ {
			got, nextNew, err := cs.ListDecisionAnswers(ctx, projects, aliceAllKeys, store.ProjectDecisionAnswerFilter{}, curNew, 2)
			require.NoError(t, err)
			gotProj, nextProj, err := cs.ListDecisionAnswersForProjects(ctx, projects, aliceKeysByProj, store.ProjectDecisionAnswerFilter{}, curProj, 2)
			require.NoError(t, err)
			want, nextOld, err := legacyListDecisionAnswers(ctx, cs, projects, aliceAllKeys, store.ProjectDecisionAnswerFilter{}, curOld, 2)
			require.NoError(t, err)
			assertAnswerEntriesEqual(t, want, got, fmt.Sprintf("flat page %d", page))
			assertAnswerEntriesEqual(t, want, gotProj, fmt.Sprintf("per-project page %d", page))
			assert.Equal(t, nextOld, nextNew, "flat page %d nextCursor", page)
			assert.Equal(t, nextOld, nextProj, "per-project page %d nextCursor", page)
			if nextNew == "" {
				break
			}
			curNew, curProj, curOld = nextNew, nextProj, nextOld
		}
	})

	// Differential filters for ListDecisionAnswers and ListDecisionAnswersPerProject.
	fromBound := base.Add(22 * time.Minute)
	toBound := base.Add(45 * time.Minute)
	filters := []struct {
		name   string
		filter store.ProjectDecisionAnswerFilter
	}{
		{name: "agent", filter: store.ProjectDecisionAnswerFilter{AgentID: ag1}},
		{name: "user_alice", filter: store.ProjectDecisionAnswerFilter{UserID: aliceID}},
		{name: "prov_direct", filter: store.ProjectDecisionAnswerFilter{Provenance: store.ProjectDecisionProvenanceDirect}},
		{name: "prov_interpreted", filter: store.ProjectDecisionAnswerFilter{Provenance: store.ProjectDecisionProvenanceInterpreted}},
		{name: "prov_confirmed", filter: store.ProjectDecisionAnswerFilter{Provenance: store.ProjectDecisionProvenanceConfirmed}},
		{name: "prov_overridden", filter: store.ProjectDecisionAnswerFilter{Provenance: store.ProjectDecisionProvenanceOverridden}},
		{name: "prov_dismissed", filter: store.ProjectDecisionAnswerFilter{Provenance: store.ProjectDecisionProvenanceDismissed}},
		{name: "date_range", filter: store.ProjectDecisionAnswerFilter{From: &fromBound, To: &toBound}},
		{name: "query_text", filter: store.ProjectDecisionAnswerFilter{Query: "us-west4"}},
		{name: "query_dismiss", filter: store.ProjectDecisionAnswerFilter{Query: "longer needed"}},
		{name: "query_chat_keys", filter: store.ProjectDecisionAnswerFilter{Query: "dm-match", QueryChatKeys: []string{dmAlice0, dmAlice1}}},
	}

	for _, tc := range filters {
		t.Run("filter_"+tc.name, func(t *testing.T) {
			got, nextNew, err := cs.ListDecisionAnswers(ctx, projects, aliceAllKeys, tc.filter, "", 50)
			require.NoError(t, err)
			want, nextOld, err := legacyListDecisionAnswers(ctx, cs, projects, aliceAllKeys, tc.filter, "", 50)
			require.NoError(t, err)
			assertAnswerEntriesEqual(t, want, got, "ListDecisionAnswers/"+tc.name)
			assert.Equal(t, nextOld, nextNew)

			gotPP, err := cs.ListDecisionAnswersPerProject(ctx, projects, aliceKeysByProj, tc.filter, 4)
			require.NoError(t, err)
			wantPP, err := legacyListDecisionAnswersPerProject(ctx, cs, projects, aliceKeysByProj, tc.filter, 4)
			require.NoError(t, err)
			require.Len(t, gotPP, len(wantPP), "ListDecisionAnswersPerProject/%s project count", tc.name)
			for pid, wantEntries := range wantPP {
				assertAnswerEntriesEqual(t, wantEntries, gotPP[pid], "ListDecisionAnswersPerProject/"+tc.name+"/"+pid)
			}
		})
	}
}

// TestDecisionAnswersP3_MultiChunkMerge verifies chunking (>100 arms) for both
// ListDecisionAnswers (in-memory merge + cursor pagination across chunks) and
// ListDecisionAnswersPerProject.
func TestDecisionAnswersP3_MultiChunkMerge(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	const numProjects = 105 // > maxDecisionAnswerUnionArms (100) -> 2 chunks
	projectIDs := make([]string, numProjects)
	keysByProject := make(map[string][]string, numProjects)
	allKeys := make([]string, 0, numProjects)
	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}}
	uid := uuid.NewString()

	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	for p := 0; p < numProjects; p++ {
		pid := uuid.New()
		projectIDs[p] = pid.String()
		ck := fmt.Sprintf("topic-chunk-%03d", p)
		keysByProject[pid.String()] = []string{ck}
		allKeys = append(allKeys, ck)
		agID := uuid.New()

		for j := 0; j < 3; j++ {
			decID := uuid.New()
			// Interleave timestamps across projects so chunk 2 holds rows
			// both newer and older than chunk 1.
			ts := base.Add(time.Duration(j*1000+p) * time.Second)
			key := fmt.Sprintf("k-%d-%d", p, j)
			_, err := tx.Decision.Create().
				SetID(decID).SetProjectID(pid).SetChatKey(ck).SetMessageID(uuid.New()).SetPosition(0).
				SetAskerAgentID(agID).SetAskerAgentName("bot").
				SetKey(key).SetKeyNorm(key).SetKeySource(decision.KeySourceAgent).SetVersion(1).
				SetQuestion("Q?").SetChoices(choices).
				SetStatus(decision.StatusDecided).SetSettled(true).SetCreatedAt(ts).
				Save(ctx)
			require.NoError(t, err)
			_, err = tx.DecisionAnswer.Create().
				SetDecisionRowID(decID).SetProjectID(pid).SetAskerAgentID(agID).
				SetKey(key).SetVersion(1).SetQuestionText("Q?").
				SetChoiceID("yes").SetChoiceLabel("Yes").
				SetAnsweredByUserID(uid).SetAnsweredByName("User").
				SetProvenance(decisionanswer.ProvenanceDirect).SetOutcome(decisionanswer.OutcomeAnswer).
				SetSurface(decisionanswer.SurfaceDecisionsPage).SetResponseMessageID(uuid.New()).
				SetCreatedAt(ts).
				Save(ctx)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tx.Commit())

	// Walk 3 pages of 10 with ListDecisionAnswers and assert exact parity with legacy.
	curNew, curOld := "", ""
	for page := 0; page < 3; page++ {
		got, nextNew, err := cs.ListDecisionAnswers(ctx, projectIDs, allKeys, store.ProjectDecisionAnswerFilter{}, curNew, 10)
		require.NoError(t, err)
		want, nextOld, err := legacyListDecisionAnswers(ctx, cs, projectIDs, allKeys, store.ProjectDecisionAnswerFilter{}, curOld, 10)
		require.NoError(t, err)
		assertAnswerEntriesEqual(t, want, got, fmt.Sprintf("chunked page %d", page))
		assert.Equal(t, nextOld, nextNew)
		curNew, curOld = nextNew, nextOld
	}

	// ListDecisionAnswersPerProject returns all 105 projects with 2 newest each.
	gotPP, err := cs.ListDecisionAnswersPerProject(ctx, projectIDs, keysByProject, store.ProjectDecisionAnswerFilter{}, 2)
	require.NoError(t, err)
	require.Len(t, gotPP, numProjects)
	for _, pid := range projectIDs {
		entries := gotPP[pid]
		require.Len(t, entries, 2, "project %s", pid)
		assert.True(t, entries[0].Answer.CreatedAt.After(entries[1].Answer.CreatedAt), "project %s newest first", pid)
	}
}

// txPlanRecordingDriver wraps planRecordingDriver so queries executed inside
// read-only transactions (such as Postgres SET LOCAL jit = off) are also
// recorded for EXPLAIN inspection.
type txPlanRecordingDriver struct {
	*planRecordingDriver
}

type txPlanRecordingTx struct {
	dialect.Tx
	rec *planRecordingDriver
}

func (t *txPlanRecordingTx) Query(ctx context.Context, query string, args, v any) error {
	var copied []any
	if s, ok := args.([]any); ok {
		copied = append([]any(nil), s...)
	}
	t.rec.mu.Lock()
	t.rec.queries = append(t.rec.queries, recordedQuery{sql: query, args: copied})
	t.rec.mu.Unlock()
	return t.Tx.Query(ctx, query, args, v)
}

func (d *txPlanRecordingDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return &txPlanRecordingTx{Tx: tx, rec: d.planRecordingDriver}, nil
}

func (d *txPlanRecordingDriver) BeginTx(ctx context.Context, opts *sql.TxOptions) (dialect.Tx, error) {
	type txBeginner interface {
		BeginTx(context.Context, *sql.TxOptions) (dialect.Tx, error)
	}
	if b, ok := d.Driver.(txBeginner); ok {
		tx, err := b.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &txPlanRecordingTx{Tx: tx, rec: d.planRecordingDriver}, nil
	}
	return d.Tx(ctx)
}

// TestDecisionAnswersP3_QueryPlans pins that multi-project ListDecisionAnswers
// (made/recent) and ListDecisionAnswersPerProject (made/project) use the
// per-project (project_id, created_at) index (and (project_id, asker_agent_id,
// created_at) with the agent filter) with no full-table scan or temp B-tree
// over the full decision_answers set on SQLite and Postgres (design-scale §7 item 5).
func TestDecisionAnswersP3_QueryPlans(t *testing.T) {
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	recClient := ent.NewClient(ent.Driver(&txPlanRecordingDriver{planRecordingDriver: rec}))
	cs := NewCompositeStore(recClient)

	const numProjects = 5
	const rowsPerProject = 1000
	projectIDs := make([]string, numProjects)
	keysByProject := make(map[string][]string, numProjects)
	allKeys := make([]string, 0, numProjects*2)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}}
	baseTime := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	uid := uuid.NewString()
	filterAgentUUID := uuid.New()
	filterAgentID := filterAgentUUID.String()

	tx, err := base.Tx(ctx)
	require.NoError(t, err)
	for p := 0; p < numProjects; p++ {
		pid := uuid.New()
		projectIDs[p] = pid.String()
		k1 := fmt.Sprintf("plan-topic-%d-a", p)
		k2 := fmt.Sprintf("plan-topic-%d-b", p)
		keysByProject[pid.String()] = []string{k1, k2}
		allKeys = append(allKeys, k1, k2)
		otherAgentUUID := uuid.New()
		decCreates := make([]*ent.DecisionCreate, 0, rowsPerProject)
		ansCreates := make([]*ent.DecisionAnswerCreate, 0, rowsPerProject)
		for j := 0; j < rowsPerProject; j++ {
			decID := uuid.New()
			ck := k1
			if j%2 == 1 {
				ck = k2
			}
			agID := filterAgentUUID
			if j%2 == 1 {
				agID = otherAgentUUID
			}
			ts := baseTime.Add(time.Duration(p*1000+j) * time.Second)
			key := fmt.Sprintf("pk-%d-%d", p, j)
			decCreates = append(decCreates, tx.Decision.Create().
				SetID(decID).SetProjectID(pid).SetChatKey(ck).SetMessageID(uuid.New()).SetPosition(0).
				SetAskerAgentID(agID).SetAskerAgentName("bot").
				SetKey(key).SetKeyNorm(key).SetKeySource(decision.KeySourceAgent).SetVersion(1).
				SetQuestion("Q?").SetChoices(choices).
				SetStatus(decision.StatusDecided).SetSettled(true).SetCreatedAt(ts))
			ansCreates = append(ansCreates, tx.DecisionAnswer.Create().
				SetDecisionRowID(decID).SetProjectID(pid).SetAskerAgentID(agID).
				SetKey(key).SetVersion(1).SetQuestionText("Q?").
				SetChoiceID("yes").SetChoiceLabel("Yes").
				SetAnsweredByUserID(uid).SetAnsweredByName("User").
				SetProvenance(decisionanswer.ProvenanceDirect).SetOutcome(decisionanswer.OutcomeAnswer).
				SetSurface(decisionanswer.SurfaceDecisionsPage).SetResponseMessageID(uuid.New()).
				SetCreatedAt(ts))
		}
		require.NoError(t, tx.Decision.CreateBulk(decCreates...).Exec(ctx))
		require.NoError(t, tx.DecisionAnswer.CreateBulk(ansCreates...).Exec(ctx))
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, base.Driver().Exec(ctx, "ANALYZE", []any{}, nil))

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

	assertArmIndexPlan := func(t *testing.T, label, wantIndex string, strictPGBackward bool, run func()) {
		t.Helper()
		rec.mu.Lock()
		rec.queries = nil
		rec.mu.Unlock()

		run()

		rec.mu.Lock()
		queries := append([]recordedQuery(nil), rec.queries...)
		rec.mu.Unlock()
		require.NotEmpty(t, queries, "%s must emit at least one query", label)

		// First query is the compound UNION ALL over decision_answers.
		q := queries[0]
		plan := explain(t, q)
		t.Logf("%s (%s) PLAN:\n%s", label, base.Driver().Dialect(), plan)
		if base.Driver().Dialect() == dialect.SQLite {
			assert.Regexp(t, `\b`+wantIndex+`\b`, plan, "%s must use index %s; plan:\n%s", label, wantIndex, plan)
			assert.NotContains(t, plan, "SCAN decision_answers", "%s must not full-scan decision_answers", label)
			// Each per-project arm must seek decision_answers via wantIndex
			// (at most USE TEMP B-TREE FOR LAST TERM OF ORDER BY for the id
			// tie-breaker) and never build a full-set temp B-tree over
			// decision_answers.
			assert.GreaterOrEqual(t, strings.Count(plan, "SEARCH decision_answers USING INDEX "+wantIndex), numProjects,
				"%s must seek %s in every project arm; plan:\n%s", label, wantIndex, plan)
			// Inside a CO-ROUTINE arm (between "CO-ROUTINE a" and "SCAN a"),
			// a full "USE TEMP B-TREE FOR ORDER BY" (without "LAST TERM") would
			// mean sorting the whole arm instead of walking the index.
			inArm := false
			for _, line := range strings.Split(plan, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "CO-ROUTINE a") {
					inArm = true
				} else if strings.HasPrefix(trimmed, "SCAN a") {
					inArm = false
				} else if inArm && strings.Contains(trimmed, "USE TEMP B-TREE FOR ORDER BY") {
					assert.Failf(t, "full temp B-tree inside arm", "%s built full temp B-tree inside arm: %s\nfull plan:\n%s", label, trimmed, plan)
				}
			}
		} else {
			if strictPGBackward {
				assert.Contains(t, plan, "Index Scan Backward using "+wantIndex,
					"%s must use Index Scan Backward on %s; plan:\n%s", label, wantIndex, plan)
			} else {
				assert.Contains(t, plan, "Index Scan",
					"%s must use an indexed plan on Postgres; plan:\n%s", label, plan)
			}
			assert.NotContains(t, plan, "Seq Scan on decision_answers decision_answers",
				"%s must not seq-scan outer decision_answers arm on Postgres; plan:\n%s", label, plan)
		}
	}

	t.Run("ListDecisionAnswersForProjects_made_recent", func(t *testing.T) {
		assertArmIndexPlan(t, "ListDecisionAnswersForProjects(multi)", "decisionanswer_project_id_created_at", true, func() {
			got, _, err := cs.ListDecisionAnswersForProjects(ctx, projectIDs, keysByProject, store.ProjectDecisionAnswerFilter{}, "", 20)
			require.NoError(t, err)
			require.Len(t, got, 20)
		})
	})

	t.Run("ListDecisionAnswers_multi_project_made_recent", func(t *testing.T) {
		assertArmIndexPlan(t, "ListDecisionAnswers(multi)", "decisionanswer_project_id_created_at", true, func() {
			got, _, err := cs.ListDecisionAnswers(ctx, projectIDs, allKeys, store.ProjectDecisionAnswerFilter{}, "", 20)
			require.NoError(t, err)
			require.Len(t, got, 20)
		})
	})

	t.Run("ListDecisionAnswers_multi_project_agent_filter", func(t *testing.T) {
		assertArmIndexPlan(t, "ListDecisionAnswers(multi,agent)", "decisionanswer_project_id_asker_agent_id_created_at", true, func() {
			got, _, err := cs.ListDecisionAnswersForProjects(ctx, projectIDs, keysByProject, store.ProjectDecisionAnswerFilter{AgentID: filterAgentID}, "", 20)
			require.NoError(t, err)
			require.Len(t, got, 20)
		})
	})

	t.Run("ListDecisionAnswersPerProject_made_project", func(t *testing.T) {
		assertArmIndexPlan(t, "ListDecisionAnswersPerProject", "decisionanswer_project_id_created_at", true, func() {
			got, err := cs.ListDecisionAnswersPerProject(ctx, projectIDs, keysByProject, store.ProjectDecisionAnswerFilter{}, 6)
			require.NoError(t, err)
			require.Len(t, got, numProjects)
		})
	})
}
