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

package registry

import (
	"context"
	"fmt"
	"time"
)

// BrokerIncarnation returns broker brokerID's reference incarnation
// (design §3.4, broker incarnation reference): the endpoint_incarnation of its live session
// with the highest connection_epoch, which is the most recently admitted
// one. Liveness is the same rule Eligible applies (relay live and of the
// session's generation, session not stale); a draining session still
// counts, because it was admitted. ok is false when the broker has no live
// session, or when its newest live session carries no incarnation: no
// session is then resolvable for the broker.
func (r *Registry) BrokerIncarnation(ctx context.Context, brokerID string, now time.Time) (inc string, ok bool, err error) {
	ref, ok, err := r.BrokerReference(ctx, brokerID, now)
	return ref.Incarnation, ok, err
}

// BrokerReference is the session that defines a broker's reference
// incarnation: its incarnation and connection epoch.
type BrokerReference struct {
	Incarnation     string
	ConnectionEpoch int64
}

// BrokerReference returns the reference session of broker brokerID, by
// the rule BrokerIncarnation documents, with its connection epoch. ok is
// false exactly when BrokerIncarnation's is.
func (r *Registry) BrokerReference(ctx context.Context, brokerID string, now time.Time) (BrokerReference, bool, error) {
	if brokerID == "" {
		return BrokerReference{}, false, fmt.Errorf("%w: empty broker id", ErrInvalidInput)
	}
	ps, err := r.store.ListPrincipalSessions(ctx, PrincipalBroker, brokerID)
	if err != nil {
		return BrokerReference{}, false, err
	}
	ref, ok := r.brokerReference(ps, now)
	return ref, ok, nil
}

// BrokerSessionSuperseded reports whether session sessionID of broker
// brokerID belongs to an older incarnation (design §3.4): the broker has a
// reference session, and the session is listed with another incarnation
// and a lower connection epoch. A session that is not listed, or a broker
// without a reference, is not superseded.
func (r *Registry) BrokerSessionSuperseded(ctx context.Context, brokerID, sessionID string, now time.Time) (bool, error) {
	if brokerID == "" || sessionID == "" {
		return false, fmt.Errorf("%w: empty broker or session id", ErrInvalidInput)
	}
	ps, err := r.store.ListPrincipalSessions(ctx, PrincipalBroker, brokerID)
	if err != nil {
		return false, err
	}
	ref, ok := r.brokerReference(ps, now)
	if !ok {
		return false, nil
	}
	for _, v := range ps.Sessions {
		if s := v.Session; s.SessionID == sessionID {
			return s.EndpointIncarnation != ref.Incarnation && s.ConnectionEpoch < ref.ConnectionEpoch, nil
		}
	}
	return false, nil
}

// brokerReference picks the reference session from a broker's rows.
func (r *Registry) brokerReference(ps PrincipalSessions, now time.Time) (BrokerReference, bool) {
	now = now.UTC()
	newest := -1 // index in ps.Sessions
	for i := range ps.Sessions {
		v := ps.Sessions[i]
		if v.Session.PrincipalKind != PrincipalBroker || r.liveness(v, now) != ReasonOK {
			continue
		}
		if newest < 0 || v.Session.ConnectionEpoch > ps.Sessions[newest].Session.ConnectionEpoch {
			newest = i
		}
	}
	if newest < 0 || ps.Sessions[newest].Session.EndpointIncarnation == "" {
		return BrokerReference{}, false
	}
	s := ps.Sessions[newest].Session
	return BrokerReference{Incarnation: s.EndpointIncarnation, ConnectionEpoch: s.ConnectionEpoch}, true
}
