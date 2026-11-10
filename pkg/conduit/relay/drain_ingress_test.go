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

package relay_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Drain through the proxy, relay side (design §3.9, v2.21.53): POST
// /internal/v1/conduit/drain sends GoAway only to the sessions the calling
// hub proxied, and only ever shortens their remaining life.

// drainSession is a dialer session and the GoAways it receives.
type drainSession struct {
	ls      conduit.LocalSession
	goAways <-chan *conduitv1.GoAway
}

// dialDrainSession dials a user session on n whose principal arrived
// through ingress (ingress "" = a direct dial), with principal cap pcap.
func dialDrainSession(t *testing.T, w *relaytest.World, n *relaytest.Node, user, ingress string, pcap time.Duration) drainSession {
	t.Helper()
	w.SetPrincipal(user, relay.Principal{Kind: registry.PrincipalUser, ID: user, IngressInstance: ingress, LifetimeCap: pcap})
	cfg := conduit.Config{}
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	ls, wel := n.MustDial(user, relaytest.UserHello(user), cfg)
	// The Welcome is sent while the session is still pending; a drain only
	// sees registered sessions. Wait until the relay has registered it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, ok := n.Relay.Local(ctx, wel.GetSessionId()); !ok {
		t.Fatalf("session %s was not registered on the relay", wel.GetSessionId())
	}
	return drainSession{ls: ls, goAways: goAways}
}

// hubDrainClient is a relay-peer client signed as the hub instance id.
func hubDrainClient(w *relaytest.World, n *relaytest.Node, id string) *relay.PeerClient {
	return &relay.PeerClient{HTTP: n.Internal.Client(), Auth: w.PeerAuth(id)}
}

func drainCall(t *testing.T, w *relaytest.World, n *relaytest.Node, signer, instance string, deadline time.Duration) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hubDrainClient(w, n, signer).Drain(ctx, n.Internal.URL, "relay-a", n.Relay.Generation(), instance, deadline)
}

func noGoAway(t *testing.T, s drainSession, what string) {
	t.Helper()
	select {
	case g := <-s.goAways:
		t.Fatalf("%s got GoAway %+v", what, g)
	default:
	}
}

// TestDrainIngress_OnlyTheCallingHubsSessions: the drain sends GoAway to
// the sessions hub-a proxied and to no other (another hub's, a direct
// dial), returns the count, and a second drain finds them already
// draining: skipped, not counted.
func TestDrainIngress_OnlyTheCallingHubsSessions(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 0, 0)
	a1 := dialDrainSession(t, w, n, "user-a1", "hub-a", 0)
	a2 := dialDrainSession(t, w, n, "user-a2", "hub-a", 0)
	b := dialDrainSession(t, w, n, "user-b", "hub-b", 0)
	direct := dialDrainSession(t, w, n, "user-d", "", 0)

	sent, err := drainCall(t, w, n, "hub-a", "hub-a", 5*time.Second)
	if err != nil || sent != 2 {
		t.Fatalf("drain = %d, %v; want 2 sessions", sent, err)
	}
	for _, s := range []drainSession{a1, a2} {
		g := relaytest.Wait(t, s.goAways, "GoAway on a session hub-a proxied")
		if g.GetDrainDeadlineMs() != 5000 || g.GetReason() != relay.ReasonDraining {
			t.Fatalf("GoAway drain_deadline_ms=%d reason=%q, want 5000 and %q", g.GetDrainDeadlineMs(), g.GetReason(), relay.ReasonDraining)
		}
	}
	noGoAway(t, b, "another hub's session")
	noGoAway(t, direct, "a direct session")

	if sent, err := drainCall(t, w, n, "hub-a", "hub-a", 5*time.Second); err != nil || sent != 0 {
		t.Fatalf("second drain = %d, %v; want 0 (already draining)", sent, err)
	}
	if sent, err := drainCall(t, w, n, "hub-c", "hub-c", 5*time.Second); err != nil || sent != 0 {
		t.Fatalf("drain for a hub with no sessions = %d, %v; want 0", sent, err)
	}
}

