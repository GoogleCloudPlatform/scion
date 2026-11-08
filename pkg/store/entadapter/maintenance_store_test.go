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
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/maintenanceoperationrun"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMaintenanceStore(t *testing.T) *MaintenanceStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewMaintenanceStore(client)
}

func TestSeedMaintenanceOperations(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()

	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	ops, err := s.ListMaintenanceOperations(ctx)
	require.NoError(t, err)
	assert.Len(t, ops, len(defaultSeedOperations))

	// Every seeded op must have a valid Go-generated UUID id (not a randomblob).
	for _, op := range ops {
		_, err := uuid.Parse(op.ID)
		assert.NoError(t, err, "seeded op %q should have a valid UUID id", op.Key)
		assert.Equal(t, store.MaintenanceStatusPending, op.Status)
	}

	// Seeding is idempotent.
	require.NoError(t, s.SeedMaintenanceOperations(ctx))
	ops, err = s.ListMaintenanceOperations(ctx)
	require.NoError(t, err)
	assert.Len(t, ops, len(defaultSeedOperations))
}

// TestSeedMaintenanceOperations_PartialAndRepeated covers the upsert path
// (ptone/scion#366): a store that already holds some seed keys gets only the
// missing ones, existing rows (and their state) are left untouched, and
// repeated seeding never errors.
func TestSeedMaintenanceOperations_PartialAndRepeated(t *testing.T) {
	require.GreaterOrEqual(t, len(defaultSeedOperations), 2, "test needs at least two seed operations")
	s := newTestMaintenanceStore(t)
	ctx := context.Background()

	// Pre-create one seed key, already in a non-default state.
	first := defaultSeedOperations[0]
	require.NoError(t, s.client.MaintenanceOperation.Create().
		SetKey(first.Key).
		SetTitle("pre-existing title").
		SetCategory(first.Category).
		SetStatus(store.MaintenanceStatusCompleted).
		Exec(ctx))
	before, err := s.GetMaintenanceOperation(ctx, first.Key)
	require.NoError(t, err)

	require.NoError(t, s.SeedMaintenanceOperations(ctx))
	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	ops, err := s.ListMaintenanceOperations(ctx)
	require.NoError(t, err)
	assert.Len(t, ops, len(defaultSeedOperations), "every seed key exactly once")

	after, err := s.GetMaintenanceOperation(ctx, first.Key)
	require.NoError(t, err)
	assert.Equal(t, before.ID, after.ID, "existing row must not be replaced")
	assert.Equal(t, "pre-existing title", after.Title, "existing row must not be overwritten")
	assert.Equal(t, store.MaintenanceStatusCompleted, after.Status, "existing row's state must be preserved")
}

// TestSeedMaintenanceOperations_ConcurrentOnEmptyStore: seeders that run at
// the same time on an empty store all succeed and every key ends up exactly
// once. In production Migrate's advisory lock serializes seeding on Postgres;
// this calls SeedMaintenanceOperations directly, outside that lock. On SQLite
// the test client serializes writes; on Postgres (make
// test-launch-store-postgres) the seeders really race into ON CONFLICT (key)
// DO NOTHING with fewer RETURNING rows than builders.
func TestSeedMaintenanceOperations_ConcurrentOnEmptyStore(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.SeedMaintenanceOperations(ctx)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		assert.NoError(t, err, "concurrent seeder %d", i)
	}

	ops, err := s.ListMaintenanceOperations(ctx)
	require.NoError(t, err)
	assert.Len(t, ops, len(defaultSeedOperations), "every seed key exactly once")
}

