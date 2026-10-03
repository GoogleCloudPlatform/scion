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

// Reset returns to the base delay.
func (b *Backoff) Reset() { b.attempt = 0 }

// Reconnector keeps one dialer-side session alive (all principals): it
// dials, hands each session to OnSession, and redials with Backoff when the
// session ends. When the relay sends GoAway the replacement is dialed at
// once while the old session drains.
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
			for i, x := range live {
				if x == s {
					live = append(live[:i], live[i+1:]...)
					break
				}
			}
		}()
		if r.OnSession != nil {
			go r.OnSession(ctx, s, w)
		}
		goAway := false
		select {
		case <-ls.Done():
		case <-ls.GoAwayReceived():
			goAway = true
		case <-ctx.Done():
			return ctx.Err()
		}
		if clk.Now().Sub(started) >= BackoffResetLive {
			bo.Reset()
		}
		if goAway {
			// Planned drain: replace at once (honouring the relay's
			// reconnect hint), the old session finishes on its own.
			if d := reconnectAfter(s); d > 0 {
				if err := wait(d); err != nil {
					return err
				}
			}
			continue
		}
		if err := wait(bo.Next()); err != nil {
			return err
		}
	}
}

// reconnectAfter returns the GoAway reconnect hint of s, capped at
// BackoffMax.
func reconnectAfter(s Session) time.Duration {
	ss, ok := s.(*session)
	if !ok {
		return 0
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.remoteGoAway == nil {
		return 0
	}
	return min(time.Duration(ss.remoteGoAway.GetReconnectAfterMs())*time.Millisecond, BackoffMax)
}
