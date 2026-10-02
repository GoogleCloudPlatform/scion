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

package hub

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/stretchr/testify/require"
)

// TestAgentKeysMessageBridge_DispatchOutcomes covers plan row A.4's
// "AK-26/27/29/30/31 (B)": every broker dispatch-layer outcome
// TestExecuteAgentKeys_DispatchOutcomes already pins for the direct /keys
// routes must classify identically via the bridge, reusing the same fake
// dispatcher and error values.
func TestAgentKeysMessageBridge_DispatchOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		dispatchErr error
		wantStatus  int
		wantCode    string
	}{
		{
			name:        "AK-27/45/46: broker reports target not found",
			dispatchErr: &agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeNotFound},
			wantStatus:  http.StatusNotFound, wantCode: "not_found",
		},
		{
			name:        "AK-27: terminal not ready",
			dispatchErr: &agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeTerminalNotReady},
			wantStatus:  http.StatusConflict, wantCode: "terminal_not_ready",
		},
		{
			name:        "AK-28/47: broker reports keys unsupported",
			dispatchErr: &agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeKeysUnsupported},
			wantStatus:  http.StatusUnprocessableEntity, wantCode: "keys_unsupported",
		},
		{
			name:        "AK-29: not dispatched",
			dispatchErr: agentkeys.ErrNotDispatched,
			wantStatus:  http.StatusServiceUnavailable, wantCode: "keys_unavailable",
		},
		{
			name:        "AK-30: ambiguous/unclassified error never claims delivery",
			dispatchErr: errors.New("keys: simulated ambiguous transport failure"),
			wantStatus:  http.StatusBadGateway, wantCode: "keys_outcome_unknown",
		},
	}

	for _, shape := range messageRouteShapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				d.err = tc.dispatchErr
				token := f.agentToken(t, tid("bridge-outcome-"+shape.name+"-"+tc.wantCode), f.projectA.ID, ScopeAgentLifecycle)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), rawTopLevelBody("C-c"), token)
				assertKeysDenialOutcome(t, shape.name+"/"+tc.name, rec, tc.wantStatus, tc.wantCode)

				if got := d.callCount(); got != 1 {
					t.Errorf("%s: dispatcher called %d times, want exactly 1 (no automatic replay)", tc.name, got)
				}
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	}
}

// TestAgentKeysMessageBridge_ManagedRuntimeUnsupported covers plan row
// AK-28(B): a managed-runtime target is refused via the bridge the same way
// it is on the direct /keys routes (TestExecuteAgentKeys_ManagedRuntimeUnsupported),
// never downgraded to an ordinary message send. The existing
// TestAgentKeysMessageBridge_RunningPhaseAndManagedRuntimeDelegated only
// exercises the not-running half despite its name; this fills the
// managed-runtime half.
func TestAgentKeysMessageBridge_ManagedRuntimeUnsupported(t *testing.T) {
	for _, shape := range messageRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("bridge-managed-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			f.agentInA.Runtime = ManagedRuntimePrefix + "google"
			require.NoError(t, f.store.UpdateAgent(context.Background(), f.agentInA))

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), rawTopLevelBody("C-c"), token)
			assertKeysDenialOutcome(t, shape.name, rec, http.StatusUnprocessableEntity, "keys_unsupported")
			require.Equal(t, 0, d.callCount())
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}
