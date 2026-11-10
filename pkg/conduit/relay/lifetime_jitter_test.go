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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// jitterHint dials one agent session on a relay with relayCap and the
// given JitterLifetimeCap, whose principal carries principalCap, and
// returns Welcome.lifetime_hint_s and how often the hook ran.
func jitterHint(t *testing.T, relayCap, principalCap time.Duration, jitter func(time.Duration) time.Duration) (uint32, int32) {
	t.Helper()
	var calls atomic.Int32
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", func(c *relay.Config) {
		c.LifetimeCap = relayCap
		if jitter != nil {
			c.JitterLifetimeCap = func(d time.Duration) time.Duration {
				calls.Add(1)
				return jitter(d)
			}
		}
		c.Session.Clock = c.Clock
		c.Session.PingInterval = 24 * time.Hour
		c.Session.PongWait = 48 * time.Hour
		c.Session.WriteWait = 7 * 24 * time.Hour
	})
	p := agentPrincipal("L1", 1)
	p.LifetimeCap = principalCap
	w.SetPrincipal("a", p)
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoConfig())
	return wel.GetLifetimeHintS(), calls.Load()
}

// ptone/scion#5245: the per-session jitter hook sets the session's relay
// cap, once per session.
func TestLifetimeCap_JitterSetsTheSessionCap(t *testing.T) {
	hint, calls := jitterHint(t, 3500*time.Second, 0, func(d time.Duration) time.Duration { return d * 8 / 10 })
	if hint != 2740 {
		t.Fatalf("Welcome.lifetime_hint_s = %d, want 2740 (0.8 x 3500s less the 60s lead)", hint)
	}
	if calls != 1 {
		t.Fatalf("JitterLifetimeCap calls = %d, want 1", calls)
	}
}

// A jittered cap at or below the GoAway lead is ignored: the configured
// cap applies.
func TestLifetimeCap_JitterAtOrBelowTheLeadIsIgnored(t *testing.T) {
	hint, _ := jitterHint(t, 3500*time.Second, 0, func(time.Duration) time.Duration { return relay.LifetimeGoAwayLead })
	if hint != 3440 {
		t.Fatalf("Welcome.lifetime_hint_s = %d, want 3440 (the configured cap less the 60s lead)", hint)
	}
}

// The principal's cap still lowers a jittered relay cap, and a jitter that
// raises the relay cap never raises the principal's.
func TestLifetimeCap_JitterNeverRaisesThePrincipalCap(t *testing.T) {
	hint, _ := jitterHint(t, 3500*time.Second, 120*time.Second, func(d time.Duration) time.Duration { return d * 12 / 10 })
	if hint != 60 {
		t.Fatalf("Welcome.lifetime_hint_s = %d, want 60 (the 120s principal cap less the 60s lead)", hint)
	}
}

// With no relay cap the hook is not called.
func TestLifetimeCap_JitterWithoutRelayCap(t *testing.T) {
	hint, calls := jitterHint(t, 0, 90*time.Second, func(d time.Duration) time.Duration { return d / 2 })
	if hint != 30 || calls != 0 {
		t.Fatalf("Welcome.lifetime_hint_s = %d, calls = %d; want 30 and 0", hint, calls)
	}
}

// ptone/scion#5245 review: under an ingress cap (the principal cap a hub in
// external relay mode sends) the relay's per-session caps still spread over
// the jitter range when the hub sends the range's top
// (config.ConduitLifetimeCapCeiling). Sending the unjittered cap instead
// would lower every session above it to that one value.
func TestLifetimeCap_JitterSpreadsUnderIngressCap(t *testing.T) {
	base := config.EffectiveConduitLifetimeCap(config.ConduitDefaultLifetimeCap, 900*time.Second) // 720s
	ceiling := config.ConduitLifetimeCapCeiling(base, true)                                       // 864s
	hints := map[uint32]bool{}
	var top uint32
	for _, u := range []float64{0, 0.25, 0.5, 0.75, 0.999} {
		u := u
		hint, _ := jitterHint(t, base, ceiling, func(d time.Duration) time.Duration {
			return config.JitterConduitLifetimeCap(d, true, u)
		})
		hints[hint] = true
		top = max(top, hint)
	}
	if len(hints) != 5 {
		t.Fatalf("lifetime hints under the ingress cap = %v, want 5 distinct values (caps spread)", hints)
	}
	if top <= uint32((base-relay.LifetimeGoAwayLead)/time.Second) {
		t.Fatalf("highest hint %d is not above the unjittered cap's %d: the upward jitter was cut", top, (base-relay.LifetimeGoAwayLead)/time.Second)
	}

	// The unjittered cap as the ingress cap collapses the upper half.
	collapsed, _ := jitterHint(t, base, base, func(d time.Duration) time.Duration {
		return config.JitterConduitLifetimeCap(d, true, 0.999)
	})
	if want := uint32((base - relay.LifetimeGoAwayLead) / time.Second); collapsed != want {
		t.Fatalf("with the unjittered ingress cap the hint = %d, want %d", collapsed, want)
	}
}
