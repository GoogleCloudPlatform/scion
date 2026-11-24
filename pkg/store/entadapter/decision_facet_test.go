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
	"math/rand"
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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionfacetentry"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

const (
	decisionFacetPartialIndex       = "decision_project_id"
	decisionAnswerFacetPartialIndex = "decisionanswer_project_id"
	facetUniqueIndex                = "decisionfacetentry_project_id_chat_key_kind_actor_id"
	facetProjectKindIndex           = "decisionfacetentry_project_id_kind_actor_id"
)

type facetSummarySnapshot struct {
	ProjectID uuid.UUID
	ChatKey   string
	Kind      decisionfacetentry.Kind
	ActorID   string
	Name      string
	Email     string
	LatestAt  time.Time
	RefCount  int
}

// recomputeFacetSummaryFromTables computes the exact expected
// decision_facet_entries rows from scratch directly from decisions and
// decision_answers using the §3.2 contribution rule.
func recomputeFacetSummaryFromTables(t *testing.T, ctx context.Context, client *ent.Client) map[facetEntryKey]facetSummarySnapshot {
	t.Helper()
	decs, err := client.Decision.Query().All(ctx)
	require.NoError(t, err)
	anss, err := client.DecisionAnswer.Query().All(ctx)
	require.NoError(t, err)

	decByID := make(map[uuid.UUID]*ent.Decision, len(decs))
	hasDirect := make(map[uuid.UUID]bool)
	for _, a := range anss {
		if a.Provenance == decisionanswer.ProvenanceDirect {
			hasDirect[a.DecisionRowID] = true
		}
	}

	out := make(map[facetEntryKey]facetSummarySnapshot)
	add := func(k facetEntryKey, name, email string, ts time.Time) {
		ts = ts.UTC().Truncate(time.Microsecond)
		cur, ok := out[k]
		if !ok {
			out[k] = facetSummarySnapshot{
				ProjectID: k.ProjectID,
				ChatKey:   k.ChatKey,
				Kind:      k.Kind,
				ActorID:   k.ActorID,
				Name:      name,
				Email:     email,
				LatestAt:  ts,
				RefCount:  1,
			}
			return
		}
		cur.RefCount++
		if facetDeltaNewer(ts, name, email, cur.LatestAt, cur.Name, cur.Email) {
			cur.Name = name
			cur.Email = email
			cur.LatestAt = ts
		}
		out[k] = cur
	}

	for _, d := range decs {
		decByID[d.ID] = d
		if d.ChatKey == nil || *d.ChatKey == "" {
			continue
		}
		add(facetEntryKey{
			ProjectID: d.ProjectID,
			ChatKey:   *d.ChatKey,
			Kind:      decisionfacetentry.KindAsker,
			ActorID:   d.AskerAgentID.String(),
		}, d.AskerAgentName, "", d.CreatedAt)
	}

	for _, a := range anss {
		if a.AnsweredByUserID == nil || *a.AnsweredByUserID == "" {
			continue
		}
		if a.Provenance == decisionanswer.ProvenanceAgentInterpreted && hasDirect[a.DecisionRowID] {
			continue
		}
		d := decByID[a.DecisionRowID]
		if d == nil || d.ChatKey == nil || *d.ChatKey == "" {
			continue
		}
		add(facetEntryKey{
			ProjectID: d.ProjectID,
			ChatKey:   *d.ChatKey,
			Kind:      decisionfacetentry.KindAnswerer,
			ActorID:   *a.AnsweredByUserID,
		}, a.AnsweredByName, a.AnsweredByEmail, a.CreatedAt)
	}
	return out
}

func assertFacetSummaryMatchesRecompute(t *testing.T, ctx context.Context, client *ent.Client) {
	t.Helper()
	want := recomputeFacetSummaryFromTables(t, ctx, client)
	rows, err := client.DecisionFacetEntry.Query().All(ctx)
	require.NoError(t, err)

	got := make(map[facetEntryKey]facetSummarySnapshot, len(rows))
	for _, r := range rows {
		// Answerer rows whose ref_count reached 0 are inert (filtered out on read).
		if r.Kind == decisionfacetentry.KindAnswerer && r.RefCount == 0 {
			continue
		}
		k := facetEntryKey{
			ProjectID: r.ProjectID,
			ChatKey:   r.ChatKey,
			Kind:      r.Kind,
			ActorID:   r.ActorID,
		}
		got[k] = facetSummarySnapshot{
			ProjectID: r.ProjectID,
			ChatKey:   r.ChatKey,
			Kind:      r.Kind,
			ActorID:   r.ActorID,
			Name:      r.Name,
			Email:     r.Email,
			LatestAt:  r.LatestAt.UTC().Truncate(time.Microsecond),
			RefCount:  r.RefCount,
		}
	}

	require.Equal(t, len(want), len(got), "summary row count mismatch vs full recompute")
	for k, w := range want {
		g, ok := got[k]
		require.True(t, ok, "missing summary row for key %+v", k)
		assert.Equal(t, w.RefCount, g.RefCount, "ref_count mismatch for %+v", k)
		assert.Equal(t, w.Name, g.Name, "name mismatch for %+v", k)
		assert.Equal(t, w.Email, g.Email, "email mismatch for %+v", k)
		assert.True(t, w.LatestAt.Equal(g.LatestAt), "latest_at mismatch for %+v: want=%v got=%v", k, w.LatestAt, g.LatestAt)
	}
}

func drainDecisionIndex(t *testing.T, ctx context.Context, cs *CompositeStore, pageSize int) (settled, decs, anss, reconciled int) {
	t.Helper()
	cursor := ""
	for {
		n, next, err := cs.SweepSettledDecisionsPage(ctx, cursor, pageSize)
		require.NoError(t, err)
		settled += n
		if next == "" {
			break
		}
		cursor = next
	}
	cursor = ""
	for {
		n, next, err := cs.IndexDecisionFacetsDecisionsPage(ctx, cursor, pageSize)
		require.NoError(t, err)
		decs += n
		if next == "" {
			break
		}
		cursor = next
	}
	cursor = ""
	for {
		n, next, err := cs.IndexDecisionFacetsAnswersPage(ctx, cursor, pageSize)
		require.NoError(t, err)
		anss += n
		if next == "" {
			break
		}
		cursor = next
	}
	var err error
	reconciled, err = cs.ReconcileDecisionFacets(ctx)
	require.NoError(t, err)
	return settled, decs, anss, reconciled
}

