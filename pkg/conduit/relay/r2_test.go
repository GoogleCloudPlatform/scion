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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestRouteWaitsForPendingRegistration (r2-F1): a session whose Welcome is
// out but which Serve has not registered yet is routable. The BeforeReady
// seam holds Serve; a route through the owner relay (local) or through
// another relay (remote, via the owner's internal API) waits for the
// session instead of failing with "no eligible session", and reaches it
// once Serve continues.
func TestRouteWaitsForPendingRegistration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote bool
	}{
		{name: "local"},
		{name: "remote", remote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			a := w.StartNode("relay-a", nil)
			b := w.StartNode("relay-b", nil)
			w.SetPrincipal("a", agentPrincipal("L1", 1))

			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			a.Relay.SetBeforeReadyHookForTest(func() { <-release })
			waiting := make(chan struct{}, 1)
			a.Relay.SetPendingWaitHookForTest(func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			})

			echo := conduit.Config{RPCHandler: conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: 200, Body: []byte("ok")}
			})}
			// Dial returns on the Welcome, while Serve is held in the seam.
			_, wel := a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echo)

			via := a
			if tc.remote {
				via = b
			}
			rt, err := router.New(router.Config{Relay: via.Relay, Registry: w.Registry, Store: w.Store, Peers: via.Peers, Now: w.Now})
			if err != nil {
				t.Fatal(err)
			}
			req := router.Request{Op: router.OpStatefulRPC, Kind: registry.PrincipalAgent, ID: agentID,
				Want: registry.Want{ProjectID: project}, Agent: relay.AgentIncarnationFacts{LaunchID: "L1", Generation: 1}}
			result := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				result <- rt.Do(ctx, req, func(ctx context.Context, res router.Resolved) error {
					if res.Record.SessionID != wel.GetSessionId() || res.Local != !tc.remote {
						return fmt.Errorf("resolved %s (local %v)", res.Record.SessionID, res.Local)
					}
					resp, err := res.Session.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "GET", Path: "/x"})
					if err != nil {
						return err
					}
					if string(resp.GetBody()) != "ok" {
						return fmt.Errorf("body = %q", resp.GetBody())
					}
					return nil
				})
			}()
			// The owner is waiting for the pending session: release Serve.
			relaytest.Wait(t, waiting, "the owner to wait for the pending session")
			unblock()
			if err := relaytest.Wait(t, result, "route"); err != nil {
				t.Fatalf("route to a session admitted before registration: %v", err)
			}
		})
	}
}

