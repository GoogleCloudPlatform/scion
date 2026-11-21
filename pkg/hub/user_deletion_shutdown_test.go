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

package hub

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type blockingUserDeletionStore struct {
	store.Store
	fault        *storeFaultSwitch
	enteredOnce  sync.Once
	entered      chan struct{}
	release      chan struct{}
	cancelled    chan struct{}
	ignoreCancel bool
	cancelDelay  time.Duration
	exited       atomic.Bool
	listCalls    atomic.Int32
	getCalls     atomic.Int32
}

func newBlockingUserDeletionStore(inner store.Store, fault *storeFaultSwitch) *blockingUserDeletionStore {
	return &blockingUserDeletionStore{
		Store:     inner,
		fault:     fault,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
}

func (b *blockingUserDeletionStore) wait(ctx context.Context) error {
	b.enteredOnce.Do(func() { close(b.entered) })
	defer b.exited.Store(true)

	if b.ignoreCancel {
		<-b.release
		return nil
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		select {
		case <-b.cancelled:
		default:
			close(b.cancelled)
		}
		if b.cancelDelay > 0 {
			select {
			case <-b.release:
			case <-time.After(b.cancelDelay):
			}
		}
		return ctx.Err()
	}
}

func (b *blockingUserDeletionStore) ListUserDeletionAgentsForAgent(ctx context.Context, agentID string) ([]store.UserDeletionAgent, error) {
	if !b.fault.Active() {
		return b.Store.ListUserDeletionAgentsForAgent(ctx, agentID)
	}
	b.listCalls.Add(1)
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	return b.Store.ListUserDeletionAgentsForAgent(ctx, agentID)
}

func (b *blockingUserDeletionStore) GetUserDeletion(ctx context.Context, id string) (*store.UserDeletion, error) {
	if !b.fault.Active() {
		return b.Store.GetUserDeletion(ctx, id)
	}
	b.getCalls.Add(1)
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	return b.Store.GetUserDeletion(ctx, id)
}

type userDeletionLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *userDeletionLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *userDeletionLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestUserDeletionShutdown_DrainsEngineFinishBeforeStoreClose verifies that
// every hub shutdown path (Shutdown, CleanupResources, and CloseAuditWriter)
// waits for in-flight noteUserDeletionAgentFinished lookups to complete before
// returning, refuses post-close notifications, and emits no "sql: database is
// closed" warnings (ptone/scion#8679).
func TestUserDeletionShutdown_DrainsEngineFinishBeforeStoreClose(t *testing.T) {
	paths := []struct {
		name  string
		close func(*Server, context.Context) error
	}{
		{"Shutdown", (*Server).Shutdown},
		{"CleanupResources", (*Server).CleanupResources},
		{"CloseAuditWriter", (*Server).CloseAuditWriter},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			var logs userDeletionLogBuffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			srv, s, wrapped, fault := testServerWithStoreFault(t, newBlockingUserDeletionStore)
			fault.Arm()

			plan := &agentDeletionPlan{snapshot: &store.Agent{ID: tid("agent-ud-shutdown-" + p.name)}}
			srv.noteUserDeletionAgentFinished(plan, deletionOutcome{kind: deletionOutcomeDeleted})

			select {
			case <-wrapped.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("noteUserDeletionAgentFinished goroutine did not enter ListUserDeletionAgentsForAgent")
			}

			done := make(chan error, 1)
			go func() {
				done <- p.close(srv, context.Background())
			}()

			select {
			case err := <-done:
				close(wrapped.release)
				t.Fatalf("%s returned before in-flight user deletion finish lookup completed (err=%v)", p.name, err)
			case <-time.After(30 * time.Millisecond):
			}

			close(wrapped.release)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatalf("%s did not return after user deletion finish lookup was released", p.name)
			}

			assert.Equal(t, int32(1), wrapped.listCalls.Load())
			assert.True(t, wrapped.exited.Load())

			// Close the store and verify post-shutdown calls do not start
			// goroutines or log "database is closed" warnings.
			require.NoError(t, s.Close())
			srv.noteUserDeletionAgentFinished(plan, deletionOutcome{kind: deletionOutcomeDeleted})
			srv.kickUserDeletion(tid("del-post-close"), false)

			assert.Equal(t, int32(1), wrapped.listCalls.Load(), "post-shutdown noteUserDeletionAgentFinished must not hit store")
			assert.Equal(t, int32(0), wrapped.getCalls.Load(), "post-shutdown kickUserDeletion must not hit store")
			assert.False(t, strings.Contains(logs.String(), "user deletion: engine finish lookup failed"), "unexpected warning: %s", logs.String())
			assert.False(t, strings.Contains(logs.String(), "database is closed"), "unexpected database-closed warning: %s", logs.String())
		})
	}
}

