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

package router_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
)

// TestResolveExcludingRelay: ResolveExcluding rules out an excluded relay
// instance (every session it holds) as well as excluded sessions, as Do
// does for an unreachable owner within one resolution.
func TestResolveExcludingRelay(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: project, Agent: facts})
	_, wel := a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoTarget())
	rt, err := router.New(router.Config{Registry: w.Registry, Store: w.Store, Peers: &relay.PeerClient{Auth: w.PeerAuth("hub-1")}, Now: w.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := agentReq(router.OpStream)

	res, err := rt.ResolveExcluding(ctx, req, map[string]bool{"another-session": true}, map[string]bool{"relay-b": true})
	if err != nil || res.Record.SessionID != wel.GetSessionId() {
		t.Fatalf("ResolveExcluding(other session, other relay) = %+v, %v; want relay-a's session", res.Record, err)
	}
	if _, err := rt.ResolveExcluding(ctx, req, nil, map[string]bool{"relay-a": true}); !errors.Is(err, router.ErrNoSession) {
		t.Fatalf("ResolveExcluding(relay-a) = %v, want ErrNoSession", err)
	}
	if _, err := rt.ResolveExcluding(ctx, req, map[string]bool{wel.GetSessionId(): true}, nil); !errors.Is(err, router.ErrNoSession) {
		t.Fatalf("ResolveExcluding(the session) = %v, want ErrNoSession", err)
	}
}
