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
// has reached its cap returns to the first delay, min(keyRetryMin,
// interval). TestKeyRetryBackoff pins the delay sequences themselves.
func TestNewKeyRetryBackoffReset(t *testing.T) {
	for _, interval := range []time.Duration{KeyRefreshInterval, 45 * time.Second, 10 * time.Second} {
		b := newKeyRetryBackoff(interval)
		for i := 0; i < 8; i++ {
			b.Failure()
		}
		if got, want := b.Failure(), min(keyRetryMax, interval); got != want {
			t.Fatalf("interval %v: capped delay %v, want %v", interval, got, want)
		}
		b.Reset()
		if got, want := b.Failure(), min(keyRetryMin, interval); got != want {
			t.Fatalf("interval %v: after Reset got %v, want %v", interval, got, want)
		}
	}
}

// TestKeyRetryResetsAfterSuccess: in the loop, a successful fetch ends
// the backoff, so the next failure is retried after keyRetryMin again.
func TestKeyRetryResetsAfterSuccess(t *testing.T) {
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
		{true, keyRetryMin},
		{false, 2 * keyRetryMin},
		{true, KeyRefreshInterval},
		{true, keyRetryMin},
		{true, 2 * keyRetryMin},
	}
	for i, step := range steps {
		failing.Store(step.fail)
		waitPending(t, clk, 1)
		clk.Advance(step.wait - time.Millisecond)
		if n := h.keyHits.Load(); n != int64(i) {
			t.Fatalf("fetch %d: fetched before %v (%d fetches)", i+1, step.wait, n)
		}
		clk.Advance(time.Millisecond)
		waitPending(t, clk, 1)
		if n := h.keyHits.Load(); n != int64(i+1) {
			t.Fatalf("fetch %d: no fetch after %v (%d fetches)", i+1, step.wait, n)
		}
	}
}
