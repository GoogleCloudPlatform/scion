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

package relayserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// grantKeyEvents is an Events whose grant key change notices and LISTEN
// (re)connects the test drives; other subjects never fire.
type grantKeyEvents struct {
	keys  chan string
	mu    sync.Mutex
	hooks []func()
}

func (e *grantKeyEvents) Subscribe(subjects ...string) (<-chan string, func()) {
	for _, s := range subjects {
		if s == GrantKeysChangedSubject {
			return e.keys, func() {}
		}
	}
	return make(chan string), func() {}
}

func (e *grantKeyEvents) AddOnListen(fn func()) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hooks = append(e.hooks, fn)
	return func() {}
}

// listen runs every registered listen hook, as a LISTEN (re)connect does.
func (e *grantKeyEvents) listen() {
	e.mu.Lock()
	hooks := append([]func(){}, e.hooks...)
	e.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// dialV2Agent connects a stream_renewal.v2 agent session straight to the
// server's relay (the relay's own Serve, as an admitted principal) and
// returns once the session is registered. Each GrantKeysChanged it receives
// increments hints.
func dialV2Agent(t *testing.T, s *Server, hints *atomic.Int32) {
	t.Helper()
	p := relay.Principal{Kind: registry.PrincipalAgent, ID: "agent-1", ProjectID: "proj-1",
		Agent:     relay.AgentIncarnationFacts{LaunchID: "L1", Generation: 1},
		GrantKeys: []*conduitv1.GrantKey{}}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := ws.Upgrade(rw, req, nil, ws.Options{})
		if err != nil {
			return
		}
		_ = s.Relay.Serve(context.Background(), conn, p)
	}))
	t.Cleanup(srv.Close)
	hello := relaytest.AgentHello("agent-1", "L1", "", "pty")
	hello.Capabilities.Features = []string{conduit.FeatureStreamRenewal}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/"}
	sess, wel, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real(), OnGrantKeysChanged: func(context.Context) { hints.Add(1) }}, hello)
	if err != nil {
		t.Fatalf("dial the v2 agent session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if _, _, ok := s.Relay.Local(ctx, wel.GetSessionId()); !ok {
		t.Fatal("the v2 agent session did not register on the relay")
	}
}

// waitHints waits until hints reaches want, then checks it stays there for
// a moment (no extra hint).
func waitHints(t *testing.T, hints *atomic.Int32, want int32, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for hints.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d hints, want %d", what, hints.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := hints.Load(); n != want {
		t.Fatalf("%s: %d hints, want exactly %d", what, n, want)
	}
}

// TestServerGrantKeysChangedHintsTargets: through Start's wiring, a grant
// key change notice on the conduit channel makes the relay send
// GrantKeysChanged to a connected stream_renewal.v2 agent session.
func TestServerGrantKeysChangedHintsTargets(t *testing.T) {
	w := relaytest.NewWorld(t)
	cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
	ev := &grantKeyEvents{keys: make(chan string, 1)}
	cfg.Events = ev
	s := startServer(t, cfg)
	var hints atomic.Int32
	dialV2Agent(t, s, &hints)

	ev.keys <- GrantKeysChangedSubject
	waitHints(t, &hints, 1, "after a grant key change notice")
}

// TestServerHintsTargetsOnListenReconnect: through Start's wiring, the
// first LISTEN sends no hint, and each later one (a reconnect after a
// drop, when a notice may have been missed) sends exactly one.
func TestServerHintsTargetsOnListenReconnect(t *testing.T) {
	w := relaytest.NewWorld(t)
	cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
	ev := &grantKeyEvents{keys: make(chan string)}
	cfg.Events = ev
	s := startServer(t, cfg)
	var hints atomic.Int32
	dialV2Agent(t, s, &hints)

	ev.listen() // the first connect
	time.Sleep(300 * time.Millisecond)
	if n := hints.Load(); n != 0 {
		t.Fatalf("the first LISTEN sent %d hints, want 0", n)
	}
	ev.listen() // a reconnect after a drop
	waitHints(t, &hints, 1, "after a LISTEN reconnect")
}

// cleanupEvents is an Events that records each subscription's unsubscribe
// and each listen hook's removal.
type cleanupEvents struct {
	mu           sync.Mutex
	unsubscribed map[string]int
	hookRemoved  []int // removals per listen hook, in registration order
}

func (e *cleanupEvents) Subscribe(subjects ...string) (<-chan string, func()) {
	key := strings.Join(subjects, ",")
	return make(chan string), func() {
		e.mu.Lock()
		e.unsubscribed[key]++
		e.mu.Unlock()
	}
}

func (e *cleanupEvents) AddOnListen(func()) func() {
	e.mu.Lock()
	i := len(e.hookRemoved)
	e.hookRemoved = append(e.hookRemoved, 0)
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		e.hookRemoved[i]++
		e.mu.Unlock()
	}
}

// TestStartFailureReleasesEventRegistrations: when the public listener
// cannot bind, Start fails and releases everything it registered on the
// events, each exactly once: the role's revocation subscription and listen
// hook, the settings subscription, and the grant key subscription and
// listen hook (the registrations end with the failed start, not only when
// Start's context ends).
func TestStartFailureReleasesEventRegistrations(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })

	w := relaytest.NewWorld(t)
	cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
	ev := &cleanupEvents{unsubscribed: map[string]int{}}
	cfg.Events = ev
	cfg.PublicListen = held.Addr().String() // in use: the bind fails
	if r := awaitStart(t, startAsync(t, cfg)); r.err == nil {
		t.Fatal("Start succeeded with its public listen address in use")
	}

	revocation := strings.Join(RevocationSubjects, ",")
	// state reports whether every registration was released exactly once.
	state := func() (string, bool) {
		ev.mu.Lock()
		defer ev.mu.Unlock()
		ok := ev.unsubscribed[revocation] == 1 && ev.unsubscribed[SettingsUpdatedSubject] == 1 &&
			ev.unsubscribed[GrantKeysChangedSubject] == 1 && len(ev.hookRemoved) == 2
		for _, n := range ev.hookRemoved {
			ok = ok && n == 1
		}
		return fmt.Sprintf("revocation unsubscribed %d, settings %d, grant keys %d; listen hook removals %v (role's, then the grant key hook's)",
			ev.unsubscribed[revocation], ev.unsubscribed[SettingsUpdatedSubject], ev.unsubscribed[GrantKeysChangedSubject], ev.hookRemoved), ok
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		desc, ok := state()
		if ok {
			time.Sleep(200 * time.Millisecond)
			if desc, ok := state(); !ok {
				t.Fatalf("a cleanup ran more than once: %s", desc)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the failed start: %s; want each exactly once (2 hooks)", desc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
