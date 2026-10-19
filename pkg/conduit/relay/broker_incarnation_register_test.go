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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// registerAdmissions records the admission steps of one relay: ch receives
// the session id of every broker session whose admission step finished,
// and seen keeps every id read, so a later wait still sees it.
type registerAdmissions struct {
	ch   chan string
	seen map[string]bool
}

func newRegisterAdmissions(n *relaytest.Node) *registerAdmissions {
	l := &registerAdmissions{ch: make(chan string, 64), seen: map[string]bool{}}
	n.Relay.SetAfterBrokerAdmissionHookForTest(func(sessionID string) { l.ch <- sessionID })
	return l
}

// wait waits until the admission step of sessionID has finished.
func (l *registerAdmissions) wait(t *testing.T, sessionID string) {
	t.Helper()
	for !l.seen[sessionID] {
		l.seen[relaytest.Wait(t, l.ch, "admission step of "+sessionID)] = true
	}
}

// registerGate holds the nth call of a relay's Serve hook until released.
// entered is signalled when that call arrives.
type registerGate struct {
	nth     int32
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func newRegisterGate(nth int32) *registerGate {
	return &registerGate{nth: nth, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *registerGate) hook() {
	if g.calls.Add(1) == g.nth {
		g.entered <- struct{}{}
		<-g.release
	}
}

// countingRPCs is a broker RPC handler that counts the requests it serves.
func countingRPCs(served *atomic.Int32) conduit.Config {
	return conduit.Config{RPCHandler: conduit.RPCHandlerFunc(func(context.Context, *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		served.Add(1)
		return &conduitv1.RpcResponse{Status: 200}
	})}
}

// TestBrokerIncarnation_RegistrationOrder_OlderClosesItself checks that a
// broker session whose registration finds a live higher-epoch session of
// another incarnation of its broker closes itself with 4409, counted once;
// that Local does not return it; that an RPC sent to it through the owner
// relay is refused as not local and never served; that the flap memory is
// unchanged (a later session of the current incarnation reports no flap);
// and that the higher-epoch session stays open.
func TestBrokerIncarnation_RegistrationOrder_OlderClosesItself(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := newRegisterAdmissions(a)
	gate := newRegisterGate(1)
	a.Relay.SetBeforeReadyHookForTest(gate.hook)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	var oldServed atomic.Int32
	old, wo := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), countingRPCs(&oldServed))
	relaytest.Wait(t, gate.entered, "the proc-1 session held before registration")
	var oldRec registry.SessionRecord
	for _, v := range w.Sessions(registry.PrincipalBroker, "broker-1").Sessions {
		if v.Session.SessionID == wo.GetSessionId() {
			oldRec = v.Session
		}
	}
	if oldRec.SessionID == "" {
		t.Fatalf("no registry row for the proc-1 session %s", wo.GetSessionId())
	}
	local := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _, ok := a.Relay.Local(ctx, wo.GetSessionId())
		local <- ok
	}()

	cur, wc := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	admitted.wait(t, wc.GetSessionId())
	if superseded, _ := ev.snapshot(); len(superseded) != 0 {
		t.Fatalf("superseded = %v before the proc-1 session registered, want none", superseded)
	}

	close(gate.release)
	requireClosedWith(t, old, relay.CloseSupersededIncarnation, "the proc-1 session")
	superseded, flaps := ev.snapshot()
	if superseded["broker-1"] != 1 || len(superseded) != 1 {
		t.Fatalf("superseded = %v, want broker-1: 1", superseded)
	}
	if len(flaps) != 0 {
		t.Fatalf("flaps = %v, want none", flaps)
	}
	requireOpen(t, cur, "the proc-2 session")
	if ok := relaytest.Wait(t, local, "Local for the proc-1 session"); ok {
		t.Fatal("Local returned the self-closed proc-1 session")
	}
	// An RPC to the proc-1 session through the owner relay's internal API,
	// as another node sends it.
	b := w.StartNode("relay-b", nil)
	remote := relay.NewRemoteSession(b.Peers, a.Internal.URL, oldRec, registry.Want{Incarnation: "proc-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := remote.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "GET", Path: "/x"})
	var stale *relay.StaleRouteError
	if !errors.As(err, &stale) || stale.Reason != "not_local" {
		t.Fatalf("RPC to the self-closed proc-1 session = %v, want a not_local stale route", err)
	}
	if n := oldServed.Load(); n != 0 {
		t.Fatalf("the proc-1 session served %d requests, want 0", n)
	}

	again, wa := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	admitted.wait(t, wa.GetSessionId())
	requireOpen(t, again, "a later proc-2 session")
	if superseded, flaps := ev.snapshot(); len(flaps) != 0 || superseded["broker-1"] != 1 {
		t.Fatalf("after a later proc-2 session: superseded %v, flaps %v; want broker-1: 1 and no flap", superseded, flaps)
	}
}

// TestBrokerIncarnation_RegistrationOrder_NewerClosesOlder checks that when
// the lower-epoch session is registered first, the higher-epoch session's
// admission closes it with 4409, counted once, and the higher-epoch session
// stays open.
func TestBrokerIncarnation_RegistrationOrder_NewerClosesOlder(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := newRegisterAdmissions(a)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	old, wo := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	admitted.wait(t, wo.GetSessionId())
	cur, wc := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	requireClosedWith(t, old, relay.CloseSupersededIncarnation, "the proc-1 session")
	admitted.wait(t, wc.GetSessionId())
	requireOpen(t, cur, "the proc-2 session")
	if superseded, _ := ev.snapshot(); superseded["broker-1"] != 1 || len(superseded) != 1 {
		t.Fatalf("superseded = %v, want broker-1: 1", superseded)
	}
}

// TestBrokerIncarnation_RegistrationOrder_ClosedOnce checks that when the
// higher-epoch session's admission step runs after the lower-epoch session
// is registered, the lower-epoch session is closed once with 4409, counted
// once, its row is deleted, and the higher-epoch session's row stays.
func TestBrokerIncarnation_RegistrationOrder_ClosedOnce(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := newRegisterAdmissions(a)
	before := newRegisterGate(1) // the proc-1 session, before it registers
	after := newRegisterGate(1)  // the proc-2 session, after it registers
	a.Relay.SetBeforeReadyHookForTest(before.hook)
	a.Relay.SetAfterReadyHookForTest(after.hook)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	old, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	relaytest.Wait(t, before.entered, "the proc-1 session held before registration")
	cur, wc := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	relaytest.Wait(t, after.entered, "the proc-2 session held after registration")

	close(before.release)
	requireClosedWith(t, old, relay.CloseSupersededIncarnation, "the proc-1 session")
	// Serve deletes the session's row before it returns.
	relaytest.Wait(t, a.Served, "the proc-1 session's Serve returned")
	close(after.release)
	admitted.wait(t, wc.GetSessionId())
	requireOpen(t, cur, "the proc-2 session")

	if superseded, _ := ev.snapshot(); superseded["broker-1"] != 1 || len(superseded) != 1 {
		t.Fatalf("superseded = %v, want broker-1: 1", superseded)
	}
	rows := w.Sessions(registry.PrincipalBroker, "broker-1").Sessions
	if len(rows) != 1 || rows[0].Session.SessionID != wc.GetSessionId() {
		t.Fatalf("broker-1 rows = %+v, want only the proc-2 session %s", rows, wc.GetSessionId())
	}
}

// TestBrokerIncarnation_RegistrationOrder_SameIncarnationKept checks that a
// session whose registration finds a higher-epoch session of the same
// incarnation stays open.
func TestBrokerIncarnation_RegistrationOrder_SameIncarnationKept(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := newRegisterAdmissions(a)
	gate := newRegisterGate(1)
	a.Relay.SetBeforeReadyHookForTest(gate.hook)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	first, wf := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	relaytest.Wait(t, gate.entered, "the first session held before registration")
	second, ws := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	admitted.wait(t, ws.GetSessionId())
	close(gate.release)
	admitted.wait(t, wf.GetSessionId())
	requireOpen(t, first, "the first proc-1 session")
	requireOpen(t, second, "the second proc-1 session")
	if superseded, _ := ev.snapshot(); len(superseded) != 0 {
		t.Fatalf("superseded = %v, want none", superseded)
	}
}

// TestBrokerIncarnation_RegistrationOrder_HigherEpochKept checks that the
// higher-epoch session of a newer incarnation stays open and closes the
// lower-epoch session.
func TestBrokerIncarnation_RegistrationOrder_HigherEpochKept(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := newRegisterAdmissions(a)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	old, wo := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	admitted.wait(t, wo.GetSessionId())
	cur, wc := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	admitted.wait(t, wc.GetSessionId())
	requireOpen(t, cur, "the proc-2 session")
	requireClosedWith(t, old, relay.CloseSupersededIncarnation, "the proc-1 session")
}

// TestBrokerIncarnation_RegistrationOrder_DoneSessionSkipped checks that a
// higher-epoch session of another incarnation that has ended, while still
// in the relay's session set, is not live: the registering session stays
// open and completes admission, and nothing is counted.
func TestBrokerIncarnation_RegistrationOrder_DoneSessionSkipped(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := newRegisterAdmissions(a)
	gate := newRegisterGate(1)
	a.Relay.SetBeforeReadyHookForTest(gate.hook)
	ended := make(chan string, 4)
	release := make(chan struct{})
	a.Relay.SetAfterDoneHookForTest(func(sessionID string) {
		ended <- sessionID
		<-release
	})
	t.Cleanup(func() { close(release) })
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	old, wo := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	relaytest.Wait(t, gate.entered, "the proc-1 session held before registration")
	cur, wc := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	admitted.wait(t, wc.GetSessionId())
	_ = cur.Close()
	if id := relaytest.Wait(t, ended, "the proc-2 session ended, still in the session set"); id != wc.GetSessionId() {
		t.Fatalf("ended session %s, want the proc-2 session %s", id, wc.GetSessionId())
	}

	close(gate.release)
	admitted.wait(t, wo.GetSessionId())
	requireOpen(t, old, "the proc-1 session")
	if superseded, flaps := ev.snapshot(); len(superseded) != 0 || len(flaps) != 0 {
		t.Fatalf("superseded %v, flaps %v; want none", superseded, flaps)
	}
}

// TestBrokerIncarnation_RegistrationOrder_DrainingSessionCounts checks that
// a higher-epoch session of another incarnation that is draining (it sent
// GoAway, with an RPC still in flight) is live: the registering session
// closes itself with 4409.
func TestBrokerIncarnation_RegistrationOrder_DrainingSessionCounts(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := newRegisterAdmissions(a)
	gate := newRegisterGate(1)
	a.Relay.SetBeforeReadyHookForTest(gate.hook)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	// The proc-2 session holds the first RPC it receives until released,
	// so its drain stays open.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce atomic.Bool
	releaseHeld := func() {
		if releaseOnce.CompareAndSwap(false, true) {
			close(release)
		}
	}
	t.Cleanup(releaseHeld)
	holding := conduit.Config{RPCHandler: conduit.RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &conduitv1.RpcResponse{Status: 200}
	})}

	old, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	relaytest.Wait(t, gate.entered, "the proc-1 session held before registration")
	cur, wc := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), holding)
	admitted.wait(t, wc.GetSessionId())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relaySide, _, ok := a.Relay.Local(ctx, wc.GetSessionId())
	if !ok {
		t.Fatal("relay-a does not hold the proc-2 session")
	}
	held := make(chan error, 1)
	go func() {
		_, err := relaySide.Call(ctx, &conduitv1.RpcRequest{RequestId: "hold", Method: "GET", Path: "/hold"})
		held <- err
	}()
	relaytest.Wait(t, entered, "the held RPC reached the proc-2 session")
	if err := cur.GoAway(conduit.GoAwayOptions{DrainDeadline: time.Minute}); err != nil {
		t.Fatal(err)
	}
	relaytest.WaitClosed(t, relaySide.GoAwayReceived(), "relay-a saw the proc-2 session's GoAway")
	select {
	case <-relaySide.Done():
		t.Fatalf("the draining proc-2 session ended before the proc-1 session registered: %v", relaySide.Err())
	default:
	}

	close(gate.release)
	requireClosedWith(t, old, relay.CloseSupersededIncarnation, "the proc-1 session")
	if superseded, flaps := ev.snapshot(); superseded["broker-1"] != 1 || len(flaps) != 0 {
		t.Fatalf("superseded %v, flaps %v; want broker-1: 1 and no flap", superseded, flaps)
	}
	releaseHeld()
	if err := relaytest.Wait(t, held, "the held RPC completed"); err != nil {
		t.Fatalf("held RPC = %v, want it to complete during the drain", err)
	}
}

