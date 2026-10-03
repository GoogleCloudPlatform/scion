// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func sess(kind, id, relay string, epoch int64) SessionView {
	return SessionView{Session: SessionRecord{SessionID: id, PrincipalKind: kind, RelayInstanceID: relay, ConnectionEpoch: epoch}}
}

func TestEpochCurrent_ByPrincipalKind(t *testing.T) {
	agentPS := PrincipalSessions{CurrentEpoch: 3, Sessions: []SessionView{
		sess(PrincipalAgent, "a1", "r1", 2), sess(PrincipalAgent, "a2", "r2", 3),
	}}
	brokerPS := PrincipalSessions{CurrentEpoch: 9, Sessions: []SessionView{
		sess(PrincipalBroker, "b1", "r1", 4), sess(PrincipalBroker, "b2", "r1", 7), sess(PrincipalBroker, "b3", "r2", 5),
	}}
	userPS := PrincipalSessions{CurrentEpoch: 2, Sessions: []SessionView{
		sess(PrincipalUser, "u1", "r1", 1), sess(PrincipalUser, "u2", "r1", 2),
	}}
	cases := []struct {
		name string
		ps   PrincipalSessions
		s    SessionView
		want bool
	}{
		{"agent newest is current", agentPS, agentPS.Sessions[1], true},
		{"agent older is fenced even on another relay", agentPS, agentPS.Sessions[0], false},
		{"agent with no counter row is never current", PrincipalSessions{}, sess(PrincipalAgent, "x", "r1", 0), false},
		{"broker superseded on the same relay", brokerPS, brokerPS.Sessions[0], false},
		{"broker newest on its relay", brokerPS, brokerPS.Sessions[1], true},
		{"broker older epoch but alone on its relay", brokerPS, brokerPS.Sessions[2], true},
		{"user older epoch still current", userPS, userPS.Sessions[0], true},
		{"user newer", userPS, userPS.Sessions[1], true},
		{"relay-peer never current", PrincipalSessions{CurrentEpoch: 1}, sess(PrincipalRelayPeer, "p", "r1", 1), false},
		{"unknown kind never current", PrincipalSessions{CurrentEpoch: 1}, sess("robot", "p", "r1", 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, EpochCurrent(tc.ps, tc.s.Session))
		})
	}
}

func TestInsertSession_Validation(t *testing.T) {
	ok := SessionRecord{SessionID: "s", PrincipalKind: PrincipalAgent, PrincipalID: "a", RelayInstanceID: "r", RelayGeneration: 1, Transport: TransportWS, EndpointIncarnation: "i"}
	mut := func(f func(*SessionRecord)) SessionRecord { r := ok; f(&r); return r }
	cases := map[string]SessionRecord{
		"relay-peer":           mut(func(r *SessionRecord) { r.PrincipalKind = PrincipalRelayPeer }),
		"unknown kind":         mut(func(r *SessionRecord) { r.PrincipalKind = "robot" }),
		"unknown transport":    mut(func(r *SessionRecord) { r.Transport = "tcp" }),
		"empty session":        mut(func(r *SessionRecord) { r.SessionID = "" }),
		"empty principal":      mut(func(r *SessionRecord) { r.PrincipalID = "" }),
		"empty relay":          mut(func(r *SessionRecord) { r.RelayInstanceID = "" }),
		"zero generation":      mut(func(r *SessionRecord) { r.RelayGeneration = 0 }),
		"agent no incarnation": mut(func(r *SessionRecord) { r.EndpointIncarnation = "" }),
	}
	reg := New(&FaultStore{}, Config{}) // Inner is nil: validation must reject before the store is touched
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := reg.InsertSessionWithNextEpoch(context.Background(), rec)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
	assert.NoError(t, validateSession(mut(func(r *SessionRecord) {
		r.PrincipalKind, r.EndpointIncarnation = PrincipalUser, ""
	})), "user sessions need no incarnation")
}

func TestNew_Defaults(t *testing.T) {
	r := New(nil, Config{})
	assert.Equal(t, DefaultRelayStaleAfter, r.cfg.RelayStaleAfter)
	assert.Equal(t, DefaultSessionStaleAfter, r.cfg.SessionStaleAfter)
	assert.WithinDuration(t, time.Now(), r.now(), time.Minute)
}

// viewStore serves one fixed principal snapshot; every other Store method
// panics via the nil embedded interface (they are not used here).
type viewStore struct {
	Store
	ps PrincipalSessions
}

func (v viewStore) ListPrincipalSessions(context.Context, string, string) (PrincipalSessions, error) {
	return v.ps, nil
}

func (v viewStore) ListPrincipalSessionsBySession(context.Context, string) (PrincipalSessions, bool, error) {
	return v.ps, true, nil
}

func TestAdmission_RelayMissingFailsClosed(t *testing.T) {
	// The FK makes a session without its relay row unrepresentable in the
	// real store, so the defensive branch is exercised through a stub.
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	rec := SessionRecord{
		SessionID: "s-1", PrincipalKind: PrincipalAgent, PrincipalID: "agent-1", ProjectID: "proj-1",
		RelayInstanceID: "relay-1", RelayGeneration: 1, EndpointIncarnation: "inc-A",
		ConnectionEpoch: 1, LastSeen: now,
	}
	ps := PrincipalSessions{PrincipalKind: PrincipalAgent, PrincipalID: "agent-1", CurrentEpoch: 1,
		Sessions: []SessionView{{Session: rec, Relay: nil}}}
	r := New(viewStore{ps: ps}, Config{Clock: ClockFunc(func() time.Time { return now })})
	w := Want{ProjectID: "proj-1", Incarnation: "inc-A"}

	d, err := r.Admission(context.Background(), "s-1", w)
	assert.NoError(t, err)
	assert.False(t, d.Admissible)
	assert.Equal(t, ReasonRelayMissing, d.Reason)
	recs, err := r.Eligible(context.Background(), PrincipalAgent, "agent-1", w, now)
	assert.NoError(t, err)
	assert.Empty(t, recs)
}
