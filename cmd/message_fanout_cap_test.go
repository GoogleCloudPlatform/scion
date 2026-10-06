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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3521: group sends and broadcast --all keep at most
// maxFanOutConcurrency sends in flight, without changing the per-recipient
// results, their order, or the interrupt semantics from ptone/scion#3510.

// fanOutStallGuard bounds how long a test waits for the next fan-out event.
// It is only a failure guard; no test relies on it for synchronisation.
const fanOutStallGuard = 20 * time.Second

// gateHub is a fake Hub whose message sends each block until the test
// releases them (or the client cancels), and that records the peak number of
// sends in flight at the Hub.
type gateHub struct {
	srv      *httptest.Server
	agents   []string           // returned by the agent list
	arrivals chan chan struct{} // one release channel per send that arrives

	mu       sync.Mutex
	inFlight int
	peak     int
}

func newGateHub(t *testing.T, agents []string) *gateHub {
	t.Helper()
	h := &gateHub{agents: agents, arrivals: make(chan chan struct{}, len(agents)+1)}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *gateHub) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/healthz":
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "u1", "email": "tester@example.com"})
		return
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
		agents := make([]map[string]string, 0, len(h.agents))
		for _, a := range h.agents {
			agents = append(agents, map[string]string{"id": a, "name": a, "slug": a, "phase": "running"})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": agents})
		return
	case r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/message"):
		w.WriteHeader(http.StatusNotFound)
		return
	}

	h.mu.Lock()
	h.inFlight++
	if h.inFlight > h.peak {
		h.peak = h.inFlight
	}
	h.mu.Unlock()

	release := make(chan struct{})
	h.arrivals <- release
	cancelled := false
	select {
	case <-release:
	case <-r.Context().Done():
		cancelled = true
	}

	h.mu.Lock()
	h.inFlight--
	h.mu.Unlock()
	if cancelled {
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"message_id": "m", "status": "delivered"})
}

func (h *gateHub) peakInFlight() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peak
}

func (h *gateHub) hubCtx(t *testing.T) *HubContext {
	t.Helper()
	client, err := hubclient.New(h.srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: h.srv.URL, ProjectID: "project-fanout-3521"}
}

func fanOutNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("agent-%02d", i)
	}
	return names
}

// driveGate releases held sends until done is closed. With no limit, every
// send arrives before any is released, so the peak equals the total. With
// the limit, a send is released only when the fan-out has a recipient queued
// for a slot and maxFanOutConcurrency sends are held, so the peak is exactly
// the limit; the rest are released once every send has arrived. Sends are
// released newest first, so completion order differs from recipient order.
func driveGate(t *testing.T, h *gateHub, queued <-chan struct{}, total int, done <-chan struct{}) {
	t.Helper()
	var held []chan struct{}
	arrived, owed := 0, 0
	releaseNewest := func() {
		close(held[len(held)-1])
		held = held[:len(held)-1]
	}
	for {
		select {
		case release := <-h.arrivals:
			arrived++
			held = append(held, release)
		case <-queued:
			owed++
		case <-done:
			return
		case <-time.After(fanOutStallGuard):
			// Let the send finish before failing, so it does not write
			// into a later test's captured output.
			for len(held) > 0 {
				releaseNewest()
			}
			<-done
			t.Fatalf("fan-out stalled: %d/%d sends arrived", arrived, total)
		}
		if arrived == total {
			for len(held) > 0 {
				releaseNewest()
			}
		}
		for owed > 0 && len(held) >= maxFanOutConcurrency {
			owed--
			releaseNewest()
		}
	}
}

// A group send to many recipients never has more than the limit in flight,
// and still reports every recipient, in recipient order.
func TestSendGroupMessage3521_FanOutIsCapped(t *testing.T) {
	groupTestState(t, "json")
	const total = 40
	names := fanOutNames(total)
	h := newGateHub(t, names)
	queued := make(chan struct{}, total)
	hooks := groupSendHooks{
		fanOutContext: newTestFanOut().hook,
		onQueued:      func() { queued <- struct{}{} },
	}

	done := make(chan struct{})
	var sendErr error
	var out string
	go func() {
		defer close(done)
		out = captureStdout(t, func() {
			sendErr = sendGroupMessageViaHubCtx(h.hubCtx(t), agentRecipients(names...), "hi", false, hooks)
		})
	}()
	driveGate(t, h, queued, total, done)

	require.NoError(t, sendErr)
	assert.Equal(t, maxFanOutConcurrency, h.peakInFlight(), "at most maxFanOutConcurrency sends may be in flight")
	var got groupSendResult
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout must be one JSON document; got:\n%s", out)
	assert.Equal(t, total, got.Delivered)
	require.Len(t, got.Results, total)
	for i, r := range got.Results {
		assert.Equal(t, "agent:"+names[i], r.Recipient, "results must be in recipient order, not completion order")
		assert.Equal(t, groupStatusDelivered, r.Status)
	}
}

