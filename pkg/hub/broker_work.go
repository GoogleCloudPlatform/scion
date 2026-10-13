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
	"time"
)

// brokerWorkStopGrace bounds how long shutdown waits for broker connection
// work (brokerWorkTracker) to finish. It is kept short because it is one
// step of the hub's shutdown time budget (ptone/scion#2931): the work it
// waits for is a handful of store writes, so it normally ends in
// milliseconds.
var brokerWorkStopGrace = 5 * time.Second

// brokerWorkTracker admits, tracks and joins the hub's broker connection
// work: the connect path (markBrokerOnline), the disconnect callback
// (handleBrokerDisconnect), and the dispatch reconcile and notification
// drain goroutines they start (ptone/scion#4869).
//
// None of that work is joined by anything else. markBrokerOnline runs on
// a hijacked HTTP connection, which http.Server.Shutdown does not wait
// for, and the rest runs on goroutines of its own. Without the tracker
// the store could close under it, and every store call it made after that
// logged an ERROR on every deploy.
//
// stop closes admission and then waits, so work that has not started yet
// is skipped instead of racing the store close. Skipping is safe: the
// dispatch and notification rows are durable and drain on the next
// connect. The zero value is ready to use.
type brokerWorkTracker struct {
	mu       sync.Mutex
	closed   bool
	running  int
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
	stopping chan struct{}
}

// initLocked creates the work context and the stopping channel. t.mu must
// be held.
func (t *brokerWorkTracker) initLocked() {
	if t.ctx == nil {
		t.ctx, t.cancel = context.WithCancel(context.Background())
	}
	if t.stopping == nil {
		t.stopping = make(chan struct{})
	}
}

// begin admits one unit of work. It returns the context the work must use
// and the function that ends the work, or ok=false once stop has started,
// in which case the caller must skip the work. The context is cancelled
// only when the work overruns the stop grace.
func (t *brokerWorkTracker) begin() (ctx context.Context, done func(), ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, nil, false
	}
	t.initLocked()
	t.wg.Add(1)
	t.running++
	var once sync.Once
	return t.ctx, func() {
		once.Do(func() {
			t.mu.Lock()
			t.running--
			t.mu.Unlock()
			t.wg.Done()
		})
	}, true
}

// goTracked runs fn on a new goroutine as tracked work. It reports false,
// and does not run fn, once stop has started.
func (t *brokerWorkTracker) goTracked(fn func(ctx context.Context)) bool {
	ctx, done, ok := t.begin()
	if !ok {
		return false
	}
	go func() {
		defer done()
		fn(ctx)
	}()
	return true
}

// stoppingCh returns a channel that is closed once stop has closed
// admission.
func (t *brokerWorkTracker) stoppingCh() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()
	return t.stopping
}

// stop closes admission and waits for the admitted work to end, until ctx
// is done or grace elapses, whichever comes first. If the wait ends early
// it cancels the work context so the remaining work stops as soon as it
// can. It returns how many units were still running when it returned; 0
// means every unit was joined. Only the first call waits; later calls
// return the count still running without waiting.
func (t *brokerWorkTracker) stop(ctx context.Context, grace time.Duration) int {
	t.mu.Lock()
	t.initLocked()
	first := !t.closed
	if first {
		t.closed = true
		close(t.stopping)
	}
	t.mu.Unlock()
	if !first {
		return t.runningCount()
	}

	joined := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(joined)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-joined:
		return 0
	case <-ctx.Done():
	case <-timer.C:
	}
	t.cancel()
	return t.runningCount()
}

func (t *brokerWorkTracker) runningCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}
