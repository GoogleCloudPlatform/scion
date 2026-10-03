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

// Package relaytest runs real relays for tests: a SQLite-backed registry
// (behind a FaultStore), relays with their internal APIs on httptest
// servers, and target sessions over real WebSockets. It is the "two relays
// over httptest" seam for the relay, router and later fault-injection
// tests.
package relaytest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// World is a registry plus any number of relay nodes sharing it.
type World struct {
	T        *testing.T
	Inner    registry.Store       // the SQLite store
	Store    *registry.FaultStore // what every node uses
	Registry *registry.Registry
	PeerKey  []byte // shared HMAC peer key (HKDF-derived)
	GrantKey *conduitv1.GrantKey

	now   atomic.Int64 // registry clock, unix nanos
	fault atomic.Pointer[func(op string) error]

	mu         sync.Mutex
	principals map[string]relay.Principal
}

// NewWorld returns an empty world. The registry clock starts at the wall
// clock and moves only with Advance.
func NewWorld(t *testing.T) *World {
	t.Helper()
	w := &World{T: t, principals: map[string]relay.Principal{}}
	w.now.Store(time.Now().UnixNano())
	w.Inner = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	w.Store = &registry.FaultStore{Inner: w.Inner, Fault: func(_ context.Context, op string) error {
		if f := w.fault.Load(); f != nil {
			return (*f)(op)
		}
		return nil
	}}
	w.Registry = registry.New(w.Store, registry.Config{Clock: registry.ClockFunc(w.Now)})
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	key, err := relay.DeriveHMACPeerKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	w.PeerKey = key
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	w.GrantKey = &conduitv1.GrantKey{Kid: "k1", PublicKey: pub}
	return w
}

// Now is the registry clock.
func (w *World) Now() time.Time { return time.Unix(0, w.now.Load()) }

// Advance moves the registry clock.
func (w *World) Advance(d time.Duration) { w.now.Add(int64(d)) }

// SetFault installs a store fault (nil clears it).
func (w *World) SetFault(f func(op string) error) {
	if f == nil {
		w.fault.Store(nil)
		return
	}
	w.fault.Store(&f)
}

// SetPrincipal registers the authenticated principal for key; a target
// dialing with that key is served as p.
func (w *World) SetPrincipal(key string, p relay.Principal) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.principals[key] = p
}

func (w *World) principal(key string) (relay.Principal, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.principals[key]
	return p, ok
}

// PeerAuth returns an HMAC peer authenticator for selfID.
func (w *World) PeerAuth(selfID string) relay.PeerAuth {
	a, err := relay.NewHMACPeerAuth(relay.HMACPeerAuthConfig{Key: w.PeerKey, SelfID: selfID})
	if err != nil {
		w.T.Fatal(err)
	}
	return a
}

// Node is one relay with its servers.
type Node struct {
	W        *World
	Relay    *relay.Relay
	Clock    *clock.Fake // relay timers (heartbeat, delete backoff)
	Internal *httptest.Server
	Public   *httptest.Server
	Peers    *relay.PeerClient

	// Served receives Serve's result for every target connection.
	Served chan error
}

// StartNode builds, serves and starts a relay. mod may adjust the config
// before New.
func (w *World) StartNode(id string, mod func(*relay.Config)) *Node {
	t := w.T
	t.Helper()
	n, err := w.NewNode(id, mod)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Relay.Start(context.Background()); err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	return n
}

