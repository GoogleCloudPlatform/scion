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

package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// testCredential builds an unsigned HS256-shaped credential for agent-1,
// issued at iat and expiring 10h later.
func testCredential(t *testing.T, jti string, iat time.Time) string {
	t.Helper()
	return testCredentialFor(t, "agent-1", jti, iat)
}

// testCredentialFor is testCredential with a given subject.
func testCredentialFor(t *testing.T, sub, jti string, iat time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	c, err := json.Marshal(map[string]any{
		"sub": sub, "jti": jti,
		"iat": iat.Unix(), "exp": iat.Add(10 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeToString(h) + "." + enc.EncodeToString(c) + "." + enc.EncodeToString(make([]byte, 32))
}

// credHub is a test hub. It accepts the credentials in accepted on every
// request; the agent record read (the credential check) can instead be
// made to answer with checkStatus. It records every credential presented
// and counts checks.
type credHub struct {
	srv         *httptest.Server
	mu          sync.Mutex
	accepted    map[string]bool
	checkStatus int  // 0: 200 or 401 by accepted; else this status
	checkHang   bool // the check never answers (until the client gives up)
	seen        []string
	checks      []string
	elsewhere   int
}

func newCredHub(t *testing.T, accept ...string) *credHub {
	t.Helper()
	h := &credHub{accepted: map[string]bool{}}
	for _, a := range accept {
		h.accepted[a] = true
	}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			h.mu.Lock()
			h.elsewhere++
			h.mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"agent-1"}`))
			return
		}
		tok := r.Header.Get("X-Scion-Agent-Token")
		h.mu.Lock()
		h.seen = append(h.seen, tok)
		ok := h.accepted[tok]
		isCheck := r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/agent-1"
		status, hang := h.checkStatus, h.checkHang
		if isCheck {
			h.checks = append(h.checks, tok)
		}
		h.mu.Unlock()
		if isCheck && hang {
			<-r.Context().Done()
			return
		}
		if isCheck && status >= 300 && status < 400 {
			// The body is the agent record, so only the status decides.
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"id":"agent-1"}`))
			return
		}
		if isCheck && status != 0 {
			w.WriteHeader(status)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if isCheck {
			_, _ = w.Write([]byte(`{"id":"agent-1"}`))
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *credHub) set(fn func(h *credHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

func (h *credHub) presented(tok string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.seen {
		if s == tok {
			n++
		}
	}
	return n
}

func (h *credHub) checkCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.checks)
}

func (h *credHub) checkCountExcept(tok string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.checks {
		if c != tok {
			n++
		}
	}
	return n
}

func (h *credHub) checked(tok string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.checks {
		if c == tok {
			return true
		}
	}
	return false
}

type resetHarness struct {
	t      *testing.T
	hub    *credHub
	client *hub.Client
	r      *authResetter
	cancel context.CancelFunc
	done   <-chan struct{}
	now    time.Time
	oldTok string
	newTok string
}

// newResetHarness sets up an agent whose in-memory and on-disk credential
// is oldTok, with a running refresh loop, against a hub that accepts only
// newTok (the old one was revoked by a reissue) plus any extra values.
func newResetHarness(t *testing.T, alsoAccept ...string) *resetHarness {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	now := time.Now().Truncate(time.Second)
	h := &resetHarness{
		t:      t,
		now:    now,
		oldTok: testCredential(t, "old", now.Add(-time.Hour)),
		newTok: testCredential(t, "new", now),
	}
	if err := hub.WriteTokenFile(h.oldTok, 0, 0); err != nil {
		t.Fatal(err)
	}
	h.hub = newCredHub(t, append([]string{h.newTok}, alsoAccept...)...)
	h.client = hub.NewClientWithConfig(h.hub.srv.URL, h.oldTok, "agent-1")
	h.startClient()
	return h
}

// startClient starts the refresh loop and builds the resetter around
// h.client.
func (h *resetHarness) startClient() {
	ctx, cancel := context.WithCancel(context.Background())
	done := h.client.StartTokenRefresh(ctx, &hub.TokenRefreshConfig{RefreshAt: h.now.Add(5 * time.Hour)})
	h.cancel, h.done = cancel, done
	h.r = &authResetter{
		hubClient:          h.client,
		tokenRefreshCancel: &h.cancel,
		tokenRefreshDone:   &h.done,
		statusHandler:      &handlers.StatusHandler{StatusPath: filepath.Join(h.t.TempDir(), "agent-info.json")},
		now:                time.Now,
	}
	h.t.Cleanup(func() {
		if h.cancel != nil {
			h.cancel()
			<-h.done
		}
	})
}

func (h *resetHarness) write(tok string) {
	h.t.Helper()
	if err := hub.WriteTokenFile(tok, 0, 0); err != nil {
		h.t.Fatal(err)
	}
}

func (h *resetHarness) usingCredential(tok string) bool {
	return h.client.CredentialInfo().Fingerprint == hub.TokenFingerprint(tok)
}

func loopRunning(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Recovery without the reset signal, through the watcher and reset path:
// the watcher sees the reissued credential, the file-change reset checks it
// with the hub and adopts it, and the shared client presents it.
func TestCredentialReload_NoSignal_EndToEnd(t *testing.T) {
	h := newResetHarness(t)
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	notify := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.r.watcher = startCredentialFileWatch(ctx, h.client, hub.TokenWatchConfig{Notify: notify, Clock: clk})
	changed := h.client.CredentialChanged()
	firstLoop := h.done

	// Before the reissue the hub refuses the agent.
	if err := h.client.Heartbeat(context.Background()); err == nil {
		t.Fatal("heartbeat with the revoked credential succeeded")
	}

	h.write(h.newTok) // the reissue; no signal follows
	start := clk.Now()
	tick := start.Add(hub.DefaultTokenWatchInterval)
	if !clk.WaitForTimer(5*time.Second, tick) {
		t.Fatal("watcher armed no check")
	}
	clk.Advance(hub.DefaultTokenWatchInterval)
	stable := tick.Add(hub.DefaultTokenWatchStableDelay)
	if !clk.WaitForTimer(5*time.Second, stable) {
		t.Fatal("watcher did not re-read for stability")
	}
	clk.Advance(hub.DefaultTokenWatchStableDelay)

	select {
	case <-notify:
	case <-time.After(5 * time.Second):
		t.Fatal("no credential file notification")
	}
	h.r.reset(authResetFileChange)

	if !h.usingCredential(h.newTok) {
		t.Fatal("the shared client did not switch to the new credential")
	}
	if h.hub.presented(h.newTok) == 0 {
		t.Fatal("the hub never saw the new credential (post-reset heartbeat)")
	}
	select {
	case <-changed:
	default:
		t.Fatal("loops waiting after a 401 were not woken")
	}
	if loopRunning(firstLoop) || h.done == firstLoop || !loopRunning(h.done) {
		t.Fatal("the refresh loop was not restarted exactly once")
	}
	if err := h.client.Heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat after the reload: %v", err)
	}
}

// A file change pending when SIGUSR2 adopts the same credential is a no-op.
func TestCredentialReload_SignalThenPendingFileChange(t *testing.T) {
	h := newResetHarness(t)
	h.write(h.newTok)
	h.r.reset(authResetSignal)
	if !h.usingCredential(h.newTok) {
		t.Fatal("signal reset did not adopt the credential")
	}
	loop := h.done
	beats := h.hub.presented(h.newTok)

	h.r.reset(authResetFileChange)
	if h.done != loop || !loopRunning(loop) {
		t.Fatal("the pending file change restarted the refresh loop")
	}
	if got := h.hub.presented(h.newTok); got != beats {
		t.Fatalf("the pending file change ran a reset (%d hub calls, want %d)", got, beats)
	}
}

// SIGUSR2 after the file change already adopted the credential runs a
// second, full reset to the same value, as a duplicate signal does today.
func TestCredentialReload_FileChangeThenSignal(t *testing.T) {
	h := newResetHarness(t)
	h.write(h.newTok)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("file change did not adopt the credential")
	}
	loop := h.done
	h.r.reset(authResetSignal)
	if !h.usingCredential(h.newTok) {
		t.Fatal("the signal changed the credential")
	}
	if h.done == loop || loopRunning(loop) || !loopRunning(h.done) {
		t.Fatal("the signal did not run a full reset")
	}
}

