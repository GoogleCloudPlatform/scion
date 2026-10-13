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

package target

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// DeadlineGrace is how long after a stream's authorization deadline the
// target closes it, so a renewal the hub sends at or near the deadline
// still arrives in time.
const DeadlineGrace = 30 * time.Second

// ReasonAuthzExpired is the close reason of a stream that reached its
// authorization deadline unrenewed (with code conduit.CloseUnauthenticated).
const ReasonAuthzExpired = "authz_expired"

// ErrUntracked refuses a renewal for a stream that has no deadline here
// (unknown, ended, or opened without one).
var ErrUntracked = errors.New("conduit target: renewal for a stream without a deadline")

// Deadlines enforces the authorization deadlines of the streams a target
// serves (design §3.5, "Enforcement point"). A stream whose open grant
// carries authz_until is tracked from its accept: it is closed with 4401
// authz_expired at authz_until plus Grace, unless a valid renewal grant
// moves the deadline first. Streams whose grant carries no authz_until
// are not tracked.
//
// A renewal is accepted only as a renewal grant signed with a key in Keys
// and bound to the stream: its target, project and subject equal the open
// grant's, its stream_id is the stream's id on the session the renewal
// arrived on, and its open_jti is the open grant's jti. It may only move
// the deadline later, to at most now + the open grant's authz_interval +
// Grace. A refused renewal changes nothing (design v2.21.16 §3.5
// "Target-side deadlines").
type Deadlines struct {
	// Keys verifies renewal grants. Required.
	Keys grant.KeySet
	// Replay consumes renewal jtis. Required.
	Replay grant.ReplayCache
	// Issuer, when non-empty, must equal each renewal's iss. When empty,
	// a renewal's iss must equal the iss of the stream's open grant.
	Issuer string
	// ClockSkew is passed to renewal verification.
	ClockSkew time.Duration
	// Clock drives deadlines (nil = real time).
	Clock clock.Clock
	// Grace is the delay after a deadline before the close (0 =
	// DeadlineGrace).
	Grace time.Duration

	mu      sync.Mutex
	streams map[deadlineKey]*deadlineEntry
}

type deadlineKey struct {
	sessionID string
	epoch     int64
	streamID  uint32
}

type deadlineEntry struct {
	open     grant.Claims
	interval time.Duration
	until    time.Time
	timer    clock.Timer
	close    func(code uint32, reason string)
}

func (d *Deadlines) clk() clock.Clock {
	if d.Clock == nil {
		return clock.Real()
	}
	return d.Clock
}

func (d *Deadlines) grace() time.Duration {
	if d.Grace > 0 {
		return d.Grace
	}
	return DeadlineGrace
}

// Track starts enforcing the deadline of the stream streamID on the session
// b, opened with the verified grant open. closeFn closes the stream; it is
// called at most once, never with Deadlines' lock held. The returned
// function stops tracking (call it when the stream ends). A grant without
// authz_until is not tracked.
func (d *Deadlines) Track(b Binding, streamID uint32, open *grant.Claims, closeFn func(code uint32, reason string)) (untrack func()) {
	if open == nil || open.AuthzUntil.IsZero() || open.AuthzInterval <= 0 || closeFn == nil {
		return func() {}
	}
	k := deadlineKey{b.SessionID, b.ConnectionEpoch, streamID}
	e := &deadlineEntry{
		open:     *open,
		interval: open.AuthzInterval,
		until:    open.AuthzUntil,
		close:    closeFn,
	}
	d.mu.Lock()
	if d.streams == nil {
		d.streams = map[deadlineKey]*deadlineEntry{}
	}
	if old := d.streams[k]; old != nil && old.timer != nil {
		old.timer.Stop()
	}
	d.streams[k] = e
	d.armLocked(k, e)
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.streams[k] == e {
			if e.timer != nil {
				e.timer.Stop()
			}
			delete(d.streams, k)
		}
	}
}

// armLocked (re)arms e's timer for its deadline plus the grace. d.mu is
// held.
func (d *Deadlines) armLocked(k deadlineKey, e *deadlineEntry) {
	if e.timer != nil {
		e.timer.Stop()
	}
	c := d.clk()
	e.timer = c.AfterFunc(e.until.Add(d.grace()).Sub(c.Now()), func() { d.expire(k, e) })
}

// expire closes the stream if its deadline (plus grace) has been reached.
// A timer that outlived a renewal re-arms instead.
func (d *Deadlines) expire(k deadlineKey, e *deadlineEntry) {
	d.mu.Lock()
	if d.streams[k] != e {
		d.mu.Unlock()
		return
	}
	if d.clk().Now().Before(e.until.Add(d.grace())) {
		d.armLocked(k, e)
		d.mu.Unlock()
		return
	}
	delete(d.streams, k)
	d.mu.Unlock()
	e.close(conduit.CloseUnauthenticated, ReasonAuthzExpired)
}

// Renew verifies token as a renewal grant for the stream streamID on the
// session b and, if it is valid, moves the stream's deadline to its
// authz_until. It returns the new deadline. On any error the deadline is
// unchanged.
func (d *Deadlines) Renew(ctx context.Context, b Binding, streamID uint32, token []byte) (time.Time, error) {
	k := deadlineKey{b.SessionID, b.ConnectionEpoch, streamID}
	// The lock is held across verification, which is local (signature,
	// claims and the in-memory jti consumption), so concurrent renewals of
	// one stream are verified one at a time against the current deadline:
	// a renewal that no longer extends it is refused without consuming its
	// jti.
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.streams[k]
	if e == nil {
		return time.Time{}, ErrUntracked
	}
	if d.Keys == nil {
		return time.Time{}, ErrNoKeys
	}
	issuer := d.Issuer
	if issuer == "" {
		issuer = e.open.Issuer
	}
	now := d.clk().Now()
	c, err := grant.VerifyRenewal(ctx, token, d.Keys, grant.RenewalExpectation{
		Target:    e.open.Target,
		ProjectID: e.open.ProjectID,
		Subject:   e.open.Subject,
		StreamID:  streamID,
		OpenJTI:   e.open.JTI,
		Issuer:    issuer,
		After:     e.until,
		NotAfter:  now.Add(e.interval + d.grace()),
		ClockSkew: d.ClockSkew,
	}, d.Replay, now)
	if err != nil {
		return time.Time{}, err
	}
	e.until = c.AuthzUntil
	d.armLocked(k, e)
	return e.until, nil
}

// Deadline returns the current deadline of the stream streamID on the
// session b, and whether it is tracked.
func (d *Deadlines) Deadline(b Binding, streamID uint32) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.streams[deadlineKey{b.SessionID, b.ConnectionEpoch, streamID}]
	if e == nil {
		return time.Time{}, false
	}
	return e.until, true
}

// Len returns how many streams are tracked.
func (d *Deadlines) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.streams)
}
