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
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// notRelayHeldSessions are resolutions external relay mode must refuse:
// no session, and a nil *relay.RemoteSession inside the interface (which
// passes a plain type assertion).
func notRelayHeldSessions() map[string]conduit.Session {
	var typedNil *relay.RemoteSession
	return map[string]conduit.Session{
		"no session":       nil,
		"typed-nil remote": typedNil,
	}
}

// TestOpenConduitTargetStream_NilRemoteSessionRefused: a hub-held leaf in
// external relay mode refuses a resolution without a usable relay-held
// session as not relay-held (agent session unavailable to its caller), and
// never calls a method on a nil session.
func TestOpenConduitTargetStream_NilRemoteSessionRefused(t *testing.T) {
	s := &Server{}
	s.conduitExternal.Store(&conduitExternalRuntime{})
	open := &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY}
	for name, sess := range notRelayHeldSessions() {
		t.Run(name, func(t *testing.T) {
			var (
				st  conduit.Stream
				b   relay.Bridge
				err error
			)
			require.NotPanics(t, func() {
				st, b, err = s.openConduitTargetStream(context.Background(), router.Resolved{Session: sess}, open)
			})
			assert.True(t, errors.Is(err, errConduitLeafNotRelayHeld), "%v", err)
			assert.Nil(t, st)
			assert.Nil(t, b)
		})
	}
}

// TestMintRelayTunnel_NilRemoteSessionRefused: a tunnel request from a
// relay-held session whose resolution has no usable relay-held session is
// refused as agent session unavailable (503), before any grant is minted,
// and never calls a method on a nil session.
func TestMintRelayTunnel_NilRemoteSessionRefused(t *testing.T) {
	ts := newConduitTunnelSession(&Server{}, NewAuthenticatedUser("user-1", "user@conduit.test", "User", store.UserRoleMember, "api"), time.Time{})
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}
	for name, sess := range notRelayHeldSessions() {
		t.Run(name, func(t *testing.T) {
			var (
				resp *conduitTunnelResponse
				open *relay.HubTunnelOpen
				err  error
			)
			require.NotPanics(t, func() {
				resp, open, err = ts.mintRelayTunnel(context.Background(), slog.Default(), "user-session", agent, "tcp", map[string]string{"port": "3000"}, router.Resolved{Session: sess})
			})
			var te *conduitTunnelError
			require.ErrorAs(t, err, &te)
			assert.Equal(t, tunnelSessionUnavailable, te.failure)
			assert.Nil(t, resp)
			assert.Nil(t, open)
		})
	}
}