// TestDrainIngress_DeadlineOnlyShortens: the requested deadline is clamped
// to the relay's own drain deadline (absent or 0: the relay's own; at
// least 1ms) and then to the session's lifetime cap; a drain never
// lengthens a session.
func TestDrainIngress_DeadlineOnlyShortens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		own     time.Duration // the relay's Session.DrainDeadline (0: 30s default)
		pcap    time.Duration // the session's principal cap
		request time.Duration
		wantMs  uint32
	}{
		{name: "shorter request", request: 5 * time.Second, wantMs: 5000},
		{name: "request above the relay's own", own: 20 * time.Second, request: time.Minute, wantMs: 20000},
		{name: "absent request is the relay's own", own: 20 * time.Second, wantMs: 20000},
		{name: "absent request is the session default", wantMs: 30000},
		{name: "1ms request", request: time.Millisecond, wantMs: 1},
		{name: "cut to the session's cap", own: 120 * time.Second, pcap: 100 * time.Second, wantMs: 100000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			n := lifetimeNode(t, w, 0, tc.own)
			s := dialDrainSession(t, w, n, "user-a", "hub-a", tc.pcap)
			if sent, err := drainCall(t, w, n, "hub-a", "hub-a", tc.request); err != nil || sent != 1 {
				t.Fatalf("drain = %d, %v; want 1", sent, err)
			}
			if g := relaytest.Wait(t, s.goAways, "GoAway"); g.GetDrainDeadlineMs() != tc.wantMs {
				t.Fatalf("drain_deadline_ms = %d, want %d", g.GetDrainDeadlineMs(), tc.wantMs)
			}
		})
	}
}

// TestDrainIngress_ReplyDoesNotWaitOnTheStore: the GoAways are sent and the
// answer given before the session rows are marked draining, so a stalled
// registry never holds the reply.
func TestDrainIngress_ReplyDoesNotWaitOnTheStore(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 0, 0)
	s := dialDrainSession(t, w, n, "user-a", "hub-a", 0)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w.SetFault(func(op string) error {
		if op == registry.OpSetSessionDraining {
			<-release
		}
		return nil
	})
	start := time.Now()
	if sent, err := drainCall(t, w, n, "hub-a", "hub-a", 0); err != nil || sent != 1 {
		t.Fatalf("drain = %d, %v; want 1", sent, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the reply took %s with the store stalled", took)
	}
	relaytest.Wait(t, s.goAways, "GoAway sent with the store stalled")
}

// signedDrainRequest builds a drain request signed as hub-a for relay-a,
// with body digest digest ("" = the body's own).
func signedDrainRequest(t *testing.T, w *relaytest.World, n *relaytest.Node, method, body, digest string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, n.Internal.URL+relay.InternalPathPrefix+"drain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		sum := sha256.Sum256([]byte(body))
		digest = hex.EncodeToString(sum[:])
	}
	req.Header.Set(relay.HeaderBodySHA256, digest)
	relay.SetPeerTarget(req, "relay-a", n.Relay.Generation())
	if err := w.PeerAuth("hub-a").Sign(req); err != nil {
		t.Fatal(err)
	}
	return req
}

// TestDrainRoute_PeerAndBodyChecks: the route takes only a relay-peer
// request, signed over its body digest, whose ingress_instance is the
// calling peer, and answers nothing else.
func TestDrainRoute_PeerAndBodyChecks(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 0, 0)
	s := dialDrainSession(t, w, n, "user-a", "hub-a", 0)
	do := func(req *http.Request) int {
		t.Helper()
		resp, err := n.Internal.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	unsigned, _ := http.NewRequest(http.MethodPost, n.Internal.URL+relay.InternalPathPrefix+"drain", strings.NewReader(`{"ingress_instance":"hub-a"}`))
	if got := do(unsigned); got != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d, want 401", got)
	}
	if _, err := drainCall(t, w, n, "hub-a", "hub-b", 0); !isDrainRefused(err, http.StatusForbidden) {
		t.Fatalf("another hub's instance: %v, want refused 403", err)
	}
	if got := do(signedDrainRequest(t, w, n, http.MethodGet, "", "")); got != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d, want 405", got)
	}
	if got := do(signedDrainRequest(t, w, n, http.MethodPost, `{"ingress_instance":"hub-a"}`, strings.Repeat("0", 64))); got != http.StatusBadRequest {
		t.Fatalf("body not matching its signed digest: %d, want 400", got)
	}
	if got := do(signedDrainRequest(t, w, n, http.MethodPost, `{"ingress_instance":`, "")); got != http.StatusBadRequest {
		t.Fatalf("malformed body: %d, want 400", got)
	}
	big := fmt.Sprintf(`{"ingress_instance":"hub-a","pad":%q}`, strings.Repeat("x", 8<<10))
	if got := do(signedDrainRequest(t, w, n, http.MethodPost, big, "")); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d, want 413", got)
	}
	noGoAway(t, s, "a session after refused drains")
}

