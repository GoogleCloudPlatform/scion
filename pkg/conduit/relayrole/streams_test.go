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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// streamRecorder records what the scheduler did to one stream.
type streamRecorder struct {
	mu      sync.Mutex
	renewed [][]byte
	closed  []uint32
}

func (r *streamRecorder) Renew(grant []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renewed = append(r.renewed, append([]byte(nil), grant...))
	return nil
}

func (r *streamRecorder) close(code uint32, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = append(r.closed, code)
}

func (r *streamRecorder) closes() []uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint32(nil), r.closed...)
}

func newStreamRole(t *testing.T, hub *fakeHub, clk *clock.Fake, events Events) *Role {
	t.Helper()
	ro, err := New(Config{Hub: hub.client(), Clock: clk, RecheckInterval: time.Minute, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

func trackStream(ro *Role, rec *streamRecorder) func() {
	return ro.Streams.Track(TrackedStream{
		UserSessionID: "user-s1",
		Target:        relay.TargetStream{SessionID: "agent-s1", StreamID: 7},
		OpenJTI:       "jti-1",
		LatestGrant:   []byte("open-grant"),
		AuthzUntil:    t0.Add(10 * time.Minute),
		Interval:      10 * time.Minute,
		Renewer:       rec,
		Close:         rec.close,
	})
}

func renewAnswer(grant string, until time.Time) func(string, []byte) hubAnswer {
	return func(path string, body []byte) hubAnswer {
		if path != "streams/recheck" {
			return hubAnswer{status: http.StatusOK, body: relay.HubSessionRecheckResponse{}}
		}
		var req relay.HubStreamRecheckRequest
		_ = json.Unmarshal(body, &req)
		res := make([]relay.HubStreamRecheckResult, len(req.Items))
		for i := range res {
			res[i] = relay.HubStreamRecheckResult{Renew: true, RenewalGrant: []byte(grant), AuthzUntil: until}
		}
		return hubAnswer{status: http.StatusOK, body: relay.HubStreamRecheckResponse{Results: res}}
	}
}

// TestStreamRenewalMovesDeadline: at about 80% of the interval the relay
// re-checks the stream with its latest grant; a renewal moves the deadline
// (the stream outlives the old one) and the next re-check presents the
// renewal as the latest grant.
func TestStreamRenewalMovesDeadline(t *testing.T) {
	hub := newFakeHub(t)
	clk := clock.NewFake(t0)
	ro := newStreamRole(t, hub, clk, nil)
	rec := &streamRecorder{}
	hub.setAnswer(renewAnswer("renewal-1", t0.Add(20*time.Minute)))
	defer trackStream(ro, rec)()

	clk.Advance(7 * time.Minute)
	if n := hub.count("streams/recheck"); n != 0 {
		t.Fatalf("re-checked before 80%% of the interval (%d calls)", n)
	}
	clk.Advance(time.Minute) // 8m: 80%
	if n := hub.count("streams/recheck"); n != 1 {
		t.Fatalf("re-checks at 80%%: %d, want 1", n)
	}
	var req relay.HubStreamRecheckRequest
	hub.last("streams/recheck", &req)
	if string(req.Items[0].LatestGrant) != "open-grant" || req.Items[0].TargetStreamID != 7 || req.Items[0].OpenJTI != "jti-1" {
		t.Fatalf("re-check item %+v", req.Items[0])
	}

	clk.Advance(4 * time.Minute) // 12m: past the old deadline
	if c := rec.closes(); len(c) != 0 {
		t.Fatalf("closed at the old deadline after a renewal: %v", c)
	}
	clk.Advance(4 * time.Minute) // 16m: 80% after the renewal at 8m
	hub.last("streams/recheck", &req)
	if string(req.Items[0].LatestGrant) != "renewal-1" {
		t.Fatalf("latest grant presented: %q, want the renewal", req.Items[0].LatestGrant)
	}
}

// fakeTargetSession records RefreshAuth calls (the rest of
// conduit.LocalSession is not used).
type fakeTargetSession struct {
	conduit.LocalSession
	mu   sync.Mutex
	cred []byte
	id   uint32
}

func (f *fakeTargetSession) RefreshAuth(cred []byte, streamID uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cred, f.id = append([]byte(nil), cred...), streamID
	return nil
}

// TestStreamRenewalSentAsAuthRefreshUnchanged (design §3.9 Delivery): a
// renewal reaches a target this relay holds as AuthRefresh carrying the
// hub's grant bytes unchanged and the target-side stream id.
func TestStreamRenewalSentAsAuthRefreshUnchanged(t *testing.T) {
	hub := newFakeHub(t)
	clk := clock.NewFake(t0)
	ro := newStreamRole(t, hub, clk, nil)
	grant := []byte("eyJ0eXAiOiJjb25kdWl0LXJlbmV3YWwrand0In0.payload.sig")
	hub.setAnswer(renewAnswer(string(grant), t0.Add(20*time.Minute)))
	target := &fakeTargetSession{}
	rec := &streamRecorder{}
	defer ro.Streams.Track(TrackedStream{
		UserSessionID: "user-s1", Target: relay.TargetStream{SessionID: "agent-s1", StreamID: 42},
		OpenJTI: "jti-1", LatestGrant: []byte("open-grant"),
		AuthzUntil: t0.Add(10 * time.Minute), Interval: 10 * time.Minute,
		Renewer: RenewerFor(target, relay.TargetStream{SessionID: "agent-s1", StreamID: 42}), Close: rec.close,
	})()
	clk.Advance(8 * time.Minute)
	target.mu.Lock()
	defer target.mu.Unlock()
	if !bytes.Equal(target.cred, grant) || target.id != 42 {
		t.Fatalf("AuthRefresh{%d, %q}; want {42, the grant unchanged}", target.id, target.cred)
	}
}

// TestStreamRefusalCloses4401: a refused re-check closes the stream with
// 4401, including a refusal for a grant that does not match its inputs.
func TestStreamRefusalCloses4401(t *testing.T) {
	for _, reason := range []string{"authz_expired", "grant mismatch"} {
		t.Run(reason, func(t *testing.T) {
			hub := newFakeHub(t)
			clk := clock.NewFake(t0)
			ro := newStreamRole(t, hub, clk, nil)
			hub.setAnswer(func(path string, body []byte) hubAnswer {
				return hubAnswer{status: http.StatusOK, body: relay.HubStreamRecheckResponse{Results: []relay.HubStreamRecheckResult{{Close: true, CloseCode: conduit.CloseUnauthenticated, Reason: reason}}}}
			})
			rec := &streamRecorder{}
			defer trackStream(ro, rec)()
			clk.Advance(8 * time.Minute)
			if c := rec.closes(); len(c) != 1 || c[0] != conduit.CloseUnauthenticated {
				t.Fatalf("closes %v, want [4401]", c)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.renewed) != 0 {
				t.Fatal("a refusal forwarded something to the target")
			}
		})
	}
}

// TestStreamHubUnreachableClosesAtDeadline: with the hub unreachable
// nothing is renewed and the stream closes at its deadline.
func TestStreamHubUnreachableClosesAtDeadline(t *testing.T) {
	hub := newFakeHub(t)
	clk := clock.NewFake(t0)
	ro := newStreamRole(t, hub, clk, nil)
	hub.srv.Close()
	rec := &streamRecorder{}
	defer trackStream(ro, rec)()
	clk.Advance(9 * time.Minute)
	if c := rec.closes(); len(c) != 0 {
		t.Fatalf("closed before the deadline: %v", c)
	}
	clk.Advance(time.Minute) // 10m: the deadline
	if c := rec.closes(); len(c) != 1 || c[0] != conduit.CloseUnauthenticated {
		t.Fatalf("closes %v, want [4401] at the deadline", c)
	}
}

// TestStreamRecheckTriggers: a sweep, a revocation event and an event
// connection reconnect each re-check the stream.
func TestStreamRecheckTriggers(t *testing.T) {
	hub := newFakeHub(t)
	clk := clock.NewFake(t0)
	events := newFakeEvents()
	ro := newStreamRole(t, hub, clk, events)
	hub.setAnswer(renewAnswer("renewal", t0.Add(20*time.Minute)))
	rec := &streamRecorder{}
	defer trackStream(ro, rec)()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ro.Start(ctx)

	clk.Advance(time.Minute) // a sweep
	if n := hub.count("streams/recheck"); n != 1 {
		t.Fatalf("sweep re-checks: %d, want 1", n)
	}
	events.reconnect()
	if n := hub.count("streams/recheck"); n != 2 {
		t.Fatalf("after a reconnect: %d re-checks, want 2", n)
	}
	events.ch <- "conduit.authz.changed"
	deadline := time.Now().Add(10 * time.Second)
	for hub.count("streams/recheck") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("no re-check after a revocation event (%d)", hub.count("streams/recheck"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
