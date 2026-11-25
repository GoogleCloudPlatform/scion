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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Postgres concurrency / race tests for decision retention purge (design-8984 §3.4 & §5).
// Pattern matches decision_race_postgres_test.go: always compiled; skipped unless
// built with -tags integration and SCION_TEST_POSTGRES_URL is set.

func backdateDecisionForRace(t *testing.T, cs *CompositeStore, decID string, created time.Time, decided *time.Time, ansTime *time.Time) {
	t.Helper()
	ctx := context.Background()
	uid := uuid.MustParse(decID)
	db := clientDB(cs.client)
	_, err := db.ExecContext(ctx, "UPDATE decisions SET created_at = $1 WHERE id = $2", created.UTC(), uid)
	require.NoError(t, err)
	if decided != nil {
		require.NoError(t, cs.client.Decision.UpdateOneID(uid).SetDecidedAt(decided.UTC()).Exec(ctx))
	}
	if ansTime != nil {
		_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = $1 WHERE decision_row_id = $2", ansTime.UTC(), uid)
		require.NoError(t, err)
	}
	// Rebuild facet_entries from the backdated timestamps so facet_entries.latest_at
	// matches the backdated rows before the race begins.
	_, err = cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))
	drainDecisionIndex(t, ctx, cs, 100)
}

// TestDecisionRetentionRace_PurgeVsAnswer: an answer holds the decision row lock
// on an old interpreted-awaiting decision; purge waits on FOR UPDATE, then sees
// the newly committed direct answer is recent and skips the message.
func TestDecisionRetentionRace_PurgeVsAnswer(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	oldTime := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Microsecond)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)

	p := f.prompt("", false, yesNo("RA1", "v1"))
	p.Message.ThreadID = "topic-race-1"
	res, err := f.cs.CreateDecisionPrompt(ctx, p)
	require.NoError(t, err)
	rowID := res.Decisions[0].RowID
	msgID := p.Message.ID
	backdateDecisionForRace(t, f.cs, rowID, oldTime, nil, nil)

	// Start a direct answer tx holding FOR UPDATE on the decision row.
	tx, err := f.cs.client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, _, err = newTxCompositeStore(tx).recordAnswerTx(ctx, f.answer(rowID, "1", "user-1", ""))
	require.NoError(t, err)

	st := f.cs.DecisionRetention()
	type purgeOut struct {
		res store.DecisionRetentionPurgeResult
		err error
	}
	done := make(chan purgeOut, 1)
	go func() {
		r, err := st.PurgeDecisionMessages(ctx, []string{msgID}, map[string]time.Time{f.projectID: cutoff})
		done <- purgeOut{r, err}
	}()

	select {
	case out := <-done:
		t.Fatalf("purge did not wait for answer's FOR UPDATE lock: %+v", out)
	case <-time.After(blockedFor):
	}
	require.NoError(t, tx.Commit())

	out := <-done
	require.NoError(t, out.err)
	assert.Equal(t, 0, out.res.PurgedMessages, "recent answer makes message ineligible")
	assert.Equal(t, 1, out.res.SkippedMessages)

	_, err = f.cs.GetDecision(ctx, rowID)
	require.NoError(t, err)
	assertFacetSummaryMatchesRecompute(t, ctx, f.cs.client)
}

// TestDecisionRetentionRace_PurgeVsDismiss: a dismiss tx holds FOR UPDATE on an
// old pending decision; purge waits, sees the fresh dismissal, and skips.
func TestDecisionRetentionRace_PurgeVsDismiss(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	oldTime := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Microsecond)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)

	p := f.prompt("", false, yesNo("RD1", "v1"))
	p.Message.ThreadID = "topic-race-dismiss"
	res, err := f.cs.CreateDecisionPrompt(ctx, p)
	require.NoError(t, err)
	rowID := res.Decisions[0].RowID
	msgID := p.Message.ID
	backdateDecisionForRace(t, f.cs, rowID, oldTime, nil, nil)

	tx, err := f.cs.client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = newTxCompositeStore(tx).dismissDecisionsTx(ctx, &store.DismissDecisionsInput{
		MessageID: msgID,
		User:      store.DecisionAnswerer{UserID: "user-1", Name: "User 1", Email: "u1@test.com"},
		Surface:   store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: f.projectID, ThreadID: "topic-race-dismiss",
			Sender: "user:user-1", SenderID: "user-1", Recipient: "agent:bot", RecipientID: f.agentID,
		},
	})
	require.NoError(t, err)

	st := f.cs.DecisionRetention()
	done := make(chan error, 1)
	go func() {
		r, err := st.PurgeDecisionMessages(ctx, []string{msgID}, map[string]time.Time{f.projectID: cutoff})
		if err == nil && r.PurgedMessages != 0 {
			t.Errorf("expected 0 purged, got %d", r.PurgedMessages)
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("purge did not wait for dismiss's FOR UPDATE lock: %v", err)
	case <-time.After(blockedFor):
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)

	d, err := f.cs.GetDecision(ctx, rowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusDismissed, d.Status)
	assertFacetSummaryMatchesRecompute(t, ctx, f.cs.client)
}

