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

package relay_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// TestRelayNotifyGrantKeysChanged: the hint goes to every local target
// session that advertised stream_renewal.v2, and to no other session
// (design v2.21.27 §3.5, Key-set freshness).
func TestRelayNotifyGrantKeysChanged(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	var v2Hints, plainHints, userHints atomic.Int32
	counting := func(n *atomic.Int32) conduit.Config {
		return conduit.Config{OnGrantKeysChanged: func(context.Context) { n.Add(1) }}
	}

	w.SetPrincipal("v2", agentPrincipal("L1", 1))
	v2Hello := relaytest.AgentHello(agentID, "L1", "", "pty")
	v2Hello.Capabilities.Features = []string{conduit.FeatureStreamRenewal}
	a.MustDial("v2", v2Hello, counting(&v2Hints))

	w.SetPrincipal("plain", relay.Principal{Kind: registry.PrincipalAgent, ID: "agent-plain", ProjectID: project,
		Agent: relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 1}})
	plainHello := relaytest.AgentHello("agent-plain", "L2", "", "pty")
	plainHello.Capabilities.Features = []string{"stream_renewal"} // the earlier name is not .v2
	a.MustDial("plain", plainHello, counting(&plainHints))

	w.SetPrincipal("user", relay.Principal{Kind: registry.PrincipalUser, ID: "user-1"})
	userHello := relaytest.UserHello("user-1")
	userHello.Capabilities.Features = []string{conduit.FeatureStreamRenewal}
	a.MustDial("user", userHello, counting(&userHints))

	if n := a.Relay.NotifyGrantKeysChanged(); n != 1 {
		t.Fatalf("sent on %d sessions, want 1", n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for v2Hints.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the stream_renewal.v2 target got no hint")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if plainHints.Load() != 0 || userHints.Load() != 0 {
		t.Fatalf("hints reached other sessions: plain %d, user %d", plainHints.Load(), userHints.Load())
	}
}