func isDrainRefused(err error, status int) bool {
	var de *relay.DrainError
	return errors.As(err, &de) && de.Cause == relay.DrainFailedRefused && de.Status == status
}

// TestPeerClientDrain_FailureCauses: a failed drain call is classified as
// refused (non-2xx, with the status), bad_response (2xx, unreadable),
// unreachable (no connection) or timeout (the caller's deadline).
func TestPeerClientDrain_FailureCauses(t *testing.T) {
	w := relaytest.NewWorld(t)
	client := func(srv *httptest.Server) *relay.PeerClient {
		return &relay.PeerClient{HTTP: srv.Client(), Auth: w.PeerAuth("hub-a")}
	}
	cause := func(err error) (string, int) {
		var de *relay.DrainError
		if !errors.As(err, &de) {
			t.Fatalf("error %v is not a *DrainError", err)
		}
		return de.Cause, de.Status
	}
	answer := func(status int, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(status)
			_, _ = rw.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	ctx := context.Background()
	var err error

	refused := answer(http.StatusServiceUnavailable, "relay unavailable")
	_, err = client(refused).Drain(ctx, refused.URL, "relay-a", 1, "hub-a", 0)
	if c, st := cause(err); c != relay.DrainFailedRefused || st != http.StatusServiceUnavailable {
		t.Fatalf("503: %s %d, want refused 503", c, st)
	}
	bad := answer(http.StatusOK, "not json")
	_, err = client(bad).Drain(ctx, bad.URL, "relay-a", 1, "hub-a", 0)
	if c, _ := cause(err); c != relay.DrainFailedBadResponse {
		t.Fatalf("unreadable 200: %s, want bad_response", c)
	}
	gone := answer(http.StatusOK, "{}")
	gone.Close()
	_, err = client(gone).Drain(ctx, gone.URL, "relay-a", 1, "hub-a", 0)
	if c, _ := cause(err); c != relay.DrainFailedUnreachable {
		t.Fatalf("closed server: %s, want unreachable", c)
	}
	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(hang); slow.Close() })
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err = client(slow).Drain(tctx, slow.URL, "relay-a", 1, "hub-a", 0)
	if c, _ := cause(err); c != relay.DrainFailedTimeout {
		t.Fatalf("slow relay: %s, want timeout", c)
	}
	ok := answer(http.StatusOK, `{"goaway_sent":3}`)
	if n, err := client(ok).Drain(ctx, ok.URL, "relay-a", 1, "hub-a", 0); err != nil || n != 3 {
		t.Fatalf("drain = %d, %v; want 3", n, err)
	}
}

// TestDrainIngress_ShutdownWaitsForTheRowPass: the background pass that
// marks drained rows draining is tracked, so Shutdown does not return (and
// the store is not closed under it) while the pass is still running.
func TestDrainIngress_ShutdownWaitsForTheRowPass(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 0, 0)
	s := dialDrainSession(t, w, n, "user-a", "hub-a", 0)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	w.SetFault(func(op string) error {
		if op == registry.OpSetSessionDraining {
			<-release
		}
		return nil
	})
	if sent, err := drainCall(t, w, n, "hub-a", "hub-a", time.Millisecond); err != nil || sent != 1 {
		t.Fatalf("drain = %d, %v; want 1", sent, err)
	}
	relaytest.Wait(t, s.goAways, "GoAway")
	_ = s.ls.Close()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- n.Relay.Shutdown(ctx)
	}()
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned (%v) while the row pass was still running", err)
	case <-time.After(300 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown after the row pass ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the row pass ended")
	}
}