func normalizeAnswererIDs(actors []store.ProjectDecisionActor) []string {
	ids := make([]string, len(actors))
	for i, a := range actors {
		ids[i] = a.ID
	}
	sort.Strings(ids)
	return ids
}

// TestDecisionFacets_WriterCoverage asserts that every write path in the
// design-scale §3.2 writers table sets facet_indexed=true and produces the
// exact summary delta (design-scale §7 item 3).
func TestDecisionFacets_WriterCoverage(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	proj1 := uuid.NewString()
	proj2 := uuid.NewString()
	for i, pid := range []string{proj1, proj2} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("fw-p-%d", i), Slug: fmt.Sprintf("fw-p-%d", i), Created: time.Now().UTC(),
		}))
	}
	userA := uuid.NewString()
	userB := uuid.NewString()
	for i, uid := range []string{userA, userB} {
		require.NoError(t, cs.CreateUser(ctx, &store.User{
			ID: uid, Email: fmt.Sprintf("u%d@test.com", i), DisplayName: fmt.Sprintf("User %d", i),
			Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
		}))
	}

	bot1 := uuid.NewString()
	topic1 := "topic-" + uuid.NewString()
	topic2 := "topic-" + uuid.NewString()
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	// 1. CreateDecisionPrompt (multi-decision + NULL chat_key).
	p1, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "agent:bot1", SenderID: bot1,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: bot1, AskerAgentName: "Bot One",
		Decisions: []messages.Decision{
			{Key: "w-1", Question: "Q1?", Choices: choices},
			{Key: "w-2", Question: "Q2?", Choices: choices},
		},
	})
	require.NoError(t, err)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// NULL chat_key prompt must still set facet_indexed=true and contribute 0.
	_, err = cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "agent:bot1", SenderID: bot1,
			Recipient: "chat", ThreadID: "", Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: bot1, AskerAgentName: "Bot One",
		Decisions: []messages.Decision{{Key: "w-null", Question: "Null ck?", Choices: choices}},
	})
	require.NoError(t, err)
	unindexed, err := cs.hasUnindexedDecisionFacets(ctx, []uuid.UUID{uuid.MustParse(proj1)})
	require.NoError(t, err)
	assert.False(t, unindexed, "NULL chat_key decision must be marked facet_indexed=true")
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// 2. RecordAnswer (direct).
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: p1.Decisions[0].RowID, ChoiceID: "yes",
		AnsweredByUserID: userA, AnsweredByName: "Alice", AnsweredByEmail: "u0@test.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "user:" + userA, SenderID: userA,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// 3. RecordInterpretedAnswers + direct confirm via RecordAnswer (supersede -1 + +1).
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: bot1, SourceMessageID: uuid.NewString(),
		AnsweredByUserID: userA, AnsweredByName: "Alice Old", AnsweredByEmail: "u0@test.com",
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: p1.Decisions[1].RowID, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "agent:bot1", SenderID: bot1,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: p1.Decisions[1].RowID, ChoiceID: "yes",
		AnsweredByUserID: userA, AnsweredByName: "Alice New", AnsweredByEmail: "u0@test.com",
		Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeConfirm,
		Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "user:" + userA, SenderID: userA,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// 4. RecordAnswerSet (mix of pending + interpreted override).
	msgSetID := uuid.NewString()
	pSet, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: msgSetID, ProjectID: proj1, Sender: "agent:bot1", SenderID: bot1,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: bot1, AskerAgentName: "Bot One",
		Decisions: []messages.Decision{
			{Key: "set-1", Question: "S1?", Choices: choices},
			{Key: "set-2", Question: "S2?", Choices: choices},
		},
	})
	require.NoError(t, err)
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: bot1, SourceMessageID: uuid.NewString(),
		AnsweredByUserID: userB, AnsweredByName: "Bob", AnsweredByEmail: "u1@test.com",
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: pSet.Decisions[0].RowID, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "agent:bot1", SenderID: bot1,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	_, err = cs.RecordAnswerSet(ctx, &store.RecordAnswerSetInput{
		MessageID: msgSetID, AnsweredByUserID: userB, AnsweredByName: "Bob", AnsweredByEmail: "u1@test.com",
		Surface: store.DecisionSurfaceNative,
		Answers: []store.AnswerSetItem{
			{DecisionRowID: pSet.Decisions[0].RowID, ChoiceID: "no", Outcome: store.DecisionOutcomeOverride},
			{DecisionRowID: pSet.Decisions[1].RowID, ChoiceID: "yes"},
		},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "user:" + userB, SenderID: userB,
			Recipient: "chat", ThreadID: topic1, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// 5. DismissDecisions over an interpreted decision (recorded answerer userA
	// dismisses -> supersede -1 + dismiss +1 keeps ref_count=1 and updates name).
	msgDsmID := uuid.NewString()
	pDsm, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: msgDsmID, ProjectID: proj1, Sender: "agent:bot1", SenderID: bot1,
			Recipient: "chat", ThreadID: topic2, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: bot1, AskerAgentName: "Bot One",
		Decisions: []messages.Decision{{Key: "dsm-1", Question: "D1?", Choices: choices}},
	})
	require.NoError(t, err)
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: bot1, SourceMessageID: uuid.NewString(),
		AnsweredByUserID: userB, AnsweredByName: "Bob Interp", AnsweredByEmail: "u1@test.com",
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: pDsm.Decisions[0].RowID, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "agent:bot1", SenderID: bot1,
			Recipient: "chat", ThreadID: topic2, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	_, err = cs.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: msgDsmID,
		RowIDs:    []string{pDsm.Decisions[0].RowID},
		User:      store.DecisionAnswerer{UserID: userB, Name: "Bob Dismiss", Email: "u1@test.com"},
		Surface:   store.DecisionSurfaceDecisionsPage,
		MinAge:    time.Nanosecond,
		Now:       func() time.Time { return time.Now().UTC().Add(time.Hour) },
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj1, Sender: "user:" + userB, SenderID: userB,
			Recipient: "chat", ThreadID: topic2, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	_, topic2Ans, _, err := cs.ListDecisionFacets(ctx, []string{proj1}, []string{topic2})
	require.NoError(t, err)
	assert.Equal(t, []string{userB}, normalizeAnswererIDs(topic2Ans))

	// 6. DeleteUser -> clears answered_by_user_id and deletes answerer summary rows.
	require.NoError(t, cs.DeleteUser(ctx, userB))
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	_, topic2AnsAfterDel, _, err := cs.ListDecisionFacets(ctx, []string{proj1}, []string{topic2})
	require.NoError(t, err)
	assert.Empty(t, topic2AnsAfterDel, "deleted userB must disappear from answerers facet")

	// 7. DeleteProject -> removes project's decision_facet_entries in same tx.
	_, err = cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: uuid.NewString(), ProjectID: proj2, Sender: "agent:bot1", SenderID: bot1,
			Recipient: "chat", ThreadID: "topic-p2", Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: bot1, AskerAgentName: "Bot One",
		Decisions: []messages.Decision{{Key: "p2-1", Question: "P2?", Choices: choices}},
	})
	require.NoError(t, err)
	require.NoError(t, cs.DeleteProject(ctx, proj2))
	p2Entries, err := cs.client.DecisionFacetEntry.Query().
		Where(decisionfacetentry.ProjectIDEQ(uuid.MustParse(proj2))).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, p2Entries, "DeleteProject must remove decision_facet_entries")
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	pending, err := cs.HasPendingDecisionIndexWork(ctx)
	require.NoError(t, err)
	assert.False(t, pending, "all live writes must leave zero pending facet index work")
}