// TestShutdownBoundedDuringRegistryOutage (r2-F2): with every draining
// write hanging (a registry outage that honours ctx), Shutdown still sends
// GoAway to every session within one store timeout bounded by its ctx,
// not one timeout per session in series, and returns.
func TestShutdownBoundedDuringRegistryOutage(t *testing.T) {
	w := relaytest.NewWorld(t)
	var outage atomic.Bool
	w.Store.Fault = func(ctx context.Context, op string) error {
		if outage.Load() && (op == registry.OpSetSessionDraining || op == registry.OpSetRelayDraining) {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	n := w.StartNode("relay-a", nil)
	const sessions = 3
	var dialed []conduit.LocalSession
	for i := range sessions {
		key := fmt.Sprintf("u%d", i)
		id := fmt.Sprintf("user-%d", i)
		w.SetPrincipal(key, relay.Principal{Kind: registry.PrincipalUser, ID: id})
		s, _ := n.MustDial(key, relaytest.UserHello(id), conduit.Config{})
		dialed = append(dialed, s)
	}
	outage.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- n.Relay.Shutdown(ctx) }()
	// Before the fix the first GoAway waited 10s per session; the 10s
	// safety net of Wait fails then.
	for i, s := range dialed {
		relaytest.WaitClosed(t, s.GoAwayReceived(), fmt.Sprintf("GoAway to session %d during the outage", i))
	}
	_ = relaytest.Wait(t, result, "Shutdown to return during the outage")
}

// TestShutdownWaitsAfterSupersede (r2-F4): after the relay stopped serving
// on its own (superseded), Shutdown still waits for Serve's row delete,
// bounded by ctx, instead of returning nil at once.
func TestShutdownWaitsAfterSupersede(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	sess, _ := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	go func() { <-sess.GoAwayReceived(); _ = sess.Close() }()

	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	entered := make(chan struct{}, 1)
	w.SetFault(func(op string) error {
		if op == registry.OpDeleteSessionCAS {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		return nil
	})

	// A second process registers the same instance id.
	if _, err := w.Registry.RegisterRelay(context.Background(), registry.RelayInstance{InstanceID: "relay-a"}); err != nil {
		t.Fatal(err)
	}
	n.Clock.Advance(relay.DefaultHeartbeatInterval)
	if err := relaytest.Wait(t, n.Relay.Fatal(), "fatal"); !errors.Is(err, relay.ErrSuperseded) {
		t.Fatalf("fatal = %v", err)
	}
	relaytest.Wait(t, entered, "the row delete to reach the store")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- n.Relay.Shutdown(ctx) }()
	cancel()
	if err := relaytest.Wait(t, result, "Shutdown to return"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown after supersede = %v, want context.Canceled (it did not wait for the row delete)", err)
	}
	unblock()
	_ = relaytest.Wait(t, n.Served, "Serve to return")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := n.Relay.Shutdown(ctx2); err != nil {
		t.Fatalf("Shutdown once everything finished = %v, want nil", err)
	}
}

// TestFallbackRecheckReadErrorDeletesOwnRow (r2-F3): when the fallback
// fence's post-insert re-check cannot read the registry, the Hello is
// refused with 4504 registry_unavailable (never 4409) and the row it
// inserted is deleted.
func TestFallbackRecheckReadErrorDeletesOwnRow(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L2", 3))
	var reads atomic.Int32
	w.SetFault(func(op string) error {
		if op == registry.OpListPrincipalSessions && reads.Add(1) == 2 {
			return errors.New("injected: registry read failed")
		}
		return nil
	})
	_, _, err := n.Dial(context.Background(), "a", relaytest.AgentHello(agentID, "", "", "pty"), conduit.Config{})
	w.SetFault(nil)
	assertClose(t, err, conduit.CloseRelayTimeout, relay.ReasonRegistryUnavailable)
	if got := reads.Load(); got < 2 {
		t.Fatalf("registry reads = %d, want the pre-check and the re-check", got)
	}
	if ps := w.Sessions(registry.PrincipalAgent, agentID); len(ps.Sessions) != 0 {
		t.Fatalf("rows after the refused fallback = %+v, want its own row deleted", ps.Sessions)
	}
}

// TestGoAwayCarriesReconnectWindow (design v2.6 §3.3): a relay-initiated
// planned close sends GoAway.reconnect_after_ms = the configured jitter
// window, not a pre-jittered value (the dialer draws from [0, window]).
func TestGoAwayCarriesReconnectWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		wantMs uint32
	}{
		{name: "default", wantMs: uint32(relay.DefaultReconnectWindow / time.Millisecond)},
		{name: "configured", window: 2 * time.Second, wantMs: 2000},
		{name: "negative is zero", window: -time.Second, wantMs: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			n := w.StartNode("relay-a", func(c *relay.Config) { c.ReconnectWindow = tc.window })
			w.SetPrincipal("a", agentPrincipal("L1", 1))
			got := make(chan *conduitv1.GoAway, 1)
			cfg := conduit.Config{Interceptor: func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
				if ga := f.GetGoAway(); dir == conduit.Inbound && ga != nil {
					select {
					case got <- ga:
					default:
					}
				}
				return []*conduitv1.Frame{f}
			}}
			sess, _ := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
			go func() { <-sess.GoAwayReceived(); _ = sess.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := n.Relay.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			ga := relaytest.Wait(t, got, "GoAway")
			if ga.GetReconnectAfterMs() != tc.wantMs || ga.GetCode() != conduit.CloseRelayRestart {
				t.Fatalf("GoAway = code %d reconnect_after_ms %d, want 4503 and %d", ga.GetCode(), ga.GetReconnectAfterMs(), tc.wantMs)
			}
		})
	}
}

