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
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
)

// The renewal driver for streams whose leaf this hub node holds (design
// v2.21.36 §3.5, Target-side deadlines, Timing; §3.9, Hub-held leaves).
// A stream whose open grant carried authz_until is renewed at about 80% of
// its interval: the node re-checks it from its latest grant
// (renewConduitStream), and sends the renewal grant unchanged as
// AuthRefresh{stream_id, grant} on the stream it holds toward the target.
//
// Each refusal is classified by ClassifyConduitRenewal:
//   - enforce (the target enforces authz_until, and so does this node):
//     a deny closes the stream with 4401 authz_expired, a gone agent with
//     4404; unavailable mints and moves nothing and retries, and a stream
//     never renewed closes at authz_until (expireEnforced). Only a delivered
//     renewal moves the deadline.
//   - off: renewals are still sent, but the driver takes no close action.
//     The stream keeps today's in-process deadline and re-checks, which
//     close it on a deny as before.
//
// Compromise and explicit revocation reach a stream through the deny
// paths (a kid revoked, an inactive principal, a permission removed), never
// through via or route removal.

const (
	// conduitAuthzTriggerRenewal: the renewal driver's attempt.
	conduitAuthzTriggerRenewal = "renewal"
	// conduitAuthzOutcomeNotDelivered: a renewal was minted but could not
	// be sent; nothing moved.
	conduitAuthzOutcomeNotDelivered = "not_delivered"
	// conduitAuthzOutcomeRenewalStopped: with off, a deny or a gone agent
	// stops renewals and takes no close action.
	conduitAuthzOutcomeRenewalStopped = "renewal_stopped"
	// conduitAuthzOutcomeExpiredUnrenewed: with enforce, the stream reached
	// authz_until unrenewed and was closed with 4401 authz_expired.
	conduitAuthzOutcomeExpiredUnrenewed = "expired_unrenewed"

	// conduitRenewalRetryFloor is the shortest wait before retrying a
	// renewal that was unavailable.
	conduitRenewalRetryFloor = 30 * time.Second
	// conduitRenewalLastAttempt is how long before authz_until the last
	// retry comes at the latest.
	conduitRenewalLastAttempt = time.Second
)

// errConduitNoRenewalSender marks a stream with no sender for renewals.
var errConduitNoRenewalSender = errors.New("conduit: no renewal sender for the stream")

// defaultConduitRenewalJitter returns a uniform offset of up to plus or
// minus 5% of interval.
func defaultConduitRenewalJitter(interval time.Duration) time.Duration {
	n := int64(interval / 20)
	if n <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(2*n+1) - n)
}

// isEnforced reports whether st's deadline is its target-side deadline.
func (st *conduitUserStream) isEnforced() bool {
	st.dmu.Lock()
	defer st.dmu.Unlock()
	return st.enforced
}

// trackTargetDeadline starts the renewal driver for a stream whose open
// grant carried authz_until. With enforce its deadline becomes authz_until.
func (a *conduitStreamAuthz) trackTargetDeadline(st *conduitUserStream) {
	if st.AuthzUntil.IsZero() || st.AuthzInterval <= 0 || a.cfg.Renew == nil {
		return
	}
	st.dmu.Lock()
	defer st.dmu.Unlock()
	if st.untracked {
		return
	}
	st.enforced = a.cfg.Enforce != nil && a.cfg.Enforce()
	if st.enforced {
		st.interval = st.AuthzInterval
		st.deadline = st.AuthzUntil
		a.armDeadlineLocked(st)
	}
	a.armRenewalLocked(st, a.renewalPoint(st.AuthzUntil, st.AuthzInterval))
}

// renewalPoint is when a stream whose deadline is until is renewed: 80% of
// its interval, offset by the jitter.
func (a *conduitStreamAuthz) renewalPoint(until time.Time, interval time.Duration) time.Time {
	jitter := a.cfg.Jitter
	if jitter == nil {
		jitter = defaultConduitRenewalJitter
	}
	return until.Add(-interval / 5).Add(jitter(interval))
}

// armRenewalLocked (re)arms st's renewal timer for at. st.dmu is held.
func (a *conduitStreamAuthz) armRenewalLocked(st *conduitUserStream, at time.Time) {
	if st.untracked || st.renewStopped {
		return
	}
	if st.rtimer != nil {
		st.rtimer.Stop()
		st.rtimer = nil
	}
	now := a.cfg.Clock.Now()
	if !st.AuthzUntil.After(now) {
		// A stream at or past its authz_until is not renewed: under
		// enforce it ends at its deadline (authz_until), under off its
		// renewals stop.
		return
	}
	d := at.Sub(now)
	if d < 0 {
		d = 0
	}
	st.rtimer = a.cfg.Clock.AfterFunc(d, func() { a.renewStream(a.runContext(), st) })
}