// TestDecisionFacets_MixedOrderSupersede covers all four mixed-order supersede
// cases from design-scale §3.2 and §7 item 2.
func TestDecisionFacets_MixedOrderSupersede(t *testing.T) {
	ctx := context.Background()
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	newCase := func(t *testing.T) (*CompositeStore, string, string, string, string, string, string) {
		t.Helper()
		cs := newTestCompositeStore(t)
		pid := uuid.NewString()
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: "mo-" + pid[:8], Slug: "mo-" + pid[:8], Created: time.Now().UTC(),
		}))
		userInterp := uuid.NewString()
		userDirect := uuid.NewString()
		botID := uuid.NewString()
		topic := "topic-" + uuid.NewString()
		p, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, Sender: "agent:bot", SenderID: botID,
				Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: botID, AskerAgentName: "bot",
			Decisions: []messages.Decision{{Key: "k1", Question: "Q?", Choices: choices}},
		})
		require.NoError(t, err)
		return cs, pid, topic, botID, p.Decisions[0].RowID, userInterp, userDirect
	}

	t.Run("case1_interpreted_indexed_then_direct_new", func(t *testing.T) {
		cs, pid, topic, botID, rowID, userI, _ := newCase(t)
		// Interpreted inserted by new binary (facet_indexed=true, +1 userI).
		_, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
			InterpretedByAgentID: botID, SourceMessageID: uuid.NewString(),
			AnsweredByUserID: userI, AnsweredByName: "InterpUser", AnsweredByEmail: "i@test.com",
			Surface: store.DecisionSurfaceNative,
			Answers: []store.InterpretedAnswerPair{{DecisionRowID: rowID, ChoiceID: "yes"}},
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, Sender: "agent:bot", SenderID: botID,
				Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
			},
		})
		require.NoError(t, err)

		// Direct confirm by userI via new binary (+1 userI, -1 userI -> net ref_count=1).
		_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID: rowID, ChoiceID: "yes",
			AnsweredByUserID: userI, AnsweredByName: "DirectUser", AnsweredByEmail: "i@test.com",
			Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeConfirm,
			Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, Sender: "user:" + userI, SenderID: userI,
				Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
			},
		})
		require.NoError(t, err)
		assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	})

	t.Run("case2_interpreted_unindexed_old_binary_then_direct_new", func(t *testing.T) {
		cs, pid, topic, botID, rowID, userI, _ := newCase(t)
		// Simulate old binary writing interpreted answer (facet_indexed=false, no summary delta).
		_, err := cs.client.DecisionAnswer.Create().
			SetDecisionRowID(uuid.MustParse(rowID)).
			SetProjectID(uuid.MustParse(pid)).
			SetAskerAgentID(uuid.MustParse(botID)).
			SetKey("k1").SetVersion(1).SetQuestionText("Q?").
			SetChoiceID("yes").SetChoiceLabel("Yes").
			SetAnsweredByUserID(userI).SetAnsweredByName("InterpUser").SetAnsweredByEmail("i@test.com").
			SetProvenance(decisionanswer.ProvenanceAgentInterpreted).
			SetOutcome(decisionanswer.OutcomeAnswer).
			SetInterpretedByAgentID(uuid.MustParse(botID)).
			SetSourceMessageID(uuid.New()).
			SetSurface(decisionanswer.SurfaceNative).
			SetResponseMessageID(uuid.New()).
			SetFacetIndexed(false).
			SetCreatedAt(time.Now().UTC()).
			Save(ctx)
		require.NoError(t, err)
		require.NoError(t, cs.client.Decision.UpdateOneID(uuid.MustParse(rowID)).SetStatus(decision.StatusDecided).Exec(ctx))

		// Direct confirm by userI via new binary (+1 userI, 0 for unindexed userI -> ref_count=1).
		_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID: rowID, ChoiceID: "yes",
			AnsweredByUserID: userI, AnsweredByName: "DirectUser", AnsweredByEmail: "i@test.com",
			Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeConfirm,
			Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, Sender: "user:" + userI, SenderID: userI,
				Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
			},
		})
		require.NoError(t, err)

		// Now run facet indexer: it must flip interpreted to facet_indexed=true with 0 contribution.
		drainDecisionIndex(t, ctx, cs, 10)
		assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	})

	t.Run("case3_interpreted_indexed_then_direct_unindexed_old_binary", func(t *testing.T) {
		cs, pid, topic, botID, rowID, userI, userD := newCase(t)
		// Interpreted inserted by new binary (facet_indexed=true, +1 userI).
		_, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
			InterpretedByAgentID: botID, SourceMessageID: uuid.NewString(),
			AnsweredByUserID: userI, AnsweredByName: "InterpUser", AnsweredByEmail: "i@test.com",
			Surface: store.DecisionSurfaceNative,
			Answers: []store.InterpretedAnswerPair{{DecisionRowID: rowID, ChoiceID: "yes"}},
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, Sender: "agent:bot", SenderID: botID,
				Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
			},
		})
		require.NoError(t, err)

		// Simulate old binary inserting direct dismiss (facet_indexed=false, no summary delta).
		_, err = cs.client.DecisionAnswer.Create().
			SetDecisionRowID(uuid.MustParse(rowID)).
			SetProjectID(uuid.MustParse(pid)).
			SetAskerAgentID(uuid.MustParse(botID)).
			SetKey("k1").SetVersion(1).SetQuestionText("Q?").
			SetChoiceID("$dismissed").SetChoiceLabel("Dismissed").
			SetAnsweredByUserID(userD).SetAnsweredByName("DirectUser").SetAnsweredByEmail("d@test.com").
			SetProvenance(decisionanswer.ProvenanceDirect).
			SetOutcome(decisionanswer.OutcomeDismiss).
			SetSurface(decisionanswer.SurfaceDecisionsPage).
			SetResponseMessageID(uuid.New()).
			SetFacetIndexed(false).
			SetCreatedAt(time.Now().UTC().Add(time.Second)).
			Save(ctx)
		require.NoError(t, err)

		// Before indexer runs, fallback must still exclude userI and include userD.
		_, ansFallback, _, err := cs.ListDecisionFacets(ctx, []string{pid}, []string{topic})
		require.NoError(t, err)
		assert.Equal(t, []string{userD}, normalizeAnswererIDs(ansFallback))

		// Indexer processes direct unindexed -> +1 userD, -1 userI.
		drainDecisionIndex(t, ctx, cs, 10)
		assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	})

	t.Run("case4_both_unindexed_old_binary", func(t *testing.T) {
		cs, pid, _, botID, rowID, userI, userD := newCase(t)
		ts := time.Now().UTC()
		// Insert both interpreted and direct with facet_indexed=false (old binary).
		_, err := cs.client.DecisionAnswer.Create().
			SetDecisionRowID(uuid.MustParse(rowID)).
			SetProjectID(uuid.MustParse(pid)).
			SetAskerAgentID(uuid.MustParse(botID)).
			SetKey("k1").SetVersion(1).SetQuestionText("Q?").
			SetChoiceID("yes").SetChoiceLabel("Yes").
			SetAnsweredByUserID(userI).SetAnsweredByName("InterpUser").SetAnsweredByEmail("i@test.com").
			SetProvenance(decisionanswer.ProvenanceAgentInterpreted).
			SetOutcome(decisionanswer.OutcomeAnswer).
			SetInterpretedByAgentID(uuid.MustParse(botID)).
			SetSourceMessageID(uuid.New()).
			SetSurface(decisionanswer.SurfaceNative).
			SetResponseMessageID(uuid.New()).
			SetFacetIndexed(false).
			SetCreatedAt(ts).
			Save(ctx)
		require.NoError(t, err)
		_, err = cs.client.DecisionAnswer.Create().
			SetDecisionRowID(uuid.MustParse(rowID)).
			SetProjectID(uuid.MustParse(pid)).
			SetAskerAgentID(uuid.MustParse(botID)).
			SetKey("k1").SetVersion(1).SetQuestionText("Q?").
			SetChoiceID("$dismissed").SetChoiceLabel("Dismissed").
			SetAnsweredByUserID(userD).SetAnsweredByName("DirectUser").SetAnsweredByEmail("d@test.com").
			SetProvenance(decisionanswer.ProvenanceDirect).
			SetOutcome(decisionanswer.OutcomeDismiss).
			SetSurface(decisionanswer.SurfaceDecisionsPage).
			SetResponseMessageID(uuid.New()).
			SetFacetIndexed(false).
			SetCreatedAt(ts.Add(time.Second)).
			Save(ctx)
		require.NoError(t, err)

		// Page size 1 forces the indexer to pick up only one row in the initial
		// LIMIT 1 query while still locking and processing both siblings cleanly.
		drainDecisionIndex(t, ctx, cs, 1)
		assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	})
}

