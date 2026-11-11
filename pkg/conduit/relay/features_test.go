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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// TestAdmitRecordsHelloFeatures: the optional features a target's Hello
// advertises are recorded in its registry row, read back from the store,
// and carried by a remote session handle's Info, so a hub that resolves the
// session through another relay sees them.
func TestAdmitRecordsHelloFeatures(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	b := w.StartNode("relay-b", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	hello := relaytest.AgentHello(agentID, "L1", "", "pty")
	features := []string{"stream_renewal", "other.v2"}
	hello.Capabilities.Features = features
	_, _ = a.MustDial("a", hello, echoConfig())

	rec := w.Sessions(registry.PrincipalAgent, agentID).Sessions[0].Session
	if !reflect.DeepEqual(rec.Capabilities.Features, features) {
		t.Fatalf("registry row features = %v, want %v", rec.Capabilities.Features, features)
	}
	rs := relay.NewRemoteSession(b.Peers, a.Internal.URL, rec, registry.Want{ProjectID: project, Incarnation: "L1"})
	if got := rs.Info().Capabilities.GetFeatures(); !reflect.DeepEqual(got, features) {
		t.Fatalf("remote session features = %v, want %v", got, features)
	}
}

// TestAdmitRecordsNoFeaturesForOlderTargets: a Hello without features
// records none.
func TestAdmitRecordsNoFeaturesForOlderTargets(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	_, _ = a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoConfig())
	rec := w.Sessions(registry.PrincipalAgent, agentID).Sessions[0].Session
	if len(rec.Capabilities.Features) != 0 || rec.Capabilities.HasFeature("stream_renewal") {
		t.Fatalf("registry row features = %v, want none", rec.Capabilities.Features)
	}
}