// The file changes to a partial write between the notification and the
// reset: nothing is adopted and the refresh loop keeps running.
func TestCredentialReload_StaleReadIsNotAdopted(t *testing.T) {
	h := newResetHarness(t)
	h.write(h.newTok) // what the watcher saw and reported
	partial := h.newTok[:len(h.newTok)-10]
	h.write(partial) // what the reset reads
	loop := h.done
	changed := h.client.CredentialChanged()

	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.oldTok) {
		t.Fatal("a partial credential replaced the working one")
	}
	if h.done != loop || !loopRunning(loop) {
		t.Fatal("the refresh loop was stopped for a refused credential")
	}
	select {
	case <-changed:
		t.Fatal("a refused credential woke the waiting loops")
	default:
	}
}

// After the hub accepted the new credential, the old one written
// back to the file is refused without a hub check, even though the hub
// would still accept it.
func TestCredentialReload_OlderAcceptedCredentialRefused(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.accepted[h.oldTok] = true })
	h.write(h.newTok)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("setup: new credential not adopted")
	}
	if got := h.client.VerifiedIssuedAt(); !got.Equal(h.now) {
		t.Fatalf("verified issue time = %v, want %v", got, h.now)
	}
	loop := h.done
	h.write(h.oldTok)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("an older credential replaced the current one")
	}
	if h.hub.checked(h.oldTok) {
		t.Fatal("the older credential was checked with the hub")
	}
	if h.done != loop || !loopRunning(loop) {
		t.Fatal("the refresh loop was stopped for a refused credential")
	}
	// A genuinely newer reissue is still adopted.
	newer := testCredential(t, "newer", h.now.Add(time.Minute))
	h.hub.set(func(c *credHub) { c.accepted[newer] = true })
	h.write(newer)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(newer) {
		t.Fatal("a newer reissue was not adopted")
	}
}

