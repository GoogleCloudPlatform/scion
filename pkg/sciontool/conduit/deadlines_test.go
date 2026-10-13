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

package conduit

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Grant keys from the hub and stream authorization deadlines at the
// target (design v2.21.16 §3.5, "Target-side deadlines"; criterion 29).

// waitFor polls cond until it holds or waitTimeout passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitKey waits until the agent's key set (fetched from the hub) has kid.
func waitKey(t *testing.T, a *Agent, kid string) {
	t.Helper()
	waitFor(t, "key "+kid+" fetched", func() bool { _, ok := a.Keys().Lookup(kid); return ok })
}

// mustRefuse opens o on s and requires a 4403 refusal.
func mustRefuse(t *testing.T, s core.LocalSession, o *conduitv1.StreamOpen, what string) {
	t.Helper()
	if _, err := s.OpenStream(context.Background(), o); core.CodeOf(err, 0) != core.CloseForbidden {
		t.Fatalf("%s: OpenStream = %v, want 4403", what, err)
	}
}

// mustOpen opens o on s and requires it to be accepted.
func mustOpen(t *testing.T, s core.LocalSession, o *conduitv1.StreamOpen, what string) core.Stream {
	t.Helper()
	st, err := s.OpenStream(context.Background(), o)
	if err != nil {
		t.Fatalf("%s: OpenStream = %v", what, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestAgentWelcomeKeysNotTrusted: a key listed only in the Welcome is not
// trusted; a grant it signs is refused. A key from the hub's key fetch is
// trusted whether or not the Welcome also lists it.
func TestAgentWelcomeKeysNotTrusted(t *testing.T) {
	k1, k2 := newTestKey(t, "k1"), newTestKey(t, "k2")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.welcomeKeys = []grant.PublicKey{k1.public, k2.public} })
	a, _ := startAgent(t, h, nil)
	s := h.nextSession(t)
	waitKey(t, a, "k1")
	port := echoListener(t)

	mustRefuse(t, s, tcpOpen(t, k2, s.Info(), "launch-1", port), "grant signed by a Welcome-only key")
	if _, ok := a.Keys().Lookup("k2"); ok {
		t.Fatal("a Welcome-only key was installed")
	}
	mustOpen(t, s, tcpOpen(t, k1, s.Info(), "launch-1", port), "grant signed by a fetched key")
}

// TestAgentRefusesGrantsWithoutFetchedKeys: while no key fetch has
// succeeded, every grant is refused, even one signed by a key the Welcome
// lists.
func TestAgentRefusesGrantsWithoutFetchedKeys(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	serve := h.srv.Config.Handler
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/grant-keys") {
			h.keyHits.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		serve.ServeHTTP(w, r)
	})
	_, done := startAgent(t, h, nil)
	s := h.nextSession(t)
	port := echoListener(t)
	mustRefuse(t, s, tcpOpen(t, k1, s.Info(), "launch-1", port), "grant with no fetched keys")
	// The failed fetches stop nothing: the agent keeps its session.
	select {
	case err := <-done:
		t.Fatalf("Run returned %v after failed key fetches", err)
	default:
	}
	if s.Err() != nil {
		t.Fatalf("session ended: %v", s.Err())
	}
}

// TestAgentUnknownKidsShareOneFetchPerInterval: grants naming distinct
// unknown kids within UnknownKidFetchInterval cause one key fetch in
// total, and every one of them is refused.
func TestAgentUnknownKidsShareOneFetchPerInterval(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	a, _ := startAgent(t, h, nil)
	s := h.nextSession(t)
	waitKey(t, a, "k1")
	port := echoListener(t)
	base := h.keyHits.Load()
	for _, kid := range []string{"u1", "u2", "u3", "u4", "u5"} {
		mustRefuse(t, s, tcpOpen(t, newTestKey(t, kid), s.Info(), "launch-1", port), "unknown kid "+kid)
	}
	if n := h.keyHits.Load() - base; n != 1 {
		t.Fatalf("%d key fetches for five unknown kids, want 1", n)
	}
}

// TestAgentRotatedKeyAcceptedAfterOneFetch: a grant signed by a key the
// hub has just published is accepted after one key fetch.
func TestAgentRotatedKeyAcceptedAfterOneFetch(t *testing.T) {
	k1, k2 := newTestKey(t, "k1"), newTestKey(t, "k2")
	h := newFakeHub(t, k1.public)
	a, _ := startAgent(t, h, nil)
	s := h.nextSession(t)
	waitKey(t, a, "k1")
	port := echoListener(t)
	base := h.keyHits.Load()
	h.set(func(h *fakeHub) { h.keys = []grant.PublicKey{k1.public, k2.public} })
	mustOpen(t, s, tcpOpen(t, k2, s.Info(), "launch-1", port), "grant signed by the new key")
	if n := h.keyHits.Load() - base; n != 1 {
		t.Fatalf("%d key fetches, want 1", n)
	}
}

