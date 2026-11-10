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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// newNoPeerAuthRelayFixture is a relay fixture in the state a node with no
// internal listener starts in: relay.NoPeerAuth and no shared signing
// secret.
func newNoPeerAuthRelayFixture(t *testing.T) *relayFixture {
	t.Helper()
	f := newRelayFixture(t, func(o *ConduitRelayOptions) { o.PeerAuth = relay.NoPeerAuth{} })
	require.Empty(t, f.srv.config.SharedSigningSecret, "no shared signing secret")
	return f
}

// TestSkipPath_AdmissionNeverMinted: without the shared signing secret no
// admission token is minted or re-minted; the refusal names the secret's
// setting and carries no value.
func TestSkipPath_AdmissionNeverMinted(t *testing.T) {
	f := newNoPeerAuthRelayFixture(t)
	tok, err := f.srv.mintAdmissionToken(conduitAdmissionClaims{SessionID: "s1"})
	require.ErrorIs(t, err, errConduitAdmissionNoKey)
	assert.Empty(t, tok)
	tok, err = f.srv.reissueAdmissionToken(conduitAdmissionClaims{SessionID: "s1"}, time.Time{}, time.Now())
	require.ErrorIs(t, err, errConduitAdmissionNoKey)
	assert.Empty(t, tok)
	assert.Contains(t, err.Error(), "--session-secret")
	assert.Contains(t, err.Error(), "SCION_SERVER_SESSION_SECRET")
}

// TestSkipPath_HubAdmitUnreachable: on such a node a request for the hub
// admit operation reaches the relay's own internal API, which refuses it
// (401), and the hub admission code never runs. The handler itself is not
// mounted at all without an internal listener (cmd:
// TestStartConduitRelay_NoInternalListenSkipsPeerAuth), and never on the
// public mux (TestConduitInternalHandler_PeerIdentity).
func TestSkipPath_HubAdmitUnreachable(t *testing.T) {
	f := newNoPeerAuthRelayFixture(t)
	admitted := false
	prev := conduitRelayAdmitObserve
	conduitRelayAdmitObserve = func(*http.Request) { admitted = true }
	t.Cleanup(func() { conduitRelayAdmitObserve = prev })

	rec := httptest.NewRecorder()
	f.srv.ConduitInternalHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, relay.HubAdmitPath, strings.NewReader("{}")))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, admitted, "the hub admit operation did not run")
}

// TestSkipPath_DialerAdmittedWithoutSecret: the relay is useful in that
// state: an agent conduit handshake is admitted and served, with grant keys
// from this node's own ring.
func TestSkipPath_DialerAdmittedWithoutSecret(t *testing.T) {
	f := newNoPeerAuthRelayFixture(t)
	tok := f.agentToken(t, f.launched)
	_, wel, err := f.dial(t, tok, f.launched.RunID)
	require.NoError(t, err)
	assert.Equal(t, "hub-a", wel.GetRelayInstanceId())
	assert.NotEmpty(t, wel.GetGrantKeys(), "Welcome carries the grant keys")
}

// TestConduitRelayNotRunning503NamesTheKey: with no relay running a conduit
// handshake gets a 503 that names the setting startup failed on, says
// startup failed when no one setting caused it, and otherwise keeps the
// restart text for hub.conduit turned on after startup.
func TestConduitRelayNotRunning503NamesTheKey(t *testing.T) {
	srv, _ := testServer(t)
	user := NewAuthenticatedUser("u1", "user@example.com", "User", "member", "web")
	decide := func() *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		_, ok := srv.decideConduitUser(rec, httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil), user, false)
		require.False(t, ok)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		return rec
	}

	assert.Contains(t, decide().Body.String(), "hub.conduit was enabled after startup; restart required")

	srv.SetConduitRelayStartFailure("server.hub.conduit.peer_service_accounts")
	body := decide().Body.String()
	assert.Contains(t, body, "startup failed on server.hub.conduit.peer_service_accounts")
	assert.NotContains(t, body, "restart required")

	srv.SetConduitRelayStartFailure("")
	body = decide().Body.String()
	assert.Contains(t, body, "startup failed; see the startup log")
	assert.NotContains(t, body, "server.hub.conduit")
}

// TestConduitRelayNotRunning503_AgentAndBrokerPaths: the agent and broker
// handshakes answer a node with no relay running with the same 503, naming
// the recorded setting.
func TestConduitRelayNotRunning503_AgentAndBrokerPaths(t *testing.T) {
	const key = "server.hub.conduit.peer_service_accounts"
	t.Run("agent", func(t *testing.T) {
		srv, _ := testServer(t)
		srv.SetConduitRelayStartFailure(key)
		ident := newTestAgentIdentity("agent-1", "project-1", []AgentTokenScope{ScopeAgentPortForward})
		rec := httptest.NewRecorder()
		_, ok := srv.decideConduitAgent(rec, httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil), ident, false)
		require.False(t, ok)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Contains(t, rec.Body.String(), "startup failed on "+key)
	})
	t.Run("broker", func(t *testing.T) {
		f := newBrokerConduitFixture(t, nil, nil)
		// No relay running on this node for the handshake; restored before
		// the fixture's cleanup stops the relay.
		prev := f.srv.conduit.Swap(nil)
		t.Cleanup(func() { f.srv.conduit.Store(prev) })
		f.srv.SetConduitRelayStartFailure(key)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
		req.Header = f.signedHeader(t, "/api/v1/conduit", nil)
		req.RemoteAddr = tpLoopbackPeer // the transport policy admits a client on the hub host
		rec := httptest.NewRecorder()
		_, ok := f.srv.decideConduitBroker(rec, req, false)
		require.False(t, ok)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Contains(t, rec.Body.String(), "startup failed on "+key)
	})
}
