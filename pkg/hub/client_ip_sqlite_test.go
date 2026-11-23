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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnifiedAuthMiddleware_TrustedProxiesNeverAuthenticatesHeaders verifies
// that configuring ServerConfig.TrustedProxies never causes
// UnifiedAuthMiddleware to authenticate requests from X-Forwarded-User-*
// headers when no ProxyAuthenticator is configured.
func TestUnifiedAuthMiddleware_TrustedProxiesNeverAuthenticatesHeaders(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)

	cfg := DefaultServerConfig()
	cfg.TrustedProxies = []string{"192.0.2.0/24"}
	cfg.ProxyAuth = nil
	srv, err := newTestHubServer(t, cfg, s)
	require.NoError(t, err)

	reached := false
	handler := UnifiedAuthMiddleware(srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-User-Id", "user-1")
	req.Header.Set("X-Forwarded-User-Email", "admin@example.com")
	req.Header.Set("X-Forwarded-User-Name", "Admin")
	req.Header.Set("X-Forwarded-User-Role", "admin")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, reached, "downstream handler must not be reached")
}

// TestBrokerAuditMiddlewareAndEnvelope_ClientIPTrustedProxies verifies that
// AuditableBrokerAuthMiddleware and the broker audit envelope's source_ip
// field ignore forwarded headers from untrusted peers and honour
// X-Forwarded-For right to left from trusted peers.
func TestBrokerAuditMiddlewareAndEnvelope_ClientIPTrustedProxies(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)

	cfg := DefaultServerConfig()
	cfg.TrustedProxies = []string{"10.0.0.0/8"}
	srv, err := newTestHubServer(t, cfg, s)
	require.NoError(t, err)

	t.Run("untrusted peer records peer host and leaves source_ip_unparsed unset", func(t *testing.T) {
		logger, sink, _ := newBrokerEnvelopeTestLogger(t)
		mw := AuditableBrokerAuthMiddleware(srv.brokerAuthService, logger, srv.clientIP)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/heartbeat", nil)
		req = req.WithContext(brokerTestCtx(t, context.Background(), "req-audit-untrusted"))
		req.RemoteAddr = "198.51.100.7:54321"
		req.Header.Set(HeaderBrokerID, "00000000-0000-4000-8000-000000000001")
		req.Header.Set("X-Forwarded-For", "203.0.113.99")
		req.Header.Set("X-Real-IP", "203.0.113.100")

		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		recs, _ := brokerEnvelopeRecords(t, sink)
		require.Len(t, recs, 1)
		payload, ok := recs[0]["payload"].(map[string]any)
		require.True(t, ok, "expected payload map in envelope")
		assert.Equal(t, "198.51.100.7", payload["source_ip"])
		_, hasUnparsed := payload["source_ip_unparsed"]
		assert.False(t, hasUnparsed, "source_ip_unparsed must be unset for a valid peer address")
	})

	t.Run("trusted peer honours X-Forwarded-For right to left", func(t *testing.T) {
		logger, sink, _ := newBrokerEnvelopeTestLogger(t)
		mw := AuditableBrokerAuthMiddleware(srv.brokerAuthService, logger, srv.clientIP)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/heartbeat", nil)
		req = req.WithContext(brokerTestCtx(t, context.Background(), "req-audit-trusted"))
		req.RemoteAddr = "10.0.0.1:54321"
		req.Header.Set(HeaderBrokerID, "00000000-0000-4000-8000-000000000002")
		req.Header.Add("X-Forwarded-For", "198.51.100.1")
		req.Header.Add("X-Forwarded-For", "203.0.113.42, 10.0.0.2")

		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		recs, _ := brokerEnvelopeRecords(t, sink)
		require.Len(t, recs, 1)
		payload, ok := recs[0]["payload"].(map[string]any)
		require.True(t, ok, "expected payload map in envelope")
		assert.Equal(t, "203.0.113.42", payload["source_ip"])
		_, hasUnparsed := payload["source_ip_unparsed"]
		assert.False(t, hasUnparsed)
	})
}

