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
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// brokerIncarnationEvents records a relay's broker incarnation hooks.
type brokerIncarnationEvents struct {
	mu         sync.Mutex
	superseded map[string]int
	flaps      []string
}

func (ev *brokerIncarnationEvents) hooks(c *relay.Config) {
	c.OnBrokerSessionsSuperseded = func(brokerID string, closed int) {
		ev.mu.Lock()
		defer ev.mu.Unlock()
		if ev.superseded == nil {
			ev.superseded = map[string]int{}
		}
		ev.superseded[brokerID] += closed
	}
	c.OnBrokerIncarnationFlap = func(brokerID, incarnation string) {
		ev.mu.Lock()
		defer ev.mu.Unlock()
		ev.flaps = append(ev.flaps, brokerID+"/"+incarnation)
	}
}

func (ev *brokerIncarnationEvents) snapshot() (map[string]int, []string) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	out := map[string]int{}
	for k, v := range ev.superseded {
		out[k] = v
	}
	return out, append([]string(nil), ev.flaps...)
}

// admissions returns a channel that receives the session id of every
// broker session whose admission step on n has finished. Install it
// before n serves sessions.
func admissions(n *relaytest.Node) <-chan string {
	ch := make(chan string, 64)
	n.Relay.SetAfterBrokerAdmissionHookForTest(func(sessionID string) { ch <- sessionID })
	return ch
}

// waitAdmission waits until the admission step of every session in
// sessionIDs has finished, in any order, within 10s overall. Ids not in
// sessionIDs are drained and ignored. The hook fires when a session has
// completed admission (is registered).
func waitAdmission(t *testing.T, ch <-chan string, sessionIDs ...string) {
	t.Helper()
	want := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		want[id] = true
	}
	deadline := time.After(10 * time.Second)
	for len(want) > 0 {
		select {
		case id := <-ch:
			delete(want, id)
		case <-deadline:
			missing := make([]string, 0, len(want))
			for id := range want {
				missing = append(missing, id)
			}
			sort.Strings(missing)
			t.Fatalf("timed out waiting for the admission step of %v", missing)
		}
	}
}

// requireClosedWith waits for ls to end and requires close code code.
func requireClosedWith(t *testing.T, ls conduit.LocalSession, code uint32, what string) {
	t.Helper()
	relaytest.WaitClosed(t, ls.Done(), what)
	if got := conduit.CodeOf(ls.Err(), 0); got != code {
		t.Fatalf("%s: close code %d (%v), want %d", what, got, ls.Err(), code)
	}
}

// requireOpen fails if ls has ended.
func requireOpen(t *testing.T, ls conduit.LocalSession, what string) {
	t.Helper()
	select {
	case <-ls.Done():
		t.Fatalf("%s closed: %v", what, ls.Err())
	default:
	}
}

// TestBrokerAdmissionClosesOlderIncarnation: admitting a broker session
// with a new incarnation closes that broker's local sessions of the older
// incarnation with 4409 superseded_incarnation and reports how many, before
// they close. The broker's session on another relay (closed by its session
// re-check), a session of another broker and the new session stay open,
// and another session of the current incarnation closes nothing.
func TestBrokerAdmissionClosesOlderIncarnation(t *testing.T) {
	w := relaytest.NewWorld(t)
	var evA, evB brokerIncarnationEvents
	a := w.StartNode("relay-a", evA.hooks)
	b := w.StartNode("relay-b", evB.hooks)
	admittedA := admissions(a)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})
	w.SetPrincipal("b2", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-2"})

	old1, w1 := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	old2, w2 := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	remote, _ := b.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	other, wo := a.MustDial("b2", relaytest.BrokerHello("broker-2", "proc-1"), conduit.Config{})
	// The test waits until each earlier session has completed admission
	// (is registered) before the next dial. The admission steps may finish
	// in any order.
	waitAdmission(t, admittedA, w1.GetSessionId(), w2.GetSessionId(), wo.GetSessionId())
	if got, _ := evA.snapshot(); len(got) != 0 {
		t.Fatalf("superseded before any new incarnation: %v", got)
	}

	cur, wc := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	requireClosedWith(t, old1, relay.CloseSupersededIncarnation, "first older-incarnation session")
	requireClosedWith(t, old2, relay.CloseSupersededIncarnation, "second older-incarnation session")
	// The count is reported before the closes: it is already in.
	superseded, flaps := evA.snapshot()
	if len(superseded) != 1 || superseded["broker-1"] != 2 {
		t.Fatalf("superseded = %v, want broker-1: 2", superseded)
	}
	waitAdmission(t, admittedA, wc.GetSessionId())
	requireOpen(t, remote, "the older-incarnation session on another relay")
	requireOpen(t, other, "another broker's session")
	requireOpen(t, cur, "the new incarnation's session")
	if len(flaps) != 0 {
		t.Fatalf("flaps = %v, want none", flaps)
	}
	if got, _ := evB.snapshot(); len(got) != 0 {
		t.Fatalf("relay-b reported %v; it admitted no new incarnation", got)
	}

	again, wa := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	waitAdmission(t, admittedA, wa.GetSessionId())
	requireOpen(t, cur, "the current incarnation's earlier session")
	requireOpen(t, again, "the current incarnation's new session")
	if superseded, _ := evA.snapshot(); superseded["broker-1"] != 2 {
		t.Fatalf("superseded = %v after a same-incarnation admission, want still 2", superseded)
	}
}