// TestDecisionFacets_DifferentialVsLegacy runs the full lifecycle fixture and
// compares summary-backed ListDecisionFacets, ListProjectDecisionAgentsForProjects,
// and ProjectsWithDecisions against the legacy queries across random readable-key
// subsets — in steady state, in simulated older-binary fallback, and after
// indexer backfill (design-scale §7 item 1, §9 items 3-4).
func TestDecisionFacets_DifferentialVsLegacy(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	const numProjects = 4
	projIDs := make([]string, numProjects)
	projUUIDs := make([]uuid.UUID, numProjects)
	for i := 0; i < numProjects; i++ {
		pid := uuid.NewString()
		projIDs[i] = pid
		projUUIDs[i] = uuid.MustParse(pid)
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("diff-p-%d", i), Slug: fmt.Sprintf("diff-p-%d", i), Created: time.Now().UTC(),
		}))
	}
	deletedProjID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: deletedProjID, Name: "diff-del-p", Slug: "diff-del-p", Created: time.Now().UTC(),
	}))

	users := make([]string, 4)
	for i := range users {
		users[i] = uuid.NewString()
		require.NoError(t, cs.CreateUser(ctx, &store.User{
			ID: users[i], Email: fmt.Sprintf("diff-u%d@test.com", i), DisplayName: fmt.Sprintf("User %d", i),
			Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
		}))
	}
	deletedUserID := users[3]

	agents := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	singleChoice := []messages.Choice{{ID: "ack", Label: "Acknowledge"}}

	allKeys := []string{
		"topic-p0-a", "topic-p0-b",
		fmt.Sprintf("dm:agent:%s:user:%s", agents[0], users[0]),
		fmt.Sprintf("dm:agent:%s:user:%s", agents[0], users[1]), // unreadable to users[0]
		"topic-p1-a",
		"topic-p2-a",
	}

	post := func(pid, ck, agID, agName, key string, ch []messages.Choice, allowOther, update bool) string {
		t.Helper()
		d := messages.Decision{Key: key, Question: "Q " + key + "?", Choices: ch}
		if allowOther {
			d.Other = &messages.OtherOption{Label: "Other"}
		}
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, Sender: "agent:" + agName, SenderID: agID,
				Recipient: "chat", ThreadID: ck, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: agID, AskerAgentName: agName, Update: update,
			Decisions: []messages.Decision{d},
		})
		require.NoError(t, err)
		return res.Decisions[0].RowID
	}

	// Project 0: pending, direct, Other, single-choice, interpreted awaiting,
	// confirmed, overridden, withdrawn+reasked, dismissed (incl. interpreted),
	// replaced, DM, unreadable DM, NULL chat_key.
	_ = post(projIDs[0], allKeys[0], agents[0], "Agent Zero", "d-pending", choices, false, false)
	rDirect := post(projIDs[0], allKeys[0], agents[0], "Agent Zero", "d-direct", choices, false, false)
	_, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rDirect, ChoiceID: "yes",
		AnsweredByUserID: users[0], AnsweredByName: "Alice", AnsweredByEmail: "diff-u0@test.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: projIDs[0], Sender: "user:" + users[0], SenderID: users[0], Recipient: "chat", ThreadID: allKeys[0], Channel: "web", CreatedAt: time.Now().UTC()},
	})
	require.NoError(t, err)

	rOther := post(projIDs[0], allKeys[0], agents[0], "Agent Zero Renamed", "d-other", choices, true, false)
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rOther, ChoiceID: messages.ChoiceIDOther, OtherText: "custom",
		AnsweredByUserID: users[1], AnsweredByName: "Bob", AnsweredByEmail: "diff-u1@test.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: projIDs[0], Sender: "user:" + users[1], SenderID: users[1], Recipient: "chat", ThreadID: allKeys[0], Channel: "web", CreatedAt: time.Now().UTC()},
	})
	require.NoError(t, err)

	_ = post(projIDs[0], allKeys[1], agents[1], "Agent One", "d-single", singleChoice, false, false)
	rInterpAwait := post(projIDs[0], allKeys[1], agents[1], "Agent One", "d-interp-await", choices, false, false)
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agents[1], SourceMessageID: uuid.NewString(),
		AnsweredByUserID: users[2], AnsweredByName: "Carol", AnsweredByEmail: "diff-u2@test.com",
		Surface:         store.DecisionSurfaceNative,
		Answers:         []store.InterpretedAnswerPair{{DecisionRowID: rInterpAwait, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: projIDs[0], Sender: "agent:a1", SenderID: agents[1], Recipient: "chat", ThreadID: allKeys[1], Channel: "web", CreatedAt: time.Now().UTC()},
	})
	require.NoError(t, err)

	// Interpreted then dismissed by the recorded answerer (deletedUserID -> later deleted).
	rInterpDsm := post(projIDs[0], allKeys[2], agents[0], "Agent Zero Renamed", "d-interp-dsm", choices, false, false)
	_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agents[0], SourceMessageID: uuid.NewString(),
		AnsweredByUserID: deletedUserID, AnsweredByName: "DelUser", AnsweredByEmail: "diff-u3@test.com",
		Surface:         store.DecisionSurfaceNative,
		Answers:         []store.InterpretedAnswerPair{{DecisionRowID: rInterpDsm, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: projIDs[0], Sender: "agent:a0", SenderID: agents[0], Recipient: "chat", ThreadID: allKeys[2], Channel: "web", CreatedAt: time.Now().UTC()},
	})
	require.NoError(t, err)
	decDsm, err := cs.GetDecision(ctx, rInterpDsm)
	require.NoError(t, err)
	_, err = cs.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: decDsm.MessageID, RowIDs: []string{rInterpDsm},
		User:    store.DecisionAnswerer{UserID: deletedUserID, Name: "DelUser", Email: "diff-u3@test.com"},
		Surface: store.DecisionSurfaceDecisionsPage, MinAge: time.Nanosecond,
		Now:             func() time.Time { return time.Now().UTC().Add(time.Hour) },
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: projIDs[0], Sender: "user:" + deletedUserID, SenderID: deletedUserID, Recipient: "chat", ThreadID: allKeys[2], Channel: "web", CreatedAt: time.Now().UTC()},
	})
	require.NoError(t, err)

	// Withdrawn + re-asked + replaced.
	_ = post(projIDs[0], allKeys[3], agents[0], "Agent Zero Renamed", "d-wd", choices, false, false)
	_, err = cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{AskerAgentID: agents[0], Keys: []string{"d-wd"}})
	require.NoError(t, err)
	_ = post(projIDs[0], allKeys[3], agents[0], "Agent Zero Renamed", "d-wd", choices, false, true)
	_ = post(projIDs[0], allKeys[3], agents[0], "Agent Zero Renamed", "d-wd", choices, false, true)

	// NULL chat_key decision in project 0.
	_ = post(projIDs[0], "", agents[0], "Agent Zero Renamed", "d-null-ck", choices, false, false)

	// Project 1 & Project 2 decisions (project 3 stays empty).
	rP1 := post(projIDs[1], allKeys[4], agents[2], "Agent Two", "p1-d1", choices, false, false)
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rP1, ChoiceID: "no",
		AnsweredByUserID: users[0], AnsweredByName: "Alice", AnsweredByEmail: "diff-u0@test.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: projIDs[1], Sender: "user:" + users[0], SenderID: users[0], Recipient: "chat", ThreadID: allKeys[4], Channel: "web", CreatedAt: time.Now().UTC()},
	})
	require.NoError(t, err)
	_ = post(projIDs[2], allKeys[5], agents[2], "Agent Two", "p2-d1", choices, false, false)

	// Deleted project + deleted user.
	_ = post(deletedProjID, "topic-del", agents[2], "Agent Two", "del-d1", choices, false, false)
	require.NoError(t, cs.DeleteProject(ctx, deletedProjID))
	require.NoError(t, cs.DeleteUser(ctx, deletedUserID))

	verifyAllReadsMatchLegacy := func(t *testing.T, phase string) {
		t.Helper()
		rng := rand.New(rand.NewSource(42))
		for trial := 0; trial < 15; trial++ {
			// Pick random non-empty subset of projects and chat keys.
			var subProjs []string
			var subProjUUIDs []uuid.UUID
			for i, pid := range projIDs {
				if rng.Intn(2) == 0 || (trial == 0) {
					subProjs = append(subProjs, pid)
					subProjUUIDs = append(subProjUUIDs, projUUIDs[i])
				}
			}
			if len(subProjs) == 0 {
				subProjs = []string{projIDs[0]}
				subProjUUIDs = []uuid.UUID{projUUIDs[0]}
			}
			var subKeys []string
			for _, k := range allKeys {
				if rng.Intn(2) == 0 || (trial == 0) {
					subKeys = append(subKeys, k)
				}
			}
			if len(subKeys) == 0 {
				subKeys = []string{allKeys[0]}
			}

			gotAg, gotAns, gotConvs, err := cs.ListDecisionFacets(ctx, subProjs, subKeys)
			require.NoError(t, err)
			legAg, legAns, legConvs, err := cs.listDecisionFacetsLegacy(ctx, subProjUUIDs, cleanChatKeys(subKeys))
			require.NoError(t, err)
			sort.Strings(gotConvs)
			sort.Strings(legConvs)
			assert.Equal(t, legAg, gotAg, "%s trial %d: agents mismatch", phase, trial)
			assert.Equal(t, normalizeAnswererIDs(legAns), normalizeAnswererIDs(gotAns), "%s trial %d: answerer IDs mismatch", phase, trial)
			assert.Equal(t, legConvs, gotConvs, "%s trial %d: convKeys mismatch", phase, trial)

			gotPWD, err := cs.ProjectsWithDecisions(ctx, subProjs)
			require.NoError(t, err)
			legPWD, err := cs.projectsWithDecisionsLegacy(ctx, subProjUUIDs)
			require.NoError(t, err)
			assert.Equal(t, legPWD, gotPWD, "%s trial %d: ProjectsWithDecisions mismatch", phase, trial)

			gotByP, err := cs.ListProjectDecisionAgentsForProjects(ctx, subProjs)
			require.NoError(t, err)
			legByP, err := cs.listProjectDecisionAgentsForProjectsLegacy(ctx, subProjUUIDs)
			require.NoError(t, err)
			assert.Equal(t, len(legByP), len(gotByP), "%s trial %d: ListProjectDecisionAgentsForProjects key count mismatch", phase, trial)
			for pid, legActors := range legByP {
				assert.Equal(t, normalizeAnswererIDs(legActors), normalizeAnswererIDs(gotByP[pid]), "%s trial %d proj %s: agent IDs mismatch", phase, trial, pid)
			}
		}
	}

	// Phase A: live writes steady state.
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	verifyAllReadsMatchLegacy(t, "steady_state")

	// Phase B: simulate older-binary rows (clear summary, flip facet_indexed=false).
	_, err = cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	_, err = cs.client.Decision.Update().SetFacetIndexed(false).Save(ctx)
	require.NoError(t, err)
	_, err = cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Save(ctx)
	require.NoError(t, err)
	verifyAllReadsMatchLegacy(t, "fallback_unindexed")

	// Phase C: run background indexer in small pages, then verify parity + recompute.
	drainDecisionIndex(t, ctx, cs, 2)
	pending, err := cs.HasPendingDecisionIndexWork(ctx)
	require.NoError(t, err)
	assert.False(t, pending)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	verifyAllReadsMatchLegacy(t, "post_backfill")
}

