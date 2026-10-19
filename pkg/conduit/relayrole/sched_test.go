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

//go:build !no_sqlite

package relayrole

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// renewalTarget is an agent session config that accepts every stream,
// echoes it, and reports every AuthRefresh it receives.
type renewalTarget struct {
	ids       chan uint32
	refreshes chan *conduitv1.AuthRefresh
	ends      chan struct{}
	config    conduit.Config
}

func newRenewalTarget() *renewalTarget {
	r := &renewalTarget{ids: make(chan uint32, 8), refreshes: make(chan *conduitv1.AuthRefresh, 8), ends: make(chan struct{}, 8)}
	r.config = conduit.Config{
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			st, err := ps.Accept()
			if err != nil {
				return err
			}
			r.ids <- st.ID()
			go func() {
				_, _ = io.Copy(st, st)
				_ = st.Close()
				r.ends <- struct{}{}
			}()
			return nil
		}),
		Interceptor: func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
			if ar := f.GetAuthRefresh(); dir == conduit.Inbound && ar != nil && ar.GetStreamId() != 0 {
				r.refreshes <- proto2AuthRefresh(ar)
			}
			return []*conduitv1.Frame{f}
		},
	}
	return r
}

// proto2AuthRefresh copies an AuthRefresh so the test reads a stable value.
func proto2AuthRefresh(ar *conduitv1.AuthRefresh) *conduitv1.AuthRefresh {
	return &conduitv1.AuthRefresh{StreamId: ar.GetStreamId(), Credential: append([]byte(nil), ar.GetCredential()...)}
}

// schedHub answers tunnel requests with tunnel (built per call) and stream
// re-checks with recheck (503 when nil), and records the re-check bodies.
type schedHub struct {
	mu      sync.Mutex
	tunnel  func() relay.HubRPCResponse
	recheck func(req relay.HubStreamRecheckRequest) relay.HubStreamRecheckResponse
	bodies  [][]byte
}

func (h *schedHub) recheckBodies() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.bodies...)
}

// schedUser dials the user through the front of f, with the fake hub
// answering per h.
func schedUser(t *testing.T, f *roleFixture, h *schedHub) (conduit.LocalSession, chan conduit.Stream) {
	t.Helper()
	f.admitAs(registry.PrincipalUser, "tok-user", func(path string, body []byte) hubAnswer {
		switch path {
		case "rpc":
			return hubAnswer{status: http.StatusOK, body: h.tunnel()}
		case "streams/recheck":
			h.mu.Lock()
			h.bodies = append(h.bodies, append([]byte(nil), body...))
			rc := h.recheck
			h.mu.Unlock()
			if rc == nil {
				return hubAnswer{status: http.StatusServiceUnavailable}
			}
			var req relay.HubStreamRecheckRequest
			_ = json.Unmarshal(body, &req)
			return hubAnswer{status: http.StatusOK, body: rc(req)}
		}
		return hubAnswer{status: http.StatusServiceUnavailable}
	})
	cfg, streams := tunnelUser()
	ls, _, err := f.dial(t, relaytest.UserHello("user-1"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ls, streams
}

// hinted is tunnelInstruction with scheduling hints.
func hinted(t *testing.T, rec registry.SessionRecord, endpoint string, untilUnix, intervalS int64) relay.HubRPCResponse {
	t.Helper()
	in := tunnelInstruction(t, rec, endpoint)
	in.Tunnel.AuthzUntil, in.Tunnel.AuthzIntervalS = untilUnix, intervalS
	return in
}

// renewAll answers every re-check item with a renewal carrying grant.
func renewAll(grant string, until time.Time) func(relay.HubStreamRecheckRequest) relay.HubStreamRecheckResponse {
	return func(req relay.HubStreamRecheckRequest) relay.HubStreamRecheckResponse {
		res := make([]relay.HubStreamRecheckResult, len(req.Items))
		for i := range res {
			res[i] = relay.HubStreamRecheckResult{Renew: true, RenewalGrant: []byte(grant), AuthzUntil: until}
		}
		return relay.HubStreamRecheckResponse{Results: res}
	}
}

// trackedStreams returns copies of the scheduler's tracked streams.
func trackedStreams(s *StreamScheduler) []TrackedStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TrackedStream, 0, len(s.streams))
	for st := range s.streams {
		st.mu.Lock()
		out = append(out, st.TrackedStream)
		st.mu.Unlock()
	}
	return out
}