// NewNode builds a relay and its servers without starting it.
func (w *World) NewNode(id string, mod func(*relay.Config)) (*Node, error) {
	t := w.T
	n := &Node{W: w, Clock: clock.NewFake(time.Now()), Served: make(chan error, 64)}
	var internal atomic.Pointer[http.Handler]
	n.Internal = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := internal.Load()
		if h == nil {
			http.Error(rw, "not ready", http.StatusServiceUnavailable)
			return
		}
		(*h).ServeHTTP(rw, r)
	}))
	t.Cleanup(n.Internal.Close)
	auth := w.PeerAuth(id)
	n.Peers = &relay.PeerClient{HTTP: n.Internal.Client(), Auth: auth}
	cfg := relay.Config{
		InstanceID:       id,
		InternalEndpoint: n.Internal.URL,
		RequireSelfCheck: true,
		RegistryNow:      w.Now,
		Registry:         w.Registry,
		Session:          conduit.Config{Clock: clock.Real()},
		GrantKeys: func(context.Context) ([]*conduitv1.GrantKey, error) {
			return []*conduitv1.GrantKey{w.GrantKey}, nil
		},
		PeerAuth:   auth,
		HTTPClient: n.Internal.Client(),
		Clock:      n.Clock,
	}
	if mod != nil {
		mod(&cfg)
	}
	r, err := relay.New(cfg)
	if err != nil {
		return nil, err
	}
	n.Relay = r
	h := r.InternalHandler()
	internal.Store(&h)
	n.Public = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		p, ok := w.principal(req.URL.Query().Get("principal"))
		if !ok {
			http.Error(rw, "unknown principal", http.StatusUnauthorized)
			return
		}
		conn, err := ws.Upgrade(rw, req, nil, ws.Options{})
		if err != nil {
			return
		}
		n.Served <- r.Serve(context.Background(), conn, p)
	}))
	t.Cleanup(func() {
		r.Kill()
		n.Public.Close()
	})
	return n, nil
}

// Dial connects a target session as the principal registered under key.
func (n *Node) Dial(ctx context.Context, key string, hello *conduitv1.Hello, cfg conduit.Config) (conduit.LocalSession, *conduitv1.Welcome, error) {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	d := &ws.Dialer{URL: "ws" + strings.TrimPrefix(n.Public.URL, "http") + "/?principal=" + key}
	s, w, err := conduit.Dial(ctx, d, cfg, hello)
	if err != nil {
		return nil, nil, err
	}
	ls := s.(conduit.LocalSession)
	n.W.T.Cleanup(func() { _ = ls.Close() })
	return ls, w, nil
}

// MustDial is Dial that fails the test on error.
func (n *Node) MustDial(key string, hello *conduitv1.Hello, cfg conduit.Config) (conduit.LocalSession, *conduitv1.Welcome) {
	n.W.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, w, err := n.Dial(ctx, key, hello, cfg)
	if err != nil {
		n.W.T.Fatalf("dial %s via %s: %v", key, n.Relay.InstanceID(), err)
	}
	return s, w
}

// AgentHello builds an agent Hello presenting launchID ("" = old
// sciontool, no launch id).
func AgentHello(id, launchID, execScope string, kinds ...string) *conduitv1.Hello {
	return &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		PrincipalId:   id,
		Capabilities: &conduitv1.Capabilities{
			StreamKinds:         kinds,
			Rpc:                 []string{"exec"},
			EndpointIncarnation: launchID,
			ExecScope:           execScope,
		},
	}
}

// BrokerHello builds a broker Hello.
func BrokerHello(id, incarnation string) *conduitv1.Hello {
	return &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   id,
		Capabilities:  &conduitv1.Capabilities{StreamKinds: []string{"pty"}, Rpc: []string{"exec"}, EndpointIncarnation: incarnation},
	}
}

// UserHello builds a user Hello.
func UserHello(id string) *conduitv1.Hello {
	return &conduitv1.Hello{PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_USER, PrincipalId: id, Capabilities: &conduitv1.Capabilities{}}
}

// Sessions lists the registry rows of a principal (bypassing faults).
func (w *World) Sessions(kind, id string) registry.PrincipalSessions {
	ps, err := w.Inner.ListPrincipalSessions(context.Background(), kind, id)
	if err != nil {
		w.T.Fatal(err)
	}
	return ps
}

// Wait waits for ch (or fails after 10s of real time; a safety net, not
// synchronisation).
func Wait[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// WaitClosed waits for a closed channel.
func WaitClosed(t testing.TB, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
