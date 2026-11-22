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

package entadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestCASActionRows(t *testing.T) {
	ctx := context.Background()

	// Zero expected is an immediate no-op success.
	ok, cur, err := casActionRows[string](ctx, 0, func(context.Context) (int, error) {
		t.Fatal("update should not be called when expected == 0")
		return 0, nil
	}, nil)
	if err != nil || !ok || cur != nil {
		t.Fatalf("expected=0: got ok=%v cur=%v err=%v", ok, cur, err)
	}

	// Exact match -> ok=true, reload not called.
	ok, cur, err = casActionRows[string](ctx, 2,
		func(context.Context) (int, error) { return 2, nil },
		func(context.Context) ([]string, error) {
			t.Fatal("reload should not be called on CAS success")
			return nil, nil
		},
	)
	if err != nil || !ok || cur != nil {
		t.Fatalf("match: got ok=%v cur=%v err=%v", ok, cur, err)
	}

	// Partial match -> ok=false, reload returns current rows.
	ok, cur, err = casActionRows[string](ctx, 2,
		func(context.Context) (int, error) { return 1, nil },
		func(context.Context) ([]string, error) { return []string{"pending", "withdrawn"}, nil },
	)
	if err != nil || ok || len(cur) != 2 || cur[1] != "withdrawn" {
		t.Fatalf("mismatch: got ok=%v cur=%v err=%v", ok, cur, err)
	}

	// Update error propagates.
	wantErr := errors.New("boom")
	ok, cur, err = casActionRows[string](ctx, 1,
		func(context.Context) (int, error) { return 0, wantErr },
		nil,
	)
	if !errors.Is(err, wantErr) || ok || cur != nil {
		t.Fatalf("update err: got ok=%v cur=%v err=%v", ok, cur, err)
	}

	// Reload error propagates.
	reloadErr := errors.New("reload failed")
	ok, cur, err = casActionRows[string](ctx, 1,
		func(context.Context) (int, error) { return 0, nil },
		func(context.Context) ([]string, error) { return nil, reloadErr },
	)
	if !errors.Is(err, reloadErr) || ok || cur != nil {
		t.Fatalf("reload err: got ok=%v cur=%v err=%v", ok, cur, err)
	}
}

func TestInsertNextActionVersion_InTxPassthrough(t *testing.T) {
	ctx := context.Background()
	c := &CompositeStore{inTx: true}

	calls := 0
	got, err := insertNextActionVersion(ctx, c, func(_ *CompositeStore) (int, error) {
		calls++
		return 42, nil
	})
	if err != nil || got != 42 || calls != 1 {
		t.Fatalf("inTx success: got=%d err=%v calls=%d", got, err, calls)
	}

	// In-tx does not swallow or retry errMessageActionRace.
	_, err = insertNextActionVersion(ctx, c, func(_ *CompositeStore) (int, error) {
		return 0, errMessageActionRace
	})
	if !errors.Is(err, errMessageActionRace) {
		t.Fatalf("inTx race err: got %v, want errMessageActionRace", err)
	}
}

// Verifies bounded retries on errMessageActionRace, mapping exhausted retries
// to store.ErrTransient, and no retries on non-race errors.
func TestInsertNextActionVersion_RetryBounds(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	// 1. Exhausted race retries: called maxMessageActionRaceRetries+1 times,
	// returning store.ErrTransient.
	calls := 0
	_, err := insertNextActionVersion(ctx, cs, func(_ *CompositeStore) (int, error) {
		calls++
		return 0, errMessageActionRace
	})
	assert.Equal(t, maxMessageActionRaceRetries+1, calls, "must attempt 1 initial + maxMessageActionRaceRetries retries")
	assert.True(t, errors.Is(err, store.ErrTransient), "exhausted race retries must return store.ErrTransient, got %v", err)

	// 2. Transient race that resolves on the 3rd attempt succeeds.
	calls = 0
	got, err := insertNextActionVersion(ctx, cs, func(_ *CompositeStore) (int, error) {
		calls++
		if calls < 3 {
			return 0, errMessageActionRace
		}
		return 99, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
	assert.Equal(t, 99, got)

	// 3. Non-race error is never retried.
	calls = 0
	_, err = insertNextActionVersion(ctx, cs, func(_ *CompositeStore) (int, error) {
		calls++
		return 0, store.ErrInvalidInput
	})
	assert.Equal(t, 1, calls, "non-race error must not be retried")
	assert.True(t, errors.Is(err, store.ErrInvalidInput), "expected store.ErrInvalidInput, got %v", err)
}

// Seam test C4: insertNextActionVersion and casActionRows compose inside one
// caller-supplied transaction so a create-and-resolve flow (e.g. an inline
// approval that mints v1 and transitions it to resolved atomically) commits as
// one unit.
func TestMessageActionHelpers_CreateAndResolveInOneTx(t *testing.T) {
	ctx := context.Background()
	f := newDecisionFixture(t)

	var createdRowID string
	err := f.cs.WithTx(ctx, func(tx store.Store) error {
		txStore := tx.(*CompositeStore)

		// 1. Insert v1 via insertNextActionVersion (inside caller's tx).
		res, err := insertNextActionVersion(ctx, txStore, func(inner *CompositeStore) (*store.DecisionPromptResult, error) {
			return inner.CreateDecisionPrompt(ctx, f.prompt("inline", false, yesNo("EXEC1", "Run tests?")))
		})
		if err != nil {
			return err
		}
		createdRowID = res.Decisions[0].RowID
		rowUID, err := uuid.Parse(createdRowID)
		if err != nil {
			return err
		}

		// 2. Transition pending -> decided in the same tx via casActionRows.
		now := time.Now().UTC().Truncate(time.Microsecond)
		ok, current, err := casActionRows[*ent.Decision](ctx, 1,
			func(ctx context.Context) (int, error) {
				return txStore.client.Decision.Update().
					Where(decision.ID(rowUID), decision.StatusEQ(decision.StatusPending)).
					SetStatus(decision.StatusDecided).
					SetDecidedAt(now).
					Save(ctx)
			},
			func(ctx context.Context) ([]*ent.Decision, error) {
				return txStore.client.Decision.Query().Where(decision.ID(rowUID)).All(ctx)
			},
		)
		if err != nil {
			return err
		}
		require.True(t, ok, "CAS on freshly inserted row must succeed in same tx")
		assert.Nil(t, current)

		// 3. A second CAS expecting pending fails and reloads the current row
		// showing StatusDecided inside the same tx.
		ok2, current2, err := casActionRows[*ent.Decision](ctx, 1,
			func(ctx context.Context) (int, error) {
				return txStore.client.Decision.Update().
					Where(decision.ID(rowUID), decision.StatusEQ(decision.StatusPending)).
					SetStatus(decision.StatusWithdrawn).
					Save(ctx)
			},
			func(ctx context.Context) ([]*ent.Decision, error) {
				return txStore.client.Decision.Query().Where(decision.ID(rowUID)).All(ctx)
			},
		)
		if err != nil {
			return err
		}
		require.False(t, ok2)
		require.Len(t, current2, 1)
		assert.Equal(t, decision.StatusDecided, current2[0].Status)
		return nil
	})
	require.NoError(t, err)

	// Committed state reflects both the insert and the resolve atomically.
	got, err := f.cs.GetDecision(ctx, createdRowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusDecided, got.Status)
	assert.NotNil(t, got.DecidedAt)
}