// TestHandleBrokerJoin_AuditClientIPTrustedProxies verifies that handler audit
// events (LogJoinEvent) record the peer host for untrusted peers and the
// rightmost untrusted X-Forwarded-For hop for trusted peers.
func TestHandleBrokerJoin_AuditClientIPTrustedProxies(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)

	cfg := DefaultServerConfig()
	cfg.TrustedProxies = []string{"10.0.0.0/8"}
	cfg.TransportTrustedProxies = []string{"10.0.0.0/8"}
	cfg.TransportTrustedProxiesSet = true
	srv, err := newTestHubServer(t, cfg, s)
	require.NoError(t, err)

	t.Run("untrusted peer ignores XFF", func(t *testing.T) {
		logger := &mockAuditLogger{}
		srv.auditLogger = logger

		req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/join",
			strings.NewReader(`{"brokerId":"00000000-0000-4000-8000-000000000010","joinToken":"scion_join_test"}`))
		req.RemoteAddr = "198.51.100.9:4321"
		req.Header.Set("X-Forwarded-For", "203.0.113.88")
		req.Header.Set("X-Real-IP", "203.0.113.89")

		rec := httptest.NewRecorder()
		srv.handleBrokerJoin(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)

		require.Len(t, logger.brokerEvents, 1)
		assert.Equal(t, BrokerAuthEventJoin, logger.brokerEvents[0].EventType)
		assert.Equal(t, "198.51.100.9", logger.brokerEvents[0].IPAddress)
	})

	t.Run("trusted peer honours XFF right to left", func(t *testing.T) {
		logger := &mockAuditLogger{}
		srv.auditLogger = logger

		req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/join",
			strings.NewReader(`{"brokerId":"00000000-0000-4000-8000-000000000011","joinToken":"scion_join_test"}`))
		req.RemoteAddr = "10.0.0.5:4321"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Add("X-Forwarded-For", "198.51.100.1")
		req.Header.Add("X-Forwarded-For", "203.0.113.77, 10.0.0.2")

		rec := httptest.NewRecorder()
		srv.handleBrokerJoin(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		require.Len(t, logger.brokerEvents, 1)
		assert.Equal(t, BrokerAuthEventJoin, logger.brokerEvents[0].EventType)
		assert.Equal(t, "203.0.113.77", logger.brokerEvents[0].IPAddress)
	})
}

// TestConduitRelayAdmit_ForwardedHeadersStripXFF verifies that
// relay.ForwardCredentialHeaders never forwards X-Forwarded-For or X-Real-IP,
// so a relayed handshake's client address stays the sentinel peer address even
// if the sentinel's CIDR is listed in TrustedProxies.
func TestConduitRelayAdmit_ForwardedHeadersStripXFF(t *testing.T) {
	h := http.Header{}
	h.Set("X-Scion-Broker-Id", "broker-1")
	h.Set("X-Scion-Signed-Headers", "X-Forwarded-For;X-Real-IP;Forwarded;Content-Type")
	h.Set("X-Forwarded-For", "203.0.113.55")
	h.Set("X-Real-IP", "203.0.113.56")
	h.Set("Forwarded", "for=203.0.113.57")
	h.Set("Content-Type", "application/json")

	forwarded := relay.ForwardCredentialHeaders(h)
	assert.NotContains(t, forwarded, "X-Forwarded-For")
	assert.NotContains(t, forwarded, "X-Real-Ip")
	assert.NotContains(t, forwarded, "Forwarded")
	assert.Contains(t, forwarded, "Content-Type")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	req.RemoteAddr = conduitRelayAdmitRemoteAddr
	for k, v := range forwarded {
		req.Header[k] = v
	}
	trusted := parseTrustedProxies([]string{"192.0.2.0/24"})
	assert.Equal(t, "192.0.2.1", trustedClientIP(req, trusted))
}
