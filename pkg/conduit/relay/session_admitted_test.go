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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func TestOnAgentSessionAdmitted(t *testing.T) {
	t.Run("fires after registration and revalidation with cloned rpc routes", func(t *testing.T) {
		type admittedEvent struct {
			agentID string
			routes  []string
		}
		admittedCh := make(chan admittedEvent, 4)

		w := relaytest.NewWorld(t)
		n := w.StartNode("relay-a", func(c *relay.Config) {
			c.OnAgentSessionAdmitted = func(id string, routes []string) {
				cp := slices.Clone(routes)
				if len(routes) > 0 {
					routes[0] = "mutated"
				}
				admittedCh <- admittedEvent{agentID: id, routes: cp}
			}
		})

		w.SetPrincipal("agent-tok", agentPrincipal("L2", 3))
		hello := relaytest.AgentHello(agentID, "L2", "", "pty")
		hello.Capabilities.Rpc = []string{"deliver-message", "custom-route"}
		n.MustDial("agent-tok", hello, conduit.Config{})

		select {
		case ev := <-admittedCh:
			if ev.agentID != agentID {
				t.Fatalf("agentID = %q, want %q", ev.agentID, agentID)
			}
			want := []string{"deliver-message", "custom-route"}
			if !slices.Equal(ev.routes, want) {
				t.Fatalf("routes = %v, want %v", ev.routes, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for OnAgentSessionAdmitted")
		}

		// Registry record was not mutated by caller altering the slice.
		ps := w.Sessions(registry.PrincipalAgent, agentID)
		if len(ps.Sessions) != 1 || !slices.Equal(ps.Sessions[0].Session.Capabilities.RPC, []string{"deliver-message", "custom-route"}) {
			t.Fatalf("registry Capabilities.RPC = %v", ps.Sessions)
		}
	})

	t.Run("suppressed when Revalidate rejects or principal is non-agent", func(t *testing.T) {
		var mu sync.Mutex
		var seen []string

		w := relaytest.NewWorld(t)
		n := w.StartNode("relay-a", func(c *relay.Config) {
			c.Revalidate = func(_ context.Context, p relay.Principal) error {
				if p.Kind == registry.PrincipalAgent {
					return errors.New("principal deleted")
				}
				return nil
			}
			c.OnAgentSessionAdmitted = func(id string, _ []string) {
				mu.Lock()
				seen = append(seen, id)
				mu.Unlock()
			}
		})

		// Broker session passes Revalidate -> must not invoke OnAgentSessionAdmitted.
		w.SetPrincipal("broker-tok", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"})
		n.MustDial("broker-tok", relaytest.BrokerHello("broker-1", "inc-1"), conduit.Config{})

		// Agent session fails Revalidate -> closed with 4401 and must not invoke OnAgentSessionAdmitted.
		w.SetPrincipal("agent-tok", agentPrincipal("L2", 3))
		sess, _, err := n.Dial(context.Background(), "agent-tok", &conduitv1.Hello{
			PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
			PrincipalId:   agentID,
			Capabilities: &conduitv1.Capabilities{
				EndpointIncarnation: "L2",
				Rpc:                 []string{"deliver-message"},
			},
		}, conduit.Config{})
		if err == nil && sess != nil {
			select {
			case <-sess.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("session rejected by Revalidate did not close")
			}
		}

		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 0 {
			t.Fatalf("unexpected OnAgentSessionAdmitted calls: %v", seen)
		}
	})
}
