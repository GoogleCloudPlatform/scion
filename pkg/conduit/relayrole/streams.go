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

package relayrole

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// Active authorization of relay-held user streams (design §3.9 "Active
// authorization"). The relay holding the user's session schedules each
// stream's re-checks: at about 80% of its authorization interval, on every
// sweep, on every revocation event and when the event connection is
// (re)established. The hub evaluates each re-check and returns a renewal
// grant or a refusal. A renewal moves the stream's deadline and is sent to
// the target unchanged as AuthRefresh{target-side stream id, grant}; a
// refusal closes the stream with 4401. When the hub cannot be reached
// nothing is renewed, and the stream closes at its deadline.

// RenewalLead is the part of a stream's authorization interval at which
// its renewal is requested (about 80%).
const RenewalLead = 0.8

// Renewer delivers a renewal grant to a stream's target: AuthRefresh{stream
// id, grant} on the target-side stream, the grant passed unchanged.
type Renewer interface {
	Renew(grant []byte) error
}

// ErrRenewalNotForwarded is returned by a Renewer that cannot deliver a
// renewal to a target on another relay yet. The stream then fails closed:
// its target ends it at its own deadline.
var ErrRenewalNotForwarded = errors.New("conduit relay: renewal forwarding across relays is not available")

// LocalRenewer delivers renewals on a target session this relay holds.
func LocalRenewer(target conduit.LocalSession, targetStreamID uint32) Renewer {
	return localRenewer{ls: target, id: targetStreamID}
}

type localRenewer struct {
	ls conduit.LocalSession
	id uint32
}

func (l localRenewer) Renew(grant []byte) error { return l.ls.RefreshAuth(grant, l.id) }

// HopRenewer delivers renewals to a target on another relay, across the
// relay-to-relay bridge. Forwarding over the bridge is not available yet,
// so every renewal fails closed (ErrRenewalNotForwarded).
func HopRenewer() Renewer { return hopRenewer{} }

type hopRenewer struct{}

func (hopRenewer) Renew([]byte) error { return ErrRenewalNotForwarded }

// RenewerFor returns the renewer of a stream opened on sess: local when this
// relay holds the target's session, the hop renewer otherwise.
func RenewerFor(sess conduit.Session, target relay.TargetStream) Renewer {
	if ls, ok := sess.(conduit.LocalSession); ok {
		return LocalRenewer(ls, target.StreamID)
	}
	return HopRenewer()
}

// TrackedStream is one relay-held user stream.
type TrackedStream struct {
	// UserSessionID is the user session holding the stream's leaf.
	UserSessionID string
	// Target is the stream on the target's own session.
	Target relay.TargetStream
	// OpenJTI is the jti of the stream's open grant.
	OpenJTI string
	// LatestGrant is the stream's latest hub-signed grant (the open
	// grant, then each renewal), stored and passed back unparsed.
	LatestGrant []byte
	// AuthzUntil is the stream's current authorization deadline, and
	// Interval its authorization interval.
	AuthzUntil time.Time
	Interval   time.Duration
	// Renewer delivers renewals to the target.
	Renewer Renewer
	// Close ends the stream on both legs (called at most once).
	Close func(code uint32, reason string)
}

// trackedStream is a TrackedStream with its timers.
type trackedStream struct {
	TrackedStream
	mu          sync.Mutex
	closed      bool
	renewAt     clock.Timer
	deadline    clock.Timer
	renewedFrom time.Time
}

// StreamScheduler schedules the re-checks of relay-held user streams.
type StreamScheduler struct {
	ro      *Role
	mu      sync.Mutex
	streams map[*trackedStream]struct{}
}

func newStreamScheduler(ro *Role) *StreamScheduler {
	return &StreamScheduler{ro: ro, streams: map[*trackedStream]struct{}{}}
}

// Track schedules ts until the returned function is called (when the stream
// ends for any reason).
func (s *StreamScheduler) Track(ts TrackedStream) (untrack func()) {
	st := &trackedStream{TrackedStream: ts, renewedFrom: s.ro.clk.Now()}
	s.mu.Lock()
	s.streams[st] = struct{}{}
	s.mu.Unlock()
	st.mu.Lock()
	s.armLocked(st)
	st.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			st.mu.Lock()
			st.closed = true
			stopTimers(st)
			st.mu.Unlock()
			s.mu.Lock()
			delete(s.streams, st)
			s.mu.Unlock()
		})
	}
}

