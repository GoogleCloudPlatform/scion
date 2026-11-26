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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
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

// TestDecisionRetentionRace_DeterministicAnswersIndexerSnapshotVsPurge
// deterministically exercises the exact interleaving that broke the answers
// facet indexer before R1:
//  1. indexDecisionFacetsAnswersPageTx reads its page of unindexed answers
//     without a lock and pauses before acquiring FOR UPDATE SKIP LOCKED.
//  2. PurgeDecisionMessages runs and commits on both an indexed message and
//     the message whose answer is in the indexer's unlocked snapshot.
//  3. Because purge treats any message with facet_indexed=false decision or
//     answer rows as ineligible, it purges only the indexed message and leaves
//     the unindexed message intact.
//  4. indexDecisionFacetsAnswersPageTx resumes, locks the decision, flips
//     facet_indexed=true with zero CAS mismatch, and the summary matches a
//     full recompute.
func TestDecisionRetentionRace_DeterministicAnswersIndexerSnapshotVsPurge(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	oldTime := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Microsecond)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)

	pIdx := f.prompt("", false, yesNo("DET-IDX", "v1"))
	pIdx.Message.ThreadID = "topic-race-det"
	rIdx, err := f.cs.CreateDecisionPrompt(ctx, pIdx)
	require.NoError(t, err)
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(rIdx.Decisions[0].RowID, "1", "user-1", ""))
	require.NoError(t, err)
	backdateDecisionForRace(t, f.cs, rIdx.Decisions[0].RowID, oldTime, &oldTime, &oldTime)

	pUnidx := f.prompt("", false, yesNo("DET-UNIDX", "v1"))
	pUnidx.Message.ThreadID = "topic-race-det"
	rUnidx, err := f.cs.CreateDecisionPrompt(ctx, pUnidx)
	require.NoError(t, err)
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(rUnidx.Decisions[0].RowID, "1", "user-1", ""))
	require.NoError(t, err)
	backdateDecisionForRace(t, f.cs, rUnidx.Decisions[0].RowID, oldTime.Add(time.Minute), &oldTime, &oldTime)

	// Leave pIdx fully indexed (decision=true, answer=true) and pUnidx with
	// decision=true, answer=false, with decision_facet_entries matching.
	unidxDecUUID := uuid.MustParse(rUnidx.Decisions[0].RowID)
	_, err = f.cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, f.cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, f.cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))
	// Index both decisions.
	_, _, err = f.cs.IndexDecisionFacetsDecisionsPage(ctx, "", 100)
	require.NoError(t, err)
	// Index only pIdx's answer (temporarily mark pUnidx's answer=true, then flip back).
	require.NoError(t, f.cs.client.DecisionAnswer.Update().
		Where(decisionanswer.DecisionRowIDEQ(unidxDecUUID)).
		SetFacetIndexed(true).Exec(ctx))
	_, _, err = f.cs.IndexDecisionFacetsAnswersPage(ctx, "", 100)
	require.NoError(t, err)
	require.NoError(t, f.cs.client.DecisionAnswer.Update().
		Where(decisionanswer.DecisionRowIDEQ(unidxDecUUID)).
		SetFacetIndexed(false).Exec(ctx))

	snapshotTaken := make(chan struct{})
	purgeDone := make(chan struct{})
	prevHook := testHookAfterAnswersPageRead
	testHookAfterAnswersPageRead = func() {
		close(snapshotTaken)
		<-purgeDone
	}
	t.Cleanup(func() { testHookAfterAnswersPageRead = prevHook })

	type idxOut struct {
		indexed int
		err     error
	}
	idxDone := make(chan idxOut, 1)
	go func() {
		n, _, err := f.cs.IndexDecisionFacetsAnswersPage(ctx, "", 10)
		idxDone <- idxOut{n, err}
	}()

	// Wait until answers page has taken its unlocked snapshot of pUnidx's answer.
	<-snapshotTaken

	// Run purge while the indexer holds its unlocked snapshot.
	st := f.cs.DecisionRetention()
	pr, err := st.PurgeDecisionMessages(ctx, []string{pIdx.Message.ID, pUnidx.Message.ID}, map[string]time.Time{f.projectID: cutoff})
	close(purgeDone)

	require.NoError(t, err)
	assert.Equal(t, 1, pr.PurgedMessages, "indexed message must be purged")
	assert.Equal(t, 1, pr.SkippedMessages, "unindexed message must be skipped while indexer holds snapshot")

	out := <-idxDone
	require.NoError(t, out.err, "answers page indexer must not suffer CAS mismatch")
	assert.Equal(t, 1, out.indexed)

	_, err = f.cs.GetDecision(ctx, rIdx.Decisions[0].RowID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.cs.GetDecision(ctx, rUnidx.Decisions[0].RowID)
	assert.NoError(t, err)
	assertFacetSummaryMatchesRecompute(t, ctx, f.cs.client)

	// Once indexed, pUnidx purges cleanly on the next pass.
	pr2, err := st.PurgeDecisionMessages(ctx, []string{pUnidx.Message.ID}, map[string]time.Time{f.projectID: cutoff})
	require.NoError(t, err)
	assert.Equal(t, 1, pr2.PurgedMessages)
	assertFacetSummaryMatchesRecompute(t, ctx, f.cs.client)
}

