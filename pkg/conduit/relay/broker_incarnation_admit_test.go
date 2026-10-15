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
	"sync"
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

// waitSuperseded waits until the superseded count reported for brokerID
// is want. The relay reports the count after it closed the sessions, so a
// test that saw a close waits for the report.
func (ev *brokerIncarnationEvents) waitSuperseded(t *testing.T, brokerID string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		superseded, _ := ev.snapshot()
		if superseded[brokerID] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("superseded = %v, want %s: %d", superseded, brokerID, want)
		}
		time.Sleep(5 * time.Millisecond)
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
// incarnation with 4409 superseded_incarnation and reports how many. The
// broker's session on another relay (closed by its session re-check), a
// session of another broker and the new session stay open.
func TestBrokerAdmissionClosesOlderIncarnation(t *testing.T) {
	w := relaytest.NewWorld(t)
	var evA, evB brokerIncarnationEvents
	a := w.StartNode("relay-a", evA.hooks)
	b := w.StartNode("relay-b", evB.hooks)
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})
	w.SetPrincipal("b2", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-2"})

	old1, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	old2, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	remote, _ := b.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	other, _ := a.MustDial("b2", relaytest.BrokerHello("broker-2", "proc-1"), conduit.Config{})
	if got, _ := evA.snapshot(); len(got) != 0 {
		t.Fatalf("superseded before any new incarnation: %v", got)
	}

	cur, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	requireClosedWith(t, old1, relay.CloseSupersededIncarnation, "first older-incarnation session")
	requireClosedWith(t, old2, relay.CloseSupersededIncarnation, "second older-incarnation session")
	requireOpen(t, remote, "the older-incarnation session on another relay")
	requireOpen(t, other, "another broker's session")
	requireOpen(t, cur, "the new incarnation's session")

	evA.waitSuperseded(t, "broker-1", 2)
	superseded, flaps := evA.snapshot()
	if len(superseded) != 1 {
		t.Fatalf("superseded = %v, want only broker-1", superseded)
	}
	if len(flaps) != 0 {
		t.Fatalf("flaps = %v, want none", flaps)
	}
	if got, _ := evB.snapshot(); len(got) != 0 {
		t.Fatalf("relay-b reported %v; it admitted no new incarnation", got)
	}

	// Another session of the current incarnation closes nothing.
	again, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
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
	w.SetPrincipal("b1", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})

	first, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	second, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-2"), conduit.Config{})
	requireClosedWith(t, first, relay.CloseSupersededIncarnation, "proc-1 session")
	ev.waitSuperseded(t, "broker-1", 1)
	if _, flaps := ev.snapshot(); len(flaps) != 0 {
		t.Fatalf("flaps = %v after a first change, want none", flaps)
	}

	back, _ := a.MustDial("b1", relaytest.BrokerHello("broker-1", "proc-1"), conduit.Config{})
	requireClosedWith(t, second, relay.CloseSupersededIncarnation, "proc-2 session")
	requireOpen(t, back, "the returning proc-1 session")
	ev.waitSuperseded(t, "broker-1", 2)
	if _, flaps := ev.snapshot(); len(flaps) != 1 || flaps[0] != "broker-1/proc-1" {
		t.Fatalf("flaps = %v, want [broker-1/proc-1]", flaps)
	}
}