// renewStream is one renewal attempt for st. It runs under st.mu, like a
// check, so a renewal and a check of the same stream never interleave.
func (a *conduitStreamAuthz) renewStream(ctx context.Context, st *conduitUserStream) {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.dmu.Lock()
	stopped, enforced := st.untracked || st.renewStopped, st.enforced
	st.dmu.Unlock()
	if stopped {
		st.mu.Unlock()
		return
	}
	start := a.cfg.Clock.Now()
	cctx, cancel := context.WithTimeout(ctx, a.cfg.CheckTimeout)
	r, err := a.cfg.Renew(cctx, st)
	cancel()
	now := a.cfg.Clock.Now()

	var outcome, detail string
	switch {
	case err == nil:
		send := st.Renew
		if send == nil {
			err = errConduitNoRenewalSender
		} else {
			err = send(r.Token)
		}
		if err != nil {
			outcome, detail = conduitAuthzOutcomeNotDelivered, err.Error()
			a.retryRenewal(st, now)
			break
		}
		outcome = conduitAuthzOutcomeRenewed
		st.Grant, st.AuthzUntil = r.Token, r.AuthzUntil
		st.dmu.Lock()
		if enforced && r.AuthzUntil.After(st.deadline) {
			st.deadline = r.AuthzUntil
			a.armDeadlineLocked(st)
		}
		next := a.renewalPoint(st.AuthzUntil, st.AuthzInterval)
		if next.After(now) {
			a.armRenewalLocked(st, next)
		}
		st.dmu.Unlock()
		if !next.After(now) {
			// The renewed authz_until leaves no renewal point ahead of
			// now. Arming at it would renew again at once, and again,
			// without end; the next attempt waits like a retry instead
			// (at least conduitRenewalRetryFloor, never past authz_until).
			a.retryRenewal(st, now)
		}
	default:
		detail = err.Error()
		switch ClassifyConduitRenewal(err) {
		case ConduitRenewalDeny:
			if enforced {
				outcome = conduitAuthzOutcomeClosed
				st.closed = true
				st.Close(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
			} else {
				outcome = conduitAuthzOutcomeRenewalStopped
				a.stopRenewal(st)
			}
		case ConduitRenewalTargetGone:
			if enforced {
				outcome = conduitAuthzOutcomeTargetGone
				st.closed = true
				st.Close(relay.CloseTargetNotFound, relay.ReasonTargetNotFound)
			} else {
				outcome = conduitAuthzOutcomeRenewalStopped
				a.stopRenewal(st)
			}
		default:
			// Nothing is minted or moved; the stream keeps its deadline.
			outcome = conduitAuthzOutcomeDeferred
			a.retryRenewal(st, now)
		}
	}
	if st.closed {
		st.stopDeadline()
		a.mu.Lock()
		delete(a.streams, st)
		a.mu.Unlock()
	}
	// authz_until is written under st.mu (a renewal), so it is read for the
	// log before the lock is released.
	until := st.AuthzUntil
	st.mu.Unlock()
	a.logRenewal(ctx, st, outcome, detail, start, now, until, enforced)
}

// retryRenewal schedules the next attempt after an unavailable or
// undelivered renewal: half the time left to authz_until, never sooner
// than conduitRenewalRetryFloor, and strictly before authz_until: at the
// latest conduitRenewalLastAttempt before it, and none once that moment
// has passed. st.mu is held.
func (a *conduitStreamAuthz) retryRenewal(st *conduitUserStream, now time.Time) {
	delay := st.AuthzUntil.Sub(now) / 2
	if delay < conduitRenewalRetryFloor {
		delay = conduitRenewalRetryFloor
	}
	at := now.Add(delay)
	if last := st.AuthzUntil.Add(-conduitRenewalLastAttempt); !at.Before(last) {
		// No attempt inside the final conduitRenewalLastAttempt: the last
		// one comes that long before authz_until, so it never races the
		// close at the deadline; none when that moment has passed.
		if !last.After(now) {
			return
		}
		at = last
	}
	st.dmu.Lock()
	a.armRenewalLocked(st, at)
	st.dmu.Unlock()
}

// stopRenewal ends the renewal driver for st. st.mu is held.
func (a *conduitStreamAuthz) stopRenewal(st *conduitUserStream) {
	st.dmu.Lock()
	defer st.dmu.Unlock()
	st.renewStopped = true
	if st.rtimer != nil {
		st.rtimer.Stop()
		st.rtimer = nil
	}
}

// expireEnforced closes an enforced stream that reached its deadline
// unrenewed with 4401 authz_expired. st.mu is held, and released here.
func (a *conduitStreamAuthz) expireEnforced(ctx context.Context, st *conduitUserStream) {
	now := a.cfg.Clock.Now()
	st.closed = true
	st.Close(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	st.stopDeadline()
	a.mu.Lock()
	delete(a.streams, st)
	a.mu.Unlock()
	st.mu.Unlock()
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.RecordConduitStreamAuthz(conduitAuthzTriggerInterval, conduitAuthzOutcomeExpiredUnrenewed, st.Kind)
	}
	a.cfg.Logger.Log(ctx, slog.LevelWarn, "conduit_stream_authz",
		"trigger", conduitAuthzTriggerInterval,
		"outcome", conduitAuthzOutcomeExpiredUnrenewed,
		"kind", st.Kind,
		"stream_id", st.StreamID,
		"session_id", st.SessionID,
		"agent_id", st.AgentID,
		"project_id", st.ProjectID,
		"principal_id", st.UserID,
		"check_start", now,
		"check_end", now,
	)
}

// logRenewal records one renewal attempt in the log and metric. until is
// st's authz_until, read by the caller under st.mu.
func (a *conduitStreamAuthz) logRenewal(ctx context.Context, st *conduitUserStream, outcome, detail string, start, end, until time.Time, enforced bool) {
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.RecordConduitStreamAuthz(conduitAuthzTriggerRenewal, outcome, st.Kind)
	}
	level := slog.LevelInfo
	switch outcome {
	case conduitAuthzOutcomeRenewed:
		level = slog.LevelDebug
	case conduitAuthzOutcomeDeferred, conduitAuthzOutcomeNotDelivered, conduitAuthzOutcomeRenewalStopped:
		level = slog.LevelWarn
	}
	a.cfg.Logger.Log(ctx, level, "conduit_stream_authz",
		"trigger", conduitAuthzTriggerRenewal,
		"outcome", outcome,
		"enforce", enforced,
		"kind", st.Kind,
		"stream_id", st.StreamID,
		"session_id", st.SessionID,
		"agent_id", st.AgentID,
		"project_id", st.ProjectID,
		"principal_id", st.UserID,
		"authz_until", until,
		"check_start", start,
		"check_end", end,
		"detail", detail,
	)
}

