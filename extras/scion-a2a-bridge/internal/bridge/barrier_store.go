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
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// BarrierTaskStore wraps a PostgresTaskStore and provides a deterministic
// signaling channel between the consumer goroutine (which calls Create) and
// the producer goroutine (which needs to know when Create has committed).
//
// This replaces the timed retry loop (50×20ms) with a channel-based barrier
// that has no timing assumptions (Constraint 2).
type BarrierTaskStore struct {
	inner    *PostgresTaskStore
	barriers sync.Map // taskID → *CreateBarrier
}

// Compile-time check.
var _ taskstore.Store = (*BarrierTaskStore)(nil)

// NewBarrierTaskStore creates a BarrierTaskStore wrapping the given
// PostgresTaskStore.
func NewBarrierTaskStore(inner *PostgresTaskStore) *BarrierTaskStore {
	return &BarrierTaskStore{inner: inner}
}

// CreateBarrier is a handle returned by PrepareBarrier. The executor holds
// this handle directly — no map lookup needed in Await. Uses sync.Once to
// ensure the channel is closed exactly once, even under duplicate Create
// attempts (e.g., SDK retry returning ErrTaskAlreadyExists).
type CreateBarrier struct {
	taskID string
	store  *BarrierTaskStore
	done   chan struct{}
	once   sync.Once
	err    error
}

// PrepareBarrier registers a barrier and returns a handle. Called by the
// executor (producer goroutine) BEFORE yielding the initial *a2a.Task event.
// The returned handle is used for Await and Cancel — the executor never
// looks up the barrier by taskID after this call.
func (b *BarrierTaskStore) PrepareBarrier(taskID string) *CreateBarrier {
	cb := &CreateBarrier{
		taskID: taskID,
		store:  b,
		done:   make(chan struct{}),
	}
	b.barriers.Store(taskID, cb)
	return cb
}

// Await blocks until Create signals completion, or ctx is cancelled.
// Returns the Create error (nil on success). Safe to call even if Create
// has already completed — the closed channel returns immediately.
func (cb *CreateBarrier) Await(ctx context.Context) error {
	select {
	case <-cb.done:
		return cb.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel removes the barrier from the store's map without signaling.
// Called on every exit path that skips Await (yield returns false, early
// return). Idempotent — safe to call after Create has already signaled.
func (cb *CreateBarrier) Cancel() {
	cb.store.barriers.Delete(cb.taskID)
}

// Create delegates to the inner store, then signals the barrier via sync.Once.
// Uses Load (not LoadAndDelete) so the map entry remains available for
// Cancel cleanup. The signal is protected by sync.Once: duplicate Create
// attempts (e.g., SDK retry after ErrTaskAlreadyExists) never double-close.
func (b *BarrierTaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	version, err := b.inner.Create(ctx, task)
	if task == nil {
		return version, err
	}
	if v, ok := b.barriers.Load(string(task.ID)); ok {
		cb := v.(*CreateBarrier)
		cb.once.Do(func() {
			cb.err = err
			close(cb.done)
		})
	}
	return version, err
}

// Update delegates to the inner store.
func (b *BarrierTaskStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	return b.inner.Update(ctx, req)
}

// Get delegates to the inner store.
func (b *BarrierTaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	return b.inner.Get(ctx, taskID)
}

// List delegates to the inner store.
func (b *BarrierTaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return b.inner.List(ctx, req)
}