// TestBrokerIncarnationFlapReported: a broker that returns to an
// incarnation the relay admitted within the window is reported once per
// return, and is still admitted: the return closes the other
// incarnation's session, as any newer admission does.
func TestBrokerIncarnationFlapReported(t *testing.T) {
	w := relaytest.NewWorld(t)
	var ev brokerIncarnationEvents
	a := w.StartNode("relay-a", ev.hooks)
	admitted := admissions(a)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	first, w1 := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	// The test waits until first has completed admission (is registered)
	// before the next dial.
	waitAdmission(t, admitted, w1.GetSessionId())
	second, w2 := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	requireClosedWith(t, first, relay.CloseSupersededIncarnation, "proc-1 session")
	waitAdmission(t, admitted, w2.GetSessionId())
	if superseded, flaps := ev.snapshot(); superseded["broker-1"] != 1 || len(flaps) != 0 {
		t.Fatalf("after a first change: superseded %v, flaps %v; want broker-1: 1 and no flap", superseded, flaps)
	}

	back, wb := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	requireClosedWith(t, second, relay.CloseSupersededIncarnation, "proc-2 session")
	waitAdmission(t, admitted, wb.GetSessionId())
	requireOpen(t, back, "the returning proc-1 session")
	superseded, flaps := ev.snapshot()
	if len(flaps) != 1 || flaps[0] != "broker-1/proc-1" {
		t.Fatalf("flaps = %v, want [broker-1/proc-1]", flaps)
	}
	if superseded["broker-1"] != 2 {
		t.Fatalf("superseded = %v, want broker-1: 2", superseded)
	}
}

// TestBrokerSupersededSessionClosedOnce: when the close path runs twice
// for the same older-incarnation session before it ends (the second run
// starts after the first marked the session, before it closed it), the
// session is closed once with 4409 and counted once.
func TestBrokerSupersededSessionClosedOnce(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	admitted := admissions(a)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})
	old, wo := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	waitAdmission(t, admitted, wo.GetSessionId())

	var reported atomic.Int64
	report := func(n int) { reported.Add(int64(n)) }
	closeOlder := func() int {
		return a.Relay.CloseOlderIncarnationSessions(registry.PrincipalBroker, "broker-1", "proc-2", 1<<62,
			relay.CloseSupersededIncarnation, relay.SupersededIncarnationReason(), report)
	}
	second := -1
	var nested atomic.Bool
	a.Relay.SetBeforeIncarnationCloseHookForTest(func() {
		if !nested.CompareAndSwap(false, true) {
			return
		}
		requireOpen(t, old, "the session marked by the first run")
		second = closeOlder()
	})

	first := closeOlder()
	requireClosedWith(t, old, relay.CloseSupersededIncarnation, "the older-incarnation session")
	if first != 1 || second != 0 {
		t.Fatalf("closed by the first run %d and by the second %d, want 1 and 0", first, second)
	}
	if got := reported.Load(); got != 1 {
		t.Fatalf("reported %d, want 1", got)
	}
	if again := closeOlder(); again != 0 {
		t.Fatalf("a run after the session ended closed %d, want 0", again)
	}
	if got := reported.Load(); got != 1 {
		t.Fatalf("reported %d after a run on the ended session, want 1", got)
	}
}
