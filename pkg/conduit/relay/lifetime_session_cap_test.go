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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// A session's lifetime cap from its principal (design §3.9 "Source of the
// cap", v2.21.53): it lowers the relay's cap and never raises it, and
// with no relay cap it is the session's cap.

// sessionCapHint dials one agent session on a relay with relayCap whose
// principal carries principalCap, and returns the node, the dial time and
// Welcome.lifetime_hint_s.
func sessionCapHint(t *testing.T, relayCap, principalCap time.Duration) (*relaytest.Node, time.Time, uint32, string) {
	t.Helper()
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, relayCap, 0)
	p := agentPrincipal("L1", 1)
	p.LifetimeCap = principalCap
	w.SetPrincipal("a", p)
	t0 := n.Clock.Now()
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoConfig())
	return n, t0, wel.GetLifetimeHintS(), wel.GetSessionId()
}

// TestLifetimeCap_PrincipalCapLowersTheRelayCap: a principal cap below the
// relay's sets the session's GoAway time.
func TestLifetimeCap_PrincipalCapLowersTheRelayCap(t *testing.T) {
	if _, _, hint, _ := sessionCapHint(t, 3500*time.Second, 120*time.Second); hint != 60 {
		t.Fatalf("Welcome.lifetime_hint_s = %d, want 60 (120s cap less the 60s lead)", hint)
	}
}

// TestLifetimeCap_PrincipalCapNeverRaisesTheRelayCap: a principal cap above
// the relay's leaves the relay's.
func TestLifetimeCap_PrincipalCapNeverRaisesTheRelayCap(t *testing.T) {
	if _, _, hint, _ := sessionCapHint(t, 90*time.Second, time.Hour); hint != 30 {
		t.Fatalf("Welcome.lifetime_hint_s = %d, want 30 (the relay's 90s cap less the 60s lead)", hint)
	}
}

// TestLifetimeCap_PrincipalCapWithoutRelayCap: a relay with no cap
// (LifetimeCap 0) applies the principal's cap, and the session ends at it:
// GoAway 60s before the cap, and the session closed by the cap.
func TestLifetimeCap_PrincipalCapWithoutRelayCap(t *testing.T) {
	n, t0, hint, sessionID := sessionCapHint(t, 0, 90*time.Second)
	if hint != 30 {
		t.Fatalf("Welcome.lifetime_hint_s = %d, want 30 (the 90s principal cap less the 60s lead)", hint)
	}
	ls, _ := openEcho(t, n, sessionID)
	waitTimer(t, n, t0.Add(30*time.Second), "lifetime GoAway")
	n.Clock.Advance(30 * time.Second)
	if !ls.Info().Draining {
		t.Fatal("no GoAway 60s before the principal cap")
	}
	waitTimer(t, n, t0.Add(60*time.Second), "drain deadline")
	n.Clock.Advance(30 * time.Second)
	relaytest.WaitClosed(t, ls.Done(), "session to end by the principal cap")
	if left := t0.Add(90 * time.Second).Sub(n.Clock.Now()); left < 0 {
		t.Fatalf("session ended %s after the principal cap", -left)
	}
}

// TestLifetimeCap_PrincipalCapBoundsTheDrainDeadline: under a principal
// cap (120s, below the relay's 3500s), the lifetime GoAway at 60s leaves
// 60s, and the 90s session drain deadline is cut to end at the principal
// cap, not the relay's.
func TestLifetimeCap_PrincipalCapBoundsTheDrainDeadline(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 3500*time.Second, 90*time.Second)
	p := agentPrincipal("L1", 1)
	p.LifetimeCap = 120 * time.Second
	w.SetPrincipal("a", p)
	t0 := n.Clock.Now()
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
	ls, st := openEcho(t, n, wel.GetSessionId())
	waitTimer(t, n, t0.Add(60*time.Second), "lifetime GoAway")

	n.Clock.Advance(60 * time.Second)
	g := relaytest.Wait(t, goAways, "GoAway")
	if g.GetDrainDeadlineMs() != 60000 {
		t.Fatalf("drain_deadline_ms = %d, want 60000 (ends at the principal cap)", g.GetDrainDeadlineMs())
	}
	waitTimer(t, n, t0.Add(120*time.Second), "drain deadline")
	n.Clock.Advance(60*time.Second - time.Millisecond)
	if isClosed(ls.Done()) {
		t.Fatal("session closed before the drain deadline")
	}
	n.Clock.Advance(time.Millisecond)
	if ce := streamEnd(t, st); ce.Code != conduit.CloseRelayRestart {
		t.Fatalf("stream closed with %d, want 4503", ce.Code)
	}
	relaytest.WaitClosed(t, ls.Done(), "session to end by the principal cap")
	if !n.Clock.Now().Equal(t0.Add(120 * time.Second)) {
		t.Fatalf("drain ended at +%s, want the principal cap (+2m0s)", n.Clock.Now().Sub(t0))
	}
}
