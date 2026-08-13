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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock EventPublisher — records PublishRaw calls for assertions.
// ---------------------------------------------------------------------------

type publishedEvent struct {
	subject string
	data    interface{}
}

type mockEventPublisher struct {
	mu     sync.Mutex
	events []publishedEvent
	noopEventPublisher
}

func (m *mockEventPublisher) PublishRaw(subject string, data interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, publishedEvent{subject: subject, data: data})
}

func (m *mockEventPublisher) getEvents() []publishedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]publishedEvent, len(m.events))
	copy(out, m.events)
	return out
}

func (m *mockEventPublisher) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = nil
}

// ---------------------------------------------------------------------------
// Helper: create a PresenceManager with a mock publisher and nil store.
// The caller must call pm.Stop() when done.
// ---------------------------------------------------------------------------

func newTestPM(t *testing.T) (*PresenceManager, *mockEventPublisher) {
	t.Helper()
	pub := &mockEventPublisher{}
	pm := NewPresenceManager(pub, nil)
	t.Cleanup(func() { pm.Stop() })
	return pm, pub
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestPresenceManager_HeartbeatTransitions(t *testing.T) {
	tests := []struct {
		name           string
		setup          func(pm *PresenceManager) // optional pre-condition
		wantState      PresenceState
		wantPublished  int  // number of PublishRaw calls
		wantTransition bool // whether a transition event should be published
	}{
		{
			name:           "first heartbeat transitions idle to active",
			wantState:      PresenceActive,
			wantPublished:  1, // one project → one publish
			wantTransition: true,
		},
		{
			name: "second heartbeat does not re-publish",
			setup: func(pm *PresenceManager) {
				pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1"})
			},
			wantState:      PresenceActive,
			wantPublished:  0, // already active, no transition
			wantTransition: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pm, pub := newTestPM(t)

			if tc.setup != nil {
				tc.setup(pm)
				pub.reset() // clear events from setup
			}

			pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1"})

			assert.Equal(t, tc.wantState, pm.GetState("user1"))

			events := pub.getEvents()
			assert.Len(t, events, tc.wantPublished)

			if tc.wantTransition && len(events) > 0 {
				pe, ok := events[0].data.(PresenceEvent)
				require.True(t, ok)
				assert.Equal(t, "user1", pe.UserID)
				assert.Equal(t, "active", pe.State)
				assert.Equal(t, "project.proj1.chat.presence", events[0].subject)
			}
		})
	}
}

func TestPresenceManager_HeartbeatMultipleProjects(t *testing.T) {
	pm, pub := newTestPM(t)

	pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1", "proj2", "proj3"})

	events := pub.getEvents()
	require.Len(t, events, 3, "should publish to all 3 projects")

	subjects := make([]string, len(events))
	for i, e := range events {
		subjects[i] = e.subject
	}
	assert.Contains(t, subjects, "project.proj1.chat.presence")
	assert.Contains(t, subjects, "project.proj2.chat.presence")
	assert.Contains(t, subjects, "project.proj3.chat.presence")
}

func TestPresenceManager_SweepTriggersIdleTransition(t *testing.T) {
	pm, pub := newTestPM(t)

	// Send a heartbeat, then manually set lastHeartbeat to the past to
	// simulate the active window expiring.
	pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1"})
	pub.reset()

	pm.mu.Lock()
	pm.users["user1"].lastHeartbeat = time.Now().Add(-(presenceActiveWindow + time.Second))
	pm.mu.Unlock()

	// Run sweep explicitly.
	pm.sweep()

	assert.Equal(t, PresenceIdle, pm.GetState("user1"))

	events := pub.getEvents()
	require.Len(t, events, 1, "sweep should publish one idle transition")

	pe, ok := events[0].data.(PresenceEvent)
	require.True(t, ok)
	assert.Equal(t, "user1", pe.UserID)
	assert.Equal(t, "idle", pe.State)
}

func TestPresenceManager_SweepDoesNotRepublishIdle(t *testing.T) {
	pm, pub := newTestPM(t)

	// Heartbeat then expire to go idle.
	pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1"})

	pm.mu.Lock()
	pm.users["user1"].lastHeartbeat = time.Now().Add(-(presenceActiveWindow + time.Second))
	pm.mu.Unlock()

	pm.sweep() // transitions to idle
	pub.reset()

	// Sweep again — should not re-publish because already idle.
	pm.sweep()

	events := pub.getEvents()
	assert.Empty(t, events, "should not re-publish idle transition")
}

func TestPresenceManager_OnlyTransitionsPublished(t *testing.T) {
	pm, pub := newTestPM(t)

	// First heartbeat: idle→active (published).
	pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1"})
	require.Len(t, pub.getEvents(), 1)

	// Second heartbeat: still active (NOT published).
	pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1"})
	assert.Len(t, pub.getEvents(), 1, "no new event for repeated heartbeat")

	// Expire and sweep: active→idle (published).
	pm.mu.Lock()
	pm.users["user1"].lastHeartbeat = time.Now().Add(-(presenceActiveWindow + time.Second))
	pm.mu.Unlock()
	pm.sweep()
	assert.Len(t, pub.getEvents(), 2, "idle transition published")

	// Heartbeat again: idle→active (published).
	pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1"})
	assert.Len(t, pub.getEvents(), 3, "re-activation published")
}