// renewConduitUserStream is the tracker's Renew: the stream's renewal
// re-check from its latest grant (renewConduitStream), presented with the
// target-side ids and the principal and kind the node holds for it.
func (s *Server) renewConduitUserStream(ctx context.Context, st *conduitUserStream) (*conduitRenewal, error) {
	sessionID, streamID := st.SessionID, st.StreamID
	if st.TargetSessionID != "" {
		sessionID, streamID = st.TargetSessionID, st.TargetStreamID
	}
	return s.renewConduitStream(ctx, conduitRenewalRequest{
		LatestGrant:     st.Grant,
		TargetSessionID: sessionID,
		TargetStreamID:  streamID,
		OpenJTI:         st.OpenJTI,
		PrincipalID:     st.UserID,
		Kind:            st.Kind,
	})
}

// conduitStreamRenewal returns the sender of AuthRefresh{stream_id, grant}
// for st, opened on res. A stream with a target-side deadline gets its own
// renewer (conduit.StreamRenewer: on this node's session, or across the
// internal hop to the owner relay), so renewals reach the target wherever
// its session is held. Any other stream keeps the advisory notice on a
// session this node holds, and none otherwise.
func conduitStreamRenewal(res router.Resolved, st conduit.Stream, targetDeadline bool) func(grant []byte) error {
	if targetDeadline {
		if r, ok := st.(conduit.StreamRenewer); ok {
			return r.Renew
		}
		return nil
	}
	ls, ok := res.Session.(conduit.LocalSession)
	if !res.Local || !ok {
		return nil
	}
	id := st.ID()
	return func(grant []byte) error { return ls.RefreshAuth(grant, id) }
}

// applyConduitOpenGrant records on st the open grant tok with claims c when
// it carried a target-side deadline, which starts the renewal driver when
// st is tracked. A grant without authz_until leaves st as it is.
func applyConduitOpenGrant(st *conduitUserStream, tok []byte, c *grant.Claims) {
	if c == nil || c.AuthzUntil.IsZero() {
		return
	}
	st.Grant, st.OpenJTI, st.AuthzUntil, st.AuthzInterval = tok, c.JTI, c.AuthzUntil, c.AuthzInterval
}
