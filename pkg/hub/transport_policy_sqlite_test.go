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
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// joinFrom redeems a join token as a request from remote with headers.
func joinFrom(t *testing.T, srv *Server, remote string, headers map[string]string, brokerID, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(BrokerJoinRequest{BrokerID: brokerID, JoinToken: token, Hostname: "transport-host", Version: "test"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/join", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// mintForTransport mints a join token for a new broker named name.
func mintForTransport(t *testing.T, srv *Server, s store.Store, name string) CreateBrokerRegistrationResponse {
	t.Helper()
	minter := newHubMemberUser(t, s, name+"-minter")
	rec := mintJoinToken(t, srv, minter, name, 600)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	return decodeRegistration(t, rec)
}

func assertTransportRefusal(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeTransportNotConfidential, errorCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "secretKey")
	assert.Contains(t, rec.Body.String(), config.TrustedProxiesKey, "the refusal names the trusted proxy setting")
}

// A join from a network peer over plain http is refused, carries no
// secret, writes no broker secret, and revokes the exposed token: the same
// token then fails even from a loopback peer.
func TestBrokerJoin_PlaintextNetworkPeerRefusedAndTokenRevoked(t *testing.T) {
	srv, s := testServer(t)
	minted := mintForTransport(t, srv, s, "tp-join-network")

	rec := joinFrom(t, srv, tpNetworkPeer, nil, minted.BrokerID, minted.JoinToken)
	assertTransportRefusal(t, rec)
	_, err := s.GetBrokerSecret(context.Background(), minted.BrokerID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a refused join stores no secret")

	again := joinFrom(t, srv, tpLoopbackPeer, nil, minted.BrokerID, minted.JoinToken)
	assert.Equal(t, http.StatusUnauthorized, again.Code, "the exposed token was revoked; body: %s", again.Body.String())
	assert.NotContains(t, again.Body.String(), "secretKey")
}

// A refused join with a wrong token revokes nothing: the broker's real
// pending token still joins from a loopback peer.
func TestBrokerJoin_RefusedWrongTokenDoesNotRevoke(t *testing.T) {
	srv, s := testServer(t)
	minted := mintForTransport(t, srv, s, "tp-join-wrong")

	rec := joinFrom(t, srv, tpNetworkPeer, nil, minted.BrokerID, JoinTokenPrefix+"not-the-token")
	assertTransportRefusal(t, rec)

	ok := joinFrom(t, srv, tpLoopbackPeer, nil, minted.BrokerID, minted.JoinToken)
	require.Equal(t, http.StatusOK, ok.Code, "body: %s", ok.Body.String())
	var joined BrokerJoinResponse
	require.NoError(t, json.Unmarshal(ok.Body.Bytes(), &joined))
	assert.NotEmpty(t, joined.SecretKey)
}

// A forged X-Forwarded-Proto: https is refused when the trusted proxy list
// is empty, from a network peer and from a loopback peer alike, and when the
// peer is not in a non-empty list.
func TestBrokerJoin_ForgedForwardedProtoRefused(t *testing.T) {
	xfp := map[string]string{"X-Forwarded-Proto": "https"}
	for name, tc := range map[string]struct {
		remote  string
		trusted []string
	}{
		"network peer, empty list":         {tpNetworkPeer, nil},
		"loopback peer, empty list":        {tpLoopbackPeer, nil},
		"network peer outside a set list":  {tpNetworkPeer, []string{tpProxyCIDR}},
		"loopback peer outside a set list": {tpLoopbackPeer, []string{tpProxyCIDR}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.transportTrustedNets = mustTrustedNets(t, tc.trusted...)
			minted := mintForTransport(t, srv, s, "tp-join-forged")
			assertTransportRefusal(t, joinFrom(t, srv, tc.remote, xfp, minted.BrokerID, minted.JoinToken))
		})
	}
}

// A join from a loopback peer with no forwarding headers succeeds, and so
// does a join relayed by a listed proxy with X-Forwarded-Proto: https.
func TestBrokerJoin_LoopbackAndTrustedProxyAccepted(t *testing.T) {
	for name, tc := range map[string]struct {
		remote  string
		headers map[string]string
		trusted []string
	}{
		"loopback, no forwarding headers": {tpLoopbackPeer, nil, nil},
		"trusted proxy with https":        {tpProxyPeer, map[string]string{"X-Forwarded-Proto": "https"}, []string{tpProxyCIDR}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.transportTrustedNets = mustTrustedNets(t, tc.trusted...)
			minted := mintForTransport(t, srv, s, "tp-join-ok")
			rec := joinFrom(t, srv, tc.remote, tc.headers, minted.BrokerID, minted.JoinToken)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var joined BrokerJoinResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &joined))
			assert.NotEmpty(t, joined.SecretKey)
		})
	}
}

