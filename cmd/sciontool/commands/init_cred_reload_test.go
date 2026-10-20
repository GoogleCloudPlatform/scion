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
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	c, err := json.Marshal(map[string]any{
		"sub": "agent-1", "jti": jti,
		"iat": iat.Unix(), "exp": iat.Add(10 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeToString(h) + "." + enc.EncodeToString(c) + "." + enc.EncodeToString(make([]byte, 32))
}

// credHub is a test hub that accepts only the credential in accept and
// records every credential presented.
type credHub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	accept string
	seen   []string
}

func newCredHub(t *testing.T, accept string) *credHub {
	t.Helper()
	h := &credHub{accept: accept}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Scion-Agent-Token")
		h.mu.Lock()
		h.seen = append(h.seen, tok)
		ok := tok == h.accept
		h.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(h.srv.Close)
	return h
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
// newTok (the old one was revoked by a reissue).
func newResetHarness(t *testing.T) *resetHarness {
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
	h.hub = newCredHub(t, h.newTok)
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

// Recovery without the reset signal, through the init wiring: the watcher
// sees the reissued credential, the wait loop's file-change case runs the
// reset path, and the shared client presents the new credential.
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
	handleAuthEvent(h.r, authResetFileChange)

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
	handleAuthEvent(h.r, authResetSignal)
	if !h.usingCredential(h.newTok) {
		t.Fatal("signal reset did not adopt the credential")
	}
	loop := h.done
	beats := h.hub.presented(h.newTok)

	handleAuthEvent(h.r, authResetFileChange)
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
	handleAuthEvent(h.r, authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("file change did not adopt the credential")
	}
	loop := h.done
	handleAuthEvent(h.r, authResetSignal)
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

	handleAuthEvent(h.r, authResetFileChange)
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

// A valid credential issued before the current one is refused on the
// file path.
func TestCredentialReload_OlderCredentialRefused(t *testing.T) {
	h := newResetHarness(t)
	h.write(h.newTok)
	handleAuthEvent(h.r, authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("setup: new credential not adopted")
	}
	loop := h.done
	h.write(h.oldTok)
	handleAuthEvent(h.r, authResetFileChange)
	if !h.usingCredential(h.newTok) {
		t.Fatal("an older credential replaced the current one")
	}
	if h.done != loop || !loopRunning(loop) {
		t.Fatal("the refresh loop was stopped for a refused credential")
	}
}

// An expired credential is refused on the signal path before the refresh
// loop is stopped.
func TestCredentialReload_ExpiredOnSignalKeepsLoop(t *testing.T) {
	h := newResetHarness(t)
	h.write(testCredential(t, "expired", h.now.Add(-11*time.Hour)))
	loop := h.done
	handleAuthEvent(h.r, authResetSignal)
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
	handleAuthEvent(h.r, authResetFileChange)
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