// withDeadline gives a grant an authorization deadline.
func withDeadline(until time.Time, interval time.Duration) func(*grant.Claims) {
	return func(c *grant.Claims) { c.AuthzUntil, c.AuthzInterval = until, interval }
}

// TestAgentEnforceModeRequiresDeadline: once the key fetch sets enforce,
// a user stream's grant without authz_until is refused; with authz_until
// it is accepted, and a stream opened by an agent principal needs none.
func TestAgentEnforceModeRequiresDeadline(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.deadlines = "enforce" })
	a, _ := startAgent(t, h, nil)
	s := h.nextSession(t)
	waitFor(t, "enforce mode", a.enforceDeadlines.Load)
	port := echoListener(t)
	now := time.Now().Truncate(time.Second)

	mustRefuse(t, s, tcpOpen(t, k1, s.Info(), "launch-1", port), "user grant without authz_until")
	mustOpen(t, s, tcpOpenWith(t, k1, s.Info(), "launch-1", port, withDeadline(now.Add(time.Hour), time.Hour)), "user grant with authz_until")
	mustOpen(t, s, tcpOpenWith(t, k1, s.Info(), "launch-1", port, func(c *grant.Claims) { c.Subject = "agent:agent-2" }), "agent grant without authz_until")
}

// TestAgentEnforceModeNotLoweredBySession: the enforcement mode and the
// keys come only from the key fetch. A later session whose Welcome lists
// no keys and carries nothing about deadlines leaves enforcement on and
// the fetched keys trusted.
func TestAgentEnforceModeNotLoweredBySession(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.deadlines = "enforce" })
	a, _ := startAgent(t, h, nil)
	first := h.nextSession(t)
	waitFor(t, "enforce mode", a.enforceDeadlines.Load)

	h.set(func(h *fakeHub) { h.welcomeKeys = []grant.PublicKey{} })
	_ = first.CloseWithCode(core.CloseRelayRestart, "relay_restart")
	s := h.nextSession(t)
	port := echoListener(t)
	now := time.Now().Truncate(time.Second)
	mustRefuse(t, s, tcpOpen(t, k1, s.Info(), "launch-1", port), "user grant without authz_until on the new session")
	mustOpen(t, s, tcpOpenWith(t, k1, s.Info(), "launch-1", port, withDeadline(now.Add(time.Hour), time.Hour)), "user grant with authz_until on the new session")
	if !a.enforceDeadlines.Load() {
		t.Fatal("enforce mode lowered by a session")
	}
}

// TestAgentDeadlineModeOff: a key fetch without target_stream_deadlines
// (or with off) leaves grants without authz_until accepted.
func TestAgentDeadlineModeOff(t *testing.T) {
	for _, mode := range []string{"", "off"} {
		t.Run("mode="+mode, func(t *testing.T) {
			k1 := newTestKey(t, "k1")
			h := newFakeHub(t, k1.public)
			h.set(func(h *fakeHub) { h.deadlines = mode })
			a, _ := startAgent(t, h, nil)
			s := h.nextSession(t)
			waitKey(t, a, "k1")
			mustOpen(t, s, tcpOpen(t, k1, s.Info(), "launch-1", echoListener(t)), "grant without authz_until")
		})
	}
}

// renewalFor mints a renewal for the stream st, opened with a grant whose
// jti is openJTI, on the session described by info.
func renewalFor(t *testing.T, key testKey, info core.SessionInfo, st core.Stream, openJTI string, until time.Time, mut func(*grant.RenewalClaims)) []byte {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	c := grant.RenewalClaims{
		Issuer: "scion-hub", Subject: "user:u1", ProjectID: testProjectID,
		Target: grant.Target{
			Kind: grant.TargetKindAgent, ID: testAgentID, EndpointIncarnation: "launch-1",
			SessionID: info.SessionID, ConnectionEpoch: info.ConnectionEpoch,
		},
		StreamID: st.ID(), OpenJTI: openJTI, AuthzUntil: until,
		NotBefore: now, Expiry: now.Add(30 * time.Second),
	}
	if mut != nil {
		mut(&c)
	}
	tok, err := grant.MintRenewal(&key.signer, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// echoes reports whether st still echoes a short write.
func echoes(st core.Stream) bool {
	if _, err := st.Write([]byte("ping")); err != nil {
		return false
	}
	buf := make([]byte, 4)
	_, err := io.ReadFull(st, buf)
	return err == nil && string(buf) == "ping"
}

// closedWith waits for st to end and returns its close code.
func closedWith(t *testing.T, st core.Stream) uint32 {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, st)
		done <- err
	}()
	select {
	case err := <-done:
		return core.CodeOf(err, 0)
	case <-time.After(waitTimeout):
		t.Fatal("stream did not end")
		return 0
	}
}