// A credential issued in the same second as the last accepted one is not
// newer, and is refused on the file path.
func TestCredentialReload_EqualIssuedAtRefused(t *testing.T) {
	h := newResetHarness(t)
	same := testCredential(t, "same-second", h.now)
	h.hub.set(func(c *credHub) { c.accepted[same] = true })
	h.write(h.newTok)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("setup: new credential not adopted")
	}
	h.write(same)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("a credential issued in the same second was adopted")
	}
}

// An expired credential is refused on the signal path before the refresh
// loop is stopped.
func TestCredentialReload_ExpiredOnSignalKeepsLoop(t *testing.T) {
	h := newResetHarness(t)
	h.write(testCredential(t, "expired", h.now.Add(-11*time.Hour)))
	loop := h.done
	h.r.reset(authResetSignal)
	if !h.usingCredential(h.oldTok) {
		t.Fatal("an expired credential replaced the working one")
	}
	if h.done != loop || !loopRunning(loop) {
		t.Fatal("the refresh loop was stopped for an expired credential")
	}
}

// When only the transport token file changes (reset-auth wrote it after
// the agent credential was adopted), the file-change reset adopts it.
func TestCredentialReload_TransportTokenWrittenLater(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	now := time.Now().Truncate(time.Second)
	tok := testCredential(t, "agent", now)
	if err := hub.WriteTokenFile(tok, 0, 0); err != nil {
		t.Fatal(err)
	}
	transportA := testCredential(t, "transport-a", now)
	if err := hub.WriteTransportTokenFile(transportA, 0, 0); err != nil {
		t.Fatal(err)
	}
	ch := newCredHub(t, tok)
	t.Setenv(hub.EnvHubEndpoint, ch.srv.URL)
	t.Setenv(hub.EnvHubURL, "")
	t.Setenv(hub.EnvAgentID, "agent-1")
	t.Setenv(hub.EnvHubToken, "")
	t.Setenv(hub.EnvLaunchID, "")
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, hub.TransportTokenFilePath())
	client := hub.NewClient()
	if client == nil {
		t.Fatal("no client")
	}
	h := &resetHarness{t: t, client: client, now: now, hub: ch}
	h.startClient()
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.r.watcher = startCredentialFileWatch(ctx, client, hub.TokenWatchConfig{Notify: make(chan struct{}, 1), Clock: clk})

	// No change yet: nothing adopted.
	if client.TransportTokenFileChanged() {
		t.Fatal("transport token counted as changed at start")
	}
	transportB := testCredential(t, "transport-b", now.Add(time.Second))
	if err := hub.WriteTransportTokenFile(transportB, 0, 0); err != nil {
		t.Fatal(err)
	}
	loop := h.done
	h.r.reset(authResetFileChange)
	if client.TransportTokenFileChanged() {
		t.Fatal("the new transport token was not adopted")
	}
	st, ok := hub.ReadTransportRefreshStatus()
	if !ok || st.Outcome != hub.TransportRefreshOutcomeReset {
		t.Fatalf("transport status = %+v, %v; want a recorded reset", st, ok)
	}
	if h.done != loop {
		t.Fatal("a transport-only change restarted the agent credential refresh loop")
	}
}

