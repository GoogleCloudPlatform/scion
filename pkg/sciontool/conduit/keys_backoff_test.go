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

package conduit

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
)

// TestNewKeyRetryBackoffReset: for each interval, Reset after the delay
// has reached its cap returns to the first delay. TestKeyRetryBackoff and
// TestKeyRetryLoop_DelaySequence pin the delay sequences themselves.
func TestNewKeyRetryBackoffReset(t *testing.T) {
	tests := []struct {
		interval, first, cap time.Duration
	}{
		{KeyRefreshInterval, 30 * time.Second, 5 * time.Minute},
		{45 * time.Second, 30 * time.Second, 45 * time.Second},
		{10 * time.Second, 10 * time.Second, 10 * time.Second},
	}
	for _, tt := range tests {
		b := newKeyRetryBackoff(tt.interval)
		for i := 0; i < 8; i++ {
			b.Failure()
		}
		if got := b.Failure(); got != tt.cap {
			t.Fatalf("interval %v: capped delay %v, want %v", tt.interval, got, tt.cap)
		}
		b.Reset()
		if got := b.Failure(); got != tt.first {
			t.Fatalf("interval %v: after Reset got %v, want %v", tt.interval, got, tt.first)
		}
	}
}

// TestKeyRetryLoop_DelaySequence pins the loop's exact retry deadlines:
// consecutive failures wait 30s, 1m, 2m, 4m, then 5m (the cap), a success
// restores the normal interval, and the next failure waits 30s again.
func TestKeyRetryLoop_DelaySequence(t *testing.T) {
	h := newFakeHub(t, newTestKey(t, "k1").public)
	var failing atomic.Bool
	serve := h.srv.Config.Handler
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			h.keyHits.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		serve.ServeHTTP(w, r)
	})
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	a, err := New(Options{
		HubURL: h.srv.URL, AgentID: testAgentID, ProjectID: testProjectID,
		Token: func() string { return "t" },
		Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.refreshKeysLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	steps := []struct {
		fail bool
		wait time.Duration // delay before this fetch
	}{
		{true, KeyRefreshInterval},
		{true, 30 * time.Second},
		{true, time.Minute},
		{true, 2 * time.Minute},
		{true, 4 * time.Minute},
		{true, 5 * time.Minute},
		{false, 5 * time.Minute},
		{true, KeyRefreshInterval},
		{true, 30 * time.Second},
	}
	now := clk.Now()
	for i, step := range steps {
		// The timer is armed only once the previous fetch has finished,
		// so the next answer can be switched safely after it.
		if !clk.WaitForTimer(waitTimeout, now.Add(step.wait)) {
			t.Fatalf("fetch %d: no timer armed for %v after the previous fetch", i+1, step.wait)
		}
		if n := h.keyHits.Load(); n != int64(i) {
			t.Fatalf("fetch %d: %d fetches before it, want %d", i+1, n, i)
		}
		failing.Store(step.fail)
		clk.Advance(step.wait)
		now = now.Add(step.wait)
	}
	if !clk.WaitForTimer(waitTimeout, now.Add(time.Minute)) {
		t.Fatalf("no 1m retry armed after fetch %d", len(steps))
	}
	if n := h.keyHits.Load(); n != int64(len(steps)) {
		t.Fatalf("%d fetches, want %d", n, len(steps))
	}
}
