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
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
// target (design v2.21.17 §3.5, "Target-side deadlines"; criterion 29).
// The agent runs on a fake clock; keepalive timers are set far out so
// moving the clock only fires the timers under test.

// fakeStart is where every test's fake clock starts.
var fakeStart = time.Unix(1_800_000_000, 0)

// renewalOutcome is one renewal result reported by the agent.
type renewalOutcome struct {
	streamID uint32
	err      error
}

// fakeAgent is an Agent running on a fake clock against a fakeHub.
type fakeAgent struct {
	a        *Agent
	h        *fakeHub
	clk      *clock.Fake
	done     <-chan error
	renewals chan renewalOutcome
}

func startFakeAgent(t *testing.T, h *fakeHub, mod func(*Options)) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{h: h, clk: clock.NewFake(fakeStart), renewals: make(chan renewalOutcome, 64)}
	fa.a, fa.done = startAgent(t, h, func(o *Options) {
		o.Clock = fa.clk
		o.Session.PingInterval = time.Hour
		o.Session.PongWait = 24 * time.Hour
		o.onRenewal = func(id uint32, err error) { fa.renewals <- renewalOutcome{id, err} }
		if mod != nil {
			mod(o)
		}
	})
	return fa
}

// keysFetched waits for the outcome of the agent's first key fetch: on
// success the loop arms the next refresh at KeyRefreshInterval, on
// failure the retry at keyRetryMin. No timer is armed before that fetch.
func (fa *fakeAgent) keysFetched(t *testing.T, ok bool) {
	t.Helper()
	next := fakeStart.Add(KeyRefreshInterval)
	if !ok {
		next = fakeStart.Add(keyRetryMin)
	}
	if !fa.clk.WaitForTimer(waitTimeout, next) {
		t.Fatalf("first key fetch did not finish (ok=%v)", ok)
	}
}

// nextRenewal waits for the next renewal outcome.
func (fa *fakeAgent) nextRenewal(t *testing.T) renewalOutcome {
	t.Helper()
	select {
	case r := <-fa.renewals:
		return r
	case <-time.After(waitTimeout):
		t.Fatal("no renewal outcome")
		return renewalOutcome{}
	}
}

// deadline returns the tracked deadline of st on s.
func (fa *fakeAgent) deadline(t *testing.T, s core.LocalSession, st core.Stream) time.Time {
	t.Helper()
	d, ok := fa.a.deadlines.Deadline(target.Binding{SessionID: s.Info().SessionID, ConnectionEpoch: s.Info().ConnectionEpoch}, st.ID())
	if !ok {
		t.Fatal("stream not tracked")
	}
	return d
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

// openAt is a TCP StreamOpen to port with a grant signed by key, valid
// from now on the fake clock; mut adjusts the claims.
func openAt(t *testing.T, key testKey, s core.LocalSession, port int, now time.Time, mut func(*grant.Claims)) *conduitv1.StreamOpen {
	t.Helper()
	return tcpOpenWith(t, key, s.Info(), "launch-1", port, func(c *grant.Claims) {
		c.NotBefore, c.Expiry = now, now.Add(50*time.Second)
		if mut != nil {
			mut(c)
		}
	})
}

// withDeadline gives a grant an authorization deadline (and a jti).
func withDeadline(jti string, until time.Time, interval time.Duration) func(*grant.Claims) {
	return func(c *grant.Claims) { c.JTI, c.AuthzUntil, c.AuthzInterval = jti, until, interval }
}

// renewalFor mints a renewal minted at now for the stream st on s, opened
// with a grant whose jti is openJTI.
func renewalFor(t *testing.T, key testKey, s core.LocalSession, st core.Stream, now time.Time, openJTI string, until time.Time, mut func(*grant.RenewalClaims)) []byte {
	t.Helper()
	info := s.Info()
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

// captureLog sends the sciontool log to a temp file for the test and
// returns a function that reads it.
func captureLog(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.log")
	log.SetLogPath(path)
	t.Cleanup(func() { log.SetLogPath("") })
	return func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

// blockKeys makes the hub's grant-key route wait until release is
// closed while blocking is set; entered receives each blocked request.
func blockKeys(t *testing.T, h *fakeHub) (blocking *atomic.Bool, entered <-chan struct{}, release func()) {
	t.Helper()
	blocking = new(atomic.Bool)
	ent := make(chan struct{}, 64)
	rel := make(chan struct{})
	var once sync.Once
	serve := h.srv.Config.Handler
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocking.Load() && strings.HasSuffix(r.URL.Path, "/grant-keys") {
			ent <- struct{}{}
			<-rel
		}
		serve.ServeHTTP(w, r)
	})
	release = func() { once.Do(func() { close(rel) }) }
	t.Cleanup(release)
	return blocking, ent, release
}

// waitEntered waits for one blocked key fetch.
func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("no key fetch reached the hub")
	}
}