// assertKept checks that the agent still uses oldTok: in memory, on the
// next request, and with the refresh loop running.
func (h *resetHarness) assertKept(loop <-chan struct{}) {
	h.t.Helper()
	if !h.usingCredential(h.oldTok) {
		h.t.Fatal("the working credential was replaced")
	}
	if h.done != loop || !loopRunning(loop) {
		h.t.Fatal("the refresh loop was stopped")
	}
	before := h.hub.presented(h.oldTok)
	_ = h.client.Heartbeat(context.Background())
	if h.hub.presented(h.oldTok) == before {
		h.t.Fatal("the next request did not present the working credential")
	}
}

// A well-formed credential for this agent that the hub does not accept is
// not adopted, even with a far-future issue time, and a later genuine
// reissue still is.
func TestCredentialReload_UnacceptedCredentialRefused(t *testing.T) {
	h := newResetHarness(t)
	unaccepted := testCredential(t, "unaccepted", h.now.Add(100*time.Hour))
	loop := h.done
	changed := h.client.CredentialChanged()
	h.write(unaccepted)
	h.r.reset(authResetFileChange)
	if !h.hub.checked(unaccepted) {
		t.Fatal("the credential was not checked with the hub")
	}
	select {
	case <-changed:
		t.Fatal("waiting loops were woken for a refused credential")
	default:
	}
	h.assertKept(loop)
	if !h.client.VerifiedIssuedAt().IsZero() {
		t.Fatal("a refused credential moved the verified issue time")
	}
	h.write(h.newTok)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("a genuine reissue after a refused credential was not adopted")
	}
}

// A credential for another agent is refused on both paths without a hub
// check, even if the hub would accept it.
func TestCredentialReload_OtherAgentRefused(t *testing.T) {
	h := newResetHarness(t)
	other := testCredentialFor(t, "agent-2", "other", h.now)
	h.hub.set(func(c *credHub) { c.accepted[other] = true })
	loop := h.done
	h.write(other)
	h.r.reset(authResetFileChange)
	h.r.reset(authResetSignal)
	if h.hub.checkCount() != 0 {
		t.Fatal("a credential for another agent was checked with the hub")
	}
	h.assertKept(loop)
}

