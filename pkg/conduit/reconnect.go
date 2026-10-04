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
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Reconnect backoff (§3.3): exponential from 1s to 60s with full jitter,
// reset after a session that lived at least 60s.
const (
	BackoffBase      = time.Second
	BackoffMax       = 60 * time.Second
	BackoffResetLive = 60 * time.Second
	// MinPlannedDrainLife is how long a session must have lived for a
	// planned GoAway to skip backoff. A relay that drains fresh sessions
	// straight away is backed off like a failure, so it cannot cause a
	// tight reconnect loop.
	MinPlannedDrainLife = 10 * time.Second
	// MaxReconnectWindow caps the GoAway reconnect window the dialer
	// honours. It matches the largest window a relay may be configured
	// with (server.hub.conduit.reconnect_window).
	MaxReconnectWindow = 5 * time.Minute
)

// Backoff computes full-jitter exponential delays. It is not safe for
// concurrent use.
type Backoff struct {
	Base, Max time.Duration
	// Rand returns a uniform value in [0, n); nil uses math/rand/v2.
	Rand    func(n int64) int64
	attempt int
}

// Ceiling returns the upper bound of the next delay: min(Max, Base·2^attempt).
func (b *Backoff) Ceiling() time.Duration {
	base, maxD := b.Base, b.Max
	if base <= 0 {
		base = BackoffBase
	}
	if maxD <= 0 {
		maxD = BackoffMax
	}
	c := base
	for i := 0; i < b.attempt && c < maxD; i++ {
		c *= 2
	}
	return min(c, maxD)
}

// Next returns a delay drawn uniformly from [0, Ceiling()] and advances the
// attempt counter.
func (b *Backoff) Next() time.Duration {
	c := b.Ceiling()
	b.attempt++
	r := b.Rand
	if r == nil {
		r = rand.Int64N
	}
	return time.Duration(r(int64(c) + 1))
}

// Window returns a delay drawn uniformly from [0, w] (0 for w <= 0). It
// does not advance the attempt counter.
func (b *Backoff) Window(w time.Duration) time.Duration {
	if w <= 0 {
		return 0
	}
	r := b.Rand
	if r == nil {
		r = rand.Int64N
	}
	return time.Duration(r(int64(w) + 1))
}

// Reset returns to the base delay.
func (b *Backoff) Reset() { b.attempt = 0 }

// Reconnector keeps one dialer-side session alive (all principals): it
// dials, hands each session to OnSession, and redials with Backoff when the
// session ends. When the relay announces a planned drain (GoAway 4503, or
// any GoAway with a drain deadline) the replacement is dialed after a delay
// drawn uniformly from [0, reconnect_after_ms] (full jitter, so the
// dialers of a draining relay spread out) while the old session drains.
// Any other GoAway (4400 protocol error, 4401/4403 auth) is a failure and
// backs off.
type Reconnector struct {
	Dialer transport.Dialer
	Config Config
	// Hello builds the Hello for each attempt.
	Hello func() *conduitv1.Hello
	// OnSession is called (in its own goroutine) for every established
	// session. It must not block the caller beyond the session's life.
	OnSession func(ctx context.Context, s Session, w *conduitv1.Welcome)
	// OnDialError is called after a failed attempt with the delay before
	// the next one (optional).
	OnDialError func(err error, delay time.Duration)
	// Backoff overrides the default 1s→60s policy (tests inject Rand).
	Backoff *Backoff
}

// Run dials until ctx is cancelled. It closes the current session and
// returns ctx.Err() on cancellation.
func (r *Reconnector) Run(ctx context.Context) error {
	cfg := r.Config.withDefaults()
	clk := cfg.Clock
	bo := r.Backoff
	if bo == nil {
		bo = &Backoff{}
	}
	var (
		mu   sync.Mutex
		live []Session
	)
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range live {
			_ = s.Close()
		}
	}()
	wait := func(d time.Duration) error {
		ch, stop := clock.After(clk, d)
		defer stop()
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s, w, err := Dial(ctx, r.Dialer, cfg, r.Hello())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d := bo.Next()
			if r.OnDialError != nil {
				r.OnDialError(err, d)
			}
			if err := wait(d); err != nil {
				return err
			}
			continue
		}
		ls := s.(LocalSession)
		started := clk.Now()
		mu.Lock()
		live = append(live, s)
		mu.Unlock()
		go func() {
			<-ls.Done()
			mu.Lock()
			defer mu.Unlock()
			live = removeSession(live, s)
		}()
		if r.OnSession != nil {
			go r.OnSession(ctx, s, w)
		}
		select {
		case <-ls.Done():
		case <-ls.GoAwayReceived():
		case <-ctx.Done():
			return ctx.Err()
		}
		// Whichever case fired, decide from the session's state: a GoAway
		// is followed by the transport close, so both may be ready.
		if d := redialDelay(receivedGoAway(ls), clk.Now().Sub(started), bo); d > 0 {
			if err := wait(d); err != nil {
				return err
			}
		}
	}
}

// removeSession removes s from live. The vacated slot of the backing
// array is cleared, so an ended session is not kept reachable by it.
func removeSession(live []Session, s Session) []Session {
	if i := slices.Index(live, s); i >= 0 {
		return slices.Delete(live, i, i+1)
	}
	return live
}

// receivedGoAway returns the GoAway the peer sent on s, or nil, without
// blocking.
func receivedGoAway(ls LocalSession) *conduitv1.GoAway {
	select {
	case <-ls.GoAwayReceived():
	default:
		return nil
	}
	ss, ok := ls.(*session)
	if !ok {
		return &conduitv1.GoAway{Code: CloseRelayRestart}
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.remoteGoAway
}

// plannedDrain reports whether ga announces a planned drain rather than a
// failure.
func plannedDrain(ga *conduitv1.GoAway) bool {
	return ga != nil && (ga.GetCode() == CloseRelayRestart || ga.GetDrainDeadlineMs() > 0)
}

// redialDelay is the wait before replacing a session that lived for lived
// and ended (or was told to go away) with ga (nil: no GoAway). A session
// that lived BackoffResetLive resets the backoff. A planned drain of a
// session that lived MinPlannedDrainLife waits a delay drawn uniformly from
// the relay's reconnect window [0, reconnect_after_ms] (capped at
// MaxReconnectWindow); everything else backs off, and a planned drain of a
// fresh session waits the longer of the backoff and the window draw.
func redialDelay(ga *conduitv1.GoAway, lived time.Duration, bo *Backoff) time.Duration {
	if lived >= BackoffResetLive {
		bo.Reset()
	}
	if !plannedDrain(ga) {
		return bo.Next()
	}
	window := min(time.Duration(ga.GetReconnectAfterMs())*time.Millisecond, MaxReconnectWindow)
	if lived >= MinPlannedDrainLife {
		return bo.Window(window)
	}
	return max(bo.Next(), bo.Window(window))
}