// A listed proxy that reports http is refused.
func TestBrokerJoin_TrustedProxyReportingHTTPRefused(t *testing.T) {
	srv, s := testServer(t)
	srv.transportTrustedNets = mustTrustedNets(t, tpProxyCIDR)
	minted := mintForTransport(t, srv, s, "tp-join-proxy-http")
	assertTransportRefusal(t, joinFrom(t, srv, tpProxyPeer, map[string]string{"X-Forwarded-Proto": "http"}, minted.BrokerID, minted.JoinToken))
}

// rotateSelfFrom asks the hub to rotate the broker's secret, signed with
// the broker's own secret, as a request from remote with headers.
func rotateSelfFrom(t *testing.T, srv *Server, brokerID string, key []byte, remote string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/"+brokerID+"/rotate-secret", nil)
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: key}).ApplyAuth(req))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// Secret rotation follows the same policy: refused (and nothing rotated)
// from a network peer over plain http or with a forged header; accepted
// from a loopback peer and from a listed proxy reporting https.
func TestBrokerRotateSecret_TransportPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		remote  string
		headers map[string]string
		trusted []string
		ok      bool
	}{
		"network peer, plain http":        {tpNetworkPeer, nil, nil, false},
		"forged XFP, empty list":          {tpNetworkPeer, map[string]string{"X-Forwarded-Proto": "https"}, nil, false},
		"forged XFP, peer outside list":   {tpNetworkPeer, map[string]string{"X-Forwarded-Proto": "https"}, []string{tpProxyCIDR}, false},
		"loopback, no forwarding headers": {tpLoopbackPeer, nil, nil, true},
		"trusted proxy with https":        {tpProxyPeer, map[string]string{"X-Forwarded-Proto": "https"}, []string{tpProxyCIDR}, true},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.transportTrustedNets = mustTrustedNets(t, tc.trusted...)
			ctx := context.Background()
			owner := newPlainUser(t, s, "tp-rotate-owner")
			broker := &store.RuntimeBroker{ID: tid("tp-rotate-broker"), Name: "TP Rotate Broker", Slug: "tp-rotate-broker", CreatedBy: owner.ID}
			require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
			key := seedBrokerSecret(t, s, broker.ID)

			rec := rotateSelfFrom(t, srv, broker.ID, key, tc.remote, tc.headers)
			stored, err := s.GetBrokerSecret(ctx, broker.ID)
			require.NoError(t, err)
			if !tc.ok {
				assertTransportRefusal(t, rec)
				assert.True(t, bytes.Equal(key, stored.SecretKey), "a refused rotation leaves the secret unchanged")
				return
			}
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var resp RotateSecretResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.NotEmpty(t, resp.SecretKey)
			assert.False(t, bytes.Equal(key, stored.SecretKey))
		})
	}
}

// upgradeRequestFrom is a WebSocket upgrade GET of path from remote.
func upgradeRequestFrom(path, remote string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// The control channel upgrade is refused before authentication from a
// network peer over plain http (and with a forged header); from a loopback
// peer the request passes the policy and reaches broker authentication.
func TestRuntimeBrokerConnect_TransportPolicy(t *testing.T) {
	srv, _ := testServer(t)
	for name, tc := range map[string]struct {
		remote  string
		headers map[string]string
	}{
		"network peer, plain http": {tpNetworkPeer, map[string]string{HeaderBrokerID: tid("tp-cc-broker")}},
		"forged XFP, empty list":   {tpNetworkPeer, map[string]string{HeaderBrokerID: tid("tp-cc-broker"), "X-Forwarded-Proto": "https"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleRuntimeBrokerConnect(rec, upgradeRequestFrom("/api/v1/runtime-brokers/connect", tc.remote, tc.headers))
			assertTransportRefusal(t, rec)
		})
	}
	t.Run("loopback reaches authentication", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleRuntimeBrokerConnect(rec, upgradeRequestFrom("/api/v1/runtime-brokers/connect", tpLoopbackPeer,
			map[string]string{HeaderBrokerID: tid("tp-cc-broker")}))
		assert.NotEqual(t, http.StatusSwitchingProtocols, rec.Code, "an unsigned upgrade is not admitted")
		if rec.Code == http.StatusForbidden {
			assert.NotEqual(t, ErrCodeTransportNotConfidential, errorCode(t, rec), "the transport policy accepts a loopback peer")
		}
	})
}