// When the hub cannot confirm the credential (an error status, no answer,
// a timeout, or an answer pointing elsewhere), the working credential is
// kept and still in use, and the verified issue time does not move.
func TestCredentialReload_UnconfirmedKeepsCurrentCredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *resetHarness)
	}{
		{"500", func(h *resetHarness) { h.hub.set(func(c *credHub) { c.checkStatus = http.StatusInternalServerError }) }},
		{"301", func(h *resetHarness) { h.hub.set(func(c *credHub) { c.checkStatus = http.StatusMovedPermanently }) }},
		{"302", func(h *resetHarness) { h.hub.set(func(c *credHub) { c.checkStatus = http.StatusFound }) }},
		{"307", func(h *resetHarness) { h.hub.set(func(c *credHub) { c.checkStatus = http.StatusTemporaryRedirect }) }},
		{"timeout", func(h *resetHarness) {
			h.hub.set(func(c *credHub) { c.checkHang = true })
			h.r.checkTimeout = 50 * time.Millisecond
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResetHarness(t)
			tc.setup(h)
			loop := h.done
			h.write(h.newTok)
			h.r.reset(authResetFileChange)
			if h.r.uncheckedAttempts != 1 {
				t.Fatalf("unchecked attempts = %d, want 1 (a re-check is scheduled)", h.r.uncheckedAttempts)
			}
			h.hub.set(func(c *credHub) { c.checkHang = false })
			h.assertKept(loop)
			if !h.client.VerifiedIssuedAt().IsZero() {
				t.Fatal("the verified issue time moved")
			}
			h.hub.mu.Lock()
			elsewhere := h.hub.elsewhere
			h.hub.mu.Unlock()
			if elsewhere != 0 {
				t.Fatal("the check followed the answer elsewhere")
			}
		})
	}
}

// With no hub reachable at all, the working credential is kept.
func TestCredentialReload_NoHubKeepsCurrentCredential(t *testing.T) {
	h := newResetHarness(t)
	loop := h.done
	h.hub.srv.Close()
	h.write(h.newTok)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.oldTok) {
		t.Fatal("the working credential was replaced")
	}
	if h.done != loop || !loopRunning(loop) {
		t.Fatal("the refresh loop was stopped")
	}
}

// watchStep advances the fake clock through one watcher check that sees a
// change (interval, then the stability re-read) and returns once the
// watcher has armed its next check.
func watchStep(t *testing.T, clk *clock.Fake) {
	t.Helper()
	tick := clk.Now().Add(hub.DefaultTokenWatchInterval)
	if !clk.WaitForTimer(5*time.Second, tick) {
		t.Fatal("watcher armed no check")
	}
	clk.Advance(tick.Sub(clk.Now()))
	stable := tick.Add(hub.DefaultTokenWatchStableDelay)
	if !clk.WaitForTimer(5*time.Second, stable) {
		t.Fatal("watcher did not re-read for stability")
	}
	clk.Advance(hub.DefaultTokenWatchStableDelay)
	if !clk.WaitForTimer(5*time.Second, stable.Add(hub.DefaultTokenWatchInterval)) {
		t.Fatal("watcher did not arm its next check")
	}
}

// watchQuietStep advances through one check that sees no change.
func watchQuietStep(t *testing.T, clk *clock.Fake) {
	t.Helper()
	tick := clk.Now().Add(hub.DefaultTokenWatchInterval)
	if !clk.WaitForTimer(5*time.Second, tick) {
		t.Fatal("watcher armed no check")
	}
	clk.Advance(tick.Sub(clk.Now()))
	if !clk.WaitForTimer(5*time.Second, tick.Add(hub.DefaultTokenWatchInterval)) {
		t.Fatal("watcher did not arm its next check")
	}
}

// startWatch starts a watcher on a fake clock for the harness and returns
// the clock and the notification channel.
func (h *resetHarness) startWatch() (*clock.Fake, chan struct{}) {
	h.t.Helper()
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	notify := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	h.r.watcher = startCredentialFileWatch(ctx, h.client, hub.TokenWatchConfig{Notify: notify, Clock: clk})
	return clk, notify
}

func pending(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A value the hub could not confirm is checked again after the retry
// delay, and adopted once the hub accepts it.
func TestCredentialReload_UnconfirmedRetriedThenAdopted(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.checkStatus = http.StatusInternalServerError })
	clk, notify := h.startWatch()
	h.write(h.newTok)
	watchStep(t, clk)
	if !pending(notify) {
		t.Fatal("no notification for the new credential")
	}
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.oldTok) {
		t.Fatal("adopted without confirmation")
	}
	h.hub.set(func(c *credHub) { c.checkStatus = 0 })
	// The first retry is due one interval later.
	watchStep(t, clk)
	if !pending(notify) {
		t.Fatal("the unconfirmed value was not reported again")
	}
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("the value was not adopted once the hub accepted it")
	}
}