// An interrupt while recipients are still queued for a slot reports the
// in-flight sends unknown and the queued ones failed (never sent, so
// retryable), in recipient order, and returns without waiting for a slot.
func TestSendGroupMessage3521_InterruptWithQueuedRecipients(t *testing.T) {
	groupTestState(t, "json")
	const total = 40
	names := fanOutNames(total)
	h := newGateHub(t, names)
	fan := newTestFanOut()
	queued := make(chan struct{}, total)
	hooks := groupSendHooks{
		fanOutContext: fan.hook,
		onQueued:      func() { queued <- struct{}{} },
	}

	done := make(chan struct{})
	var sendErr error
	var out string
	go func() {
		defer close(done)
		out = captureStdout(t, func() {
			sendErr = sendGroupMessageViaHubCtx(h.hubCtx(t), agentRecipients(names...), "hi", false, hooks)
		})
	}()

	// Interrupt once the limit is reached at the hub and the next recipient
	// is waiting for a slot. Nothing is released, so no slot ever frees up.
	var held []chan struct{}
	defer func() {
		for _, c := range held {
			close(c)
		}
	}()
	sawQueued := false
	for len(held) < maxFanOutConcurrency || !sawQueued {
		select {
		case release := <-h.arrivals:
			held = append(held, release)
		case <-queued:
			sawQueued = true
		case <-time.After(fanOutStallGuard):
			fan.cancel()
			<-done // see driveGate
			t.Fatalf("limit not reached: %d sends held, queued=%v", len(held), sawQueued)
		}
	}
	fan.cancel()
	select {
	case <-done:
	case <-time.After(fanOutStallGuard):
		t.Fatal("an interrupt with queued recipients must return promptly")
	}

	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr), "in-flight sends may have been delivered")
	var got groupSendResult
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout must be one JSON document; got:\n%s", out)
	require.Len(t, got.Results, total)
	assert.Equal(t, maxFanOutConcurrency, got.Unknown)
	assert.Equal(t, total-maxFanOutConcurrency, got.Failed)
	var wantRetry []string
	for i, r := range got.Results {
		assert.Equal(t, "agent:"+names[i], r.Recipient, "results must be in recipient order")
		if i < maxFanOutConcurrency {
			assert.Equal(t, groupStatusUnknown, r.Status, "recipient %d was in flight", i)
			continue
		}
		assert.Equal(t, groupStatusFailed, r.Status, "recipient %d was queued", i)
		assert.Contains(t, r.Error, "not sent")
		wantRetry = append(wantRetry, "agent:"+names[i])
	}
	assertRetryRoundTrips(t, got.RetryRecipient, wantRetry...)
}

// broadcast --all fans out on its own path; it is capped the same way.
func TestBroadcastAll3521_FanOutIsCapped(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	origAll, origInterrupt, origHook := bcastAll, bcastInterrupt, broadcastQueuedHook
	defer func() { bcastAll, bcastInterrupt, broadcastQueuedHook = origAll, origInterrupt, origHook }()

	const total = 40
	h := newGateHub(t, fanOutNames(total))
	queued := make(chan struct{}, total)
	bcastAll, bcastInterrupt = true, false
	broadcastQueuedHook = func() { queued <- struct{}{} }

	done := make(chan struct{})
	var sendErr error
	var out string
	go func() {
		defer close(done)
		out = captureStdout(t, func() { sendErr = broadcastViaHub(h.hubCtx(t), "maintenance") })
	}()
	driveGate(t, h, queued, total, done)

	require.NoError(t, sendErr)
	assert.Equal(t, maxFanOutConcurrency, h.peakInFlight(), "at most maxFanOutConcurrency sends may be in flight")
	assert.Equal(t, total, strings.Count(out, "Message delivered to agent"), out)
}

