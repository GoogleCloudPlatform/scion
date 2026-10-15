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

package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// TestRoutedBrokerRPC_StaleReasonClosedSet: on the routed broker path, an
// owner relay's stale route (404 or 409) is re-resolved only when its
// X-Conduit-Stale-Reason is in the owner's closed set. Without the header,
// or with any other value, it is handled under the operation class: a
// non-idempotent exec gets outcome unknown and is not sent to the broker's
// other session. The exec is used because it tells the two apart.
func TestRoutedBrokerRPC_StaleReasonClosedSet(t *testing.T) {
	t.Run("in set: re-resolved", func(t *testing.T) {
		for _, reason := range []string{
			"not_local", "relay_target_mismatch", "relay_not_serving", "not_sent",
			string(registry.ReasonDraining), string(registry.ReasonEpochObsolete),
			string(registry.ReasonIncarnationMismatch), string(registry.ReasonCapabilityMissing),
			string(registry.ReasonRelayStale), string(registry.ReasonSessionStale),
			string(registry.ReasonReadError), string(registry.ReasonRelayMissing), string(registry.ReasonRelaySuperseded),
			string(registry.ReasonNotFound), string(registry.ReasonNotRoutable), string(registry.ReasonProjectMismatch),
			string(registry.ReasonExecScopeMismatch),
		} {
			s1, s2 := &scriptedSession{err: &relay.StaleRouteError{Reason: reason}, relayID: "relay-a"}, answered()
			s2.relayID = "relay-b"
			resp, err := tunnelOnce(t, scriptedTunnel(s1, s2), conduit.RPCAgentExec, false)
			require.NoError(t, err, "reason %q", reason)
			assert.Equal(t, "served", string(resp.Body), "reason %q", reason)
			assert.Equal(t, 1, s2.calls, "reason %q: re-resolved to the other session", reason)
		}
	})
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"out of set: not a stale route", "relay_busy"},
		{"missing header: not a stale route", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s1, s2 := &scriptedSession{err: &relay.StaleRouteError{Reason: tc.reason}}, answered()
			_, err := tunnelOnce(t, scriptedTunnel(s1, s2), conduit.RPCAgentExec, false)
			var ou *brokerOutcomeUnknownError
			require.ErrorAs(t, err, &ou, "handled under the operation class: exec outcome unknown")
			assert.False(t, routedBrokerFallback(err), "no fallback")
			assert.Equal(t, 0, s2.calls, "not re-resolved to the other session")
		})
	}
}
