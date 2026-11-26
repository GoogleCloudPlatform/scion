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
	"log/slog"
	"sync"
	"time"
)

// defaultPendingResumeTTL bounds how long a held-back wake-on-message resume
// instruction waits for the waking message before falling back to standalone
// delivery. Sized above the hub's wakeReadyTimeout (30s) plus message
// persistence and dispatch headroom.
const defaultPendingResumeTTL = 90 * time.Second

// standaloneResumeDeliverTimeout bounds the background fallback delivery when
// a pending resume entry's TTL expires.
const standaloneResumeDeliverTimeout = 30 * time.Second

// pendingResume holds a deferred wake-on-message resume instruction for an
// agent that was started promptless with WakeForMessage=true.
type pendingResume struct {
	agentID    string
	projectID  string
	wake       string      // instruction to append after the waking message(s)
	standalone string      // instruction to deliver alone if no message arrives
	runID      string      // run the entry belongs to (opts.RunID)
	seq        uint64      // monotonic generation to guard timer expiry
	timer      *time.Timer // TTL fallback timer
}

// pendingResumeRegistry tracks per-agent deferred wake-on-message resume
// instructions on the Runtime Broker Server. All methods are safe for
// concurrent use and nil-receiver safe.
type pendingResumeRegistry struct {
	mu      sync.Mutex
	nextSeq uint64
	closed  bool
	entries map[string]pendingResume
}

func newPendingResumeRegistry() *pendingResumeRegistry {
	return &pendingResumeRegistry{
		entries: make(map[string]pendingResume),
	}
}

func pendingResumeKey(agentID, projectID string) string {
	return agentID + "\x00" + projectID
}

// put stores p under key, replacing any previous entry (and stopping its
// timer). If ttl > 0 and onExpire != nil, a timer is armed to invoke onExpire
// if the entry is still present under the same generation and runID when ttl
// elapses.
func (r *pendingResumeRegistry) put(key string, p pendingResume, ttl time.Duration, onExpire func(pendingResume)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if prev, ok := r.entries[key]; ok && prev.timer != nil {
		prev.timer.Stop()
	}
	r.nextSeq++
	seq := r.nextSeq
	p.seq = seq
	p.timer = nil
	if ttl > 0 && onExpire != nil {
		runID := p.runID
		p.timer = time.AfterFunc(ttl, func() {
			r.mu.Lock()
			cur, ok := r.entries[key]
			if !ok || cur.seq != seq || cur.runID != runID {
				r.mu.Unlock()
				return
			}
			delete(r.entries, key)
			r.mu.Unlock()
			onExpire(cur)
		})
	}
	r.entries[key] = p
}

// take atomically removes and returns the pending entry for key, stopping its
// TTL timer. Exactly one of take and timer expiry can win for a given entry.
func (r *pendingResumeRegistry) take(key string) (pendingResume, bool) {
	if r == nil {
		return pendingResume{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.entries[key]
	if !ok {
		return pendingResume{}, false
	}
	if cur.timer != nil {
		cur.timer.Stop()
	}
	delete(r.entries, key)
	return cur, true
}

// drop removes any pending entry for key and stops its timer.
func (r *pendingResumeRegistry) drop(key string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.entries[key]; ok {
		if cur.timer != nil {
			cur.timer.Stop()
		}
		delete(r.entries, key)
	}
}

// dropIfRun removes the pending entry for key only when runID is empty or
// matches the entry's runID, stopping its timer.
func (r *pendingResumeRegistry) dropIfRun(key, runID string) {
	if r == nil {
		return
	}
	if runID == "" {
		r.drop(key)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.entries[key]; ok && cur.runID == runID {
		if cur.timer != nil {
			cur.timer.Stop()
		}
		delete(r.entries, key)
	}
}

// close stops all pending timers and prevents new entries from being added.
func (r *pendingResumeRegistry) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for key, cur := range r.entries {
		if cur.timer != nil {
			cur.timer.Stop()
		}
		delete(r.entries, key)
	}
}

func (s *Server) effectivePendingResumeTTL() time.Duration {
	if s.pendingResumeTTL > 0 {
		return s.pendingResumeTTL
	}
	return defaultPendingResumeTTL
}

// deliverStandaloneResume is the TTL fallback when a message-triggered wake
// started the agent promptless but no waking message arrived in time.
func (s *Server) deliverStandaloneResume(p pendingResume) {
	if p.standalone == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), standaloneResumeDeliverTimeout)
	defer cancel()
	mgr := s.resolveManagerForAgent(ctx, p.agentID, p.projectID)
	if mgr == nil {
		return
	}
	if err := mgr.Message(ctx, p.agentID, p.projectID, p.standalone, false); err != nil {
		slog.Debug("pending resume fallback delivery skipped or failed",
			"agent_id", p.agentID,
			"project_id", p.projectID,
			"run_id", p.runID,
			"error", err,
		)
	}
}
