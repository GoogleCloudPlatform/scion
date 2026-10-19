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
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
)

// relayTunnelResolution is a resolution to a session relay-0 holds for the
// fixture's agent, advertising features.
func relayTunnelResolution(cf *conduitFixture, features ...string) router.Resolved {
	rec := registry.SessionRecord{
		SessionID: "agent-session", PrincipalKind: registry.PrincipalAgent, PrincipalID: cf.agent.ID,
		RelayInstanceID: "relay-0", RelayGeneration: 1, ConnectionEpoch: 1,
		Capabilities: registry.Capabilities{StreamKinds: []string{"pty", "tcp"}, Features: features},
	}
	want := registry.Want{ProjectID: cf.agent.ProjectID, Incarnation: "L1"}
	return router.Resolved{Session: relay.NewRemoteSession(&relay.PeerClient{}, "http://relay-0.invalid", rec, want), Record: rec, Want: want}
}

// grantDeadlineClaims reads the signed authz_until and authz_interval of a
// grant (compact JWS) without verifying it.
func grantDeadlineClaims(t *testing.T, tok []byte) (until, intervalS int64) {
	t.Helper()
	parts := strings.Split(string(tok), ".")
	require.Len(t, parts, 3)
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var c struct {
		AuthzUntil    int64 `json:"authz_until"`
		AuthzInterval int64 `json:"authz_interval"`
	}
	require.NoError(t, json.Unmarshal(body, &c))
	return c.AuthzUntil, c.AuthzInterval
}

// TestRelayTunnel_HintsCopiedFromMintedClaims (design §3.9 "Scheduling
// hints"): the tunnel open's authz_until and authz_interval_s equal the
// deadline claims of the open grant the hub just minted for a
// renewal-capable target.
func TestRelayTunnel_HintsCopiedFromMintedClaims(t *testing.T) {
	cf := newConduitFixture(t)
	ts := newConduitTunnelSession(cf.srv, cf.owner, time.Time{})
	_, open, err := ts.mintRelayTunnel(context.Background(), slog.Default(), "user-session", cf.agent, "pty",
		map[string]string{"cols": "80", "rows": "24", "session": conduitPTYSession}, relayTunnelResolution(cf, conduit.FeatureStreamRenewal))
	require.NoError(t, err)
	require.NotNil(t, open)
	until, intervalS := grantDeadlineClaims(t, open.Grant)
	require.NotZero(t, until, "the open grant to a renewal-capable target carries a deadline")
	assert.Equal(t, until, open.AuthzUntil)
	assert.Equal(t, intervalS, open.AuthzIntervalS)
	assert.Equal(t, int64(cf.srv.conduitUserStreamAuthzInterval().Truncate(time.Second)/time.Second), open.AuthzIntervalS)
}

// TestRelayTunnel_NoHintsWithoutDeadline: for a target without renewal the
// open grant carries no deadline, and the tunnel open carries neither hint.
func TestRelayTunnel_NoHintsWithoutDeadline(t *testing.T) {
	cf := newConduitFixture(t)
	ts := newConduitTunnelSession(cf.srv, cf.owner, time.Time{})
	_, open, err := ts.mintRelayTunnel(context.Background(), slog.Default(), "user-session", cf.agent, "pty",
		map[string]string{"cols": "80", "rows": "24", "session": conduitPTYSession}, relayTunnelResolution(cf))
	require.NoError(t, err)
	require.NotNil(t, open)
	until, intervalS := grantDeadlineClaims(t, open.Grant)
	assert.Zero(t, until)
	assert.Zero(t, intervalS)
	assert.Zero(t, open.AuthzUntil)
	assert.Zero(t, open.AuthzIntervalS)
}

// TestRelayTunnel_HintsNeverFromTheRequest: no request field can carry a
// hint. The forwarded request type has no hint field, so hint-like keys a
// relay sends are dropped when the hub decodes it, and the mint step takes
// the resolution and the minted claims only.
func TestRelayTunnel_HintsNeverFromTheRequest(t *testing.T) {
	rt := reflect.TypeOf(relay.HubRPCRequest{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		assert.NotContains(t, strings.ToLower(f.Name), "authz", "HubRPCRequest field %s", f.Name)
		assert.NotContains(t, strings.ToLower(f.Tag.Get("json")), "authz", "HubRPCRequest field %s", f.Name)
	}
	var req relay.HubRPCRequest
	require.NoError(t, json.Unmarshal([]byte(`{"admission_token":"t","rpc":"","authz_until":4102444800,"authz_interval_s":60}`), &req))
	out, err := json.Marshal(req)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "authz")
}