// TestBrokerIncarnation_FlapEpochOrder checks that the flap memory updates
// in connection_epoch order. A proc-1 session is admitted. A second proc-1
// session is held before registration while a higher-epoch proc-2 session
// is admitted (closing the first proc-1 session) and then ends, staying in
// the session set. The held proc-1 session is then admitted below the
// recorded epoch: no flap is reported and the memory is unchanged, which a
// later proc-2 session shows by reporting no flap either.
func TestBrokerIncarnation_FlapEpochOrder(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := newRegisterAdmissions(a)
	gate := newRegisterGate(2) // the second session to be served: the held proc-1 one
	a.Relay.SetBeforeReadyHookForTest(gate.hook)
	var holdID atomic.Pointer[string]
	ended := make(chan string, 4)
	release := make(chan struct{})
	a.Relay.SetAfterDoneHookForTest(func(sessionID string) {
		if p := holdID.Load(); p != nil && *p == sessionID {
			ended <- sessionID
			<-release
		}
	})
	t.Cleanup(func() { close(release) })
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	first, wf := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	admitted.wait(t, wf.GetSessionId())
	held, wh := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	relaytest.Wait(t, gate.entered, "the second proc-1 session held before registration")
	cur, wc := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	id := wc.GetSessionId()
	holdID.Store(&id)
	admitted.wait(t, wc.GetSessionId())
	requireClosedWith(t, first, relay.CloseSupersededIncarnation, "the first proc-1 session")
	_ = cur.Close()
	relaytest.Wait(t, ended, "the proc-2 session ended, still in the session set")

	close(gate.release)
	admitted.wait(t, wh.GetSessionId())
	requireOpen(t, held, "the second proc-1 session")
	superseded, flaps := ev.snapshot()
	if len(flaps) != 0 {
		t.Fatalf("flaps = %v, want none: the proc-1 admission is below the recorded epoch", flaps)
	}
	if superseded["broker-1"] != 1 {
		t.Fatalf("superseded = %v, want broker-1: 1 (the first proc-1 session)", superseded)
	}

	again, wa := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	admitted.wait(t, wa.GetSessionId())
	requireOpen(t, again, "a later proc-2 session")
	if _, flaps := ev.snapshot(); len(flaps) != 0 {
		t.Fatalf("flaps = %v after a later proc-2 session, want none: the memory still records proc-2", flaps)
	}
}