// boundedFanOut starts calls in index order and, with a limit of 1, runs
// them one at a time.
func TestBoundedFanOut3521_StartOrderAndLimit(t *testing.T) {
	var mu sync.Mutex
	var started []int
	inFlight, maxInFlight := 0, 0
	boundedFanOut(t.Context(), 10, 1, nil, func(i int) {
		mu.Lock()
		started = append(started, i)
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		// Yield so that, without the limit, other calls would overlap.
		runtime.Gosched()
		mu.Lock()
		inFlight--
		mu.Unlock()
	}, func(int) { t.Error("nothing should be skipped") })
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, started, "with limit 1, calls start in index order")
	assert.Equal(t, 1, maxInFlight, "with limit 1, calls run one at a time")
}

// Once ctx is done, the indices still waiting for a slot are skipped at
// once, without waiting for a running call to free its slot, even when that
// call ignores ctx. An interrupt therefore never hangs on the limit.
func TestBoundedFanOut3521_CancelSkipsQueuedWithoutWaitingForSlot(t *testing.T) {
	const n = 5
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	started := make(chan struct{})
	release := make(chan struct{})
	queued := make(chan struct{}, n)
	skipped := make(chan int, n)
	done := make(chan struct{})
	go func() {
		defer close(done)
		boundedFanOut(ctx, n, 1, func() { queued <- struct{}{} }, func(i int) {
			if i != 0 {
				t.Errorf("run(%d) started after cancel; only run(0) should run", i)
				return
			}
			close(started)
			<-release // ignores ctx: the slot frees only when the test says so
		}, func(i int) { skipped <- i })
	}()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer func() { releaseOnce(); <-done }()

	for _, ch := range []<-chan struct{}{started, queued} {
		select {
		case <-ch:
		case <-time.After(fanOutStallGuard):
			t.Fatal("run(0) did not start or index 1 was not queued")
		}
	}
	cancel()

	// run(0) still holds the only slot, so every skip must happen now.
	for want := 1; want < n; want++ {
		select {
		case got := <-skipped:
			assert.Equal(t, want, got, "queued indices are skipped in order")
		case <-time.After(fanOutStallGuard):
			t.Fatalf("index %d was not skipped while run(0) held the slot: the skip waited for a slot", want)
		}
	}
	select {
	case <-done:
		t.Fatal("boundedFanOut returned while run(0) was still running")
	default:
	}

	releaseOnce()
	select {
	case <-done:
	case <-time.After(fanOutStallGuard):
		t.Fatal("boundedFanOut did not return after run(0) finished")
	}
}

// tripContext models an interrupt that lands just after the fan-out's own
// check: its first Err call reports no error and then cancels it, so every
// later Err call (and Done) sees the cancellation.
type tripContext struct {
	context.Context
	once sync.Once
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func newTripContext() *tripContext {
	return &tripContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *tripContext) Done() <-chan struct{} { return c.done }

func (c *tripContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.err
	c.once.Do(func() {
		c.err = context.Canceled
		close(c.done)
	})
	return err
}

func (c *tripContext) cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.once.Do(func() {
		c.err = context.Canceled
		close(c.done)
	})
}

// An interrupt that arrives after a recipient's slot was taken but before
// its request started still means the message was never sent: the send
// checks for it and reports the recipient failed ("not sent", retryable),
// not unknown, and makes no request.
func TestSendGroupMessage3521_InterruptAfterSlotTakenIsNotSent(t *testing.T) {
	groupTestState(t, "json")
	names := fanOutNames(1)
	h := newGateHub(t, names)
	trip := newTripContext()
	hooks := groupSendHooks{
		fanOutContext: func(context.Context) (context.Context, context.CancelFunc) { return trip, trip.cancel },
	}

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHubCtx(h.hubCtx(t), agentRecipients(names...), "hi", false, hooks)
	})

	require.Error(t, sendErr)
	assert.Zero(t, h.peakInFlight(), "no request may reach the Hub")
	var got groupSendResult
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout must be one JSON document; got:\n%s", out)
	require.Len(t, got.Results, 1)
	assert.Equal(t, groupStatusFailed, got.Results[0].Status, "a send interrupted before its request started is not unknown")
	assert.Contains(t, got.Results[0].Error, "not sent")
	assertRetryRoundTrips(t, got.RetryRecipient, "agent:"+names[0])
}