// The broker arm of the conduit endpoint is refused from a network peer
// over plain http. A relay admission (forRelay), which the hub builds
// itself with the synthetic peer conduitRelayAdmitRemoteAddr, is not judged
// by the transport policy: it goes on to broker authentication.
func TestConduitBroker_TransportPolicyAndRelayAdmit(t *testing.T) {
	srv, _ := testServer(t)
	headers := map[string]string{HeaderBrokerID: tid("tp-conduit-broker")}

	rec := httptest.NewRecorder()
	_, ok := srv.decideConduitBroker(rec, upgradeRequestFrom("/api/v1/conduit", tpNetworkPeer, headers), false)
	assert.False(t, ok)
	assertTransportRefusal(t, rec)

	relay := httptest.NewRecorder()
	_, ok = srv.decideConduitBroker(relay, upgradeRequestFrom("/api/v1/conduit", conduitRelayAdmitRemoteAddr, headers), true)
	assert.False(t, ok, "an unsigned admission is still refused, by authentication")
	if relay.Code == http.StatusForbidden {
		assert.NotEqual(t, ErrCodeTransportNotConfidential, errorCode(t, relay), "a relay admission is not judged by the transport policy")
	}
	assert.NotContains(t, relay.Body.String(), ErrCodeTransportNotConfidential)
}

// server.trusted_proxies is never API-writable and never shown: a settings
// write that names it is refused in file mode, and GET does not show it.
func TestNeverAPIWritable_TrustedProxies(t *testing.T) {
	for name, body := range map[string]string{
		"nested":      `{"server":{"trusted_proxies":["10.0.0.0/8"]}}`,
		"flat dotted": `{"server.trusted_proxies":["10.0.0.0/8"]}`,
	} {
		t.Run("file mode "+name, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			assert.Equal(t, []string{config.TrustedProxiesKey}, tiaErrorKeys(t, rr))
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
	}

	path := tempSettingsHome(t)
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  log_level: warn\n  trusted_proxies:\n    - 10.0.0.0/8\n"), 0o600))
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.Contains(t, getRR.Body.String(), `"log_level":"warn"`, "control: the settings file was read")
	assert.NotContains(t, getRR.Body.String(), "trusted_proxies")

	db, _, ops := newSQLiteHubInMode(t, false, nil)
	rr := httptest.NewRecorder()
	db.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "trusted_proxies")
}

// The deprecated embedded-broker branch of project register returns the
// broker's HMAC secret (an existing secret unchanged). From a network peer
// over plain http the registration still succeeds but carries no secret and
// changes no stored secret; from a loopback peer the secret is returned.
func TestProjectRegisterEmbeddedBroker_SecretOnlyOnConfidentialArrival(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "tp-register-owner")
	broker := registerBrokerTestExistingBroker(t, s, "tp-register-broker", owner.ID)
	body := RegisterProjectRequest{
		Name:   "tp-register-project",
		Broker: &RegisterProjectBrokerInfo{ID: broker.ID, Name: broker.Name, Version: "2.0.0"},
	}

	// The broker has no stored secret yet. doRequestAsUser sends from
	// httptest's default network peer: no secret is returned, and none is
	// minted, so the first secret is left for a confidential arrival.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/register", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp RegisterProjectResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.SecretKey, "no broker secret over a non-confidential arrival")
	_, err := s.GetBrokerSecret(ctx, broker.ID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no secret is minted over a non-confidential arrival: %v", err)

	// A loopback (co-located) registration mints the broker's first secret
	// and returns it.
	rec = doRequestAsUserLoopback(t, srv, owner, http.MethodPost, "/api/v1/projects/register", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	resp = RegisterProjectResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.SecretKey, "a loopback (co-located) first registration gets the minted secret")
	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, resp.SecretKey, base64.StdEncoding.EncodeToString(stored.SecretKey))

	// A broker that already has a secret gets none back, even over a
	// confidential arrival: the stored secret is never returned here.
	rec = doRequestAsUserLoopback(t, srv, owner, http.MethodPost, "/api/v1/projects/register", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	resp = RegisterProjectResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.SecretKey, "an existing broker secret is not returned")
	assert.NotContains(t, rec.Body.String(), base64.StdEncoding.EncodeToString(stored.SecretKey))
}

// server.trusted_proxies is refused by the DB-backed settings write path too,
// on a hosted (DB) hub and on a workstation hub: no DB row is written and
// settings.yaml does not gain the key.
func TestNeverAPIWritable_TrustedProxiesRefusedInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range map[string]string{
			"nested":      `{"server":{"trusted_proxies":["10.0.0.0/8"]}}`,
			"flat dotted": `{"server.trusted_proxies":["10.0.0.0/8"]}`,
		} {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				assert.Equal(t, []string{config.TrustedProxiesKey}, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "trusted_proxies")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
}
