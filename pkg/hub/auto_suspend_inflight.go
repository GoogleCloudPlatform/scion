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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// autoSuspendEntry tracks one in-flight hub auto-suspend (stalled or idle) on
// this replica so concurrent DM sends can observe it, wait boundedly for the
// stop to settle, and then wake (when permitted) instead of racing the
// stopping container.
type autoSuspendEntry struct {
	reason state.SuspendReason
	done   chan struct{}
}

// autoSuspendTracker tracks agents currently being auto-suspended by this hub
// replica. The zero value is ready for use.
type autoSuspendTracker struct {
	mu      sync.Mutex
	entries map[string]*autoSuspendEntry
}

// begin marks agentID as actively auto-suspending for reason and returns an
// idempotent end callback that removes the entry and closes its done channel.
// Callers must defer end() immediately so errors and panics never leak entries
// or strand waiters.
func (t *autoSuspendTracker) begin(agentID string, reason state.SuspendReason) func() {
	if t == nil || agentID == "" {
		return func() {}
	}
	entry := &autoSuspendEntry{
		reason: reason,
		done:   make(chan struct{}),
	}
	t.mu.Lock()
	if t.entries == nil {
		t.entries = make(map[string]*autoSuspendEntry)
	}
	t.entries[agentID] = entry
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.entries[agentID] == entry {
				delete(t.entries, agentID)
			}
			t.mu.Unlock()
			close(entry.done)
		})
	}
}

// active reports whether agentID is currently being auto-suspended on this
// replica, returning its suspend reason and a channel closed when the suspend
// finishes.
func (t *autoSuspendTracker) active(agentID string) (state.SuspendReason, <-chan struct{}, bool) {
	if t == nil || agentID == "" {
		return "", nil, false
	}
	t.mu.Lock()
	entry, ok := t.entries[agentID]
	t.mu.Unlock()
	if !ok {
		return "", nil, false
	}
	return entry.reason, entry.done, true
}

// activeReason reports whether agentID is currently being auto-suspended on
// this replica and returns its suspend reason.
func (t *autoSuspendTracker) activeReason(agentID string) (state.SuspendReason, bool) {
	reason, _, ok := t.active(agentID)
	return reason, ok
}

// isActive reports whether agentID is currently being auto-suspended on this
// replica.
func (t *autoSuspendTracker) isActive(agentID string) bool {
	_, _, ok := t.active(agentID)
	return ok
}
