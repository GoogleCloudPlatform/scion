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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
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
	_, v2Welcome := a.MustDial("v2", v2Hello, counting(&v2Hints))

	w.SetPrincipal("plain", relay.Principal{Kind: registry.PrincipalAgent, ID: "agent-plain", ProjectID: project,
		Agent: relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 1}})
	plainHello := relaytest.AgentHello("agent-plain", "L2", "", "pty")
	plainHello.Capabilities.Features = []string{"stream_renewal"} // the earlier name is not .v2
	plain, plainWelcome := a.MustDial("plain", plainHello, counting(&plainHints))

	w.SetPrincipal("user", relay.Principal{Kind: registry.PrincipalUser, ID: "user-1"})
	userHello := relaytest.UserHello("user-1")
	userHello.Capabilities.Features = []string{conduit.FeatureStreamRenewal}
	user, userWelcome := a.MustDial("user", userHello, counting(&userHints))

	// MustDial returns once the Welcome arrives, which the relay sends
	// while the session is still pending; NotifyGrantKeysChanged only sees
	// registered sessions. Wait for every dialled session to register so
	// the count, and the absence of hints on the others, are both
	// meaningful (ptone/scion#5338).
	for name, wel := range map[string]*conduitv1.Welcome{"v2": v2Welcome, "plain": plainWelcome, "user": userWelcome} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _, ok := a.Relay.Local(ctx, wel.GetSessionId())
		cancel()
		if !ok {
			t.Fatalf("%s: the session did not register on the relay", name)
		}
	}

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
	// Barrier: the relay answers each RPC after anything it queued on that
	// session before, so a hint sent to these sessions would arrive first.
	for name, s := range map[string]conduit.LocalSession{"plain": plain, "user": user} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := s.Call(ctx, &conduitv1.RpcRequest{Path: "/barrier"})
		cancel()
		if err != nil || resp == nil {
			t.Fatalf("%s: the barrier call did not reach the relay: %v", name, err)
		}
	}
	if plainHints.Load() != 0 || userHints.Load() != 0 {
		t.Fatalf("hints reached other sessions: plain %d, user %d", plainHints.Load(), userHints.Load())
	}
}
