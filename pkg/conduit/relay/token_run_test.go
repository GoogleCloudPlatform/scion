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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// TestAdmitTokenRunBinding: with an enforced TokenRunBinding, an agent
// Hello whose launch id differs from the credential's run is closed with
// 4401 unauthenticated before the incarnation check; without Enforce it is
// only reported. A Hello that matches the credential but not the agent row
// keeps the existing 4409.
func TestAdmitTokenRunBinding(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tokenRun     string
		enforce      bool
		helloLaunch  string
		wantCode     uint32 // 0: admitted
		wantToken    string
		wantMismatch int
	}{
		{name: "enforced, credential for another run", tokenRun: "L1", enforce: true, helloLaunch: "L2",
			wantCode: conduit.CloseUnauthenticated, wantToken: relay.ReasonUnauthenticated, wantMismatch: 1},
		{name: "enforced, credential without a run", tokenRun: "", enforce: true, helloLaunch: "L2",
			wantCode: conduit.CloseUnauthenticated, wantToken: relay.ReasonUnauthenticated, wantMismatch: 1},
		{name: "enforced, hello without a launch id", tokenRun: "L2", enforce: true, helloLaunch: "",
			wantCode: conduit.CloseUnauthenticated, wantToken: relay.ReasonUnauthenticated, wantMismatch: 1},
		{name: "enforced, matching", tokenRun: "L2", enforce: true, helloLaunch: "L2"},
		{name: "reported only", tokenRun: "L1", enforce: false, helloLaunch: "L2", wantMismatch: 1},
		{name: "matches the credential, not the row", tokenRun: "L1", enforce: true, helloLaunch: "L1",
			wantCode: relay.CloseSupersededIncarnation, wantToken: relay.ReasonSupersededIncarnation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			n := w.StartNode("relay-a", nil)
			mismatches := 0
			p := agentPrincipal("L2", 3)
			p.TokenRun = &relay.TokenRunBinding{RunID: tc.tokenRun, Enforce: tc.enforce, OnMismatch: func() { mismatches++ }}
			adm, _ := n.Relay.NewAdmitterForTest(p, registry.TransportWS)
			_, err := adm.Admit(context.Background(), relaytest.AgentHello(agentID, tc.helloLaunch, "", "pty"))
			if tc.wantCode == 0 {
				if err != nil {
					t.Fatalf("Admit = %v, want admitted", err)
				}
			} else {
				ce, ok := err.(*conduit.CloseError)
				if !ok || ce.Code != tc.wantCode || ce.Reason != tc.wantToken {
					t.Fatalf("Admit = %v, want %d %s", err, tc.wantCode, tc.wantToken)
				}
			}
			if mismatches != tc.wantMismatch {
				t.Fatalf("OnMismatch calls = %d, want %d", mismatches, tc.wantMismatch)
			}
		})
	}
}
