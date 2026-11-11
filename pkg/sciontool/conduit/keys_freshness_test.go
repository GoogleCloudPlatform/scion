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
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Key-set freshness at the target (design v2.21.27 §3.5; criterion 29).

func waitKeyHits(t *testing.T, h *fakeHub, want int64) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for h.keyHits.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("key fetches = %d, want %d", h.keyHits.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func newLoopAgent(t *testing.T, h *fakeHub, clk *clock.Fake, mod func(*Options)) *Agent {
	t.Helper()
	opts := Options{HubURL: h.srv.URL, AgentID: testAgentID, ProjectID: testProjectID, Token: func() string { return "token-1" }, Clock: clk}
	if mod != nil {
		mod(&opts)
	}
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.refreshKeysLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return a
}

// TestKeyRefreshJitterBounds: each refresh period is within ±10% of
// KeyRefreshInterval, and periods vary.
func TestKeyRefreshJitterBounds(t *testing.T) {
	lo, hi := KeyRefreshInterval*9/10, KeyRefreshInterval*11/10
	seen := map[time.Duration]bool{}
	for range 1000 {
		d := jitterKeyRefresh(KeyRefreshInterval)
		if d < lo || d > hi {
			t.Fatalf("period %v outside [%v, %v]", d, lo, hi)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("refresh periods do not vary")
	}
}

// TestAgentKeyRefreshUsesJitter: the loop arms each refresh at the jittered
// period.
func TestAgentKeyRefreshUsesJitter(t *testing.T) {
	h := newFakeHub(t)
	start := time.Unix(1_700_000_000, 0)
	clk := clock.NewFake(start)
	newLoopAgent(t, h, clk, func(o *Options) {
		o.keyJitter = func(d time.Duration) time.Duration { return d + d/10 }
	})
	if !clk.WaitForTimer(waitTimeout, start.Add(KeyRefreshInterval+KeyRefreshInterval/10)) {
		t.Fatal("the first refresh is not armed at the jittered period")
	}
}

// TestAgentGrantKeysHintCoalesced: a hint is served at once when no fetch
// started in the last GrantKeysHintInterval; hints inside that window are
// served by one fetch at its end; afterwards the next hint is served at
// once again.
func TestAgentGrantKeysHintCoalesced(t *testing.T) {
	h := newFakeHub(t)
	start := time.Unix(1_700_000_000, 0)
	clk := clock.NewFake(start)
	a := newLoopAgent(t, h, clk, nil)
	ctx := context.Background()

	a.handleGrantKeysChanged(ctx)
	waitKeyHits(t, h, 1)
	waitPending(t, clk, 1) // the periodic refresh is armed

	clk.Advance(time.Second)
	a.handleGrantKeysChanged(ctx)
	if !clk.WaitForTimer(waitTimeout, start.Add(GrantKeysHintInterval)) {
		t.Fatal("a hint inside the window is not scheduled at its end")
	}
	a.handleGrantKeysChanged(ctx) // coalesces with the scheduled fetch
	clk.Advance(GrantKeysHintInterval - time.Second - time.Millisecond)
	if n := h.keyHits.Load(); n != 1 {
		t.Fatalf("fetched inside the window (%d fetches)", n)
	}
	clk.Advance(time.Millisecond)
	waitKeyHits(t, h, 2)
	waitPending(t, clk, 1)

	clk.Advance(3 * GrantKeysHintInterval)
	if n := h.keyHits.Load(); n != 2 {
		t.Fatalf("coalesced hints caused %d fetches, want 2", n)
	}
	a.handleGrantKeysChanged(ctx)
	waitKeyHits(t, h, 3)
}

// TestAgentGrantKeysHintDuringFetchNotDropped: a hint that arrives while a
// fetch is running gets a fetch of its own after it.
func TestAgentGrantKeysHintDuringFetchNotDropped(t *testing.T) {
	h := newFakeHub(t)
	start := time.Unix(1_700_000_000, 0)
	clk := clock.NewFake(start)
	entered, release := make(chan struct{}, 4), make(chan struct{})
	var gate atomic.Bool
	gate.Store(true)
	serve := h.srv.Config.Handler
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gate.Load() {
			entered <- struct{}{}
			<-release
		}
		serve.ServeHTTP(w, r)
	})
	a := newLoopAgent(t, h, clk, nil)
	ctx := context.Background()

	a.handleGrantKeysChanged(ctx)
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("no fetch")
	}
	a.handleGrantKeysChanged(ctx) // arrives during the fetch
	gate.Store(false)
	close(release)
	waitKeyHits(t, h, 1)
	if !clk.WaitForTimer(waitTimeout, start.Add(GrantKeysHintInterval)) {
		t.Fatal("the hint that arrived during the fetch is not scheduled")
	}
	clk.Advance(GrantKeysHintInterval)
	waitKeyHits(t, h, 2)
}

