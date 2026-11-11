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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// The answer and --update repost interleavings below need row locks that
// only Postgres takes (SQLite has one writer at a time). Always compiled;
// they skip unless the enttest Postgres backend is built (-tags
// integration) and SCION_TEST_POSTGRES_URL is set, as in CI's entadapter
// Postgres job:
//
//	SCION_TEST_POSTGRES_URL='postgres://user:pass@host:5432/postgres?sslmode=disable' \
//	  go test -tags integration -run 'TestDecisionRace_' ./pkg/store/entadapter/

func requirePostgres(t *testing.T) {
	t.Helper()
	if !enttest.Active() {
		t.Skip("needs the Postgres backend: -tags integration and SCION_TEST_POSTGRES_URL")
	}
}

// blockedFor is how long a call must stay blocked to count as waiting on
// the other transaction's row lock.
const blockedFor = 300 * time.Millisecond

// TestDecisionRace_RepostWaitsForAnswerInFlight: an answer holds the
// decision row's lock; a concurrent --update repost waits for it and then
// reports the replaced version as decided, with its answer (not the stale
// pending state).
func TestDecisionRace_RepostWaitsForAnswerInFlight(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	row := f.create(t, false, yesNo("D1", "v1")).Decisions[0].RowID

	tx, err := f.cs.client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, _, err = newTxCompositeStore(tx).recordAnswerTx(ctx, f.answer(row, "1", "user-1", ""))
	require.NoError(t, err)

	type result struct {
		res *store.ChoicePromptResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := f.cs.CreateChoicePrompt(ctx, f.prompt("", true, yesNo("D1", "v2")))
		done <- result{res, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the repost did not wait for the answer's lock: %+v", r)
	case <-time.After(blockedFor):
	}
	require.NoError(t, tx.Commit())

	r := <-done
	require.NoError(t, r.err)
	replaced := r.res.Decisions[0].Replaced
	require.NotNil(t, replaced)
	assert.Equal(t, store.DecisionStatusDecided, replaced.Status, "the repost read the committed answer")
	require.NotNil(t, replaced.Answer)
	assert.Equal(t, "yes", replaced.Answer.ChoiceLabel)
}

// TestDecisionRace_AnswerWaitsForRepostInFlight: a repost holds the lock of
// the version it replaces; a concurrent answer to that version waits for
// it and then gets ErrDecisionReplaced, and nothing of the answer is
// stored.
func TestDecisionRace_AnswerWaitsForRepostInFlight(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	row := f.create(t, false, yesNo("D1", "v1")).Decisions[0].RowID

	tx, err := f.cs.client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = newTxCompositeStore(tx).createChoicePromptTx(ctx, f.prompt("", true, yesNo("D1", "v2")))
	require.NoError(t, err)

	in := f.answer(row, "1", "user-1", "")
	done := make(chan error, 1)
	go func() {
		_, _, err := f.cs.RecordAnswer(ctx, in)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the answer did not wait for the repost's lock: %v", err)
	case <-time.After(blockedFor):
	}
	require.NoError(t, tx.Commit())

	require.ErrorIs(t, <-done, store.ErrDecisionReplaced)
	d, err := f.cs.GetDecision(ctx, row)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusReplaced, d.Status)
	assert.Empty(t, d.Answers, "the refused answer stored nothing")
	_, err = f.cs.GetMessage(ctx, in.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "nor its response message")
}

// TestDecisionRace_ConcurrentSetSubmitsOneWins: a set submit holds the
// locks of its prompt's rows; a concurrent set submit of the same prompt
// waits for it and then gets a changed set (every row decided), and
// nothing of the loser is stored.
func TestDecisionRace_ConcurrentSetSubmitsOneWins(t *testing.T) {
	requirePostgres(t)
	f := newDecisionFixture(t)
	ctx := context.Background()
	msgID, rows := f.createSet(t, threeDecisions()...)

	tx, err := f.cs.client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	won, err := newTxCompositeStore(tx).recordAnswerSetTx(ctx, f.answerSet(msgID, "user-1", "", rows[0], "1", rows[1], "1", rows[2], "1"))
	require.NoError(t, err)

	loser := f.answerSet(msgID, "user-2", "", rows[0], "2", rows[1], "2", rows[2], "2")
	done := make(chan error, 1)
	go func() {
		_, err := f.cs.RecordAnswerSet(ctx, loser)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the second submit did not wait for the first one's locks: %v", err)
	case <-time.After(blockedFor):
	}
	require.NoError(t, tx.Commit())

	err = <-done
	var changed *store.AnswerSetChangedError
	require.ErrorAs(t, err, &changed)
	assert.Len(t, changed.Reasons, 3)
	for _, id := range rows {
		assert.Equal(t, store.AnswerSetReasonDecided, changed.Reasons[id])
		d, err := f.cs.GetDecision(ctx, id)
		require.NoError(t, err)
		require.Len(t, d.Answers, 1, "only the winner's answer")
		assert.Equal(t, won.BatchID, d.Answers[0].BatchID)
	}
	_, err = f.cs.GetMessage(ctx, loser.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the loser's response message is not stored")
}