// TestSeedMaintenanceOperations_DoesNotMaskOtherConstraintErrors pins the
// behaviour change in ptone/scion#366: only a conflict on the seed key is
// skipped. The old code swallowed every ent constraint error, so a seed row
// rejected for any other reason silently went missing while seeding reported
// success. A hook forces one seed row onto an existing primary key, a
// constraint violation that is not the key conflict.
func TestSeedMaintenanceOperations_DoesNotMaskOtherConstraintErrors(t *testing.T) {
	require.GreaterOrEqual(t, len(defaultSeedOperations), 2, "test needs at least two seed operations")
	s := newTestMaintenanceStore(t)
	ctx := context.Background()

	existing, err := s.client.MaintenanceOperation.Create().
		SetKey("unrelated-existing-op").
		SetTitle("unrelated").
		SetCategory("operation").
		Save(ctx)
	require.NoError(t, err)

	target := defaultSeedOperations[1].Key
	s.client.MaintenanceOperation.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if mm, ok := m.(*ent.MaintenanceOperationMutation); ok && mm.Op().Is(ent.OpCreate) {
				if key, ok := mm.Key(); ok && key == target {
					mm.SetID(existing.ID)
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	err = s.SeedMaintenanceOperations(ctx)
	require.Error(t, err, "a non-key constraint violation must not be reported as a successful seed")
	assert.True(t, ent.IsConstraintError(err), "want the underlying constraint error, got %v", err)
}

func TestGetMaintenanceOperationNotFound(t *testing.T) {
	s := newTestMaintenanceStore(t)
	_, err := s.GetMaintenanceOperation(context.Background(), "does-not-exist")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateMaintenanceOperation(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	startedAt := time.Now().UTC().Truncate(time.Second)
	op := &store.MaintenanceOperation{
		Key:       "pull-images",
		Status:    store.MaintenanceStatusRunning,
		StartedAt: &startedAt,
		StartedBy: "admin",
		Metadata:  `{"foo":"bar"}`,
	}
	require.NoError(t, s.UpdateMaintenanceOperation(ctx, op))

	got, err := s.GetMaintenanceOperation(ctx, "pull-images")
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusRunning, got.Status)
	assert.Equal(t, "admin", got.StartedBy)
	assert.Equal(t, `{"foo":"bar"}`, got.Metadata)
	require.NotNil(t, got.StartedAt)
}

func TestUpdateMaintenanceOperationNotFound(t *testing.T) {
	s := newTestMaintenanceStore(t)
	err := s.UpdateMaintenanceOperation(context.Background(), &store.MaintenanceOperation{
		Key:    "ghost",
		Status: store.MaintenanceStatusRunning,
	})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestMaintenanceRunRMW(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	run := &store.MaintenanceOperationRun{
		ID:           uuid.NewString(),
		OperationKey: "pull-images",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    time.Now().UTC().Truncate(time.Second),
		StartedBy:    "admin",
		Log:          "starting",
	}
	require.NoError(t, s.CreateMaintenanceRun(ctx, run))

	got, err := s.GetMaintenanceRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusRunning, got.Status)
	assert.Equal(t, "pull-images", got.OperationKey)

	// Read-modify-write to completed.
	completedAt := time.Now().UTC().Truncate(time.Second)
	run.Status = store.MaintenanceStatusCompleted
	run.CompletedAt = &completedAt
	run.Result = `{"ok":true}`
	run.Log = "starting\ndone"
	require.NoError(t, s.UpdateMaintenanceRun(ctx, run))

	got, err = s.GetMaintenanceRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusCompleted, got.Status)
	require.NotNil(t, got.CompletedAt)
	assert.Equal(t, `{"ok":true}`, got.Result)
	assert.Equal(t, "starting\ndone", got.Log)

	// List runs for the operation.
	runs, err := s.ListMaintenanceRuns(ctx, "pull-images", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, run.ID, runs[0].ID)
}

func TestGetMaintenanceRunNotFound(t *testing.T) {
	s := newTestMaintenanceStore(t)
	_, err := s.GetMaintenanceRun(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAbortRunningMaintenanceOps(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	// A running migration should be reset to pending.
	startedAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.UpdateMaintenanceOperation(ctx, &store.MaintenanceOperation{
		Key:       "secret-hub-id-migration",
		Status:    store.MaintenanceStatusRunning,
		StartedAt: &startedAt,
	}))

	// A running run should be marked failed.
	run := &store.MaintenanceOperationRun{
		ID:           uuid.NewString(),
		OperationKey: "pull-images",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    startedAt,
	}
	require.NoError(t, s.CreateMaintenanceRun(ctx, run))

	runs, migrations, err := s.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), runs)
	assert.Equal(t, int64(1), migrations)

	gotRun, err := s.GetMaintenanceRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusFailed, gotRun.Status)
	require.NotNil(t, gotRun.CompletedAt)

	gotMig, err := s.GetMaintenanceOperation(ctx, "secret-hub-id-migration")
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusPending, gotMig.Status)
	assert.Nil(t, gotMig.StartedAt)
}