// TestAgentWelcomeKeysNotTrusted: a key listed only in the Welcome is not
// trusted; a grant it signs is refused. A key from the hub's key fetch is
// trusted whether or not the Welcome also lists it.
func TestAgentWelcomeKeysNotTrusted(t *testing.T) {
	k1, k2 := newTestKey(t, "k1"), newTestKey(t, "k2")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.welcomeKeys = []grant.PublicKey{k1.public, k2.public} })
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	port := echoListener(t)

	mustRefuse(t, s, openAt(t, k2, s, port, fakeStart, nil), "grant signed by a Welcome-only key")
	if _, ok := fa.a.Keys().Lookup("k2"); ok {
		t.Fatal("a Welcome-only key was installed")
	}
	mustOpen(t, s, openAt(t, k1, s, port, fakeStart, nil), "grant signed by a fetched key")
}

// TestAgentRefusesGrantsWithoutFetchedKeys: while no key fetch has
// succeeded, every grant is refused, even one signed by a key the Welcome
// lists, and the failed fetches stop nothing.
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
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, false)
	mustRefuse(t, s, openAt(t, k1, s, echoListener(t), fakeStart, nil), "grant with no fetched keys")
	select {
	case err := <-fa.done:
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
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	port := echoListener(t)
	base := h.keyHits.Load()
	for _, kid := range []string{"u1", "u2", "u3", "u4", "u5"} {
		mustRefuse(t, s, openAt(t, newTestKey(t, kid), s, port, fakeStart, nil), "unknown kid "+kid)
	}
	if n := h.keyHits.Load() - base; n != 1 {
		t.Fatalf("%d key fetches for five unknown kids, want 1", n)
	}
	// Once the interval has passed, another unknown kid gets a fetch.
	fa.clk.Advance(UnknownKidFetchInterval)
	mustRefuse(t, s, openAt(t, newTestKey(t, "u6"), s, port, fa.clk.Now(), nil), "unknown kid u6")
	if n := h.keyHits.Load() - base; n != 2 {
		t.Fatalf("%d key fetches after the interval, want 2", n)
	}
}

// TestAgentRotatedKeyAcceptedAfterOneFetch: a grant signed by a key the
// hub has just published is accepted after one key fetch.
func TestAgentRotatedKeyAcceptedAfterOneFetch(t *testing.T) {
	k1, k2 := newTestKey(t, "k1"), newTestKey(t, "k2")
	h := newFakeHub(t, k1.public)
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	base := h.keyHits.Load()
	h.set(func(h *fakeHub) { h.keys = []grant.PublicKey{k1.public, k2.public} })
	mustOpen(t, s, openAt(t, k2, s, echoListener(t), fakeStart, nil), "grant signed by the new key")
	if n := h.keyHits.Load() - base; n != 1 {
		t.Fatalf("%d key fetches, want 1", n)
	}
}

// TestAgentEnforceModeRequiresDeadline: once the key fetch sets enforce,
// a user stream's grant without authz_until is refused without consuming
// its jti; with authz_until it is accepted, and a stream opened by an
// agent principal needs none.
func TestAgentEnforceModeRequiresDeadline(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.deadlines = "enforce" })
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	port := echoListener(t)
	now := fakeStart

	mustRefuse(t, s, openAt(t, k1, s, port, now, func(c *grant.Claims) { c.JTI = "open-1" }), "user grant without authz_until")
	// The refused grant did not consume its jti.
	mustOpen(t, s, openAt(t, k1, s, port, now, withDeadline("open-1", now.Add(time.Hour), time.Hour)), "user grant with authz_until and the same jti")
	mustOpen(t, s, openAt(t, k1, s, port, now, func(c *grant.Claims) { c.Subject = "agent:agent-2" }), "agent grant without authz_until")
}

