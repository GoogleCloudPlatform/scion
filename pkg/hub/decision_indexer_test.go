//go:build !no_sqlite

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

package hub

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestDecisionIndexer_PassAndLifecycle(t *testing.T) {
	f, cs, _ := newCountingDecisionsFixture(t)
	ctx := context.Background()

	// 1. Empty DB: pass finds nothing pending and does zero pages.
	stats, err := f.srv.runDecisionIndexPass(ctx)
	require.NoError(t, err)
	assert.False(t, stats.HadPending)
	assert.Equal(t, 0, stats.Pages)

	// 2. Seed 8 settled decisions + 1 pending + 1 interpreted-awaiting, then
	// simulate older-binary writes by resetting settled=false on all rows.
	tpID := createTestDecisionTopic(t, f.wcs, f.project.ID, "idx-topic", f.bob.ID)
	botID := api.NewUUID()
	choices := []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	for i := 0; i < 8; i++ {
		res, cerr := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:bot", SenderID: botID,
				Recipient: "chat", ThreadID: tpID, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			AskerAgentID: botID, AskerAgentName: "bot",
			Decisions: []messages.Decision{{Key: fmt.Sprintf("idx-s-%d", i), Question: "Settled?", Choices: choices}},
		})
		require.NoError(t, cerr)
		_, _, aerr := f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID: res.Decisions[0].RowID, ChoiceID: "yes",
			AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName, AnsweredByEmail: f.bob.Email,
			Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "user:" + f.bob.ID, SenderID: f.bob.ID,
				Recipient: "chat", ThreadID: tpID, Channel: "web", CreatedAt: time.Now().UTC(),
			},
		})
		require.NoError(t, aerr)
	}

	// 1 pending + 1 interpreted-awaiting (both must stay settled=false).
	_, err = f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:bot", SenderID: botID,
			Recipient: "chat", ThreadID: tpID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: botID, AskerAgentName: "bot",
		Decisions: []messages.Decision{{Key: "idx-open-p", Question: "Pending?", Choices: choices}},
	})
	require.NoError(t, err)
	interpPrompt, err := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:bot", SenderID: botID,
			Recipient: "chat", ThreadID: tpID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		AskerAgentID: botID, AskerAgentName: "bot",
		Decisions: []messages.Decision{{Key: "idx-open-i", Question: "Interp?", Choices: choices}},
	})
	require.NoError(t, err)
	_, err = f.store.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: botID, SourceMessageID: api.NewUUID(),
		AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName, AnsweredByEmail: f.bob.Email,
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: interpPrompt.Decisions[0].RowID, ChoiceID: "yes"}},
		ResponseMessage: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:bot", SenderID: botID,
			Recipient: "chat", ThreadID: tpID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
	})
	require.NoError(t, err)

	_, err = cs.DB().ExecContext(ctx, "UPDATE decisions SET settled = false, facet_indexed = false")
	require.NoError(t, err)
	_, err = cs.DB().ExecContext(ctx, "UPDATE decision_answers SET facet_indexed = false")
	require.NoError(t, err)
	_, err = cs.DB().ExecContext(ctx, "DELETE FROM decision_facet_entries")
	require.NoError(t, err)

	// Run with a small page size so pagination + SQLite yield are exercised.
	origPage := decisionIndexPageSize
	origYield := decisionIndexSQLiteYield
	decisionIndexPageSize = 3
	decisionIndexSQLiteYield = time.Millisecond
	t.Cleanup(func() {
		decisionIndexPageSize = origPage
		decisionIndexSQLiteYield = origYield
	})

	stats, err = f.srv.runDecisionIndexPass(ctx)
	require.NoError(t, err)
	assert.True(t, stats.HadPending)
	assert.Equal(t, 8, stats.StepCounts["settled"])
	assert.Equal(t, 10, stats.StepCounts["facet_decisions"], "all 10 decisions must be facet-indexed")
	assert.Equal(t, 9, stats.StepCounts["facet_answers"], "all 9 answers (8 direct + 1 interpreted) must be facet-indexed")
	assert.GreaterOrEqual(t, stats.Pages, 3)

	// Second pass must find nothing pending.
	stats2, err := f.srv.runDecisionIndexPass(ctx)
	require.NoError(t, err)
	assert.False(t, stats2.HadPending)
	assert.Equal(t, 0, stats2.Pages)

	// 2b. Reconciler step: insert a stale answerer row for a deleted user and
	// verify runDecisionIndexPass heals it even when no unindexed rows remain.
	staleUserID := api.NewUUID()
	_, err = cs.DB().ExecContext(ctx,
		`INSERT INTO decision_facet_entries (id, project_id, chat_key, kind, actor_id, name, email, latest_at, ref_count)
		 VALUES (?, ?, ?, 'answerer', ?, 'Ghost', 'ghost@test.com', ?, 1)`,
		api.NewUUID(), f.project.ID, tpID, staleUserID, time.Now().UTC(),
	)
	require.NoError(t, err)
	statsRec, err := f.srv.runDecisionIndexPass(ctx)
	require.NoError(t, err)
	assert.True(t, statsRec.HadPending)
	assert.Equal(t, 1, statsRec.StepCounts["reconcile"])

	// 3. Boot-time goroutine lifecycle: reset rows to settled=false & facet_indexed=false, start the
	// indexer, wait for it to drain, then verify stopDecisionIndexer is clean
	// and idempotent.
	_, err = cs.DB().ExecContext(ctx, "UPDATE decisions SET settled = false, facet_indexed = false")
	require.NoError(t, err)
	_, err = cs.DB().ExecContext(ctx, "UPDATE decision_answers SET facet_indexed = false")
	require.NoError(t, err)
	_, err = cs.DB().ExecContext(ctx, "DELETE FROM decision_facet_entries")
	require.NoError(t, err)

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.srv.startDecisionIndexer(loopCtx)
	// Double start is a no-op.
	f.srv.startDecisionIndexer(loopCtx)

	require.Eventually(t, func() bool {
		pending, perr := f.store.HasPendingDecisionIndexWork(ctx)
		return perr == nil && !pending
	}, 5*time.Second, 20*time.Millisecond, "boot-time indexer pass must drain pending rows")

	f.srv.stopDecisionIndexer()
	// Double stop is safe.
	f.srv.stopDecisionIndexer()
}
