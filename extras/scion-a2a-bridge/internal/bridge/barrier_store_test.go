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

package bridge

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupBarrierTest creates a real PostgresTaskStore and BarrierTaskStore backed
// by the test database. Returns a cleanup function.
func setupBarrierTest(t *testing.T) (*BarrierTaskStore, *PostgresTaskStore, context.Context) {
	t.Helper()
	dbURL := barrierTestDatabaseURL(t)
	pgStore := mustOpenPgStore(t, dbURL)
	barrierStore := NewBarrierTaskStore(pgStore)
	ctx := withTestRouteAndCaller(context.Background(), "proj1", "agent1", "user1")
	return barrierStore, pgStore, ctx
}

func mustOpenPgStore(t *testing.T, dbURL string) *PostgresTaskStore {
	t.Helper()
	db, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	pgStore := &PostgresTaskStore{db: db, ownsPool: false}
	err = pgStore.migrate()
	require.NoError(t, err)
	return pgStore
}

func withTestRouteAndCaller(ctx context.Context, proj, agent, userID string) context.Context {
	ctx = WithRouteInfo(ctx, RouteInfo{ProjectSlug: proj, AgentSlug: agent})
	if userID != "" {
		ctx = WithCallerIdentity(ctx, &CallerIdentity{UserID: userID})
	}
	return ctx
}

func makeTask(id string) *a2a.Task {
	return &a2a.Task{
		ID: a2a.TaskID(id),
		Status: a2a.TaskStatus{
			State: a2a.TaskStateSubmitted,
		},
	}
}

// barrierTestDatabaseURL returns the test database URL or skips the test.
func barrierTestDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return url
}

// TestBarrier_CreateBeforeAwait verifies that Await returns immediately when
// Create has already signaled.
func TestBarrier_CreateBeforeAwait(t *testing.T) {
	barrierStore, _, ctx := setupBarrierTest(t)
	taskID := "barrier-create-before-" + randomSuffix()

	barrier := barrierStore.PrepareBarrier(taskID)
	defer barrier.Cancel()

	// Create signals before Await — channel already closed.
	_, err := barrierStore.Create(ctx, makeTask(taskID))
	require.NoError(t, err)

	// Await returns immediately.
	err = barrier.Await(ctx)
	assert.NoError(t, err)
}

// TestBarrier_AwaitBeforeCreate verifies that Await blocks until Create signals.
func TestBarrier_AwaitBeforeCreate(t *testing.T) {
	barrierStore, _, ctx := setupBarrierTest(t)
	taskID := "barrier-await-before-" + randomSuffix()

	barrier := barrierStore.PrepareBarrier(taskID)
	defer barrier.Cancel()

	awaitDone := make(chan error, 1)
	go func() { awaitDone <- barrier.Await(ctx) }()

	// Create in separate goroutine.
	createDone := make(chan struct{})
	go func() {
		_, _ = barrierStore.Create(ctx, makeTask(taskID))
		close(createDone)
	}()
	<-createDone

	assert.NoError(t, <-awaitDone)
}

// TestBarrier_ContextCancel verifies that Await returns ctx.Err() when
// cancelled before Create.
func TestBarrier_ContextCancel(t *testing.T) {
	barrierStore, _, ctx := setupBarrierTest(t)
	taskID := "barrier-ctx-cancel-" + randomSuffix()

	barrier := barrierStore.PrepareBarrier(taskID)
	defer barrier.Cancel()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	err := barrier.Await(cancelCtx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestBarrier_CancelCleanup verifies that Cancel removes the map entry.
func TestBarrier_CancelCleanup(t *testing.T) {
	barrierStore, _, _ := setupBarrierTest(t)
	taskID := "barrier-cancel-" + randomSuffix()

	barrier := barrierStore.PrepareBarrier(taskID)
	barrier.Cancel()

	_, loaded := barrierStore.barriers.Load(taskID)
	assert.False(t, loaded, "barrier map entry should be deleted after Cancel")
}

// TestBarrier_CreateError verifies that Create errors propagate through Await.
func TestBarrier_CreateError(t *testing.T) {
	barrierStore, _, ctx := setupBarrierTest(t)
	taskID := "barrier-create-error-" + randomSuffix()

	// First create the task so second Create returns ErrTaskAlreadyExists.
	_, err := barrierStore.Create(ctx, makeTask(taskID))
	require.NoError(t, err)

	// Prepare barrier for duplicate create.
	barrier := barrierStore.PrepareBarrier(taskID)
	defer barrier.Cancel()

	// Second Create returns ErrTaskAlreadyExists — error propagates.
	_, err = barrierStore.Create(ctx, makeTask(taskID))
	assert.ErrorIs(t, err, taskstore.ErrTaskAlreadyExists)

	// Await returns the error from Create.
	awaitErr := barrier.Await(ctx)
	assert.ErrorIs(t, awaitErr, taskstore.ErrTaskAlreadyExists)
}

// TestBarrier_YieldFalsePath verifies that defer Cancel without Await works
// without leak or panic.
func TestBarrier_YieldFalsePath(t *testing.T) {
	barrierStore, _, _ := setupBarrierTest(t)
	taskID := "barrier-yield-false-" + randomSuffix()

	barrier := barrierStore.PrepareBarrier(taskID)

	// Simulate yield returning false — executor exits without Await.
	barrier.Cancel()
	_, loaded := barrierStore.barriers.Load(taskID)
	assert.False(t, loaded)

	// Double cancel is idempotent.
	assert.NotPanics(t, func() { barrier.Cancel() })
}

// TestBarrier_DuplicateCreate verifies that sync.Once prevents double-close
// panics under concurrent Create calls.
func TestBarrier_DuplicateCreate(t *testing.T) {
	barrierStore, _, ctx := setupBarrierTest(t)
	taskID := "barrier-dup-create-" + randomSuffix()

	barrier := barrierStore.PrepareBarrier(taskID)
	defer barrier.Cancel()

	// Concurrent Create calls — some succeed, some return ErrTaskAlreadyExists.
	// sync.Once ensures no panic from double-close.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			barrierStore.Create(ctx, makeTask(taskID))
		}()
	}
	wg.Wait()

	// Await returns without panic — the first Create's result is captured.
	assert.NotPanics(t, func() {
		_ = barrier.Await(ctx)
	})
	assert.NotPanics(t, func() { barrier.Cancel() })
}

// TestBarrier_DuplicateCreateSameOwnerVerification verifies that on
// ErrTaskAlreadyExists, the durable row exists for the same owner
// (EM binding constraint 2).
func TestBarrier_DuplicateCreateSameOwnerVerification(t *testing.T) {
	barrierStore, pgStore, ctx := setupBarrierTest(t)
	taskID := "barrier-dup-verify-" + randomSuffix()

	// First create succeeds.
	_, err := barrierStore.Create(ctx, makeTask(taskID))
	require.NoError(t, err)

	// Prepare barrier and attempt duplicate create.
	barrier := barrierStore.PrepareBarrier(taskID)
	defer barrier.Cancel()

	_, err = barrierStore.Create(ctx, makeTask(taskID))
	assert.ErrorIs(t, err, taskstore.ErrTaskAlreadyExists)

	// Verify the durable row exists for the same owner.
	stored, getErr := pgStore.Get(ctx, a2a.TaskID(taskID))
	require.NoError(t, getErr)
	assert.Equal(t, a2a.TaskID(taskID), stored.Task.ID)
}

func randomSuffix() string {
	return string(a2a.NewTaskID())[:8]
}
