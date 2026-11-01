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

package runtimebroker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// newHubTransportRefusal is a hub's transport refusal with details.
	newHubTransportRefusal = `{"error":{"code":"transport_not_confidential","message":"The hub received this request from a proxy on its own host.",` +
		`"details":{"reason":"untrusted_local_proxy","setting":"server.trusted_proxies"}}}`
	// oldHubTransportRefusal is the same refusal from a hub without details.
	oldHubTransportRefusal = `{"error":{"code":"transport_not_confidential","message":"... list the proxy's addresses in server.trusted_proxies ..."}}`
)

func TestParseHubRefusal(t *testing.T) {
	t.Run("transport refusal with details", func(t *testing.T) {
		e := parseHubRefusal(http.StatusForbidden, []byte(newHubTransportRefusal))
		require.NotNil(t, e)
		assert.True(t, e.transportPolicy())
		assert.Equal(t, http.StatusForbidden, e.Status)
		assert.Equal(t, "untrusted_local_proxy", e.Reason)
		assert.Equal(t, config.TrustedProxiesKey, e.Setting)
		msg := e.logMessage("control channel")
		assert.Contains(t, msg, "reason untrusted_local_proxy")
		assert.Contains(t, msg, "set server.trusted_proxies on the hub")
	})
	t.Run("unknown reason and setting stay out of the message text", func(t *testing.T) {
		body := `{"error":{"code":"transport_not_confidential","message":"refused",` +
			`"details":{"reason":"injected\nFAKE LOG LINE","setting":"other.setting"}}}`
		e := parseHubRefusal(http.StatusForbidden, []byte(body))
		require.NotNil(t, e)
		assert.Equal(t, "injected\nFAKE LOG LINE", e.Reason, "the raw reason is kept for the log attributes")
		assert.Equal(t, "other.setting", e.Setting, "the raw setting is kept for the log attributes")
		msg := e.logMessage("conduit")
		assert.Contains(t, msg, "reason "+hubRefusalReasonUnspecified)
		assert.Contains(t, msg, "set "+config.TrustedProxiesKey+" on the hub")
		assert.NotContains(t, msg, "FAKE LOG LINE")
		assert.NotContains(t, msg, "other.setting")
	})
	t.Run("every known reason is named in the message", func(t *testing.T) {
		for _, reason := range []string{config.ArrivalRefusalUntrustedLocalProxy, config.ArrivalRefusalLocalProxyNotTrusted,
			config.ArrivalRefusalProxyProtoNotHTTPS, config.ArrivalRefusalNotTLS} {
			e := &hubRefusalError{Code: hubTransportRefusalCode, Reason: reason, Setting: config.TrustedProxiesKey}
			assert.Contains(t, e.logMessage("control channel"), "reason "+reason+")")
		}
	})
	t.Run("transport refusal from an older hub", func(t *testing.T) {
		e := parseHubRefusal(http.StatusForbidden, []byte(oldHubTransportRefusal))
		require.NotNil(t, e)
		assert.True(t, e.transportPolicy())
		assert.Equal(t, hubRefusalReasonUnspecified, e.Reason)
		assert.Equal(t, config.TrustedProxiesKey, e.Setting, "the setting is named even without details")
	})
	t.Run("another error code", func(t *testing.T) {
		e := parseHubRefusal(http.StatusUnauthorized, []byte(`{"error":{"code":"broker_auth_failed","message":"bad signature"}}`))
		require.NotNil(t, e)
		assert.False(t, e.transportPolicy())
		assert.Empty(t, e.Reason)
		assert.Empty(t, e.Setting)
	})
	t.Run("not JSON", func(t *testing.T) {
		assert.Nil(t, parseHubRefusal(http.StatusForbidden, []byte("forbidden\n")))
		assert.Nil(t, parseHubRefusal(http.StatusForbidden, nil))
		assert.Nil(t, parseHubRefusal(http.StatusForbidden, []byte(`{"error":{}}`)))
	})
	t.Run("oversized body", func(t *testing.T) {
		body := `{"error":{"code":"transport_not_confidential","message":"` + strings.Repeat("x", 2*hubRefusalBodyLimit) + `"}}`
		assert.Nil(t, parseHubRefusal(http.StatusForbidden, []byte(body)), "a body cut at the limit is not parsed")
	})
	t.Run("nil is not a transport refusal", func(t *testing.T) {
		var e *hubRefusalError
		assert.False(t, e.transportPolicy())
	})
}

// refusingHub answers every request with status and body.
func refusingHub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runRefusedControlChannel connects a control channel to a hub answering
// status and body, waits for the first Error record and closes it.
func runRefusedControlChannel(t *testing.T, status int, body string) (*levelRecorder, *ControlChannelClient) {
	t.Helper()
	hub := refusingHub(t, status, body)
	rec := newLevelRecorder()
	c := newLoggedCloseTestClient(hub.URL, rec)
	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()
	waitChan(t, (<-chan struct{})(rec.firstError), "the Error record of the refused connect")
	refused := c.TransportRefused()
	_ = c.Close()
	if err := waitChan(t, done, "the connect loop to end after Close"); !errors.Is(err, context.Canceled) {
		t.Errorf("Connect after Close = %v, want context.Canceled", err)
	}
	if refused != c.TransportRefused() {
		t.Errorf("Close changed TransportRefused")
	}
	return rec, c
}

