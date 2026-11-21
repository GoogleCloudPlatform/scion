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
	"encoding/json"
	"errors"
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

// lastSeenRecorder is a minimal store stub that records UpdateUserLastSeen calls.
type lastSeenRecorder struct {
	store.Store
	mu    sync.Mutex
	calls []lastSeenCall
	count atomic.Int64
}

type lastSeenCall struct {
	id string
	t  time.Time
}

func (r *lastSeenRecorder) UpdateUserLastSeen(_ context.Context, id string, t time.Time) error {
	r.mu.Lock()
	r.calls = append(r.calls, lastSeenCall{id: id, t: t})
	r.mu.Unlock()
	r.count.Add(1)
	return nil
}

func (r *lastSeenRecorder) getCalls() []lastSeenCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]lastSeenCall, len(r.calls))
	copy(cp, r.calls)
	return cp
}

func TestUserActivityTracker_ThrottlesWrites(t *testing.T) {
	rec := &lastSeenRecorder{}
	tracker := NewUserActivityTracker(rec, time.Hour)

	// First touch should write
	tracker.Touch(tid("user-1"))

	// Wait for async goroutine
	time.Sleep(50 * time.Millisecond)

	calls := rec.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call after first touch, got %d", len(calls))
	}
	if calls[0].id != tid("user-1") {
		t.Errorf("expected user-1, got %s", calls[0].id)
	}

	// Second touch within the interval should be throttled
	tracker.Touch(tid("user-1"))
	time.Sleep(50 * time.Millisecond)

	calls = rec.getCalls()
	if len(calls) != 1 {
		t.Errorf("expected 1 call (throttled), got %d", len(calls))
	}
}

func TestUserActivityTracker_DifferentUsers(t *testing.T) {
	rec := &lastSeenRecorder{}
	tracker := NewUserActivityTracker(rec, time.Hour)

	tracker.Touch(tid("user-1"))
	tracker.Touch("user-2")

	time.Sleep(50 * time.Millisecond)

	calls := rec.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls for different users, got %d", len(calls))
	}
}

func TestUserActivityTracker_WritesAfterInterval(t *testing.T) {
	rec := &lastSeenRecorder{}
	// Use a very short interval for testing
	tracker := NewUserActivityTracker(rec, 10*time.Millisecond)

	tracker.Touch(tid("user-1"))
	time.Sleep(50 * time.Millisecond)

	// After interval has passed, a second touch should write again
	tracker.Touch(tid("user-1"))
	time.Sleep(50 * time.Millisecond)

	calls := rec.getCalls()
	if len(calls) != 2 {
		t.Errorf("expected 2 calls after interval expired, got %d", len(calls))
	}
}

type blockingLastSeenStore struct {
	store.Store
	enteredOnce  sync.Once
	entered      chan struct{}
	release      chan struct{}
	cancelled    chan struct{}
	ignoreCancel bool
	cancelDelay  time.Duration
	exited       atomic.Bool
	err          error
	calls        atomic.Int32
}

func newBlockingLastSeenStore(inner store.Store) *blockingLastSeenStore {
	return &blockingLastSeenStore{
		Store:     inner,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
}

func (b *blockingLastSeenStore) UpdateUserLastSeen(ctx context.Context, id string, lastSeen time.Time) error {
	b.calls.Add(1)
	b.enteredOnce.Do(func() { close(b.entered) })
	defer b.exited.Store(true)

	if b.ignoreCancel {
		<-b.release
		if b.err != nil {
			return b.err
		}
		if b.Store != nil {
			return b.Store.UpdateUserLastSeen(ctx, id, lastSeen)
		}
		return nil
	}

	select {
	case <-b.release:
		if b.err != nil {
			return b.err
		}
		if b.Store != nil {
			return b.Store.UpdateUserLastSeen(ctx, id, lastSeen)
		}
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

func TestUserActivityTracker_CloseDrainsAndRejectsPostCloseWrites(t *testing.T) {
	bs := newBlockingLastSeenStore(nil)
	tracker := NewUserActivityTracker(bs, time.Hour)

	tracker.Touch(tid("user-drain"))
	select {
	case <-bs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Touch goroutine did not enter UpdateUserLastSeen")
	}

	done := make(chan struct{})
	go func() {
		tracker.Close(context.Background())
		close(done)
	}()

	select {
	case <-done:
		close(bs.release)
		t.Fatal("Close returned before in-flight UpdateUserLastSeen finished")
	case <-time.After(30 * time.Millisecond):
	}

	close(bs.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after in-flight UpdateUserLastSeen was released")
	}

	assert.Equal(t, int32(1), bs.calls.Load())

	// Subsequent Touch calls after Close must not start any store write.
	tracker.Touch(tid("user-after-close"))
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(1), bs.calls.Load(), "post-close Touch must not call UpdateUserLastSeen")

	// Close is idempotent and nil-safe.
	tracker.Close(context.Background())
	var nilTracker *UserActivityTracker
	nilTracker.Close(context.Background())
}

func TestUserActivityTracker_CloseCancelsStuckWriteAtDeadline(t *testing.T) {
	bs := newBlockingLastSeenStore(nil)
	bs.cancelDelay = 25 * time.Millisecond
	t.Cleanup(func() {
		select {
		case <-bs.release:
		default:
			close(bs.release)
		}
	})

	tracker := NewUserActivityTracker(bs, time.Hour)
	tracker.Touch(tid("user-stuck"))
	select {
	case <-bs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Touch goroutine did not enter UpdateUserLastSeen")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	start := time.Now()
	tracker.Close(closeCtx)
	elapsed := time.Since(start)

	assert.True(t, bs.exited.Load(), "Close must wait for cancelled cooperative write to exit")
	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
	assert.Less(t, elapsed, 500*time.Millisecond)
	select {
	case <-bs.cancelled:
	default:
		t.Fatal("stuck UpdateUserLastSeen context was not cancelled at shutdown deadline")
	}
}

func TestUserActivityTracker_TouchCapturesCallTimeLogger(t *testing.T) {
	callLogs := &syncBuffer{}
	laterLogs := &syncBuffer{}
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	bs := newBlockingLastSeenStore(nil)
	bs.err = errors.New("injected last-seen write failure")
	tracker := NewUserActivityTracker(bs, time.Hour)

	slog.SetDefault(slog.New(slog.NewJSONHandler(callLogs, nil)))
	userID := tid("user-logger")
	tracker.Touch(userID)

	select {
	case <-bs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Touch goroutine did not enter UpdateUserLastSeen")
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(laterLogs, nil)))
	close(bs.release)
	tracker.Close(context.Background())

	assert.Empty(t, strings.TrimSpace(laterLogs.String()),
		"async last-seen warning must not bleed into a later slog.SetDefault logger")

	var matched bool
	for _, line := range strings.Split(callLogs.String(), "\n") {
		var rec struct {
			Msg    string `json:"msg"`
			UserID string `json:"userId"`
			Error  string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "Failed to update user last-seen" {
			matched = true
			assert.Equal(t, userID, rec.UserID)
			assert.Contains(t, rec.Error, "injected last-seen write failure")
		}
	}
	require.True(t, matched, "expected warning in call-time logger; got:\n%s", callLogs.String())
}