// TestRoleTunnel_TrackedWithTargetIDsAndOpenGrant: a relay-held tunnel is
// handed to the stream scheduler with the target-side ids, the open grant's
// jti and the open grant itself as the latest grant.
func TestRoleTunnel_TrackedWithTargetIDsAndOpenGrant(t *testing.T) {
	f := newRoleFixture(t)
	target := newRenewalTarget()
	rec := dialTarget(t, f.w, f.node, target.config)
	h := &schedHub{tunnel: func() relay.HubRPCResponse { return tunnelInstruction(t, rec, f.node.Internal.URL) }}
	ls, _ := schedUser(t, f, h)
	_ = streamIDOf(t, requestTunnel(t, ls))
	targetID := relaytest.Wait(t, target.ids, "the target stream")

	tracked := trackedStreams(f.role.Streams)
	if len(tracked) != 1 {
		t.Fatalf("%d tracked streams, want 1", len(tracked))
	}
	ts := tracked[0]
	if ts.Target != (relay.TargetStream{SessionID: rec.SessionID, StreamID: targetID}) || ts.OpenJTI != "jti-1" ||
		!bytes.Equal(ts.LatestGrant, []byte("hub-signed-open-grant")) || ts.UserSessionID != ls.Info().SessionID {
		t.Fatalf("tracked %+v, want the target ids, jti-1 and the open grant", ts)
	}
	if ts.Renewer == nil || ts.Close == nil {
		t.Fatal("the tracked stream has no renewer or close")
	}
}

// TestRoleTunnel_HintSchedulesRecheck (design §3.9 "Scheduling hints"):
// with usable hints the first re-check runs at authz_until minus 0.2 of the
// interval, presenting the open grant, and not before.
func TestRoleTunnel_HintSchedulesRecheck(t *testing.T) {
	f := newRoleFixture(t)
	rec := dialTarget(t, f.w, f.node, newRenewalTarget().config)
	now := f.clk.Now()
	until := now.Add(10 * time.Minute).Unix()
	h := &schedHub{tunnel: func() relay.HubRPCResponse { return hinted(t, rec, f.node.Internal.URL, until, 600) }}
	ls, _ := schedUser(t, f, h)
	_ = streamIDOf(t, requestTunnel(t, ls))
	if a, i := f.role.Streams.HintFallbacks(HintFallbackAbsent), f.role.Streams.HintFallbacks(HintFallbackInvalid); a != 0 || i != 0 {
		t.Fatalf("fallbacks absent=%d invalid=%d, want none", a, i)
	}

	first := time.Unix(until, 0).Add(-2 * time.Minute)
	f.clk.Advance(first.Sub(now) - time.Second)
	if n := len(h.recheckBodies()); n != 0 {
		t.Fatalf("%d re-checks before the hinted time, want 0", n)
	}
	f.clk.Advance(time.Second)
	bodies := h.recheckBodies()
	if len(bodies) != 1 {
		t.Fatalf("%d re-checks at the hinted time, want 1", len(bodies))
	}
	var req relay.HubStreamRecheckRequest
	if err := json.Unmarshal(bodies[0], &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Items) != 1 || req.Items[0].Trigger != TriggerInterval || req.Items[0].OpenJTI != "jti-1" ||
		!bytes.Equal(req.Items[0].LatestGrant, []byte("hub-signed-open-grant")) {
		t.Fatalf("re-check items %+v, want one interval re-check presenting the open grant", req.Items)
	}
}

// TestRoleTunnel_MissingHintFallsBackToSweep: a tunnel open without hints
// arms no interval timer, is counted as absent, and is re-checked by the
// sweep.
func TestRoleTunnel_MissingHintFallsBackToSweep(t *testing.T) {
	f := newRoleFixture(t)
	rec := dialTarget(t, f.w, f.node, newRenewalTarget().config)
	h := &schedHub{tunnel: func() relay.HubRPCResponse { return tunnelInstruction(t, rec, f.node.Internal.URL) }}
	ls, _ := schedUser(t, f, h)
	_ = streamIDOf(t, requestTunnel(t, ls))
	if n := f.role.Streams.HintFallbacks(HintFallbackAbsent); n != 1 {
		t.Fatalf("absent fallbacks %d, want 1", n)
	}
	f.clk.Advance(24 * time.Hour)
	if n := len(h.recheckBodies()); n != 0 {
		t.Fatalf("%d re-checks without a hint or a sweep, want 0", n)
	}
	f.role.Streams.RecheckAll(context.Background(), TriggerSweep)
	if n := len(h.recheckBodies()); n != 1 {
		t.Fatalf("%d re-checks after the sweep, want 1", n)
	}
}