// TestAgentEnforceModeNotLoweredBySession: the enforcement mode and the
// keys come only from the key fetch. A later session whose Welcome lists
// no keys and carries nothing about deadlines leaves enforcement on and
// the fetched keys trusted.
func TestAgentEnforceModeNotLoweredBySession(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.deadlines = "enforce" })
	fa := startFakeAgent(t, h, nil)
	first := h.nextSession(t)
	fa.keysFetched(t, true)

	h.set(func(h *fakeHub) { h.welcomeKeys = []grant.PublicKey{} })
	// noBackoff: the agent redials at once, without a timer.
	_ = first.CloseWithCode(core.CloseRelayRestart, "relay_restart")
	s := h.nextSession(t)
	port := echoListener(t)
	now := fa.clk.Now()
	mustRefuse(t, s, openAt(t, k1, s, port, now, nil), "user grant without authz_until on the new session")
	mustOpen(t, s, openAt(t, k1, s, port, now, withDeadline("open-2", now.Add(time.Hour), time.Hour)), "user grant with authz_until on the new session")
	if !fa.a.enforceDeadlines.Load() {
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
			fa := startFakeAgent(t, h, nil)
			s := h.nextSession(t)
			fa.keysFetched(t, true)
			mustOpen(t, s, openAt(t, k1, s, echoListener(t), fakeStart, nil), "grant without authz_until")
		})
	}
}

// TestAgentStreamClosesAtDeadline: a stream whose grant carries
// authz_until is closed with 4401 at authz_until + 30s, not before, when
// no valid renewal arrives. Refused renewals (another open_jti, another
// iss, past the cap) neither extend the deadline nor close the stream
// early, and each is logged.
func TestAgentStreamClosesAtDeadline(t *testing.T) {
	readLog := captureLog(t)
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	now := fakeStart
	until := now.Add(10 * time.Second)
	st := mustOpen(t, s, openAt(t, k1, s, echoListener(t), now, withDeadline("open-1", until, time.Minute)), "stream with a deadline")

	r := st.(core.StreamRenewer)
	refused := [][]byte{
		renewalFor(t, k1, s, st, now, "open-2", until.Add(20*time.Second), nil),
		renewalFor(t, k1, s, st, now, "open-1", until.Add(20*time.Second), func(c *grant.RenewalClaims) { c.Issuer = "other-hub" }),
		renewalFor(t, k1, s, st, now, "open-1", now.Add(time.Minute+target.DeadlineGrace+time.Second), nil),
	}
	for _, tok := range refused {
		if err := r.Renew(tok); err != nil {
			t.Fatal(err)
		}
		if out := fa.nextRenewal(t); out.err == nil {
			t.Fatal("an invalid renewal was applied")
		}
	}
	if got := fa.deadline(t, s, st); !got.Equal(until) {
		t.Fatalf("deadline %v after refused renewals, want %v", got, until)
	}
	if n := strings.Count(readLog(), "renewal for stream"); n != len(refused) {
		t.Fatalf("%d refused-renewal log lines, want %d", n, len(refused))
	}

	fa.clk.Advance(until.Add(target.DeadlineGrace - time.Second).Sub(fa.clk.Now()))
	if !echoes(st) {
		t.Fatal("stream closed before authz_until + 30s")
	}
	fa.clk.Advance(time.Second)
	if code := closedWith(t, st); code != core.CloseUnauthenticated {
		t.Fatalf("close code %d, want 4401", code)
	}
}

// TestAgentRenewalExtendsDeadline: a valid renewal moves the stream's
// deadline: the stream outlives its first deadline and closes with 4401
// at the renewed one (+30s).
func TestAgentRenewalExtendsDeadline(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	now := fakeStart
	until := now.Add(10 * time.Second)
	st := mustOpen(t, s, openAt(t, k1, s, echoListener(t), now, withDeadline("open-1", until, time.Minute)), "stream with a deadline")

	renewed := until.Add(20 * time.Second)
	if err := st.(core.StreamRenewer).Renew(renewalFor(t, k1, s, st, now, "open-1", renewed, nil)); err != nil {
		t.Fatal(err)
	}
	if out := fa.nextRenewal(t); out.err != nil || out.streamID != st.ID() {
		t.Fatalf("renewal outcome %+v", out)
	}
	if got := fa.deadline(t, s, st); !got.Equal(renewed) {
		t.Fatalf("deadline %v, want %v", got, renewed)
	}
	fa.clk.Advance(until.Add(target.DeadlineGrace + time.Second).Sub(fa.clk.Now()))
	if !echoes(st) {
		t.Fatal("stream closed at its first deadline after a renewal")
	}
	fa.clk.Advance(renewed.Add(target.DeadlineGrace - time.Second).Sub(fa.clk.Now()))
	if !echoes(st) {
		t.Fatal("stream closed before its renewed deadline + 30s")
	}
	fa.clk.Advance(time.Second)
	if code := closedWith(t, st); code != core.CloseUnauthenticated {
		t.Fatalf("close code %d, want 4401", code)
	}
}

