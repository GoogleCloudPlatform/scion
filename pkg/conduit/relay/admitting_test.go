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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

func relayRow(t *testing.T, w *relaytest.World, id string) registry.RelayInstance {
	t.Helper()
	relays, err := w.Store.ListRelays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ri := range relays {
		if ri.InstanceID == id {
			return ri
		}
	}
	t.Fatalf("no relay row %s", id)
	return registry.RelayInstance{}
}

// TestSetAdmitting (design §3.9): stopping admission refuses new sessions,
// marks the relay row draining and drains the open ones, while the relay
// keeps running; resuming admits again.
func TestSetAdmitting(t *testing.T) {
	p := newPair(t, echoConfig())
	r := p.a.Relay
	if !r.Admitting() {
		t.Fatal("a started relay admits")
	}

	r.SetAdmitting(false)
	if r.Admitting() {
		t.Fatal("admission still on")
	}
	relaytest.WaitClosed(t, p.target.GoAwayReceived(), "GoAway to the open session")
	if !relayRow(t, p.w, "relay-a").Draining {
		t.Fatal("relay row not marked draining")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := p.a.Dial(ctx, "a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoConfig()); err == nil {
		t.Fatal("a new session was admitted while admission is off")
	}
	select {
	case err := <-r.Fatal():
		t.Fatalf("the relay stopped: %v", err)
	default:
	}

	r.SetAdmitting(true)
	if !r.Admitting() {
		t.Fatal("admission not resumed")
	}
	if relayRow(t, p.w, "relay-a").Draining {
		t.Fatal("relay row still draining")
	}
	if _, _, err := p.a.Dial(ctx, "a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoConfig()); err != nil {
		t.Fatalf("a session after resuming: %v", err)
	}
}