// TestTestHelpersWaitForRegistration (r3-F1): the session lookup test
// helpers wait for a session whose Welcome is out but which Serve has not
// registered yet, instead of returning a nil interceptor or an empty
// source.
func TestTestHelpersWaitForRegistration(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(t *testing.T, r *relay.Relay, id string) bool
	}{
		{name: "touch interceptor", call: func(t *testing.T, r *relay.Relay, id string) bool {
			return r.TouchInterceptorForTest(t, id, nil) != nil
		}},
		{name: "source", call: func(t *testing.T, r *relay.Relay, id string) bool {
			return r.SourceForTest(t, id) == relay.IncarnationSourceLaunchID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			n := w.StartNode("relay-a", nil)
			w.SetPrincipal("a", agentPrincipal("L1", 1))
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			n.Relay.SetBeforeReadyHookForTest(func() { <-release })
			waiting := make(chan struct{}, 1)
			n.Relay.SetPendingWaitHookForTest(func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			})
			_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
			result := make(chan bool, 1)
			go func() { result <- tc.call(t, n.Relay, wel.GetSessionId()) }()
			relaytest.Wait(t, waiting, "the helper to wait for the pending session")
			select {
			case <-result:
				t.Fatal("helper returned while the session was still pending")
			default:
			}
			unblock()
			if !relaytest.Wait(t, result, "helper") {
				t.Fatal("helper did not see the registered session")
			}
		})
	}
}

// TestShutdownBoundsDrainWriteConcurrency (r3-F2): Shutdown never has more
// than DrainWriteConcurrency session draining writes in flight, however
// many sessions the relay holds, and still sends GoAway to every session.
// Every write hangs until the shared drain deadline (a registry outage), so
// all writes the relay would issue at once are in flight together: with
// the bound exactly DrainWriteConcurrency, without it every session.
func TestShutdownBoundsDrainWriteConcurrency(t *testing.T) {
	const sessions = 3 * relay.DrainWriteConcurrency
	w := relaytest.NewWorld(t)
	var (
		armed    atomic.Bool
		inflight atomic.Int32
		maxSeen  atomic.Int32
	)
	w.Store.Fault = func(ctx context.Context, op string) error {
		if !armed.Load() || op != registry.OpSetSessionDraining {
			return nil
		}
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			m := maxSeen.Load()
			if cur <= m || maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
	n := w.StartNode("relay-a", nil)
	var dialed []conduit.LocalSession
	for i := range sessions {
		key := fmt.Sprintf("u%d", i)
		id := fmt.Sprintf("user-%d", i)
		w.SetPrincipal(key, relay.Principal{Kind: registry.PrincipalUser, ID: id})
		s, wel := n.MustDial(key, relaytest.UserHello(id), conduit.Config{})
		// Shutdown drains registered sessions only.
		_ = n.Relay.SourceForTest(t, wel.GetSessionId())
		dialed = append(dialed, s)
	}
	armed.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- n.Relay.Shutdown(ctx) }()
	for i, s := range dialed {
		relaytest.WaitClosed(t, s.GoAwayReceived(), fmt.Sprintf("GoAway to session %d", i))
	}
	_ = relaytest.Wait(t, result, "Shutdown")
	if got := maxSeen.Load(); got != relay.DrainWriteConcurrency {
		t.Fatalf("in-flight draining writes peaked at %d, want exactly the bound %d", got, relay.DrainWriteConcurrency)
	}
}
