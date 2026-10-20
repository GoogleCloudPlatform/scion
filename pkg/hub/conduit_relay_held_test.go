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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
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
// session, never calls a method on a nil session, and every caller answers
// it as an internal error: 500 internal_error for a tunnel and for the port
// proxy, and the internal error close (1011) for a PTY.
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
			require.True(t, errors.Is(err, errConduitLeafNotRelayHeld), "%v", err)
			assert.Nil(t, st)
			assert.Nil(t, b)

			// Tunnel on a hub-held session (openTarget's mapping).
			var te *conduitTunnelError
			require.ErrorAs(t, conduitTunnelOpenError(err), &te)
			resp := te.response()
			assert.Equal(t, int32(http.StatusInternalServerError), resp.GetStatus())
			assert.Contains(t, string(resp.GetBody()), ErrCodeInternalError)

			// Port proxy.
			rec := httptest.NewRecorder()
			writeConduitProxyError(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-1/ports/3000/proxy/", nil), "agent-1", err)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Contains(t, rec.Body.String(), ErrCodeInternalError)

			// PTY.
			var sc *StreamClosedError
			require.ErrorAs(t, ptyAgentOpenError(err), &sc)
			assert.Equal(t, wsprotocol.ClosePTYInternalError, sc.Code)
			assert.Equal(t, wsprotocol.CloseReasonInternalError, sc.Reason)
		})
	}
}

// TestMintRelayTunnel_NilRemoteSessionRefused: a tunnel request from a
// relay-held session whose resolution has no usable relay-held session gets
// 500 internal_error before any grant is minted, and no method is called on
// a nil session. hub.conduit is on, so without the check the typed-nil case
// would go on into the mint and the instruction instead of being refused.
func TestMintRelayTunnel_NilRemoteSessionRefused(t *testing.T) {
	srv := &Server{}
	setConduitExperiment(t, srv, true)
	require.True(t, srv.experimentEnabled(conduitExperiment))
	ts := newConduitTunnelSession(srv, NewAuthenticatedUser("user-1", "user@conduit.test", "User", store.UserRoleMember, "api"), time.Time{})
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}
	for name, sess := range notRelayHeldSessions() {
		t.Run(name, func(t *testing.T) {
			var (
				resp *conduitTunnelResponse
				open *relay.HubTunnelOpen
				err  error
			)
			require.NotPanics(t, func() {
				resp, open, err = ts.mintRelayTunnel(context.Background(), slog.Default(), "user-session", agent, "tcp", map[string]string{"port": "3000"}, router.Resolved{Session: sess}, false)
			})
			var te *conduitTunnelError
			require.ErrorAs(t, err, &te)
			assert.Equal(t, tunnelInternal, te.failure)
			answer := te.response()
			assert.Equal(t, int32(http.StatusInternalServerError), answer.GetStatus())
			assert.Contains(t, string(answer.GetBody()), ErrCodeInternalError)
			assert.Nil(t, resp)
			assert.Nil(t, open)
		})
	}
}
