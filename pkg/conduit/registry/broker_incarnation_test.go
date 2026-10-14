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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestBrokerIncarnationReferenceIsNewestSession: the reference is the
// incarnation of the broker's live session with the highest epoch; dead
// sessions do not count, and a newest session without an incarnation
// leaves no reference at all.
func TestBrokerIncarnationReferenceIsNewestSession(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	live := &RelayInstance{InstanceID: "r1", Generation: 1, LastSeen: now}
	live2 := &RelayInstance{InstanceID: "r2", Generation: 1, LastSeen: now}
	stale := &RelayInstance{InstanceID: "r3", Generation: 1, LastSeen: now.Add(-time.Hour)}
	view := func(id string, relay *RelayInstance, epoch int64, inc string, draining bool) SessionView {
		return SessionView{Relay: relay, Session: SessionRecord{
			SessionID: id, PrincipalKind: PrincipalBroker, PrincipalID: "b1",
			RelayInstanceID: relay.InstanceID, RelayGeneration: 1,
			EndpointIncarnation: inc, ConnectionEpoch: epoch, LastSeen: now, Draining: draining,
		}}
	}
	cases := []struct {
		name     string
		sessions []SessionView
		want     string
		wantOK   bool
	}{
		{name: "no session"},
		{name: "one session", sessions: []SessionView{view("s1", live, 1, "inc-1", false)}, want: "inc-1", wantOK: true},
		{name: "newest epoch wins across relays", sessions: []SessionView{
			view("s1", live, 4, "inc-1", false), view("s2", live2, 7, "inc-2", false), view("s3", live, 5, "inc-1", false),
		}, want: "inc-2", wantOK: true},
		{name: "a dead relay's newer session does not count", sessions: []SessionView{
			view("s1", live, 4, "inc-1", false), view("s2", stale, 9, "inc-2", false),
		}, want: "inc-1", wantOK: true},
		{name: "a draining newest session still counts", sessions: []SessionView{
			view("s1", live, 4, "inc-1", false), view("s2", live2, 5, "inc-2", true),
		}, want: "inc-2", wantOK: true},
		{name: "newest without an incarnation leaves no reference", sessions: []SessionView{
			view("s1", live, 4, "inc-1", false), view("s2", live2, 5, "", false),
		}},
		{name: "only dead sessions", sessions: []SessionView{view("s1", stale, 2, "inc-1", false)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := PrincipalSessions{PrincipalKind: PrincipalBroker, PrincipalID: "b1", CurrentEpoch: 9, Sessions: tc.sessions}
			r := New(viewStore{ps: ps}, Config{Clock: ClockFunc(func() time.Time { return now })})
			got, ok, err := r.BrokerIncarnation(context.Background(), "b1", now)
			assert.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
	_, _, err := New(viewStore{}, Config{}).BrokerIncarnation(context.Background(), "", now)
	assert.ErrorIs(t, err, ErrInvalidInput)
}