// TestAgentStreamClosesAtDeadline: a stream whose grant carries
// authz_until is closed with 4401 at authz_until plus the grace when no
// valid renewal arrives. Refused renewals (another open_jti, past the cap)
// neither extend the deadline nor close the stream early, and are logged.
func TestAgentStreamClosesAtDeadline(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "agent.log")
	log.SetLogPath(logPath)
	t.Cleanup(func() { log.SetLogPath("") })

	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	a, _ := startAgent(t, h, func(o *Options) { o.deadlineGrace = 200 * time.Millisecond })
	s := h.nextSession(t)
	waitKey(t, a, "k1")
	now := time.Now().Truncate(time.Second)
	until := now.Add(2 * time.Second)
	st := mustOpen(t, s, tcpOpenWith(t, k1, s.Info(), "launch-1", echoListener(t), func(c *grant.Claims) {
		c.JTI = "open-1"
		c.AuthzUntil, c.AuthzInterval = until, 10*time.Second
	}), "stream with a deadline")

	r := st.(core.StreamRenewer)
	if err := r.Renew(renewalFor(t, k1, s.Info(), st, "open-2", until.Add(3*time.Second), nil)); err != nil {
		t.Fatal(err)
	}
	if err := r.Renew(renewalFor(t, k1, s.Info(), st, "open-1", time.Now().Add(time.Minute), nil)); err != nil {
		t.Fatal(err)
	}
	if !echoes(st) {
		t.Fatal("stream closed early by refused renewals")
	}
	if code := closedWith(t, st); code != core.CloseUnauthenticated {
		t.Fatalf("close code %d, want 4401", code)
	}
	if late := time.Since(until); late < 0 || late > 5*time.Second {
		t.Fatalf("closed %v after authz_until, want just after the grace", late)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "renewal for stream"); n != 2 {
		t.Fatalf("%d refused-renewal log lines, want 2:\n%s", n, b)
	}
}

// TestAgentRenewalExtendsDeadline: a valid renewal moves the stream's
// deadline: the stream outlives its first deadline and closes with 4401
// at the renewed one.
func TestAgentRenewalExtendsDeadline(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	a, _ := startAgent(t, h, func(o *Options) { o.deadlineGrace = 200 * time.Millisecond })
	s := h.nextSession(t)
	waitKey(t, a, "k1")
	now := time.Now().Truncate(time.Second)
	until := now.Add(2 * time.Second)
	st := mustOpen(t, s, tcpOpenWith(t, k1, s.Info(), "launch-1", echoListener(t), func(c *grant.Claims) {
		c.JTI = "open-1"
		c.AuthzUntil, c.AuthzInterval = until, 10*time.Second
	}), "stream with a deadline")

	renewed := until.Add(2 * time.Second)
	if err := st.(core.StreamRenewer).Renew(renewalFor(t, k1, s.Info(), st, "open-1", renewed, nil)); err != nil {
		t.Fatal(err)
	}
	b := target.Binding{SessionID: s.Info().SessionID, ConnectionEpoch: s.Info().ConnectionEpoch}
	waitFor(t, "renewed deadline", func() bool {
		d, ok := a.deadlines.Deadline(b, st.ID())
		return ok && d.Equal(renewed)
	})
	time.Sleep(time.Until(until.Add(500 * time.Millisecond)))
	if !echoes(st) {
		t.Fatal("stream closed at its first deadline after a renewal")
	}
	if code := closedWith(t, st); code != core.CloseUnauthenticated {
		t.Fatalf("close code %d, want 4401", code)
	}
	if time.Now().Before(renewed) {
		t.Fatal("stream closed before its renewed deadline")
	}
}

