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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// TestAgentAcceptsRotatedKeyWithoutReconnect: after the hub publishes a
// new key, a key refresh lets the long-lived session accept grants signed
// with it; the session is never replaced.
func TestAgentAcceptsRotatedKeyWithoutReconnect(t *testing.T) {
	k1, k2 := newTestKey(t, "k1"), newTestKey(t, "k2")
	h := newFakeHub(t, k1.public)
	a, _ := startAgent(t, h, nil)
	s := h.nextSession(t)
	port := echoListener(t)

	if _, err := s.OpenStream(context.Background(), tcpOpen(t, k2, s.Info(), "launch-1", port)); err == nil {
		t.Fatal("grant with the unpublished key accepted")
	}
	h.set(func(h *fakeHub) { h.keys = []grant.PublicKey{k1.public, k2.public} })
	if err := a.RefreshKeys(context.Background()); err != nil {
		t.Fatalf("RefreshKeys: %v", err)
	}
	st, err := s.OpenStream(context.Background(), tcpOpen(t, k2, s.Info(), "launch-1", port))
	if err != nil {
		t.Fatalf("grant with the rotated key refused: %v", err)
	}
	_ = st.Close()
	if n := h.conduitHits.Load(); n != 1 {
		t.Fatalf("%d conduit sessions, want 1 (no reconnect)", n)
	}
}

// TestAgentKeyRefreshCadence: the refresh loop fetches keys every
// KeyRefreshInterval with the agent token; a configured longer interval
// is capped at KeyRefreshInterval.
func TestAgentKeyRefreshCadence(t *testing.T) {
	h := newFakeHub(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	a, err := New(Options{
		HubURL: h.srv.URL, AgentID: testAgentID, ProjectID: testProjectID,
		Token:              func() string { return "token-1" },
		KeyRefreshInterval: time.Hour, // capped
		Clock:              clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.refreshKeysLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	for i := int64(1); i <= 3; i++ {
		waitPending(t, clk, 1)
		clk.Advance(KeyRefreshInterval - time.Second)
		if n := h.keyHits.Load(); n != i-1 {
			t.Fatalf("refresh %d fetched early (%d fetches)", i, n)
		}
		clk.Advance(time.Second)
		waitPending(t, clk, 1) // the fetch ran and the next one is armed
		if n := h.keyHits.Load(); n != i {
			t.Fatalf("after %d intervals: %d fetches", i, n)
		}
	}
}

// TestAgentKeyRefreshFailureKeepsKeys: a failed fetch keeps the current
// key set.
func TestAgentKeyRefreshFailureKeepsKeys(t *testing.T) {
	k1 := newTestKey(t, "k1")
	h := newFakeHub(t, k1.public)
	a, err := New(Options{HubURL: h.srv.URL, AgentID: testAgentID, ProjectID: testProjectID, Token: func() string { return "t" }})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RefreshKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.srv.Config.Handler = http.NotFoundHandler()
	if err := a.RefreshKeys(context.Background()); err == nil {
		t.Fatal("RefreshKeys succeeded against a 404")
	}
	if _, ok := a.Keys().Lookup("k1"); !ok {
		t.Fatal("key set lost after a failed refresh")
	}
}

func waitPending(t *testing.T, clk *clock.Fake, want int) {
	t.Helper()
	if !clk.WaitFor(waitTimeout, func(n int) bool { return n == want }) {
		t.Fatalf("timers did not settle at %d (have %d)", want, clk.Pending())
	}
}