// TestDecisionFacets_DowngradeReconcile simulates an older binary deleting a
// user and a project without touching decision_facet_entries, then verifies
// ReconcileDecisionFacets heals both (design-scale §3.6, §9 item 4).
func TestDecisionFacets_DowngradeReconcile(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	pidKeep := uuid.NewString()
	pidOldDel := uuid.NewString()
	for i, pid := range []string{pidKeep, pidOldDel} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("rec-p-%d", i), Slug: fmt.Sprintf("rec-p-%d", i), Created: time.Now().UTC(),
		}))
	}
	uKeep := uuid.NewString()
	uOldDel := uuid.NewString()
	for i, uid := range []string{uKeep, uOldDel} {
		require.NoError(t, cs.CreateUser(ctx, &store.User{
			ID: uid, Email: fmt.Sprintf("rec-u%d@test.com", i), DisplayName: fmt.Sprintf("Rec %d", i),
			Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
		}))
	}

	botID := uuid.NewString()
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	pKeep, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message:      &store.Message{ID: uuid.NewString(), ProjectID: pidKeep, Sender: "agent:bot", SenderID: botID, Recipient: "chat", ThreadID: "t-keep", Channel: "web", CreatedAt: time.Now().UTC()},
		AskerAgentID: botID, AskerAgentName: "bot",
		Decisions: []messages.Decision{
			{Key: "k-keep", Question: "Keep?", Choices: choices},
			{Key: "k-delu", Question: "DelU?", Choices: choices},
		},
	})
	require.NoError(t, err)
	for i, uid := range []string{uKeep, uOldDel} {
		_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID: pKeep.Decisions[i].RowID, ChoiceID: "yes",
			AnsweredByUserID: uid, AnsweredByName: "U", AnsweredByEmail: "u@test.com",
			Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: pidKeep, Sender: "user:" + uid, SenderID: uid, Recipient: "chat", ThreadID: "t-keep", Channel: "web", CreatedAt: time.Now().UTC()},
		})
		require.NoError(t, err)
	}
	_, err = cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message:      &store.Message{ID: uuid.NewString(), ProjectID: pidOldDel, Sender: "agent:bot", SenderID: botID, Recipient: "chat", ThreadID: "t-del", Channel: "web", CreatedAt: time.Now().UTC()},
		AskerAgentID: botID, AskerAgentName: "bot",
		Decisions: []messages.Decision{{Key: "k-delp", Question: "DelP?", Choices: choices}},
	})
	require.NoError(t, err)

	// Simulate older-binary DeleteUser (deletes user + clears answered_by_user_id,
	// leaving stale answerer row in decision_facet_entries).
	require.NoError(t, cs.client.DecisionAnswer.Update().Where(decisionanswer.AnsweredByUserIDEQ(uOldDel)).ClearAnsweredByUserID().Exec(ctx))
	require.NoError(t, cs.client.User.DeleteOneID(uuid.MustParse(uOldDel)).Exec(ctx))

	// Simulate older-binary DeleteProject (deletes decisions + project,
	// leaving stale asker row in decision_facet_entries).
	_, err = cs.client.Decision.Delete().Where(decision.ProjectIDEQ(uuid.MustParse(pidOldDel))).Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, cs.client.Project.DeleteOneID(uuid.MustParse(pidOldDel)).Exec(ctx))

	// Reconciler must delete both stale rows and keep live ones intact.
	deleted, err := cs.ReconcileDecisionFacets(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// Second reconcile pass is a no-op.
	deleted2, err := cs.ReconcileDecisionFacets(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted2)
}