// TestAgentAdvertisesStreamRenewal: the Hello lists the stream_renewal
// feature.
func TestAgentAdvertisesStreamRenewal(t *testing.T) {
	h := newFakeHub(t, newTestKey(t, "k1").public)
	startAgent(t, h, nil)
	hello := h.nextHello(t)
	if !slices.Contains(hello.GetCapabilities().GetFeatures(), core.FeatureStreamRenewal) {
		t.Fatalf("features %v lack %q", hello.GetCapabilities().GetFeatures(), core.FeatureStreamRenewal)
	}
}

// TestAgentRenewalUnknownKidDoesNotStallFrames: while the key fetch for a
// renewal's unknown kid is in flight, the session keeps processing frames:
// the stream echoes and another valid renewal is applied. When the fetch
// returns the new key, the queued renewal is verified and applied.
func TestAgentRenewalUnknownKidDoesNotStallFrames(t *testing.T) {
	k1, k2 := newTestKey(t, "k1"), newTestKey(t, "k2")
	h := newFakeHub(t, k1.public)
	release := make(chan struct{})
	var blocking atomic.Bool
	var blocked atomic.Int64
	serve := h.srv.Config.Handler
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocking.Load() && strings.HasSuffix(r.URL.Path, "/grant-keys") {
			blocked.Add(1)
			<-release
		}
		serve.ServeHTTP(w, r)
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a, _ := startAgent(t, h, nil)
	s := h.nextSession(t)
	waitKey(t, a, "k1")
	now := time.Now().Truncate(time.Second)
	st := mustOpen(t, s, tcpOpenWith(t, k1, s.Info(), "launch-1", echoListener(t), func(c *grant.Claims) {
		c.JTI = "open-1"
		c.AuthzUntil, c.AuthzInterval = now.Add(time.Minute), time.Hour
	}), "stream with a deadline")
	b := target.Binding{SessionID: s.Info().SessionID, ConnectionEpoch: s.Info().ConnectionEpoch}
	deadline := func() time.Time { d, _ := a.deadlines.Deadline(b, st.ID()); return d }

	h.set(func(h *fakeHub) { h.keys = []grant.PublicKey{k1.public, k2.public} })
	blocking.Store(true)
	r := st.(core.StreamRenewer)
	viaK2 := now.Add(30 * time.Minute)
	if err := r.Renew(renewalFor(t, k2, s.Info(), st, "open-1", viaK2, nil)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the key fetch for the unknown kid", func() bool { return blocked.Load() == 1 })

	// The fetch is blocked; frames still flow.
	if !echoes(st) {
		t.Fatal("stream stalled while a key fetch was in flight")
	}
	viaK1 := now.Add(10 * time.Minute)
	if err := r.Renew(renewalFor(t, k1, s.Info(), st, "open-1", viaK1, nil)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the k1 renewal applied during the fetch", func() bool { return deadline().Equal(viaK1) })

	close(release)
	waitFor(t, "the queued k2 renewal applied after the fetch", func() bool { return deadline().Equal(viaK2) })
}

// TestAgentFirstKeyFetchAtOnceAndRetries: the first key fetch runs at
// once rather than after the refresh interval. A failure stops nothing:
// the fetch is retried with backoff.
func TestAgentFirstKeyFetchAtOnceAndRetries(t *testing.T) {
	h := newFakeHub(t, newTestKey(t, "k1").public)
	var failing atomic.Bool
	failing.Store(true)
	serve := h.srv.Config.Handler
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			h.keyHits.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		serve.ServeHTTP(w, r)
	})
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	a, err := New(Options{
		HubURL: h.srv.URL, AgentID: testAgentID, ProjectID: testProjectID,
		Token: func() string { return "t" },
		Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.kickKeyRefresh() // as Run does before starting the loop
	go func() { a.refreshKeysLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	start := clk.Now()
	if !clk.WaitForTimer(waitTimeout, start.Add(keyRetryMin)) {
		t.Fatal("no retry armed after the first (failed) fetch")
	}
	if n := h.keyHits.Load(); n != 1 {
		t.Fatalf("%d fetches before any time passed, want 1", n)
	}
	if _, ok := a.Keys().Lookup("k1"); ok {
		t.Fatal("keys installed from a failed fetch")
	}
	failing.Store(false)
	clk.Advance(keyRetryMin)
	if !clk.WaitForTimer(waitTimeout, start.Add(keyRetryMin+KeyRefreshInterval)) {
		t.Fatal("normal cadence not restored after the retry succeeded")
	}
	if _, ok := a.Keys().Lookup("k1"); !ok {
		t.Fatal("retry did not install the keys")
	}
}
