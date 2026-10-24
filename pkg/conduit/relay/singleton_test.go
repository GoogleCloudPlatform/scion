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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// The Cloud Run singleton relay (design §3.9, v2.21.54 and v2.21.55).

// warnCounter counts WARN records whose message contains match.
type warnCounter struct {
	match string
	n     atomic.Int64
}

func (h *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn && strings.Contains(r.Message, h.match) {
		h.n.Add(1)
	}
	return nil
}
func (h *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnCounter) WithGroup(string) slog.Handler      { return h }

func singletonRow(t *testing.T, w *relaytest.World, id string) registry.RelayInstance {
	t.Helper()
	all, err := w.Store.ListRelays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ri := range all {
		if ri.InstanceID == id {
			return ri
		}
	}
	t.Fatalf("relay %s is not registered", id)
	return registry.RelayInstance{}
}

// TestSingleton_RegistersTheFeature: a singleton's relay_instances row
// lists the feature singleton; another relay's lists none.
func TestSingleton_RegistersTheFeature(t *testing.T) {
	w := relaytest.NewWorld(t)
	w.StartNode("relay-a", func(c *relay.Config) { c.Singleton = true })
	w.StartNode("relay-b", nil)
	if a := singletonRow(t, w, "relay-a"); !a.HasFeature(registry.RelayFeatureSingleton) {
		t.Fatalf("singleton row features = %v, want singleton", a.Features)
	}
	if b := singletonRow(t, w, "relay-b"); len(b.Features) != 0 {
		t.Fatalf("non-singleton row features = %v, want none", b.Features)
	}
}

