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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerFrom asks the hub for a broker registration (a join token) as
// user, from remote with headers; withTLS marks the request as arriving over
// TLS the hub terminated.
func registerFrom(t *testing.T, srv *Server, user *store.User, remote string, headers map[string]string, withTLS bool, name string) *httptest.ResponseRecorder {
	t.Helper()
	return registerFromHost(t, srv, user, remote, headers, "", withTLS, name)
}

// registerFromHost is registerFrom with the request's Host set to host (when
// not empty).
func registerFromHost(t *testing.T, srv *Server, user *store.User, remote string, headers map[string]string, host string, withTLS bool, name string) *httptest.ResponseRecorder {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	body, err := json.Marshal(CreateBrokerRegistrationRequest{
		Name:             name,
		PreserveSettings: true,
		Labels:           map[string]string{"scion.io/broker-role": "remote"},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if host != "" {
		req.Host = host
	}
	if withTLS {
		req.TLS = &tls.ConnectionState{}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// assertNoRegistrationIssued checks a transport refusal of a registration:
// 403 with the transport code, no join token in the body.
func assertNoRegistrationIssued(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeTransportNotConfidential, errorCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "joinToken")
}

// A registration requested by a network peer over plain http is refused:
// no join token in the response and no broker row created. A forwarded
// protocol from an unlisted peer does not count, nor does a loopback Host
// header or a loopback X-Forwarded-For (the peer address alone decides), and a
// loopback peer that relays (a forwarding header present) is judged as a
// proxy, of which none is listed.
func TestBrokerRegistration_PlaintextNetworkPeerGetsNoJoinToken(t *testing.T) {
	for name, tc := range map[string]struct {
		remote  string
		headers map[string]string
		host    string
	}{
		"plain http":                        {remote: tpNetworkPeer},
		"forged X-Forwarded-Proto":          {remote: tpNetworkPeer, headers: map[string]string{"X-Forwarded-Proto": "https"}},
		"forged Forwarded proto":            {remote: tpNetworkPeer, headers: map[string]string{"Forwarded": "proto=https"}},
		"loopback Host header":              {remote: tpNetworkPeer, host: "localhost"},
		"loopback X-Forwarded-For":          {remote: tpNetworkPeer, headers: map[string]string{"X-Forwarded-For": "127.0.0.1"}},
		"loopback peer is not a proxy here": {remote: tpLoopbackPeer, headers: map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https"}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			user := newHubMemberUser(t, s, "tp-reg-refused")
			assertNoRegistrationIssued(t, registerFromHost(t, srv, user, tc.remote, tc.headers, tc.host, false, "tp-reg-refused"))
			existing, err := srv.brokerAuthService.FindExistingBroker(context.Background(), "tp-reg-refused", "", nil)
			require.NoError(t, err)
			assert.Nil(t, existing, "no broker row is created for a refused registration")
		})
	}
}

// A re-registration over plain http neither re-mints nor revokes: the join
// token issued earlier over a confidential channel still works.
func TestBrokerRegistration_PlaintextReregistrationKeepsEarlierToken(t *testing.T) {
	srv, s := testServer(t)
	minted := mintForTransport(t, srv, s, "tp-reg-again")
	owner, err := s.GetUser(context.Background(), mustBrokerCreator(t, s, minted.BrokerID))
	require.NoError(t, err)

	assertNoRegistrationIssued(t, registerFrom(t, srv, owner, tpNetworkPeer, nil, false, "tp-reg-again"))

	rec := joinFrom(t, srv, tpLoopbackPeer, nil, minted.BrokerID, minted.JoinToken)
	require.Equal(t, http.StatusOK, rec.Code, "the earlier token still redeems; body: %s", rec.Body.String())
}

// Loopback, TLS the hub terminated, and a listed proxy reporting https all
// get a join token.
func TestBrokerRegistration_ConfidentialArrivalGetsJoinToken(t *testing.T) {
	for name, tc := range map[string]struct {
		remote  string
		headers map[string]string
		tls     bool
		trusted []string
	}{
		"loopback, no forwarding headers": {remote: tpLoopbackPeer},
		"TLS terminated by the hub":       {remote: tpNetworkPeer, tls: true},
		"trusted proxy with https":        {remote: tpProxyPeer, headers: map[string]string{"X-Forwarded-Proto": "https"}, trusted: []string{tpProxyCIDR}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.transportTrustedNets = mustTrustedNets(t, tc.trusted...)
			user := newHubMemberUser(t, s, "tp-reg-ok")
			rec := registerFrom(t, srv, user, tc.remote, tc.headers, tc.tls, "tp-reg-ok")
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			assert.NotEmpty(t, decodeRegistration(t, rec).JoinToken)
		})
	}
}

// mustBrokerCreator returns the user that created the broker.
func mustBrokerCreator(t *testing.T, s store.Store, brokerID string) string {
	t.Helper()
	b, err := s.GetRuntimeBroker(context.Background(), brokerID)
	require.NoError(t, err)
	require.NotEmpty(t, b.CreatedBy)
	return b.CreatedBy
}
