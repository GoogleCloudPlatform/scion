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

package runtimebroker

import (
	"context"
	"sort"
	"sync"
	"time"
)

// startTracker records the agent starts running on this broker's start,
// restart and synchronous create handlers. Each entry lives from handler
// entry until Manager.Start, including Run's deferred cleanup, has
// returned. The heartbeat reports the tracked keys (plus registered
// launches) as starts in flight, so the hub never reads an absent container
// as "nothing running" while a start could still create one; a stop cancels
// and waits for a tracked start of its agent before stopping; and Shutdown
// cancels and waits for every tracked start before the HTTP drain.
//
// A new process starts with an empty tracker, which is accurate: no start
// of this process is running.
type startTracker struct {
	mu      sync.Mutex
	entries map[launchKey]map[*trackedStart]struct{}
}

type trackedStart struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newStartTracker() *startTracker {
	return &startTracker{entries: make(map[launchKey]map[*trackedStart]struct{})}
}

// begin tracks a start for key. It returns a context derived from ctx that
// cancel calls cancel, and a finish func the caller must call exactly once,
// after the start (including its cleanup) has fully returned. A nil tracker
// tracks nothing and returns ctx unchanged.
func (t *startTracker) begin(ctx context.Context, key launchKey) (context.Context, func()) {
	if t == nil {
		return ctx, func() {}
	}
	startCtx, cancel := context.WithCancel(ctx)
	e := &trackedStart{cancel: cancel, done: make(chan struct{})}
	t.mu.Lock()
	set := t.entries[key]
	if set == nil {
		set = make(map[*trackedStart]struct{})
		t.entries[key] = set
	}
	set[e] = struct{}{}
	t.mu.Unlock()

	var once sync.Once
	return startCtx, func() {
		once.Do(func() {
			t.mu.Lock()
			if set := t.entries[key]; set != nil {
				delete(set, e)
				if len(set) == 0 {
					delete(t.entries, key)
				}
			}
			t.mu.Unlock()
			close(e.done)
			cancel()
		})
	}
}

// keys returns the tracked keys, sorted.
func (t *startTracker) keys() []launchKey {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	out := make([]launchKey, 0, len(t.entries))
	for k := range t.entries {
		out = append(out, k)
	}
	t.mu.Unlock()
	sortLaunchKeys(out)
	return out
}

// cancelAndWait cancels every tracked start for key and waits until each
// has finished or ctx is done. It reports whether all finished.
func (t *startTracker) cancelAndWait(ctx context.Context, key launchKey) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	var waits []*trackedStart
	for e := range t.entries[key] {
		waits = append(waits, e)
	}
	t.mu.Unlock()
	return cancelAndWaitAll(ctx, waits)
}

// cancelAllAndWait cancels every tracked start and waits until each has
// finished or ctx is done. It reports whether all finished.
func (t *startTracker) cancelAllAndWait(ctx context.Context) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	var waits []*trackedStart
	for _, set := range t.entries {
		for e := range set {
			waits = append(waits, e)
		}
	}
	t.mu.Unlock()
	return cancelAndWaitAll(ctx, waits)
}

func cancelAndWaitAll(ctx context.Context, waits []*trackedStart) bool {
	for _, e := range waits {
		e.cancel()
	}
	for _, e := range waits {
		select {
		case <-e.done:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// keys returns the keys of every registered launch, sorted.
func (r *launchRegistry) keys() []launchKey {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	out := make([]launchKey, 0, len(r.records))
	for k := range r.records {
		out = append(out, k)
	}
	r.mu.Unlock()
	sortLaunchKeys(out)
	return out
}

func sortLaunchKeys(keys []launchKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ProjectID != keys[j].ProjectID {
			return keys[i].ProjectID < keys[j].ProjectID
		}
		return keys[i].Slug < keys[j].Slug
	})
}

// startsInFlightSnapshot returns every start in flight on this broker: the
// tracked handler starts plus every registered launch (synchronous create
// or async launch), deduplicated and sorted.
func (s *Server) startsInFlightSnapshot() []launchKey {
	seen := map[launchKey]bool{}
	var out []launchKey
	for _, k := range append(s.startsInFlight.keys(), s.launchRegistry.keys()...) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sortLaunchKeys(out)
	return out
}

// inFlightStartStopWait bounds how long a stop waits for a cancelled start
// of the same agent to finish its cleanup before stopping anyway. It is
// below the hub's 60s stop-all dispatch deadline.
const inFlightStartStopWait = 45 * time.Second

// shutdownDeadline bounds Shutdown's two waits together: for cancelled
// starts to finish their cleanup, then for the HTTP server to drain.
const shutdownDeadline = 30 * time.Second

// cancelInFlightStart cancels the tracked starts of key and waits, bounded
// by ctx and inFlightStartStopWait, for their cleanup.
func (s *Server) cancelInFlightStart(ctx context.Context, key launchKey) {
	waitCtx, cancel := context.WithTimeout(ctx, inFlightStartStopWait)
	defer cancel()
	began := time.Now()
	finished := s.startsInFlight.cancelAndWait(waitCtx, key)
	if waited := time.Since(began); !finished {
		s.agentLifecycleLog.Warn("Stop proceeding before a cancelled start finished its cleanup",
			"project_id", key.ProjectID, "agent", key.Slug, "waited", waited)
	} else if waited > time.Second {
		s.agentLifecycleLog.Info("Stop waited for a cancelled start to finish its cleanup",
			"project_id", key.ProjectID, "agent", key.Slug, "waited", waited)
	}
}
