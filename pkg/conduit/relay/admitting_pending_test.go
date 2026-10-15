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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestSetAdmitting_DrainsSessionAdmittedBeforeRegistration (design §3.9):
// a session that is admitted (Welcome sent) but not yet registered when
// admission stops is drained like a registered one. The before-ready seam
// stops admission in exactly that window, so the session is not in
// SetAdmitting's snapshot; Serve's post-register check must send it the
// drain GoAway (reason draining, the default 30s drain deadline).
func TestSetAdmitting_DrainsSessionAdmittedBeforeRegistration(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	var once sync.Once
	n.Relay.SetBeforeReadyHookForTest(func() {
		once.Do(func() { n.Relay.SetAdmitting(false) })
	})
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	target, _ := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)

	g := relaytest.Wait(t, goAways, "drain GoAway to the session admitted before registration")
	if g.GetReason() != relay.ReasonDraining || g.GetDrainDeadlineMs() != 30000 {
		t.Fatalf("GoAway = {%q, %dms}, want {draining, 30000ms}", g.GetReason(), g.GetDrainDeadlineMs())
	}
	relaytest.WaitClosed(t, target.GoAwayReceived(), "GoAway on the target session")
	if n.Relay.Admitting() {
		t.Fatal("admission still on")
	}
	if !relayRow(t, w, "relay-a").Draining {
		t.Fatal("relay row not marked draining")
	}
}

// TestServe_NotPausedSessionRegistersWithoutGoAway is the control for the
// test above: a session that passes the same window while the relay keeps
// admitting registers and serves, and gets no GoAway. It passes with and
// without the post-register paused check; it guards against a fix that
// drains every session.
func TestServe_NotPausedSessionRegistersWithoutGoAway(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	reached := make(chan struct{})
	var once sync.Once
	n.Relay.SetBeforeReadyHookForTest(func() {
		once.Do(func() { close(reached) })
	})
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)

	relaytest.WaitClosed(t, reached, "the window between admission and registration")
	// The echo round trip runs after registration on the same connection,
	// so a GoAway sent at registration would already have been received.
	openEcho(t, n, wel.GetSessionId())
	select {
	case g := <-goAways:
		t.Fatalf("unexpected GoAway {%q, %dms} to a session admitted while the relay admits", g.GetReason(), g.GetDrainDeadlineMs())
	default:
	}
	if !n.Relay.Admitting() {
		t.Fatal("admission stopped")
	}
	if relayRow(t, w, "relay-a").Draining {
		t.Fatal("relay row marked draining")
	}
}

// TestSetAdmitting_ResumeBeforeRegistrationRegistersWithoutGoAway: when
// admission stops and resumes again inside the window between admission
// and registration, the relay admits again by the time the session
// registers, so it registers and serves with no GoAway. It passes with and
// without the post-register paused check; it guards the resume path against
// a fix that drains a session admission has already resumed for.
func TestSetAdmitting_ResumeBeforeRegistrationRegistersWithoutGoAway(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	reached := make(chan struct{})
	var once sync.Once
	n.Relay.SetBeforeReadyHookForTest(func() {
		once.Do(func() {
			n.Relay.SetAdmitting(false)
			n.Relay.SetAdmitting(true)
			close(reached)
		})
	})
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)

	relaytest.WaitClosed(t, reached, "admission stopped and resumed in the window")
	openEcho(t, n, wel.GetSessionId())
	select {
	case g := <-goAways:
		t.Fatalf("unexpected GoAway {%q, %dms} after admission resumed", g.GetReason(), g.GetDrainDeadlineMs())
	default:
	}
	if !n.Relay.Admitting() {
		t.Fatal("admission not resumed")
	}
	if relayRow(t, w, "relay-a").Draining {
		t.Fatal("relay row still draining")
	}
}
