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

package messaging

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ConsistencyCheckTimeout bounds one CheckConversationConsistency run, all of
// its lookups together. The check is a diagnostic: when the deadline passes it
// is skipped (fails open) and counted, and the message is unaffected.
const ConsistencyCheckTimeout = 200 * time.Millisecond

// consistencyCheckSlots is how many consistency checks may run at once in
// this process. A check that finds every slot taken is skipped and counted
// rather than queued, so the checks never pile up behind slow lookups.
const consistencyCheckSlots = 8

// ConsistencyInput carries everything a background consistency check reads.
// The values are copied when the check is scheduled, so the goroutine never
// reads request state (or a server field) that the caller may change later.
type ConsistencyInput struct {
	Store          MessageQueryStore
	Log            *slog.Logger
	MessageID      string
	ConversationID string
	ThreadID       string
	SenderID       string
	RecipientID    string
}

// key identifies the lookup this input runs: the thread, or the unordered
// sender/recipient pair (the check reads both directions), together with
// the conversation the message resolved to. Returns "" when the check would
// not query anything.
func (in ConsistencyInput) key() string {
	switch {
	case in.ThreadID != "":
		return "thread\x00" + in.ConversationID + "\x00" + in.ThreadID
	case in.SenderID != "" && in.RecipientID != "":
		a, b := in.SenderID, in.RecipientID
		if b < a {
			a, b = b, a
		}
		return "pair\x00" + in.ConversationID + "\x00" + a + "\x00" + b
	default:
		return ""
	}
}

// ConsistencyRunner runs consistency checks off the message path, each on its
// own context with a hard deadline, never on the caller's context. It runs at
// most a fixed number of checks at once and at most one per lookup key; a
// check that would exceed either bound is skipped (and counted) instead of
// waiting. Nothing waits for a running check, so shutdown is never held up:
// a check in flight ends at its deadline.
type ConsistencyRunner struct {
	timeout  time.Duration
	max      int
	mu       sync.Mutex
	idle     *sync.Cond // signalled when running drops to zero
	running  int
	inflight map[string]struct{}
}

// NewConsistencyRunner returns a runner that runs up to slots checks at once,
// each with the given deadline.
func NewConsistencyRunner(slots int, timeout time.Duration) *ConsistencyRunner {
	r := &ConsistencyRunner{
		timeout:  timeout,
		max:      slots,
		inflight: make(map[string]struct{}),
	}
	r.idle = sync.NewCond(&r.mu)
	return r
}

// ConsistencyCheckRunner is the process-wide runner used by the message path.
var ConsistencyCheckRunner = NewConsistencyRunner(consistencyCheckSlots, ConsistencyCheckTimeout)

// GoConsistencyCheck schedules fn on ConsistencyCheckRunner. See
// ConsistencyRunner.Go.
func GoConsistencyCheck(in ConsistencyInput, fn func(ctx context.Context, in ConsistencyInput)) bool {
	return ConsistencyCheckRunner.Go(in, fn)
}

// Go runs fn(ctx, in) in a new goroutine and returns immediately. ctx is
// derived from context.Background with the runner's deadline. Go returns
// false, without running fn, when every slot is busy or a check with the same
// key is already running; the skip is counted in DivergenceMetrics. It also
// returns false, uncounted and without taking a slot, when the input has
// nothing to check (no conversation, or neither a thread nor a pair):
// CheckConversationConsistency would return at once for it.
func (r *ConsistencyRunner) Go(in ConsistencyInput, fn func(ctx context.Context, in ConsistencyInput)) bool {
	key := in.key()
	if key == "" || in.ConversationID == "" {
		return false
	}
	r.mu.Lock()
	if _, busy := r.inflight[key]; busy || r.running >= r.max {
		r.mu.Unlock()
		DivergenceMetrics.IncConsistencySkipped()
		return false
	}
	r.inflight[key] = struct{}{}
	r.running++
	r.mu.Unlock()

	go func() {
		// Registered first so it runs last: the slot and the key are
		// released on every path, including a panic in fn.
		defer r.release(key)
		defer func() {
			if p := recover(); p != nil {
				log := in.Log
				if log == nil {
					log = slog.Default()
				}
				log.Error("conversation consistency check panicked; skipped",
					"message_id", in.MessageID, "panic", p)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		defer cancel()
		fn(ctx, in)
	}()
	return true
}

func (r *ConsistencyRunner) release(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, key)
	r.running--
	if r.running == 0 {
		r.idle.Broadcast()
	}
}

// Wait blocks until no check is running. Each check ends by its deadline, so
// Wait is bounded while no new checks are scheduled. The message path never
// calls it; it lets tests observe a check's outcome deterministically.
func (r *ConsistencyRunner) Wait() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.running > 0 {
		r.idle.Wait()
	}
}