// createRunWithResult stores a run of pull-images with the given status and
// Result, and returns its ID.
func createRunWithResult(t *testing.T, s *MaintenanceStore, status, result string) string {
	t.Helper()
	run := &store.MaintenanceOperationRun{
		ID:           uuid.NewString(),
		OperationKey: "pull-images",
		Status:       status,
		StartedAt:    time.Now().UTC().Truncate(time.Second),
		Result:       result,
	}
	if status != store.MaintenanceStatusRunning {
		done := run.StartedAt.Add(time.Minute)
		run.CompletedAt = &done
	}
	require.NoError(t, s.CreateMaintenanceRun(context.Background(), run))
	return run.ID
}

// A running run whose Result is a JSON object keeps every key it recorded
// (values byte for byte) and gains the abort error and "aborted": true.
// Any other Result is replaced by the abort text, as before, and a run that
// is not running is left alone. Runs on SQLite, and on Postgres under
// -tags integration.
func TestAbortRunningMaintenanceOps_KeepsObjectResultKeys(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	const recorded = `{"rerunOf":"run-1","override":{"actorId":"u-1","findings":[{"code":"c"}]},"big":12345678901234567890,"error":"old"}`
	objectRun := createRunWithResult(t, s, store.MaintenanceStatusRunning, recorded)
	replaced := map[string]string{}
	for _, prev := range []string{"", "null", `["a"]`, `"text"`, "12", "{not json"} {
		replaced[createRunWithResult(t, s, store.MaintenanceStatusRunning, prev)] = prev
	}
	const doneResult = `{"commit":"abc"}`
	completedRun := createRunWithResult(t, s, store.MaintenanceStatusCompleted, doneResult)
	completedBefore, err := s.GetMaintenanceRun(ctx, completedRun)
	require.NoError(t, err)

	runs, _, err := s.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1+len(replaced)), runs, "the count is the number of running runs updated")

	got, err := s.GetMaintenanceRun(ctx, objectRun)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusFailed, got.Status)
	require.NotNil(t, got.CompletedAt)
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(got.Result), &obj), got.Result)
	var want map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(recorded), &want))
	for k, v := range want {
		if k == "error" {
			continue
		}
		assert.JSONEq(t, string(v), string(obj[k]), "key %q must be kept", k)
	}
	assert.Equal(t, "12345678901234567890", string(obj["big"]), "values are kept byte for byte")
	assert.Equal(t, `"`+abortText+`"`, string(obj["error"]))
	assert.Equal(t, "true", string(obj["aborted"]))
	assert.Len(t, obj, len(want)+1, "only error and aborted are added")

	for id, prev := range replaced {
		got, err := s.GetMaintenanceRun(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, store.MaintenanceStatusFailed, got.Status, "Result %q", prev)
		assert.Equal(t, abortResult, got.Result, "a Result %q that is not a JSON object is replaced", prev)
	}

	completedAfter, err := s.GetMaintenanceRun(ctx, completedRun)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusCompleted, completedAfter.Status)
	assert.Equal(t, doneResult, completedAfter.Result)
	require.NotNil(t, completedAfter.CompletedAt)
	assert.True(t, completedBefore.CompletedAt.Equal(*completedAfter.CompletedAt))
}

