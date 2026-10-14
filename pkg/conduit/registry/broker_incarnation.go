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
// (design §3.4, v2.21.32): the endpoint_incarnation of its live session
// with the highest connection_epoch, which is the most recently admitted
// one. Liveness is the same rule Eligible applies (relay live and of the
// session's generation, session not stale); a draining session still
// counts, because it was admitted. ok is false when the broker has no live
// session, or when its newest live session carries no incarnation: no
// session is then resolvable for the broker.
func (r *Registry) BrokerIncarnation(ctx context.Context, brokerID string, now time.Time) (inc string, ok bool, err error) {
	if brokerID == "" {
		return "", false, fmt.Errorf("%w: empty broker id", ErrInvalidInput)
	}
	ps, err := r.store.ListPrincipalSessions(ctx, PrincipalBroker, brokerID)
	if err != nil {
		return "", false, err
	}
	now = now.UTC()
	var newest *SessionRecord
	for i := range ps.Sessions {
		v := ps.Sessions[i]
		if v.Session.PrincipalKind != PrincipalBroker || r.liveness(v, now) != ReasonOK {
			continue
		}
		if newest == nil || v.Session.ConnectionEpoch > newest.ConnectionEpoch {
			s := v.Session
			newest = &s
		}
	}
	if newest == nil || newest.EndpointIncarnation == "" {
		return "", false, nil
	}
	return newest.EndpointIncarnation, true, nil
}
