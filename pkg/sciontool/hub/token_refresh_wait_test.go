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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
)

// waitResult starts KickTokenRefreshWait on l's client and returns a
// channel carrying its result. It returns once the wait has subscribed
// and kicked.
func (l *kickLoop) waitResult(bound time.Duration) <-chan error {
	l.t.Helper()
	kicked := make(chan struct{})
	l.c.onKickWait = func() { close(kicked) }
	res := make(chan error, 1)
	go func() { res <- l.c.KickTokenRefreshWait(context.Background(), bound) }()
	select {
	case <-kicked:
	case <-time.After(kickWait):
		l.t.Fatal("KickTokenRefreshWait did not kick")
	}
	return res
}

func waitErr(t *testing.T, res <-chan error) error {
	t.Helper()
	select {
	case err := <-res:
		return err
	case <-time.After(kickWait):
		t.Fatal("KickTokenRefreshWait did not return")
		return nil
	}
}

// TestSubscribeTokenRefresh_EachAttempt: subscribers see every attempt,
// with the new token on success and the error on failure; an
// unsubscribed function sees nothing more.
func TestSubscribeTokenRefresh_EachAttempt(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	h := newKickHub(t, clk)
	h.fail[2] = true
	l := startKickLoop(t, h)

	results := make(chan TokenRefreshResult, 8)
	unsubscribe := l.c.SubscribeTokenRefresh(func(r TokenRefreshResult) { results <- r })
	next := func() TokenRefreshResult {
		t.Helper()
		select {
		case r := <-results:
			return r
		case <-time.After(kickWait):
			t.Fatal("no refresh result")
			return TokenRefreshResult{}
		}
	}

	l.c.KickTokenRefresh()
	h.waitStarted(1)
	r := next()
	if r.Err != nil || r.Token != "t" || !r.ExpiresAt.Equal(start.Add(11*time.Hour)) || r.Seq != 1 {
		t.Fatalf("result 1 = %+v, want token t expiring at %v, seq 1", r, start.Add(11*time.Hour))
	}
	if got := l.c.GetToken(); got != "t" {
		t.Fatalf("client token = %q when notified, want t", got)
	}
	l.waitRefreshed()
	l.nextWait(start.Add(9*time.Hour), true, 0)

	l.c.KickTokenRefresh()
	h.waitStarted(2)
	if r := next(); r.Err == nil || r.Token != "" || r.Seq != 2 {
		t.Fatalf("result 2 = %+v, want an error, no token, seq 2", r)
	}
	l.nextWait(start.Add(tokenRefreshRetryBaseDelay), false, 0)

	unsubscribe()
	clk.Advance(tokenRefreshRetryBaseDelay)
	h.waitStarted(3)
	l.waitRefreshed()
	select {
	case r := <-results:
		t.Fatalf("result after unsubscribe: %+v", r)
	default:
	}
}

// TestKickTokenRefreshWait_IdleLoop: a wait while the loop is idle wakes
// it and returns that refresh's result.
func TestKickTokenRefreshWait_IdleLoop(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	h := newKickHub(t, clock.NewFake(start))
	l := startKickLoop(t, h)

	if err := waitErr(t, l.waitResult(kickWait)); err != nil {
		t.Fatalf("KickTokenRefreshWait = %v, want nil", err)
	}
	if calls, _ := h.stats(); calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

// TestKickTokenRefreshWait_CoalescesWithInFlight: a wait that starts
// while a refresh is in flight returns that refresh's result (here a
// failure), not a later one, and causes no overlapping refresh.
func TestKickTokenRefreshWait_CoalescesWithInFlight(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	h := newKickHub(t, clock.NewFake(start))
	h.gate = true
	h.fail[1] = true
	l := startKickLoop(t, h)

	l.c.KickTokenRefresh()
	h.waitStarted(1) // refresh 1 is in flight
	res := l.waitResult(kickWait)
	h.release <- struct{}{} // refresh 1 answers 503

	if err := waitErr(t, res); err == nil {
		t.Fatal("KickTokenRefreshWait = nil, want refresh 1's error")
	}
	// The wait's kick stays pending for the loop (served after its
	// backoff); it started no refresh of its own.
	l.nextWait(start.Add(tokenRefreshRetryBaseDelay), false, 1)
	if calls, maxInFlight := h.stats(); calls != 1 || maxInFlight != 1 {
		t.Fatalf("calls = %d, max in flight = %d; want 1 and 1", calls, maxInFlight)
	}
}

// TestKickTokenRefreshWait_Bound: a refresh that never completes, or no
// loop at all, returns ErrTokenRefreshWaitTimeout within the bound.
func TestKickTokenRefreshWait_Bound(t *testing.T) {
	const bound = 20 * time.Millisecond

	t.Run("refresh never completes", func(t *testing.T) {
		start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
		h := newKickHub(t, clock.NewFake(start))
		h.gate = true // never released before the test ends
		l := startKickLoop(t, h)
		began := time.Now()
		res := l.waitResult(bound)
		h.waitStarted(1)
		if err := waitErr(t, res); !errors.Is(err, ErrTokenRefreshWaitTimeout) {
			t.Fatalf("KickTokenRefreshWait = %v, want ErrTokenRefreshWaitTimeout", err)
		}
		if d := time.Since(began); d > kickWait/2 {
			t.Fatalf("returned after %v, bound %v", d, bound)
		}
	})

	t.Run("no loop", func(t *testing.T) {
		c := NewClientWithConfig("http://127.0.0.1:1", "t", "agent-1")
		if err := c.KickTokenRefreshWait(context.Background(), bound); !errors.Is(err, ErrTokenRefreshWaitTimeout) {
			t.Fatalf("KickTokenRefreshWait = %v, want ErrTokenRefreshWaitTimeout", err)
		}
	})

	t.Run("context ends first", func(t *testing.T) {
		c := NewClientWithConfig("http://127.0.0.1:1", "t", "agent-1")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.KickTokenRefreshWait(ctx, kickWait); !errors.Is(err, context.Canceled) {
			t.Fatalf("KickTokenRefreshWait = %v, want context.Canceled", err)
		}
	})
}
