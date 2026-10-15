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

package relay

import (
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
)

// Broker incarnation rules at admission (design §3.4, broker incarnation
// reference). A newly admitted broker session has the highest
// connection_epoch of its broker, so its incarnation is the broker's
// reference: every local session of the broker with another incarnation
// and a lower epoch is closed with 4409 superseded_incarnation. Sessions
// on other relays close on their session re-check. A return to an
// incarnation this relay admitted for the broker within
// BrokerIncarnationFlapWindow is reported as broker_incarnation_flap. It
// is diagnostic only: nothing is suppressed.

// BrokerIncarnationFlapWindow is how long a relay remembers the
// incarnations it admitted for a broker.
const BrokerIncarnationFlapWindow = 10 * time.Minute

// supersededIncarnationDetail is the detail of the 4409 close of a
// broker session whose incarnation a newer one replaced.
const supersededIncarnationDetail = "a newer broker incarnation was admitted"

// SupersededIncarnationReason is the close reason of a broker session
// closed because a newer incarnation of its broker was admitted.
func SupersededIncarnationReason() string {
	return reason(ReasonSupersededIncarnation, supersededIncarnationDetail)
}

// brokerIncarnations is one relay's memory of the incarnations it
// admitted per broker, for the flap report. The zero value is ready.
type brokerIncarnations struct {
	mu sync.Mutex
	// last is the incarnation of each broker's latest admission here.
	last map[string]string
	// seen is, per broker, when each incarnation was last admitted here.
	seen map[string]map[string]time.Time
}

// admit records that broker id was admitted with incarnation inc at now.
// It reports a flap: inc differs from the broker's previous admission here
// and was admitted here within BrokerIncarnationFlapWindow. Entries older
// than the window are forgotten.
func (b *brokerIncarnations) admit(id, inc string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = map[string]map[string]time.Time{}
		b.last = map[string]string{}
	}
	for bid, incs := range b.seen {
		for i, at := range incs {
			if now.Sub(at) > BrokerIncarnationFlapWindow {
				delete(incs, i)
			}
		}
		if len(incs) == 0 {
			delete(b.seen, bid)
			delete(b.last, bid)
		}
	}
	incs := b.seen[id]
	_, recent := incs[inc]
	flap := recent && b.last[id] != inc
	if incs == nil {
		incs = map[string]time.Time{}
		b.seen[id] = incs
	}
	incs[inc] = now
	b.last[id] = inc
	return flap
}

// CloseOlderIncarnationSessions closes, with code and reason, every open
// local session of the principal whose endpoint incarnation is not inc and
// whose connection epoch is below epoch. It returns the number it closed;
// a session is closed and counted by at most one call. report, when set,
// is called with that number (when it is not zero) before any session is
// closed, so whoever observes a close also observes its count. Rows are
// deleted as each session ends.
func (r *Relay) CloseOlderIncarnationSessions(kind, id, inc string, epoch int64, code uint32, reason string, report func(closed int)) int {
	r.mu.Lock()
	var hit []*entry
	for _, e := range r.sessions {
		if e.rec.PrincipalKind != kind || e.rec.PrincipalID != id ||
			e.rec.EndpointIncarnation == inc || e.rec.ConnectionEpoch >= epoch {
			continue
		}
		select {
		case <-e.sess.Done():
			continue
		default:
		}
		if e.supersededClosed.CompareAndSwap(false, true) {
			hit = append(hit, e)
		}
	}
	hook := r.testHookBeforeIncarnationClose
	r.mu.Unlock()
	if len(hit) > 0 && report != nil {
		report(len(hit))
	}
	if hook != nil {
		hook()
	}
	for _, e := range hit {
		_ = e.sess.CloseWithCode(code, reason)
	}
	return len(hit)
}

// admittedBroker applies the broker incarnation rules to e, a newly
// registered session that passed its post-registration check.
func (r *Relay) admittedBroker(e *entry) {
	rec := e.rec
	if rec.PrincipalKind != registry.PrincipalBroker || rec.EndpointIncarnation == "" {
		return
	}
	r.mu.Lock()
	h := r.testHookAfterBrokerAdmission
	r.mu.Unlock()
	if h != nil {
		defer h(rec.SessionID)
	}
	select {
	case <-e.sess.Done():
		return
	default:
	}
	if r.brokerIncs.admit(rec.PrincipalID, rec.EndpointIncarnation, r.clk.Now()) {
		r.log.Warn("broker_incarnation_flap: a broker returned to an incarnation admitted within the last 10 minutes; two processes may be running with one broker credential",
			"broker_id", rec.PrincipalID, "incarnation", rec.EndpointIncarnation, "session_id", rec.SessionID)
		if f := r.cfg.OnBrokerIncarnationFlap; f != nil {
			f(rec.PrincipalID, rec.EndpointIncarnation)
		}
	}
	r.CloseOlderIncarnationSessions(registry.PrincipalBroker, rec.PrincipalID, rec.EndpointIncarnation, rec.ConnectionEpoch,
		CloseSupersededIncarnation, SupersededIncarnationReason(), func(n int) {
			r.log.Info("Conduit: closing broker sessions of an older incarnation (4409 superseded_incarnation)",
				"broker_id", rec.PrincipalID, "incarnation", rec.EndpointIncarnation, "closed", n)
			if f := r.cfg.OnBrokerSessionsSuperseded; f != nil {
				f(rec.PrincipalID, n)
			}
		})
}