// A run that finishes between the abort's read and its update is left as it
// finished: each update matches only a run that is still running, and the
// count includes only the updates that matched.
func TestAbortRunningMaintenanceOps_SkipsRunFinishedMeanwhile(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))
	racing := createRunWithResult(t, s, store.MaintenanceStatusRunning, `{"commit":"abc"}`)

	// Before the abort's first update runs, the run completes (as if its
	// executor wrote its final result at that moment).
	const finalResult = `{"commit":"abc","installed":true}`
	var once sync.Once
	s.client.MaintenanceOperationRun.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if m.Op().Is(ent.OpUpdate) {
				var finishErr error
				once.Do(func() {
					done := time.Now()
					finishErr = s.client.MaintenanceOperationRun.UpdateOneID(uuid.MustParse(racing)).
						SetStatus(store.MaintenanceStatusCompleted).
						SetCompletedAt(done).
						SetResult(finalResult).
						Exec(ctx)
				})
				if finishErr != nil {
					return nil, finishErr
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	runs, _, err := s.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), runs)

	got, err := s.GetMaintenanceRun(ctx, racing)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusCompleted, got.Status)
	assert.Equal(t, finalResult, got.Result)
}

// A Result written to a run that is still running, between the abort's read
// and its update, is not overwritten: the update matches only the Result it
// read, so it misses, and the run is read again and aborted with the new
// Result. Runs on SQLite, and on Postgres under -tags integration.
func TestAbortRunningMaintenanceOps_KeepsResultWrittenMeanwhile(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))
	racing := createRunWithResult(t, s, store.MaintenanceStatusRunning, `{"commit":"abc"}`)

	// Before the abort's first update runs, the still-running run records a
	// new Result (as a checkpoint would); only the Result changes.
	const written = `{"commit":"abc","step":"2"}`
	var once sync.Once
	s.client.MaintenanceOperationRun.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if m.Op().Is(ent.OpUpdate) {
				var writeErr error
				once.Do(func() {
					writeErr = s.client.MaintenanceOperationRun.UpdateOneID(uuid.MustParse(racing)).
						SetResult(written).
						Exec(ctx)
				})
				if writeErr != nil {
					return nil, writeErr
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	runs, _, err := s.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), runs)

	got, err := s.GetMaintenanceRun(ctx, racing)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusFailed, got.Status)
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(got.Result), &obj), got.Result)
	assert.Equal(t, `"abc"`, string(obj["commit"]))
	assert.Equal(t, `"2"`, string(obj["step"]), "the Result written meanwhile is kept")
	assert.Equal(t, `"`+abortText+`"`, string(obj["error"]))
	assert.Equal(t, "true", string(obj["aborted"]))
	assert.Len(t, obj, 4)
}

// A running run with no Result (NULL in the column) and one with an empty
// Result are both aborted: the update's Result check matches either. Runs
// on SQLite, and on Postgres under -tags integration.
func TestAbortRunningMaintenanceOps_AbortsNullAndEmptyResult(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))
	nullRun := createRunWithResult(t, s, store.MaintenanceStatusRunning, "")
	emptyID := uuid.New()
	require.NoError(t, s.client.MaintenanceOperationRun.Create().
		SetID(emptyID).
		SetOperationKey("pull-images").
		SetStatus(store.MaintenanceStatusRunning).
		SetResult("").
		Exec(ctx))
	nulls, err := s.client.MaintenanceOperationRun.Query().
		Where(maintenanceoperationrun.IDEQ(uuid.MustParse(nullRun)), maintenanceoperationrun.ResultIsNil()).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, nulls, "the first run's Result column is NULL")
	empties, err := s.client.MaintenanceOperationRun.Query().
		Where(maintenanceoperationrun.IDEQ(emptyID), maintenanceoperationrun.ResultEQ("")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, empties, "the second run's Result is the empty string")

	runs, _, err := s.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), runs)
	for _, id := range []string{nullRun, emptyID.String()} {
		got, err := s.GetMaintenanceRun(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, store.MaintenanceStatusFailed, got.Status)
		assert.Equal(t, abortResult, got.Result)
	}
}