// TestDecisionFacets_IndexerConcurrency runs two concurrent facet indexers
// alongside concurrent live answers (including confirmations over interpreted
// answers) and asserts the final summary equals a full recompute
// (design-scale §7 item 2).
func TestDecisionFacets_IndexerConcurrency(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	pid := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: pid, Name: "conc-p", Slug: "conc-p", Created: time.Now().UTC(),
	}))
	botID := uuid.NewString()
	topic := "topic-conc"
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	users := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for i, uid := range users {
		require.NoError(t, cs.CreateUser(ctx, &store.User{
			ID: uid, Email: fmt.Sprintf("conc-%d@test.com", i), DisplayName: fmt.Sprintf("Conc %d", i),
			Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
		}))
	}

	const total = 30
	rowIDs := make([]string, total)
	for i := 0; i < total; i++ {
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, Sender: "agent:bot", SenderID: botID,
				Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: botID, AskerAgentName: "bot",
			Decisions: []messages.Decision{{Key: fmt.Sprintf("c-%02d", i), Question: "Q?", Choices: choices}},
		})
		require.NoError(t, err)
		rowIDs[i] = res.Decisions[0].RowID

		// Half start with an interpreted answer; the other half stay pending.
		if i < total/2 {
			_, err = cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
				InterpretedByAgentID: botID, SourceMessageID: uuid.NewString(),
				AnsweredByUserID: users[i%len(users)], AnsweredByName: "U", AnsweredByEmail: "u@test.com",
				Surface: store.DecisionSurfaceNative,
				Answers: []store.InterpretedAnswerPair{{DecisionRowID: rowIDs[i], ChoiceID: "yes"}},
				ResponseMessage: &store.Message{
					ID: uuid.NewString(), ProjectID: pid, Sender: "agent:bot", SenderID: botID,
					Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
				},
			})
			require.NoError(t, err)
		}
	}

	// Reset markers & clear summary so both indexers have real backfill work.
	_, err := cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	_, err = cs.client.Decision.Update().SetFacetIndexed(false).Save(ctx)
	require.NoError(t, err)
	_, err = cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Save(ctx)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errCh := make(chan error, 8)

	// Two concurrent indexers draining in small pages using keyset cursors.
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decCursor, ansCursor := "", ""
			for pass := 0; pass < 15; pass++ {
				_, nextDec, ierr := cs.IndexDecisionFacetsDecisionsPage(ctx, decCursor, 4)
				if ierr != nil {
					errCh <- ierr
					return
				}
				decCursor = nextDec
				_, nextAns, ierr := cs.IndexDecisionFacetsAnswersPage(ctx, ansCursor, 4)
				if ierr != nil {
					errCh <- ierr
					return
				}
				ansCursor = nextAns
			}
		}()
	}

	// Concurrent live answer writer: confirms interpreted rows and answers pending rows.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < total; i++ {
			uid := users[i%len(users)]
			outcome := store.DecisionOutcomeAnswer
			if i < total/2 {
				outcome = store.DecisionOutcomeConfirm
			}
			_, _, aerr := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
				DecisionRowID: rowIDs[i], ChoiceID: "yes",
				AnsweredByUserID: uid, AnsweredByName: "U Confirmed", AnsweredByEmail: "u@test.com",
				Provenance: store.DecisionProvenanceDirect, Outcome: outcome,
				Surface: store.DecisionSurfaceNative,
				ResponseMessage: &store.Message{
					ID: uuid.NewString(), ProjectID: pid, Sender: "user:" + uid, SenderID: uid,
					Recipient: "chat", ThreadID: topic, Channel: "web", CreatedAt: time.Now().UTC(),
				},
			})
			if aerr != nil {
				errCh <- aerr
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for e := range errCh {
		require.NoError(t, e)
	}

	// Drain any remaining unindexed rows (e.g. skipped by SKIP LOCKED while locked).
	drainDecisionIndex(t, ctx, cs, 10)
	pending, err := cs.HasPendingDecisionIndexWork(ctx)
	require.NoError(t, err)
	assert.False(t, pending)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
}