// TestAgentKeysStaleInEnforceMode: in enforce mode, once the last
// successful refresh is older than KeyStaleAfter, the verification key set
// holds no key (every grant is refused) until a refresh succeeds; a failed
// refresh does not count. The off-mode pair is TestAgentKeysNotStaleInOffMode.
func TestAgentKeysStaleInEnforceMode(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.deadlines = deadlinesEnforce })
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	a, err := New(Options{HubURL: h.srv.URL, AgentID: testAgentID, ProjectID: testProjectID, Token: func() string { return "t" }, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	keys := freshKeys{a}
	if _, ok := keys.Lookup("k1"); ok {
		t.Fatal("a key verifies before any refresh")
	}
	if err := a.RefreshKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk.Advance(KeyStaleAfter)
	if _, ok := keys.Lookup("k1"); !ok {
		t.Fatal("stale at exactly KeyStaleAfter")
	}
	clk.Advance(time.Second)
	if _, ok := keys.Lookup("k1"); ok {
		t.Fatal("a key verifies after KeyStaleAfter without a refresh")
	}

	serve := h.srv.Config.Handler
	h.srv.Config.Handler = http.NotFoundHandler()
	if err := a.RefreshKeys(context.Background()); err == nil {
		t.Fatal("RefreshKeys succeeded against a 404")
	}
	if _, ok := keys.Lookup("k1"); ok {
		t.Fatal("a failed refresh made the key set fresh")
	}
	h.srv.Config.Handler = serve
	if err := a.RefreshKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.Lookup("k1"); !ok {
		t.Fatal("a successful refresh did not make the key set fresh")
	}

}

// TestAgentKeysNotStaleInOffMode: the pair of TestAgentKeysStaleInEnforceMode.
// A target in off mode keeps verifying with keys whose last successful
// refresh is older than KeyStaleAfter, as before.
func TestAgentKeysNotStaleInOffMode(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.deadlines = deadlinesOff })
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	a, err := New(Options{HubURL: h.srv.URL, AgentID: testAgentID, ProjectID: testProjectID, Token: func() string { return "t" }, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RefreshKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.srv.Config.Handler = http.NotFoundHandler() // no refresh succeeds from here on
	clk.Advance(2 * KeyStaleAfter)
	if err := a.RefreshKeys(context.Background()); err == nil {
		t.Fatal("RefreshKeys succeeded against a 404")
	}
	if _, ok := (freshKeys{a}).Lookup("k1"); !ok {
		t.Fatal("in off mode a stale key set stopped verifying")
	}
}

// TestAgentStaleKeysRefuseOpenGrants: in enforce mode, once the last
// successful key refresh is older than KeyStaleAfter, a stream open is
// refused even with a grant from a known key; after a refresh succeeds the
// same kind of open is accepted.
func TestAgentStaleKeysRefuseOpenGrants(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	h.set(func(h *fakeHub) { h.deadlines = deadlinesEnforce })
	var failing atomic.Bool
	serve := h.srv.Config.Handler
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() && r.URL.Path == "/api/v1/conduit/grant-keys" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		serve.ServeHTTP(w, r)
	})
	fa := startFakeAgent(t, h, nil)
	s := h.nextSession(t)
	fa.keysFetched(t, true)
	port := echoListener(t)
	open := func(jti string) *conduitv1.StreamOpen {
		now := fa.clk.Now()
		return openAt(t, k1, s, port, now, withDeadline(jti, now.Add(time.Hour), time.Hour))
	}
	mustOpen(t, s, open("open-fresh"), "open with fresh keys")

	failing.Store(true)
	fa.clk.Advance(KeyStaleAfter + time.Second)
	mustRefuse(t, s, open("open-stale"), "open with keys not refreshed within KeyStaleAfter")

	failing.Store(false)
	fa.clk.Advance(UnknownKidFetchInterval + time.Second) // past the fetch budget the refusal used
	mustOpen(t, s, open("open-refreshed"), "open after a successful refresh")
}

// TestAgentStaleKeysRenewalReportedAsKeysStale (66b2-T1): in enforce mode,
// a renewal refused because the key set was not refreshed within
// KeyStaleAfter (every kid then reads as unknown) is reported as
// errKeysStale, log reason keys_stale, never as grant.ErrUnknownKey. With
// a fresh key set, or in off mode, an unknown kid is still ErrUnknownKey.
func TestAgentStaleKeysRenewalReportedAsKeysStale(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	var got []error
	a, err := New(Options{HubURL: "http://127.0.0.1:1", AgentID: testAgentID, ProjectID: testProjectID, Token: func() string { return "t" }, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	a.opts.onRenewal = func(_ uint32, err error) { got = append(got, err) }
	unknown := fmt.Errorf("verify renewal: %w", grant.ErrUnknownKey)

	a.enforceDeadlines.Store(true)
	a.lastKeyRefresh.Store(clk.Now().UnixNano())
	clk.Advance(KeyStaleAfter + time.Second)
	a.renewalDone(7, unknown)

	a.lastKeyRefresh.Store(clk.Now().UnixNano())
	a.renewalDone(8, unknown)

	a.enforceDeadlines.Store(false)
	clk.Advance(2 * KeyStaleAfter)
	a.renewalDone(9, unknown)

	if len(got) != 3 {
		t.Fatalf("renewal outcomes %v, want 3", got)
	}
	if !errors.Is(got[0], errKeysStale) || errors.Is(got[0], grant.ErrUnknownKey) {
		t.Fatalf("stale key set: %v, want errKeysStale and not ErrUnknownKey", got[0])
	}
	for i, err := range got[1:] {
		if !errors.Is(err, grant.ErrUnknownKey) || errors.Is(err, errKeysStale) {
			t.Fatalf("case %d: %v, want ErrUnknownKey", i+1, err)
		}
	}
}
