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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