// TestDecisionFacets_PartialIndexAutoMigrate verifies that AutoMigrate creates
// and recreates both facet_indexed=false partial indexes on SQLite and Postgres.
func TestDecisionFacets_PartialIndexAutoMigrate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)

	hasIndex := func(name string) bool {
		rows := &entsql.Rows{}
		if client.Driver().Dialect() == dialect.Postgres {
			require.NoError(t, client.Driver().Query(ctx,
				"SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1",
				[]any{name}, rows))
		} else {
			require.NoError(t, client.Driver().Query(ctx,
				"SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?",
				[]any{name}, rows))
		}
		found := rows.Next()
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		return found
	}

	for _, idxName := range []string{decisionFacetPartialIndex, decisionAnswerFacetPartialIndex} {
		require.True(t, hasIndex(idxName), "fresh migrated DB must have %s", idxName)
		require.NoError(t, client.Driver().Exec(ctx, "DROP INDEX "+idxName, []any{}, nil))
		require.False(t, hasIndex(idxName))
		require.NoError(t, entc.AutoMigrate(ctx, client))
		require.True(t, hasIndex(idxName), "AutoMigrate must recreate %s on an existing DB", idxName)
	}
}

// TestDecisionFacets_QueryPlans pins that the fallback probe hits the
// facet_indexed=false partial indexes and that ListDecisionFacets,
// ListProjectDecisionAgentsForProjects, and ProjectsWithDecisions hit
// decision_facet_entries indexes on SQLite and Postgres (design-scale §7 item 5).
func TestDecisionFacets_QueryPlans(t *testing.T) {
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	recClient := ent.NewClient(ent.Driver(rec))
	cs := NewCompositeStore(recClient)

	const numProjects = 20
	const queryProjects = 3
	const rowsPerProject = 20
	projectIDs := make([]string, numProjects)
	allKeys := make([]string, 0, numProjects*2)
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
	baseTime := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	userID := uuid.NewString()

	tx, err := base.Tx(ctx)
	require.NoError(t, err)
	for p := 0; p < numProjects; p++ {
		pid := uuid.New()
		projectIDs[p] = pid.String()
		k1 := fmt.Sprintf("ftopic-%d-a", p)
		k2 := fmt.Sprintf("ftopic-%d-b", p)
		allKeys = append(allKeys, k1, k2)
		agID := uuid.New()
		decCreates := make([]*ent.DecisionCreate, 0, rowsPerProject)
		ansCreates := make([]*ent.DecisionAnswerCreate, 0, rowsPerProject)
		for j := 0; j < rowsPerProject; j++ {
			decID := uuid.New()
			ck := k1
			if j%2 == 1 {
				ck = k2
			}
			ts := baseTime.Add(time.Duration(p*1000+j) * time.Second)
			decCreates = append(decCreates, tx.Decision.Create().
				SetID(decID).SetProjectID(pid).SetChatKey(ck).SetMessageID(uuid.New()).SetPosition(0).
				SetAskerAgentID(agID).SetAskerAgentName("bot").
				SetKey(fmt.Sprintf("fk-%d-%d", p, j)).SetKeyNorm(fmt.Sprintf("fk-%d-%d", p, j)).
				SetKeySource(decision.KeySourceAgent).SetVersion(1).SetQuestion("Q?").SetChoices(choices).
				SetStatus(decision.StatusDecided).SetSettled(true).SetFacetIndexed(true).SetCreatedAt(ts))
			ansCreates = append(ansCreates, tx.DecisionAnswer.Create().
				SetDecisionRowID(decID).SetProjectID(pid).SetAskerAgentID(agID).
				SetKey(fmt.Sprintf("fk-%d-%d", p, j)).SetVersion(1).SetQuestionText("Q?").
				SetChoiceID("yes").SetChoiceLabel("Yes").
				SetAnsweredByUserID(userID).SetAnsweredByName("User").SetAnsweredByEmail("u@test.com").
				SetProvenance(decisionanswer.ProvenanceDirect).SetOutcome(decisionanswer.OutcomeAnswer).
				SetSurface(decisionanswer.SurfaceNative).SetResponseMessageID(uuid.New()).
				SetFacetIndexed(true).SetCreatedAt(ts))
		}
		require.NoError(t, tx.Decision.CreateBulk(decCreates...).Exec(ctx))
		require.NoError(t, tx.DecisionAnswer.CreateBulk(ansCreates...).Exec(ctx))
		for _, ck := range []string{k1, k2} {
			require.NoError(t, tx.DecisionFacetEntry.CreateBulk(
				tx.DecisionFacetEntry.Create().SetProjectID(pid).SetChatKey(ck).SetKind(decisionfacetentry.KindAsker).SetActorID(agID.String()).SetName("bot").SetLatestAt(baseTime.Add(time.Hour)).SetRefCount(rowsPerProject/2),
				tx.DecisionFacetEntry.Create().SetProjectID(pid).SetChatKey(ck).SetKind(decisionfacetentry.KindAnswerer).SetActorID(userID).SetName("User").SetEmail("u@test.com").SetLatestAt(baseTime.Add(time.Hour)).SetRefCount(rowsPerProject/2),
			).Exec(ctx))
		}
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

	captureQueries := func(run func()) []recordedQuery {
		rec.mu.Lock()
		rec.queries = nil
		rec.mu.Unlock()
		run()
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return append([]recordedQuery(nil), rec.queries...)
	}

	assertProbeAndSummary := func(t *testing.T, label string, wantSummaryIdxPattern string, run func()) {
		t.Helper()
		qs := captureQueries(run)
		require.GreaterOrEqual(t, len(qs), 2, "%s must emit probe + summary query", label)

		probePlan := explain(t, qs[0])
		t.Logf("%s PROBE (%s) SQL: %s\nPLAN:\n%s", label, base.Driver().Dialect(), qs[0].sql, probePlan)
		assert.Regexp(t, `\b`+decisionFacetPartialIndex+`\b`, probePlan, "%s probe must use %s", label, decisionFacetPartialIndex)
		assert.Regexp(t, `\b`+decisionAnswerFacetPartialIndex+`\b`, probePlan, "%s probe must use %s", label, decisionAnswerFacetPartialIndex)

		sumPlan := explain(t, qs[1])
		t.Logf("%s SUMMARY (%s) SQL: %s\nPLAN:\n%s", label, base.Driver().Dialect(), qs[1].sql, sumPlan)
		assert.Contains(t, qs[1].sql, decisionfacetentry.Table, "%s must query decision_facet_entries", label)
		if base.Driver().Dialect() == dialect.SQLite {
			assert.Regexp(t, wantSummaryIdxPattern, sumPlan, "%s summary must use index %s; plan:\n%s", label, wantSummaryIdxPattern, sumPlan)
		} else {
			// On tiny 20-row Postgres fixtures, planner may choose Seq Scan on
			// decision_facet_entries (still tiny O(actors)), so verify it never
			// touches decisions or decision_answers in the summary query.
			assert.NotContains(t, sumPlan, "on decisions", "%s summary must not scan decisions", label)
			assert.NotContains(t, sumPlan, "on decision_answers", "%s summary must not scan decision_answers", label)
		}
	}

	t.Run("ListDecisionFacets_single_project", func(t *testing.T) {
		assertProbeAndSummary(t, "ListDecisionFacets(single)", `\b`+facetUniqueIndex+`\b`, func() {
			ag, ans, ck, err := cs.ListDecisionFacets(ctx, projectIDs[:1], allKeys[:2])
			require.NoError(t, err)
			require.Len(t, ag, 1)
			require.Len(t, ans, 1)
			require.Len(t, ck, 2)
		})
	})

	t.Run("ListDecisionFacets_multi_project", func(t *testing.T) {
		assertProbeAndSummary(t, "ListDecisionFacets(multi)", `\b(`+facetUniqueIndex+`|`+facetProjectKindIndex+`)\b`, func() {
			ag, ans, ck, err := cs.ListDecisionFacets(ctx, projectIDs[:queryProjects], allKeys[:queryProjects*2])
			require.NoError(t, err)
			require.Len(t, ag, queryProjects)
			require.Len(t, ans, 1)
			require.Len(t, ck, queryProjects*2)
		})
	})

	t.Run("ListProjectDecisionAgentsForProjects", func(t *testing.T) {
		assertProbeAndSummary(t, "ListProjectDecisionAgentsForProjects", `\b`+facetProjectKindIndex+`\b`, func() {
			got, err := cs.ListProjectDecisionAgentsForProjects(ctx, projectIDs[:queryProjects])
			require.NoError(t, err)
			require.Len(t, got, queryProjects)
		})
	})

	t.Run("ProjectsWithDecisions", func(t *testing.T) {
		assertProbeAndSummary(t, "ProjectsWithDecisions", `\b`+facetProjectKindIndex+`\b`, func() {
			got, err := cs.ProjectsWithDecisions(ctx, projectIDs[:queryProjects])
			require.NoError(t, err)
			require.Len(t, got, queryProjects)
		})
	})
}