// Len reports the number of tracked streams.
func (s *StreamScheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func stopTimers(st *trackedStream) {
	if st.renewAt != nil {
		st.renewAt.Stop()
	}
	if st.deadline != nil {
		st.deadline.Stop()
	}
}

// armLocked (re)arms st's renewal and deadline timers. st.mu is held.
func (s *StreamScheduler) armLocked(st *trackedStream) {
	stopTimers(st)
	now := s.ro.clk.Now()
	if st.Interval > 0 {
		at := st.renewedFrom.Add(time.Duration(float64(st.Interval) * RenewalLead))
		st.renewAt = s.ro.clk.AfterFunc(at.Sub(now), func() { s.recheck(context.Background(), TriggerInterval, []*trackedStream{st}) })
	}
	if !st.AuthzUntil.IsZero() {
		st.deadline = s.ro.clk.AfterFunc(st.AuthzUntil.Sub(now), func() { s.expire(st) })
	}
}

// expire closes a stream that reached its deadline unrenewed.
func (s *StreamScheduler) expire(st *trackedStream) {
	st.mu.Lock()
	if st.closed || s.ro.clk.Now().Before(st.AuthzUntil) {
		st.mu.Unlock()
		return
	}
	st.closed = true
	stopTimers(st)
	st.mu.Unlock()
	s.ro.log.Info("Conduit relay: stream reached its authorization deadline", "session_id", st.UserSessionID)
	st.Close(conduit.CloseUnauthenticated, "authz_expired")
}

// RecheckAll re-checks every tracked stream.
func (s *StreamScheduler) RecheckAll(ctx context.Context, trigger string) {
	s.mu.Lock()
	all := make([]*trackedStream, 0, len(s.streams))
	for st := range s.streams {
		all = append(all, st)
	}
	s.mu.Unlock()
	for start := 0; start < len(all); start += relay.MaxHubRecheckBatch {
		s.recheck(ctx, trigger, all[start:min(start+relay.MaxHubRecheckBatch, len(all))])
	}
}

// forgetSession stops tracking the streams of a user session that ended.
func (s *StreamScheduler) forgetSession(sessionID string) {
	s.mu.Lock()
	var gone []*trackedStream
	for st := range s.streams {
		if st.UserSessionID == sessionID {
			gone = append(gone, st)
			delete(s.streams, st)
		}
	}
	s.mu.Unlock()
	for _, st := range gone {
		st.mu.Lock()
		st.closed = true
		stopTimers(st)
		st.mu.Unlock()
	}
}

// recheck asks the hub to re-check batch and acts on each answer.
func (s *StreamScheduler) recheck(ctx context.Context, trigger string, batch []*trackedStream) {
	items := make([]relay.HubStreamRecheckItem, len(batch))
	for i, st := range batch {
		st.mu.Lock()
		items[i] = relay.HubStreamRecheckItem{
			UserSessionID:   st.UserSessionID,
			AdmissionToken:  s.ro.token(st.UserSessionID),
			TargetSessionID: st.Target.SessionID,
			TargetStreamID:  st.Target.StreamID,
			OpenJTI:         st.OpenJTI,
			LatestGrant:     st.LatestGrant,
			Trigger:         trigger,
		}
		st.mu.Unlock()
	}
	var resp relay.HubStreamRecheckResponse
	if err := s.ro.cfg.Hub.Call(ctx, relay.HubStreamRecheckPath, relay.HubStreamRecheckRequest{Items: items}, &resp); err != nil {
		// Nothing is renewed; each stream ends at its deadline unless a
		// later re-check renews it.
		s.ro.log.Warn("Conduit relay: stream re-check unavailable; nothing renewed", "trigger", trigger, "streams", len(batch), "error", err)
		return
	}
	for i, res := range resp.Results {
		if i >= len(batch) {
			break
		}
		st := batch[i]
		switch {
		case res.Close:
			st.mu.Lock()
			if st.closed {
				st.mu.Unlock()
				continue
			}
			st.closed = true
			stopTimers(st)
			st.mu.Unlock()
			st.Close(res.CloseCode, "authz_expired")
		case res.Renew:
			st.mu.Lock()
			if st.closed {
				st.mu.Unlock()
				continue
			}
			st.LatestGrant = res.RenewalGrant
			if res.AuthzUntil.After(st.AuthzUntil) {
				st.AuthzUntil = res.AuthzUntil
			}
			st.renewedFrom = s.ro.clk.Now()
			s.armLocked(st)
			renewer := st.Renewer
			st.mu.Unlock()
			if renewer != nil {
				if err := renewer.Renew(res.RenewalGrant); err != nil {
					s.ro.log.Warn("Conduit relay: renewal not delivered to the target; it ends the stream at its own deadline", "session_id", st.UserSessionID, "error", err)
				}
			}
		}
	}
}