// TestUserDeletionShutdown_DrainsKickBeforeStoreClose verifies that Shutdown
// waits for an in-flight kickUserDeletion reconcile to finish before returning.
func TestUserDeletionShutdown_DrainsKickBeforeStoreClose(t *testing.T) {
	srv, _, wrapped, fault := testServerWithStoreFault(t, newBlockingUserDeletionStore)
	fault.Arm()

	srv.kickUserDeletion(tid("del-kick-drain"), false)

	select {
	case <-wrapped.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("kickUserDeletion goroutine did not enter GetUserDeletion")
	}

	done := make(chan error, 1)
	go func() {
		done <- srv.Shutdown(context.Background())
	}()

	select {
	case err := <-done:
		close(wrapped.release)
		t.Fatalf("Shutdown returned before in-flight kickUserDeletion finished (err=%v)", err)
	case <-time.After(30 * time.Millisecond):
	}

	close(wrapped.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after kickUserDeletion was released")
	}

	assert.Equal(t, int32(1), wrapped.getCalls.Load())
	assert.True(t, wrapped.exited.Load())
}

// TestUserDeletionShutdown_CancelsStuckLookupAtDeadline verifies that when a
// background user-deletion lookup is stuck past the shutdown context deadline,
// CloseAuditWriter cancels its context, waits a bounded grace for cooperative
// unwind, and never hangs on a non-cooperative store.
func TestUserDeletionShutdown_CancelsStuckLookupAtDeadline(t *testing.T) {
	t.Run("cooperative lookup unwinds before return", func(t *testing.T) {
		srv, _, wrapped, fault := testServerWithStoreFault(t, newBlockingUserDeletionStore)
		wrapped.cancelDelay = 30 * time.Millisecond
		fault.Arm()

		plan := &agentDeletionPlan{snapshot: &store.Agent{ID: tid("agent-ud-cancel-coop")}}
		srv.noteUserDeletionAgentFinished(plan, deletionOutcome{kind: deletionOutcomeDeleted})

		select {
		case <-wrapped.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("noteUserDeletionAgentFinished did not enter ListUserDeletionAgentsForAgent")
		}

		closeCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		start := time.Now()
		require.NoError(t, srv.CloseAuditWriter(closeCtx))
		elapsed := time.Since(start)

		assert.True(t, wrapped.exited.Load(), "CloseAuditWriter must wait for cancelled cooperative lookup to exit")
		assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
		assert.Less(t, elapsed, 500*time.Millisecond)
	})

	t.Run("non-cooperative lookup does not hang shutdown", func(t *testing.T) {
		srv, _, wrapped, fault := testServerWithStoreFault(t, newBlockingUserDeletionStore)
		wrapped.ignoreCancel = true
		fault.Arm()

		released := false
		t.Cleanup(func() {
			if !released {
				close(wrapped.release)
			}
		})

		plan := &agentDeletionPlan{snapshot: &store.Agent{ID: tid("agent-ud-cancel-stuck")}}
		srv.noteUserDeletionAgentFinished(plan, deletionOutcome{kind: deletionOutcomeDeleted})

		select {
		case <-wrapped.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("noteUserDeletionAgentFinished did not enter ListUserDeletionAgentsForAgent")
		}

		closeCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		start := time.Now()
		require.NoError(t, srv.CloseAuditWriter(closeCtx))
		elapsed := time.Since(start)

		released = true
		close(wrapped.release)

		assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond)
		assert.Less(t, elapsed, time.Second)
	})
}