// A new value is checked at the next check even while another value waits
// out a long retry delay.
func TestCredentialReload_NewValueNotDelayedByRetry(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.checkStatus = http.StatusInternalServerError })
	clk, notify := h.startWatch()
	first := testCredential(t, "first", h.now.Add(-time.Minute))
	h.write(first)
	watchStep(t, clk)
	_ = pending(notify)
	h.r.uncheckedFP, h.r.uncheckedAttempts = hub.TokenFingerprint(first), 4 // next delay: 60s
	h.r.reset(authResetFileChange)
	h.hub.set(func(c *credHub) { c.checkStatus = 0 })
	h.write(h.newTok)
	watchStep(t, clk)
	if !pending(notify) {
		t.Fatal("the new value was not reported at the next check")
	}
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("the new value was not adopted")
	}
}

// Heavy churn of malformed and unaccepted values, then a genuine reissue:
// the reissue is adopted at the check that first sees it, and only the
// well-formed values for this agent were checked with the hub.
func TestCredentialReload_ChurnThenReissue(t *testing.T) {
	h := newResetHarness(t)
	clk, notify := h.startWatch()
	n := 0
	step := func(v string) {
		n++
		h.write(v)
		watchStep(t, clk)
		if !pending(notify) {
			t.Fatalf("no notification for value %d", n)
		}
		h.r.reset(authResetFileChange)
	}
	for i := range 4 {
		step(fmt.Sprintf("malformed-%d", i))
	}
	for i := range 4 {
		step(testCredential(t, fmt.Sprintf("unaccepted-%d", i), h.now.Add(time.Duration(i+1)*time.Hour)))
	}
	if !h.usingCredential(h.oldTok) {
		t.Fatal("a churned value was adopted")
	}
	step(h.newTok)
	if !h.usingCredential(h.newTok) {
		t.Fatal("the genuine reissue was not adopted at the check that saw it")
	}
	// The watcher's start-up check of the current credential is not counted.
	if got := h.hub.checkCountExcept(h.oldTok); got != 5 {
		t.Fatalf("hub checks = %d, want 5 (4 unaccepted + the reissue)", got)
	}
}

// The signal adopts without a hub check, even when the hub accepts
// nothing.
func TestCredentialReload_SignalAdoptsWithoutHubCheck(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.accepted = map[string]bool{} })
	h.write(h.newTok)
	h.r.reset(authResetSignal)
	if !h.usingCredential(h.newTok) {
		t.Fatal("the signal did not adopt the credential")
	}
}

// An unaccepted far-future credential adopted on the signal does not move
// the verified issue time, so a later genuine reissue is still adopted
// from the file. Covered with the hub refusing it and with the hub's
// answer pointing elsewhere.
func TestCredentialReload_SignalAdoptionDoesNotBlockLaterReissue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"refused", 0}, {"302", http.StatusFound}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResetHarness(t)
			h.hub.set(func(c *credHub) { c.accepted[h.oldTok] = true })
			if err := h.client.CheckCredentialAccepted(context.Background(), h.oldTok); err != nil {
				t.Fatalf("setup: %v", err)
			}
			anchor := h.client.VerifiedIssuedAt()
			unaccepted := testCredential(t, "unaccepted", h.now.Add(100*time.Hour))
			h.hub.set(func(c *credHub) { c.checkStatus = tc.status })
			h.write(unaccepted)
			h.r.reset(authResetSignal)
			if !h.usingCredential(unaccepted) {
				t.Fatal("setup: the signal did not adopt")
			}
			if got := h.client.VerifiedIssuedAt(); !got.Equal(anchor) {
				t.Fatalf("verified issue time moved to %v", got)
			}
			h.hub.set(func(c *credHub) { c.checkStatus = 0 })
			h.write(h.newTok)
			h.r.reset(authResetFileChange)
			if !h.usingCredential(h.newTok) {
				t.Fatal("a later genuine reissue was not adopted")
			}
		})
	}
}