// TestRoleTunnel_OutOfRangeHintFallsBackToSweep (contracts §3): each
// out-of-range or partial hint leaves the tunnel to the sweep, counted as
// invalid, with no interval timer.
func TestRoleTunnel_OutOfRangeHintFallsBackToSweep(t *testing.T) {
	for _, tc := range []struct {
		name      string
		until     func(now time.Time) int64
		intervalS int64
	}{
		{name: "authz_until only", until: func(now time.Time) int64 { return now.Add(10 * time.Minute).Unix() }},
		{name: "interval only", until: func(time.Time) int64 { return 0 }, intervalS: 600},
		{name: "interval under 1m", until: func(now time.Time) int64 { return now.Add(30 * time.Second).Unix() }, intervalS: 30},
		{name: "interval over 7d", until: func(now time.Time) int64 { return now.Add(time.Hour).Unix() }, intervalS: 8 * 24 * 3600},
		{name: "authz_until at receipt", until: func(now time.Time) int64 { return now.Unix() }, intervalS: 600},
		{name: "authz_until past receipt plus interval plus skew", until: func(now time.Time) int64 { return now.Add(600*time.Second + 31*time.Second).Unix() }, intervalS: 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoleFixture(t)
			rec := dialTarget(t, f.w, f.node, newRenewalTarget().config)
			now := f.clk.Now()
			h := &schedHub{tunnel: func() relay.HubRPCResponse { return hinted(t, rec, f.node.Internal.URL, tc.until(now), tc.intervalS) }}
			ls, _ := schedUser(t, f, h)
			_ = streamIDOf(t, requestTunnel(t, ls))
			if n := f.role.Streams.HintFallbacks(HintFallbackInvalid); n != 1 {
				t.Fatalf("invalid fallbacks %d, want 1", n)
			}
			f.clk.Advance(8 * 24 * time.Hour)
			if n := len(h.recheckBodies()); n != 0 {
				t.Fatalf("%d re-checks from an unusable hint, want 0", n)
			}
		})
	}
}

// TestRoleTunnel_NotifyRecheckCarriesNoHint: a NOTIFY-triggered re-check
// sends the unchanged item shape, with no hint in it, and does not move the
// hinted schedule.
func TestRoleTunnel_NotifyRecheckCarriesNoHint(t *testing.T) {
	f := newRoleFixture(t)
	rec := dialTarget(t, f.w, f.node, newRenewalTarget().config)
	now := f.clk.Now()
	until := now.Add(10 * time.Minute).Unix()
	h := &schedHub{tunnel: func() relay.HubRPCResponse { return hinted(t, rec, f.node.Internal.URL, until, 600) }}
	ls, _ := schedUser(t, f, h)
	_ = streamIDOf(t, requestTunnel(t, ls))

	f.role.Streams.RecheckAll(context.Background(), TriggerNotify)
	bodies := h.recheckBodies()
	if len(bodies) != 1 {
		t.Fatalf("%d re-checks after the event, want 1", len(bodies))
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(bodies[0], &raw); err != nil || len(raw.Items) != 1 {
		t.Fatalf("re-check body %s: %v", bodies[0], err)
	}
	for _, k := range []string{"authz_until", "authz_interval", "authz_interval_s"} {
		if _, ok := raw.Items[0][k]; ok {
			t.Fatalf("the re-check item carries %q: %s", k, bodies[0])
		}
	}
	if string(raw.Items[0]["trigger"]) != `"`+TriggerNotify+`"` {
		t.Fatalf("trigger %s, want %s", raw.Items[0]["trigger"], TriggerNotify)
	}
	// The hub could not evaluate it (503): the hinted first re-check is
	// still due at its time.
	f.clk.Advance(time.Unix(until, 0).Add(-2 * time.Minute).Sub(now))
	if n := len(h.recheckBodies()); n != 2 {
		t.Fatalf("%d re-checks, want the event one and the hinted one", n)
	}
}

// TestRoleTunnel_RenewalDelivered (design §3.9 "Delivery"): a renewal from
// the hub reaches the target as AuthRefresh on the target-side stream, the
// grant bytes unchanged and never parsed (they are not a JWT), on the same
// relay and across a bridge. Timing comes from the hints alone.
func TestRoleTunnel_RenewalDelivered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bridge bool
	}{{name: "same relay"}, {name: "across a bridge", bridge: true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoleFixture(t)
			owner := f.node
			if tc.bridge {
				owner = f.w.StartNode("relay-1", nil)
			}
			target := newRenewalTarget()
			rec := dialTarget(t, f.w, owner, target.config)
			now := f.clk.Now()
			until := now.Add(10 * time.Minute).Unix()
			renewal := "renewal-grant: opaque bytes, not a JWT"
			h := &schedHub{
				tunnel:  func() relay.HubRPCResponse { return hinted(t, rec, owner.Internal.URL, until, 600) },
				recheck: renewAll(renewal, time.Unix(until, 0).Add(10*time.Minute)),
			}
			ls, _ := schedUser(t, f, h)
			_ = streamIDOf(t, requestTunnel(t, ls))
			targetID := relaytest.Wait(t, target.ids, "the target stream")

			f.clk.Advance(time.Unix(until, 0).Add(-2 * time.Minute).Sub(now))
			ar := relaytest.Wait(t, target.refreshes, "the renewal at the target")
			if ar.GetStreamId() != targetID || string(ar.GetCredential()) != renewal {
				t.Fatalf("AuthRefresh{%d, %q}, want {%d, the hub's grant unchanged}", ar.GetStreamId(), ar.GetCredential(), targetID)
			}
			tracked := trackedStreams(f.role.Streams)
			if len(tracked) != 1 || string(tracked[0].LatestGrant) != renewal {
				t.Fatalf("tracked %+v, want the renewal as the latest grant", tracked)
			}
		})
	}
}

