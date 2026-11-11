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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
)

const kickWait = 10 * time.Second

// kickHub is a fake token refresh endpoint. Each request is reported on
// started, with the fake clock's time recorded, and, when gate is set,
// held until release is signalled. The n-th successful refresh returns a
// token expiring at start + (10+n)h, so the loop's next scheduled refresh
// is start + (8+n)h.
type kickHub struct {
	t       *testing.T
	clk     *clock.Fake
	start   time.Time
	gate    bool
	fail    map[int]bool // request numbers (1-based) answered 503
	started chan int
	release chan struct{}

	mu          sync.Mutex
	calls       int
	at          []time.Time // fake-clock time each request arrived
	inFlight    int
	maxInFlight int
}

func newKickHub(t *testing.T, clk *clock.Fake) *kickHub {
	return &kickHub{t: t, clk: clk, start: clk.Now(), fail: map[int]bool{},
		started: make(chan int, 8), release: make(chan struct{})}
}

func (h *kickHub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	h.calls++
	n := h.calls
	h.at = append(h.at, h.clk.Now())
	h.inFlight++
	if h.inFlight > h.maxInFlight {
		h.maxInFlight = h.inFlight
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.inFlight--
		h.mu.Unlock()
	}()
	h.started <- n
	if h.gate {
		<-h.release
	}
	if h.fail[n] {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	exp := h.start.Add(time.Duration(10+n) * time.Hour).UTC().Format(time.RFC3339)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"token":"t","expires_at":"` + exp + `"}`))
}

func (h *kickHub) stats() (calls, maxInFlight int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, h.maxInFlight
}

func (h *kickHub) arrivedAt(n int) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.at[n-1]
}

func (h *kickHub) waitStarted(want int) {
	h.t.Helper()
	select {
	case n := <-h.started:
		if n != want {
			h.t.Fatalf("refresh request %d started, want %d", n, want)
		}
	case <-time.After(kickWait):
		h.t.Fatalf("refresh request %d did not start", want)
	}
}

// loopWait is one report from the loop's onWait hook.
type loopWait struct {
	at       time.Time
	kickable bool
	pending  int
}

type kickLoop struct {
	t         *testing.T
	c         *Client
	refreshed chan time.Time
	waits     chan loopWait
}

// startKickLoop runs StartTokenRefresh against h on h.clk, scheduled at
// start + 5h, so only kicks (or Advance) trigger refreshes. It returns
// once the loop waits for that schedule.
func startKickLoop(t *testing.T, h *kickHub) *kickLoop {
	t.Helper()
	cleanup := SetTokenHome(t.TempDir())
	t.Cleanup(cleanup)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(h.release) }) // unblock a held request before srv.Close

	l := &kickLoop{
		t:         t,
		c:         NewClientWithConfig(srv.URL, "old", "agent-1"),
		refreshed: make(chan time.Time, 8),
		waits:     make(chan loopWait, 8),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := l.c.StartTokenRefresh(ctx, &TokenRefreshConfig{
		RefreshAt:   h.start.Add(5 * time.Hour),
		Timeout:     kickWait,
		OnRefreshed: func(exp time.Time) { l.refreshed <- exp },
		clock:       h.clk,
		onWait: func(at time.Time, kickable bool, pending int) {
			l.waits <- loopWait{at, kickable, pending}
		},
	})
	t.Cleanup(func() { cancel(); <-done })
	l.nextWait(h.start.Add(5*time.Hour), true, 0)
	return l
}

// nextWait waits for the loop to start waiting and checks when it will
// refresh, whether a kick can wake it, and how many kicks are pending as
// it starts waiting.
func (l *kickLoop) nextWait(at time.Time, kickable bool, pending int) {
	l.t.Helper()
	select {
	case w := <-l.waits:
		want := loopWait{at, kickable, pending}
		if !w.at.Equal(at) || w.kickable != kickable || w.pending != pending {
			l.t.Fatalf("loop wait = %+v, want %+v", w, want)
		}
	case <-time.After(kickWait):
		l.t.Fatal("refresh loop did not start waiting")
	}
}

func (l *kickLoop) waitRefreshed() {
	l.t.Helper()
	select {
	case <-l.refreshed:
	case <-time.After(kickWait):
		l.t.Fatal("refresh did not complete")
	}
}

// TestKickTokenRefresh_IdleWakesAndCoalesces: a kick while the loop waits
// for its scheduled refresh runs one refresh now; two kicks during that
// refresh coalesce into exactly one more; refreshes never overlap.
func TestKickTokenRefresh_IdleWakesAndCoalesces(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	h := newKickHub(t, clock.NewFake(start))
	h.gate = true
	l := startKickLoop(t, h)

	l.c.KickTokenRefresh()
	h.waitStarted(1)
	l.c.KickTokenRefresh() // both arrive while refresh 1 is in flight
	l.c.KickTokenRefresh()
	h.release <- struct{}{}
	l.waitRefreshed()

	l.nextWait(start.Add(9*time.Hour), true, 1)
	h.waitStarted(2) // the coalesced kicks: one extra refresh, no Advance
	h.release <- struct{}{}
	l.waitRefreshed()

	// The loop waits for its schedule again with no kick pending, so
	// nothing else can run.
	l.nextWait(start.Add(10*time.Hour), true, 0)
	calls, maxInFlight := h.stats()
	if calls != 2 || maxInFlight != 1 {
		t.Fatalf("calls = %d, max in flight = %d; want 2 and 1", calls, maxInFlight)
	}
	if !h.arrivedAt(2).Equal(start) {
		t.Fatalf("refresh 2 at %v, want %v (the kick, not the schedule)", h.arrivedAt(2), start)
	}
}

// TestKickTokenRefresh_BackoffNotShortened: a kick pending while the loop
// enters a failure backoff does not wake it; it runs exactly when the
// backoff ends.
func TestKickTokenRefresh_BackoffNotShortened(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	h := newKickHub(t, clk)
	h.gate = true
	h.fail[1] = true
	l := startKickLoop(t, h)

	l.c.KickTokenRefresh()
	h.waitStarted(1)
	// Pending before the loop starts its backoff: a loop that took kicks
	// during a backoff would wake on it at once.
	l.c.KickTokenRefresh()
	l.c.KickTokenRefresh()
	h.release <- struct{}{} // 503: back off for tokenRefreshRetryBaseDelay

	backoffEnd := start.Add(tokenRefreshRetryBaseDelay)
	l.nextWait(backoffEnd, false, 1)
	// Nothing can take the kick before the backoff timer fires.
	if n := len(l.c.refreshKicks()); n != 1 {
		t.Fatalf("pending kicks during backoff = %d, want 1", n)
	}

	clk.Advance(tokenRefreshRetryBaseDelay - time.Second)
	if calls, _ := h.stats(); calls != 1 {
		t.Fatalf("calls before the backoff ends = %d, want 1", calls)
	}
	if n := len(l.c.refreshKicks()); n != 1 {
		t.Fatalf("pending kicks before the backoff ends = %d, want 1", n)
	}
	if !clk.WaitForTimer(kickWait, backoffEnd) {
		t.Fatal("backoff timer is no longer armed before the backoff ends")
	}
	clk.Advance(time.Second)
	h.waitStarted(2)
	if got := h.arrivedAt(2); !got.Equal(backoffEnd) {
		t.Fatalf("refresh 2 at %v, want the end of the backoff %v", got, backoffEnd)
	}
	h.release <- struct{}{}
	l.waitRefreshed()

	// That refresh served the pending kick: none is left to run a
	// second refresh once the loop has recovered.
	l.nextWait(start.Add(10*time.Hour), true, 0)
	calls, maxInFlight := h.stats()
	if calls != 2 || maxInFlight != 1 {
		t.Fatalf("calls = %d, max in flight = %d; want 2 and 1", calls, maxInFlight)
	}
}

// TestKickTokenRefresh_NeverBlocks: kicks with no loop running return at
// once and leave a single pending kick.
func TestKickTokenRefresh_NeverBlocks(t *testing.T) {
	c := NewClientWithConfig("http://127.0.0.1:1", "t", "agent-1")
	for range 5 {
		c.KickTokenRefresh()
	}
	if n := len(c.refreshKicks()); n != 1 {
		t.Fatalf("pending kicks = %d, want 1", n)
	}
}