// TestAgentAdvertisesStreamRenewal: the Hello lists the stream_renewal
// feature.
func TestAgentAdvertisesStreamRenewal(t *testing.T) {
	h := newFakeHub(t, newTestKey(t, "k1").public)
	startFakeAgent(t, h, nil)
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
	blocking, entered, release := blockKeys(t, h)
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	now := fakeStart
	st := mustOpen(t, s, openAt(t, k1, s, echoListener(t), now, withDeadline("open-1", now.Add(time.Minute), time.Hour)), "stream with a deadline")

	h.set(func(h *fakeHub) { h.keys = []grant.PublicKey{k1.public, k2.public} })
	blocking.Store(true)
	r := st.(core.StreamRenewer)
	viaK2 := now.Add(30 * time.Minute)
	if err := r.Renew(renewalFor(t, k2, s, st, now, "open-1", viaK2, nil)); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered)

	// The fetch is blocked; frames still flow.
	if !echoes(st) {
		t.Fatal("stream stalled while a key fetch was in flight")
	}
	viaK1 := now.Add(10 * time.Minute)
	if err := r.Renew(renewalFor(t, k1, s, st, now, "open-1", viaK1, nil)); err != nil {
		t.Fatal(err)
	}
	if out := fa.nextRenewal(t); out.err != nil {
		t.Fatalf("k1 renewal during the fetch: %v", out.err)
	}
	if got := fa.deadline(t, s, st); !got.Equal(viaK1) {
		t.Fatalf("deadline %v during the fetch, want %v", got, viaK1)
	}

	release()
	if out := fa.nextRenewal(t); out.err != nil {
		t.Fatalf("queued k2 renewal: %v", out.err)
	}
	if got := fa.deadline(t, s, st); !got.Equal(viaK2) {
		t.Fatalf("deadline %v after the fetch, want %v", got, viaK2)
	}
}

// TestAgentRenewalQueueFull: while a key fetch is blocked, renewals that
// need one beyond the queue's capacity are refused and logged, the session
// keeps echoing, and a refused renewal never moves the deadline, even
// after the fetch returns its key.
func TestAgentRenewalQueueFull(t *testing.T) {
	readLog := captureLog(t)
	k1, k2 := newTestKey(t, "k1"), newTestKey(t, "k2")
	h := newFakeHub(t, k1.public)
	blocking, entered, release := blockKeys(t, h)
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	now := fakeStart
	st := mustOpen(t, s, openAt(t, k1, s, echoListener(t), now, withDeadline("open-1", now.Add(time.Minute), time.Hour)), "stream with a deadline")

	h.set(func(h *fakeHub) { h.keys = []grant.PublicKey{k1.public, k2.public} })
	blocking.Store(true)
	r := st.(core.StreamRenewer)
	// The first unknown-kid renewal takes the worker, which blocks in the
	// key fetch.
	if err := r.Renew(renewalFor(t, k2, s, st, now, "open-1", now.Add(2*time.Minute), nil)); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered)
	// renewalQueueSize more fill the queue; the next ones overflow.
	queued := now.Add(2 * time.Minute)
	for i := range renewalQueueSize {
		queued = now.Add(2*time.Minute + time.Duration(i+1)*time.Second)
		if err := r.Renew(renewalFor(t, k2, s, st, now, "open-1", queued, nil)); err != nil {
			t.Fatal(err)
		}
	}
	overflow := now.Add(50 * time.Minute)
	const extra = 2
	for range extra {
		if err := r.Renew(renewalFor(t, k2, s, st, now, "open-1", overflow, nil)); err != nil {
			t.Fatal(err)
		}
		if out := fa.nextRenewal(t); !errors.Is(out.err, errRenewalQueueFull) {
			t.Fatalf("overflowing renewal: %v, want errRenewalQueueFull", out.err)
		}
	}
	if !strings.Contains(readLog(), "too many renewals waiting for a key fetch") {
		t.Fatal("no log line for the refused renewal")
	}
	if !echoes(st) {
		t.Fatal("stream stalled with a full renewal queue")
	}

	release()
	for range 1 + renewalQueueSize {
		if out := fa.nextRenewal(t); out.err != nil {
			t.Fatalf("queued renewal: %v", out.err)
		}
	}
	if got := fa.deadline(t, s, st); !got.Equal(queued) {
		t.Fatalf("deadline %v, want %v (the last queued renewal; never the overflow %v)", got, queued, overflow)
	}
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