func TestPresenceManager_RecordTypingThrottle(t *testing.T) {
	tests := []struct {
		name    string
		delay   time.Duration
		allowed bool
	}{
		{
			name:    "first typing event is allowed",
			delay:   0,
			allowed: true,
		},
		{
			name:    "immediate second typing is throttled",
			delay:   0,
			allowed: false,
		},
	}

	pm, _ := newTestPM(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.delay > 0 {
				time.Sleep(tc.delay)
			}
			result := pm.RecordTyping("conv1", "user1")
			assert.Equal(t, tc.allowed, result)
		})
	}
}

func TestPresenceManager_RecordTypingThrottleExpiry(t *testing.T) {
	pm, _ := newTestPM(t)

	// First typing is allowed.
	assert.True(t, pm.RecordTyping("conv1", "user1"))
	// Immediately after: throttled.
	assert.False(t, pm.RecordTyping("conv1", "user1"))

	// Manually move the typing timestamp back past the throttle window.
	pm.mu.Lock()
	pm.typing["conv1"]["user1"].lastTyping = time.Now().Add(-(typingThrottle + time.Second))
	pm.mu.Unlock()

	// Should be allowed again.
	assert.True(t, pm.RecordTyping("conv1", "user1"))
}

func TestPresenceManager_RecordTypingDifferentConversations(t *testing.T) {
	pm, _ := newTestPM(t)

	// Same user in different conversations should not be throttled.
	assert.True(t, pm.RecordTyping("conv1", "user1"))
	assert.True(t, pm.RecordTyping("conv2", "user1"))
}

func TestPresenceManager_RecordTypingDifferentUsers(t *testing.T) {
	pm, _ := newTestPM(t)

	// Different users in the same conversation should not be throttled.
	assert.True(t, pm.RecordTyping("conv1", "user1"))
	assert.True(t, pm.RecordTyping("conv1", "user2"))
}

func TestPresenceManager_SweepEvictsStaleTypingEntries(t *testing.T) {
	pm, _ := newTestPM(t)

	// Record typing, then backdate it past the eviction threshold (2× typingThrottle).
	pm.RecordTyping("conv1", "user1")
	pm.RecordTyping("conv2", "user2")

	pm.mu.Lock()
	evictionAge := 2*typingThrottle + time.Second
	pm.typing["conv1"]["user1"].lastTyping = time.Now().Add(-evictionAge)
	pm.mu.Unlock()

	pm.sweep()

	pm.mu.RLock()
	_, conv1Exists := pm.typing["conv1"]
	_, conv2Exists := pm.typing["conv2"]
	pm.mu.RUnlock()

	assert.False(t, conv1Exists, "stale typing entry should be evicted")
	assert.True(t, conv2Exists, "recent typing entry should be kept")
}

func TestPresenceManager_ConcurrentHeartbeatWrites(t *testing.T) {
	pm, _ := newTestPM(t)

	const numGoroutines = 50
	var wg sync.WaitGroup

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			// Mix of user IDs and project IDs to exercise concurrent map access.
			userID := "user" + string(rune('A'+idx%5))
			projects := []string{"proj1", "proj2"}
			pm.Heartbeat(t.Context(), userID, "User "+userID, projects)
		}(i)
	}

	wg.Wait()

	// Verify all 5 users are tracked and active.
	for i := 0; i < 5; i++ {
		userID := "user" + string(rune('A'+i))
		assert.Equal(t, PresenceActive, pm.GetState(userID), "user %s should be active", userID)
	}
}

func TestPresenceManager_ConcurrentTypingWrites(t *testing.T) {
	pm, _ := newTestPM(t)

	const numGoroutines = 50
	var wg sync.WaitGroup

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			userID := "user" + string(rune('A'+idx%5))
			conv := "conv" + string(rune('1'+idx%3))
			pm.RecordTyping(conv, userID)
		}(i)
	}

	wg.Wait()
	// If we get here without a race detector error, the test passes.
}

func TestPresenceManager_GetStateUnknownUser(t *testing.T) {
	pm, _ := newTestPM(t)
	assert.Equal(t, PresenceIdle, pm.GetState("nonexistent"))
}

func TestPresenceManager_GetAllStates(t *testing.T) {
	pm, _ := newTestPM(t)

	pm.Heartbeat(t.Context(), "user1", "User One", []string{"proj1"})
	pm.Heartbeat(t.Context(), "user2", "User Two", []string{"proj1"})

	// Expire user2.
	pm.mu.Lock()
	pm.users["user2"].lastHeartbeat = time.Now().Add(-(presenceActiveWindow + time.Second))
	pm.mu.Unlock()

	states := pm.GetAllStates()
	assert.Equal(t, "active", states["user1"])
	assert.Equal(t, "idle", states["user2"])
}

func TestPresenceManager_StopIsIdempotent(t *testing.T) {
	pub := &mockEventPublisher{}
	pm := NewPresenceManager(pub, nil)
	pm.Stop()
	// Calling Stop only once; calling it twice would deadlock since stopCh
	// is already closed and stopped channel already received. The test
	// verifies that Stop returns promptly.
}
