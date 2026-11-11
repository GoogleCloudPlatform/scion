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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// TestRoutedBrokerRPC_StaleReasonScope: a relay-wide owner stale reason
// excludes the whole owner relay, so another session on that relay is not
// tried and a session on another relay is; a session-only reason excludes
// just the session, so the replacement on the same relay is used. A budget
// exhausted on relay exclusions sent nothing, so it takes the fallback.
func TestRoutedBrokerRPC_StaleReasonScope(t *testing.T) {
	sessions := func(reason string) (s1, s2, s3 *scriptedSession) {
		s1 = &scriptedSession{err: &relay.StaleRouteError{Reason: reason}, relayID: "relay-a"}
		s2 = answered()
		s2.relayID = "relay-a" // the replacement on the same relay
		s3 = answered()
		s3.relayID = "relay-b"
		return s1, s2, s3
	}
	for _, reason := range []string{string(registry.ReasonRelayStale), string(registry.ReasonReadError)} {
		t.Run("relay-wide "+reason, func(t *testing.T) {
			s1, s2, s3 := sessions(reason)
			resp, err := tunnelOnce(t, scriptedTunnel(s1, s2, s3), conduit.RPCAgentExec, false)
			require.NoError(t, err)
			assert.Equal(t, "served", string(resp.Body))
			assert.Equal(t, 0, s2.calls, "the other session on the excluded relay is not tried")
			assert.Equal(t, 1, s3.calls, "a session on another relay serves the request")
		})
	}
	t.Run("session-only epoch_obsolete", func(t *testing.T) {
		s1, s2, s3 := sessions(string(registry.ReasonEpochObsolete))
		resp, err := tunnelOnce(t, scriptedTunnel(s1, s2, s3), conduit.RPCAgentExec, false)
		require.NoError(t, err)
		assert.Equal(t, "served", string(resp.Body))
		assert.Equal(t, 1, s2.calls, "the replacement on the same relay is used")
		assert.Equal(t, 0, s3.calls)
	})
	t.Run("relay-wide budget exhausted falls back", func(t *testing.T) {
		var all []*scriptedSession
		for _, id := range []string{"relay-a", "relay-b", "relay-c", "relay-d"} {
			all = append(all, &scriptedSession{err: &relay.StaleRouteError{Reason: string(registry.ReasonRelayStale)}, relayID: id})
		}
		_, err := tunnelOnce(t, scriptedTunnel(all...), conduit.RPCAgentExec, false)
		require.ErrorIs(t, err, errBrokerNotRouted)
		assert.True(t, routedBrokerFallback(err), "nothing was sent: the fallback runs")
		var ou *brokerOutcomeUnknownError
		assert.False(t, errors.As(err, &ou), "never outcome unknown")
		assert.Equal(t, 0, all[3].calls, "within the shared budget")
	})
}

// TestRoutedBrokerRPC_StaleReasonScopeTable: every owner stale reason of
// the closed set, written as a literal, with its exclusion scope. A
// relay-wide reason skips the replacement session on the same relay and
// is served on another relay; a session-only reason is served by the
// replacement on the same relay. Moving any value between the scopes, or
// out of the set, fails this test.
func TestRoutedBrokerRPC_StaleReasonScopeTable(t *testing.T) {
	const relayWide, sessionOnly = "relay-wide", "session-only"
	for _, tc := range []struct{ reason, scope string }{
		{"relay_missing", relayWide},
		{"relay_superseded", relayWide},
		{"relay_stale", relayWide},
		{"relay_target_mismatch", relayWide},
		{"relay_not_serving", relayWide},
		{"read_error", relayWide},
		{"not_local", sessionOnly},
		{"not_found", sessionOnly},
		{"session_stale", sessionOnly},
		{"epoch_obsolete", sessionOnly},
		{"incarnation_mismatch", sessionOnly},
		{"draining", sessionOnly},
		{"not_sent", sessionOnly},
		{"project_mismatch", sessionOnly},
		{"exec_scope_mismatch", sessionOnly},
		{"capability_missing", sessionOnly},
		{"not_routable", sessionOnly},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			stale := &scriptedSession{err: &relay.StaleRouteError{Reason: tc.reason}, relayID: "relay-a"}
			sameRelay := answered()
			sameRelay.relayID = "relay-a"
			otherRelay := answered()
			otherRelay.relayID = "relay-b"
			resp, err := tunnelOnce(t, scriptedTunnel(stale, sameRelay, otherRelay), conduit.RPCAgentExec, false)
			require.NoError(t, err)
			assert.Equal(t, "served", string(resp.Body))
			if tc.scope == relayWide {
				assert.Equal(t, 0, sameRelay.calls, "a relay-wide reason skips the same relay")
				assert.Equal(t, 1, otherRelay.calls)
			} else {
				assert.Equal(t, 1, sameRelay.calls, "a session-only reason uses the replacement on the same relay")
				assert.Equal(t, 0, otherRelay.calls)
			}
		})
	}
}