// TestDecisionRetentionRace_PurgeVsRepost: a --update repost holds FOR UPDATE on
// an old decided row (marking it replaced with a fresh replaced_at); purge waits,
// sees the fresh replaced_at, and skips.
func TestDecisionRetentionRace_PurgeVsRepost(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	oldTime := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Microsecond)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)

	p1 := f.prompt("", false, yesNo("RR1", "v1"))
	p1.Message.ThreadID = "topic-race-repost"
	r1, err := f.cs.CreateDecisionPrompt(ctx, p1)
	require.NoError(t, err)
	rowID := r1.Decisions[0].RowID
	msgID := p1.Message.ID
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(rowID, "1", "user-1", ""))
	require.NoError(t, err)
	backdateDecisionForRace(t, f.cs, rowID, oldTime, &oldTime, &oldTime)

	tx, err := f.cs.client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	p2 := f.prompt("", true, yesNo("RR1", "v2"))
	p2.Message.ThreadID = "topic-race-repost"
	_, err = newTxCompositeStore(tx).createDecisionPromptTx(ctx, p2)
	require.NoError(t, err)

	st := f.cs.DecisionRetention()
	done := make(chan error, 1)
	go func() {
		r, err := st.PurgeDecisionMessages(ctx, []string{msgID}, map[string]time.Time{f.projectID: cutoff})
		if err == nil && r.PurgedMessages != 0 {
			t.Errorf("expected 0 purged after fresh repost, got %d", r.PurgedMessages)
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("purge did not wait for repost's FOR UPDATE lock: %v", err)
	case <-time.After(blockedFor):
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)

	d, err := f.cs.GetDecision(ctx, rowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusReplaced, d.Status)
	assertFacetSummaryMatchesRecompute(t, ctx, f.cs.client)
}

// TestDecisionRetentionRace_PurgeVsFacetIndexerAndUserDelete runs purge
// concurrently with IndexDecisionFacetsDecisionsPage, IndexDecisionFacetsAnswersPage,
// and clearDecisionAnswerUser across a batch of old settled decisions and asserts
// that the final decision_facet_entries table equals a full recompute.
func TestDecisionRetentionRace_PurgeVsFacetIndexerAndUserDelete(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	oldTime := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Microsecond)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)

	const total = 20
	var msgIDs []string
	for i := 0; i < total; i++ {
		uid := "user-keep"
		if i%2 == 0 {
			uid = "user-del"
		}
		p := f.prompt("", false, messages.Decision{
			Key:      "K" + uuid.NewString()[:8],
			Question: "Race Q",
			Choices:  []messages.Choice{{ID: "1", Label: "Yes"}, {ID: "2", Label: "No"}},
		})
		p.Message.ThreadID = "topic-race-mix"
		r, err := f.cs.CreateDecisionPrompt(ctx, p)
		require.NoError(t, err)
		_, _, err = f.cs.RecordAnswer(ctx, f.answer(r.Decisions[0].RowID, "1", uid, ""))
		require.NoError(t, err)
		backdateDecisionForRace(t, f.cs, r.Decisions[0].RowID, oldTime.Add(time.Duration(i)*time.Minute), &oldTime, &oldTime)
		msgIDs = append(msgIDs, p.Message.ID)
	}

	// Reset facet_indexed to false on half of the rows so the facet indexer has real work to race with purge.
	_, err := f.cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, f.cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, f.cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))

	st := f.cs.DecisionRetention()
	var wg sync.WaitGroup
	wg.Add(4)

	// Goroutine 1: purge first half of messages.
	go func() {
		defer wg.Done()
		_, err := st.PurgeDecisionMessages(ctx, msgIDs[:total/2], map[string]time.Time{f.projectID: cutoff})
		assert.NoError(t, err)
	}()

	// Goroutine 2: facet indexer decisions page.
	go func() {
		defer wg.Done()
		cursor := ""
		for {
			_, next, err := f.cs.IndexDecisionFacetsDecisionsPage(ctx, cursor, 5)
			assert.NoError(t, err)
			if next == "" {
				break
			}
			cursor = next
		}
	}()

	// Goroutine 3: facet indexer answers page.
	go func() {
		defer wg.Done()
		cursor := ""
		for {
			_, next, err := f.cs.IndexDecisionFacetsAnswersPage(ctx, cursor, 5)
			assert.NoError(t, err)
			if next == "" {
				break
			}
			cursor = next
		}
	}()

	// Goroutine 4: user deletion clearing user-del's answers & answerer facet rows.
	go func() {
		defer wg.Done()
		err := f.cs.WithTx(ctx, func(tx store.Store) error {
			return clearDecisionAnswerUser(ctx, tx.(*CompositeStore).client, "user-del")
		})
		assert.NoError(t, err)
	}()

	wg.Wait()

	// Drain any remaining unindexed rows (on the kept second half) and assert exact summary parity.
	drainDecisionIndex(t, ctx, f.cs, 100)
	assertFacetSummaryMatchesRecompute(t, ctx, f.cs.client)
}