// TestRoleTunnel_DenyClosesBothLegs: a re-check the hub refuses closes the
// user stream and the target stream with the hub's code.
func TestRoleTunnel_DenyClosesBothLegs(t *testing.T) {
	f := newRoleFixture(t)
	target := newRenewalTarget()
	rec := dialTarget(t, f.w, f.node, target.config)
	h := &schedHub{
		tunnel: func() relay.HubRPCResponse { return tunnelInstruction(t, rec, f.node.Internal.URL) },
		recheck: func(req relay.HubStreamRecheckRequest) relay.HubStreamRecheckResponse {
			res := make([]relay.HubStreamRecheckResult, len(req.Items))
			for i := range res {
				res[i] = relay.HubStreamRecheckResult{Close: true, CloseCode: conduit.CloseUnauthenticated, Reason: "denied"}
			}
			return relay.HubStreamRecheckResponse{Results: res}
		},
	}
	ls, streams := schedUser(t, f, h)
	_ = streamIDOf(t, requestTunnel(t, ls))
	user := relaytest.Wait(t, streams, "the user stream")
	relaytest.Wait(t, target.ids, "the target stream")

	f.role.Streams.RecheckAll(context.Background(), TriggerSweep)
	relaytest.Wait(t, target.ends, "the target stream's end")
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(user)
		done <- err
	}()
	if err := relaytest.Wait(t, done, "the user stream's end"); conduit.CodeOf(err, 0) != conduit.CloseUnauthenticated {
		t.Fatalf("user stream ended with %v, want 4401", err)
	}
}

// TestRoleTunnel_UntrackedWhenSpliceEnds: when a tunnel ends, it is no
// longer tracked or recorded.
func TestRoleTunnel_UntrackedWhenSpliceEnds(t *testing.T) {
	ended := make(chan struct{}, 4)
	testHookTunnelEnded = func() { ended <- struct{}{} }
	t.Cleanup(func() { testHookTunnelEnded = nil })
	f := newRoleFixture(t)
	target := newRenewalTarget()
	rec := dialTarget(t, f.w, f.node, target.config)
	h := &schedHub{tunnel: func() relay.HubRPCResponse { return tunnelInstruction(t, rec, f.node.Internal.URL) }}
	ls, streams := schedUser(t, f, h)
	_ = streamIDOf(t, requestTunnel(t, ls))
	user := relaytest.Wait(t, streams, "the user stream")
	if n := f.role.Streams.Len(); n != 1 {
		t.Fatalf("%d tracked streams, want 1", n)
	}
	_ = user.Close()
	relaytest.Wait(t, ended, "the tunnel's end")
	if n := f.role.Streams.Len(); n != 0 {
		t.Fatalf("%d tracked streams after the tunnel ended, want 0", n)
	}
	if tns := f.role.Tunnels(); len(tns) != 0 {
		t.Fatalf("tunnels %+v after the tunnel ended, want none", tns)
	}
}