// The control channel reads the hub's transport refusal and logs the
// reason and the hub setting instead of a bare "bad handshake".
func TestControlChannel_TransportRefusalNamesSetting(t *testing.T) {
	t.Run("hub with details", func(t *testing.T) {
		rec, c := runRefusedControlChannel(t, http.StatusForbidden, newHubTransportRefusal)
		msgs := rec.errorMessages()
		require.NotEmpty(t, msgs)
		assert.Contains(t, msgs[0], "reason untrusted_local_proxy")
		assert.Contains(t, msgs[0], "set "+config.TrustedProxiesKey+" on the hub")
		assert.True(t, c.TransportRefused())
	})
	t.Run("older hub without details", func(t *testing.T) {
		rec, c := runRefusedControlChannel(t, http.StatusForbidden, oldHubTransportRefusal)
		msgs := rec.errorMessages()
		require.NotEmpty(t, msgs)
		assert.Contains(t, msgs[0], "reason "+hubRefusalReasonUnspecified)
		assert.Contains(t, msgs[0], config.TrustedProxiesKey)
		assert.True(t, c.TransportRefused())
	})
	t.Run("other refusal keeps the generic line", func(t *testing.T) {
		rec, c := runRefusedControlChannel(t, http.StatusUnauthorized, `{"error":{"code":"broker_auth_failed","message":"no"}}`)
		msgs := rec.errorMessages()
		require.NotEmpty(t, msgs)
		assert.Equal(t, "Control channel connection failed", msgs[0])
		assert.False(t, c.TransportRefused())
	})
	t.Run("non-JSON body keeps the generic line", func(t *testing.T) {
		rec, c := runRefusedControlChannel(t, http.StatusForbidden, "forbidden")
		msgs := rec.errorMessages()
		require.NotEmpty(t, msgs)
		assert.Equal(t, "Control channel connection failed", msgs[0])
		assert.False(t, c.TransportRefused())
	})
}

func newRefusalTestDialer(t *testing.T, logs *syncBuffer) *ConduitDialer {
	t.Helper()
	d, err := NewConduitDialer(ConduitDialConfig{
		HubEndpoint: "http://127.0.0.1:1",
		BrokerID:    "broker-1",
		SecretKey:   []byte("0123456789abcdef0123456789abcdef"),
		Version:     "test",
		Log:         slog.New(slog.NewTextHandler(logs, nil)),
	})
	require.NoError(t, err)
	return d
}

func preUpgrade(status int, body string) *conduitPreUpgradeError {
	return &conduitPreUpgradeError{DialError: &ws.DialError{StatusCode: status, Err: errors.New("bad handshake")}, Body: body}
}

// The conduit logs the same reason and setting for a transport refusal,
// and reports it until a refusal of another kind.
func TestConduitDialer_TransportRefusalNamesSetting(t *testing.T) {
	logs := &syncBuffer{}
	d := newRefusalTestDialer(t, logs)

	d.fallback(preUpgrade(http.StatusForbidden, newHubTransportRefusal))
	assert.True(t, d.TransportRefused())
	out := logs.String()
	assert.Contains(t, out, "level=ERROR")
	assert.Contains(t, out, "Hub refused the conduit")
	assert.Contains(t, out, "reason untrusted_local_proxy")
	assert.Contains(t, out, "set "+config.TrustedProxiesKey+" on the hub")
	assert.Contains(t, out, "control channel only")

	d.fallback(preUpgrade(http.StatusServiceUnavailable, `{}`))
	assert.False(t, d.TransportRefused(), "another failure clears it")

	d.fallback(preUpgrade(http.StatusForbidden, oldHubTransportRefusal))
	assert.True(t, d.TransportRefused())
	assert.Contains(t, logs.String(), "reason "+hubRefusalReasonUnspecified)
}

// While a hub connection's control channel or conduit is refused by the
// hub's transport policy, the broker's health (and so its heartbeat)
// reports control_channel or conduit unhealthy and the broker degraded.
func TestHeartbeatHealth_TransportRefusalReportsCommandPath(t *testing.T) {
	srv := newTestServer(t)

	cc := newLoggedCloseTestClient("http://127.0.0.1:1", newLevelRecorder())
	cc.setTransportRefusal(parseHubRefusal(http.StatusForbidden, []byte(newHubTransportRefusal)))
	cd := newRefusalTestDialer(t, &syncBuffer{})
	cd.transportRefused.Store(true)
	hc := &HubConnection{Name: "refusal-test", ControlChannel: cc, conduitDialer: cd}

	srv.hubMu.Lock()
	if srv.hubConnections == nil {
		srv.hubConnections = map[string]*HubConnection{}
	}
	srv.hubConnections[hc.Name] = hc
	srv.hubMu.Unlock()
	t.Cleanup(func() {
		srv.hubMu.Lock()
		delete(srv.hubConnections, hc.Name)
		srv.hubMu.Unlock()
	})

	heartbeat := sendConnectionHealthHeartbeat(t, srv, hc)
	require.NotNil(t, heartbeat.Health)
	assert.Equal(t, api.BrokerHealthDegraded, heartbeat.Health.Status)
	assert.Equal(t, api.BrokerHealthUnhealthy, heartbeat.Health.Checks[healthCheckControlChannel])
	assert.Equal(t, api.BrokerHealthUnhealthy, heartbeat.Health.Checks[healthCheckConduit])
	assert.Equal(t, "online", heartbeat.Status, "health never changes liveness")

	// Refusal cleared: the channel, created just now and not connected,
	// is within its grace, and a conduit with no session; neither is
	// reported, and neither degrades the broker.
	cc.setTransportRefusal(nil)
	cd.transportRefused.Store(false)
	info := srv.GetHealthInfo(context.Background())
	assert.NotContains(t, info.Checks, healthCheckControlChannel)
	assert.NotContains(t, info.Checks, healthCheckConduit)
	assert.Equal(t, "healthy", info.Status)
}
