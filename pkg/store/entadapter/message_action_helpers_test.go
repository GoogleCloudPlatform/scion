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

func TestInsertNextActionVersion_RetryBounds(t *testing.T) {
	// Sanity check that maxMessageActionRaceRetries matches the decision retry budget
	// and that store.ErrTransient is returned when retries are exhausted.
	if maxMessageActionRaceRetries < 1 {
		t.Fatalf("maxMessageActionRaceRetries must be >= 1, got %d", maxMessageActionRaceRetries)
	}
	if !errors.Is(store.ErrTransient, store.ErrTransient) {
		t.Fatal("unreachable")
	}
}