// waitForAnchor waits (in real time, bounded) until the verified issue
// time equals want.
func (h *resetHarness) waitForAnchor(want time.Time) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !h.client.VerifiedIssuedAt().Equal(want) {
		if time.Now().After(deadline) {
			h.t.Fatalf("verified issue time = %v, want %v", h.client.VerifiedIssuedAt(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The watcher's start-up check of the current credential, once accepted,
// sets the verified issue time, so an older accepted value is then refused
// from the file.
func TestCredentialReload_StartupCheckSetsVerifiedIssuedAt(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.accepted[h.oldTok] = true })
	_, _ = h.startWatch()
	h.waitForAnchor(h.now.Add(-time.Hour))
	older := testCredential(t, "older", h.now.Add(-2*time.Hour))
	h.hub.set(func(c *credHub) { c.accepted[older] = true })
	h.write(older)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.oldTok) {
		t.Fatal("an older credential was adopted after the start-up check")
	}
}

// The check after a signal adoption, once accepted, sets the verified
// issue time to the adopted credential's, so an older accepted value is
// then refused from the file.
func TestCredentialReload_SignalCheckSetsVerifiedIssuedAt(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.accepted[h.oldTok] = true })
	h.write(h.newTok)
	h.r.reset(authResetSignal)
	if got := h.client.VerifiedIssuedAt(); !got.Equal(h.now) {
		t.Fatalf("verified issue time = %v, want %v", got, h.now)
	}
	h.write(h.oldTok)
	h.r.reset(authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("an older credential was adopted after the signal check")
	}
}

// A refused value resets the re-check pacing, so a value that returns
// later starts again from the first delay.
func TestCredentialReload_RefusalResetsRecheckDelay(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.checkStatus = http.StatusInternalServerError })
	h.write(h.newTok)
	h.r.reset(authResetFileChange)
	h.r.reset(authResetFileChange)
	if h.r.uncheckedAttempts != 2 {
		t.Fatalf("setup: unchecked attempts = %d, want 2", h.r.uncheckedAttempts)
	}
	h.write("malformed")
	h.r.reset(authResetFileChange)
	if h.r.uncheckedAttempts != 0 || h.r.uncheckedFP != (hub.Fingerprint{}) {
		t.Fatalf("a refusal left the re-check pacing at %d attempts", h.r.uncheckedAttempts)
	}
	h.write(h.newTok)
	h.r.reset(authResetFileChange)
	if h.r.uncheckedAttempts != 1 {
		t.Fatalf("unchecked attempts = %d, want 1 after the value returned", h.r.uncheckedAttempts)
	}
}

// The re-check delay is paced per value: a different unconfirmed value
// starts again from the first delay.
func TestCredentialReload_RecheckDelayIsPerValue(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.checkStatus = http.StatusInternalServerError })
	h.write(h.newTok)
	h.r.reset(authResetFileChange)
	h.r.reset(authResetFileChange)
	if h.r.uncheckedAttempts != 2 {
		t.Fatalf("setup: unchecked attempts = %d, want 2", h.r.uncheckedAttempts)
	}
	other := testCredential(t, "other-value", h.now.Add(time.Minute))
	h.write(other)
	h.r.reset(authResetFileChange)
	if h.r.uncheckedAttempts != 1 || h.r.uncheckedFP != hub.TokenFingerprint(other) {
		t.Fatalf("a different value continued the previous value's pacing (%d attempts)", h.r.uncheckedAttempts)
	}
}

// With no verified issue time yet, the file holding the credential already
// in use is a no-op: no hub check, no refresh loop restart, no wake.
func TestCredentialReload_CurrentValueIsNoOpWithoutVerifiedTime(t *testing.T) {
	h := newResetHarness(t)
	h.hub.set(func(c *credHub) { c.accepted[h.oldTok] = true })
	loop := h.done
	changed := h.client.CredentialChanged()
	h.write(h.oldTok)
	h.r.reset(authResetFileChange)
	if h.hub.checkCount() != 0 {
		t.Fatal("the credential in use was checked with the hub")
	}
	if h.done != loop || !loopRunning(loop) {
		t.Fatal("the refresh loop was restarted for the credential in use")
	}
	select {
	case <-changed:
		t.Fatal("waiting loops were woken for the credential in use")
	default:
	}
}