// waitPeers waits until the singleton's peer count is want.
func waitPeers(t *testing.T, n *relaytest.Node, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for n.Relay.SingletonEndpointPeers() != want {
		if time.Now().After(deadline) {
			t.Fatalf("singleton endpoint peers = %d, want %d", n.Relay.SingletonEndpointPeers(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// beat runs one heartbeat of n (fake clock).
func beat(t *testing.T, n *relaytest.Node) {
	t.Helper()
	at := n.Clock.Now().Add(relay.DefaultHeartbeatInterval)
	if !n.Clock.WaitForTimer(10*time.Second, at) {
		t.Fatal("heartbeat timer not armed")
	}
	n.Clock.Advance(relay.DefaultHeartbeatInterval)
}

// TestSingleton_SharedEndpointPeersGauge: registration never refuses. Each
// heartbeat counts the other live, non-draining rows with the singleton's
// internal endpoint; the count falls to 0 when the peer goes stale or
// draining; WARN is logged only when the count goes from 0 to non-zero.
//
// Leaving out draining relays is sound only because a drain is bounded:
// the reduce-only invariant pinned by TestReduceOnly_NoPathLengthensASession
// (a drain deadline or cap only ever shortens a session). A change to that
// invariant means revisiting this count.
func TestSingleton_SharedEndpointPeersGauge(t *testing.T) {
	w := relaytest.NewWorld(t)
	warns := &warnCounter{match: "shares this singleton's internal endpoint"}
	n := w.StartNode("relay-a", func(c *relay.Config) {
		c.Singleton = true
		c.Logger = slog.New(warns)
	})
	ctx := context.Background()
	endpoint := singletonRow(t, w, "relay-a").InternalEndpoint
	waitPeers(t, n, 0)

	register := func(id string) int64 {
		t.Helper()
		gen, err := w.Registry.RegisterRelay(ctx, registry.RelayInstance{InstanceID: id, InternalEndpoint: endpoint})
		if err != nil {
			t.Fatal(err)
		}
		return gen
	}
	register("relay-b")
	beat(t, n)
	waitPeers(t, n, 1)
	beat(t, n)
	waitPeers(t, n, 1)
	if got := warns.n.Load(); got != 1 {
		t.Fatalf("WARN logged %d times, want once (on 0 to non-zero only)", got)
	}

	// relay-b goes stale (the registry clock passes its last_seen by more
	// than the staleness bound); relay-a stays live by its heartbeat.
	w.Advance(2 * time.Minute)
	beat(t, n)
	waitPeers(t, n, 0)

	// A fresh but draining peer is not counted.
	genB := register("relay-b")
	if err := w.Registry.SetRelayDraining(ctx, "relay-b", genB, true); err != nil {
		t.Fatal(err)
	}
	beat(t, n)
	waitPeers(t, n, 0)

	// A new live peer warns again (0 to non-zero).
	register("relay-c")
	beat(t, n)
	waitPeers(t, n, 1)
	if got := warns.n.Load(); got != 2 {
		t.Fatalf("WARN logged %d times, want 2 (one per 0 to non-zero transition)", got)
	}
}

// flakyEndpoint serves 503 until answer is set, then forwards to target.
func flakyEndpoint(t *testing.T, target string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	var answer atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !answer.Load() {
			http.Error(w, "the old instance still holds the service URL", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &answer
}

// singletonBehind builds (not starts) a singleton relay whose internal
// endpoint is a flaky front for its own internal API.
func singletonBehind(t *testing.T, w *relaytest.World) (*relaytest.Node, *atomic.Bool, string) {
	t.Helper()
	var front *httptest.Server
	var answer *atomic.Bool
	var once sync.Once
	n, err := w.NewNode("relay-s", func(c *relay.Config) {
		c.Singleton = true
		once.Do(func() { front, answer = flakyEndpoint(t, c.InternalEndpoint) })
		c.InternalEndpoint = front.URL
	})
	if err != nil {
		t.Fatal(err)
	}
	return n, answer, front.URL
}

// TestSingleton_SelfCheckRetriedUntilItAnswers: a singleton keeps retrying
// its self-check (the service URL still reaches the old instance) and
// registers once it answers itself, within the 60s bound.
func TestSingleton_SelfCheckRetriedUntilItAnswers(t *testing.T) {
	w := relaytest.NewWorld(t)
	n, answer, front := singletonBehind(t, w)
	started := make(chan error, 1)
	go func() { started <- n.Relay.Start(context.Background()) }()
	for i := 0; i < 3; i++ { // three failed attempts, 2s apart
		if !n.Clock.WaitForTimer(10*time.Second, n.Clock.Now().Add(2*time.Second)) {
			t.Fatal("self-check retry timer not armed")
		}
		n.Clock.Advance(2 * time.Second)
	}
	if !n.Clock.WaitForTimer(10*time.Second, n.Clock.Now().Add(2*time.Second)) {
		t.Fatal("self-check retry timer not armed")
	}
	answer.Store(true)
	n.Clock.Advance(2 * time.Second)
	if err := relaytest.Wait(t, started, "Start"); err != nil {
		t.Fatalf("Start = %v, want registration after the self-check answered", err)
	}
	if got := singletonRow(t, w, "relay-s"); got.InternalEndpoint != front || !got.HasFeature(registry.RelayFeatureSingleton) {
		t.Fatalf("registered row %+v, want the front endpoint and the singleton feature", got)
	}
}

// TestSingleton_SelfCheckFailsStartupAfterTheBound: a singleton whose
// self-check never answers itself fails startup after 60s and does not
// register.
func TestSingleton_SelfCheckFailsStartupAfterTheBound(t *testing.T) {
	w := relaytest.NewWorld(t)
	n, _, _ := singletonBehind(t, w)
	started := make(chan error, 1)
	go func() { started <- n.Relay.Start(context.Background()) }()
	start := n.Clock.Now()
	for n.Clock.Now().Sub(start) < relay.SingletonSelfCheckBound {
		select {
		case err := <-started:
			t.Fatalf("Start returned early (%v) at +%s", err, n.Clock.Now().Sub(start))
		default:
		}
		if !n.Clock.WaitForTimer(10*time.Second, n.Clock.Now().Add(2*time.Second)) {
			t.Fatal("self-check retry timer not armed")
		}
		n.Clock.Advance(2 * time.Second)
	}
	if err := relaytest.Wait(t, started, "Start"); err == nil || !strings.Contains(err.Error(), "singleton") {
		t.Fatalf("Start = %v, want a singleton startup failure", err)
	}
	all, err := w.Store.ListRelays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ri := range all {
		if ri.InstanceID == "relay-s" {
			t.Fatal("the singleton registered after its self-check failed")
		}
	}
}

// TestStart_FeaturesColumnMissingNamesTheFix: when registration fails
// because relay_instances.features does not exist (the hubs have not
// migrated the schema), Start fails with an error that names the fix.
func TestStart_FeaturesColumnMissingNamesTheFix(t *testing.T) {
	w := relaytest.NewWorld(t)
	w.SetFault(func(op string) error {
		if op == registry.OpRegisterRelay {
			return fmt.Errorf("store: %w", registry.ErrRelayFeaturesColumnMissing)
		}
		return nil
	})
	n, err := w.NewNode("relay-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = n.Relay.Start(context.Background())
	if !errors.Is(err, registry.ErrRelayFeaturesColumnMissing) || !strings.Contains(err.Error(), "upgrade the hubs") {
		t.Fatalf("Start = %v, want a startup error naming the hub upgrade", err)
	}
}