// TestDecisionRetentionRace_DeterministicPurgeVsRepostVersionLockOrder
// deterministically exercises the multi-version lock-order interleaving:
// an agent key K has 6 versions (v1..v6), each answered and replaced on its
// own message, back-dated 60 days, with a later version's UUID sorting below
// v1's UUID. Holding v1's FOR UPDATE lock while both PurgeDecisionMessages
// (all 6 messages) and a --update repost of K queue up would deadlock with
// 40P01 if purge locked by id while loadKeyStates locked by (key_norm, version).
// With purge locking in (key_norm, version, asker_agent_id, id) order, both
// serialize on v1 and succeed with zero deadlocks.
func TestDecisionRetentionRace_DeterministicPurgeVsRepostVersionLockOrder(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	oldTime := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Microsecond)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)

	const versions = 6
	var (
		key    string
		msgIDs []string
		rowIDs []string
	)
	// Seed 6 versions of a key; guarantee a later version's UUID sorts below
	// v1's UUID so an id-ordered purge would lock it before blocking on v1.
	for attempt := 0; attempt < 32; attempt++ {
		key = "LOCK-" + uuid.NewString()[:8]
		msgIDs = make([]string, 0, versions)
		rowIDs = make([]string, 0, versions)
		for v := 0; v < versions; v++ {
			p := f.prompt("", v > 0, yesNo(key, "v"))
			p.Message.ThreadID = "topic-race-lockorder"
			res, err := f.cs.CreateDecisionPrompt(ctx, p)
			require.NoError(t, err)
			rowID := res.Decisions[0].RowID
			_, _, err = f.cs.RecordAnswer(ctx, f.answer(rowID, "1", "user-1", ""))
			require.NoError(t, err)
			msgIDs = append(msgIDs, p.Message.ID)
			rowIDs = append(rowIDs, rowID)
		}
		hasLowerLaterID := false
		for v := 1; v < versions; v++ {
			if rowIDs[v] < rowIDs[0] {
				hasLowerLaterID = true
				break
			}
		}
		if hasLowerLaterID {
			break
		}
		if attempt == 31 {
			t.Fatal("failed to generate a later version UUID sorting below v1")
		}
	}

	// Back-date all 6 versions (created_at, decided_at, replaced_at, answer.created_at)
	// and rebuild facet entries so all 6 messages are purge-eligible.
	db := clientDB(f.cs.client)
	for v, rowID := range rowIDs {
		uid := uuid.MustParse(rowID)
		ts := oldTime.Add(time.Duration(v) * time.Minute)
		_, err := db.ExecContext(ctx, "UPDATE decisions SET created_at = $1 WHERE id = $2", ts, uid)
		require.NoError(t, err)
		upd := f.cs.client.Decision.UpdateOneID(uid).SetDecidedAt(ts)
		if v < versions-1 {
			upd = upd.SetReplacedAt(ts)
		}
		require.NoError(t, upd.Exec(ctx))
		_, err = db.ExecContext(ctx, "UPDATE decision_answers SET created_at = $1 WHERE decision_row_id = $2", ts, uid)
		require.NoError(t, err)
	}
	_, err := f.cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, f.cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, f.cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))
	drainDecisionIndex(t, ctx, f.cs, 100)

	// Hold FOR UPDATE on v1 first.
	holdTx, err := f.cs.client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holdTx.Rollback() })
	_, err = holdTx.Decision.Query().
		Where(decision.IDEQ(uuid.MustParse(rowIDs[0]))).
		ForUpdate().
		Only(ctx)
	require.NoError(t, err)

	st := f.cs.DecisionRetention()
	purgeDone := make(chan error, 1)
	go func() {
		_, err := st.PurgeDecisionMessages(ctx, msgIDs, map[string]time.Time{f.projectID: cutoff})
		purgeDone <- err
	}()

	// Wait until purge is blocked on v1 (not holding a later version lock ahead of v1).
	select {
	case err := <-purgeDone:
		t.Fatalf("purge did not block on v1 lock: %v", err)
	case <-time.After(blockedFor):
	}

	repostDone := make(chan error, 1)
	go func() {
		p := f.prompt("", true, yesNo(key, "v7"))
		p.Message.ThreadID = "topic-race-lockorder"
		_, err := f.cs.CreateDecisionPrompt(ctx, p)
		repostDone <- err
	}()

	select {
	case err := <-repostDone:
		t.Fatalf("repost did not block on v1 lock: %v", err)
	case <-time.After(blockedFor):
	}

	// Release v1: both purge and repost must succeed without 40P01 deadlock.
	require.NoError(t, holdTx.Rollback())
	require.NoError(t, <-purgeDone, "purge must succeed without lock-order deadlock")
	require.NoError(t, <-repostDone, "repost must succeed without lock-order deadlock")
	assertFacetSummaryMatchesRecompute(t, ctx, f.cs.client)
}
