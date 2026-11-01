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

// The broker trusts the proxy's test certificate through SSL_CERT_FILE,
// which Go reads only on Linux and the BSDs (elsewhere it uses the
// platform verifier), so the test runs on Linux.

//go:build linux && !no_sqlite

package proxye2e_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

const (
	devToken = "scion_dev_proxye2e_test_0123456789abcdef"

	// Refusal reasons, as the hub reports them in the 403 details.
	reasonUntrustedLocalProxy = "untrusted_local_proxy"
	reasonNotTLS              = "not_tls"

	// hubRefusalLogPrefix starts the hub's ERROR for a local-proxy refusal.
	hubRefusalLogPrefix = "Refused broker credentials from a local TLS-terminating proxy"
	// brokerRefusalLogPrefix starts the broker's line for a refused
	// control channel.
	brokerRefusalLogPrefix = "Hub refused the control channel"
	// transportProxyHealthCheck is the hub's health check key.
	transportProxyHealthCheck = "broker_transport_proxy"

	// networkPeer is a non-loopback client address (TEST-NET-1).
	networkPeer = "192.0.2.10:40000"

	stepTimeout = 15 * time.Second
	// heartbeatTimeout covers the broker's minimum heartbeat interval
	// (runtimebroker.MinHeartbeatInterval) plus a margin.
	heartbeatTimeout = runtimebroker.MinHeartbeatInterval*2 + 5*time.Second
)

var dbSeq atomic.Int64

// TestMain points SSL_CERT_FILE at httptest's TLS certificate before
// anything in the process loads the system roots (Go loads them once), so
// the Runtime Broker's own HTTP and WebSocket clients trust the proxy.
func TestMain(m *testing.M) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	cert := ts.Certificate()
	ts.Close()
	dir, err := os.MkdirTemp("", "proxye2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	file := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("SSL_CERT_FILE", file); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestHubBehindLocalTLSProxy restarts the hub on one store with each
// server.trusted_proxies form an operator can have, with a Runtime Broker
// connecting through a TLS-terminating proxy on the hub's host:
//   - not set (an existing settings file, upgraded): the broker connects,
//     the hub reaches it, and the hub logs one startup warning;
//   - []: the control channel is refused with the reason, which the hub
//     logs at ERROR and reports as a health check, and the broker logs
//     and reports in its heartbeat;
//   - [127.0.0.1, ::1]: the broker connects, with no warning.
//
// Each phase takes the setting from settings.yaml after the hub has
// rewritten the file (startHubFromSettings), so [] must survive the
// rewrite to stay strict. In every phase a non-loopback peer that claims
// https is refused.
func TestHubBehindLocalTLSProxy(t *testing.T) {
	logs := captureLogs(t)
	f := newFixture(t)

	t.Run("NotSet", func(t *testing.T) {
		logs.reset()
		srv := f.startHubFromSettings(t, "", false)
		require.Equal(t, 1, logs.count(func(r logRecord) bool {
			return r.level == slog.LevelWarn && r.msg == config.TransportTrustedProxiesDefaultWarning
		}), "one startup warning names the default")

		f.startBroker(t)
		f.requireConnected(t)
		f.requireBrokerRoundTrip(t)
		require.NotContains(t, srv.GetHealthInfo(context.Background()).Checks, transportProxyHealthCheck)
		require.Zero(t, logs.count(isHubRefusal), "no refusal under the default")
		f.requireNetworkPeerRefused(t, srv)
	})

	t.Run("ExplicitEmpty", func(t *testing.T) {
		logs.reset()
		srv := f.startHubFromSettings(t, "  trusted_proxies: []\n", true)
		require.Zero(t, logs.count(isDefaultWarning), "no default warning when the setting is set")

		// The refusal through the proxy, as the broker receives it.
		status, code, details := f.connectThroughProxy(t)
		require.Equal(t, http.StatusForbidden, status)
		require.Equal(t, hub.ErrCodeTransportNotConfidential, code)
		require.Equal(t, reasonUntrustedLocalProxy, details["reason"])
		require.Equal(t, config.TrustedProxiesKey, details["setting"])

		f.startBroker(t)
		require.Eventually(t, func() bool { return logs.count(isBrokerRefusal) > 0 },
			stepTimeout, 50*time.Millisecond, "the broker logs the hub's refusal")
		broker := logs.first(isBrokerRefusal)
		require.Equal(t, slog.LevelError, broker.level)
		require.Equal(t, reasonUntrustedLocalProxy, broker.attrs["reason"])
		require.Equal(t, config.TrustedProxiesKey, broker.attrs["setting"])
		require.Contains(t, broker.msg, config.TrustedProxiesKey)
		require.False(t, f.ccMgr.IsConnected(f.brokerID), "the control channel is refused")

		require.Equal(t, 1, logs.count(isHubRefusal), "the hub logs one ERROR (rate-limited)")
		hubErr := logs.first(isHubRefusal)
		require.Equal(t, slog.LevelError, hubErr.level)
		require.Equal(t, reasonUntrustedLocalProxy, hubErr.attrs["reason"])
		require.Contains(t, hubErr.msg, config.TrustedProxiesKey)

		health := srv.GetHealthInfo(context.Background())
		require.Contains(t, health.Checks, transportProxyHealthCheck)
		require.Equal(t, hub.HealthStatusDegraded, health.Status, "non-critical: degraded, not unhealthy")

		require.Equal(t, api.BrokerHealthUnhealthy, f.broker.GetHealthInfo(context.Background()).Checks["control_channel"])
		require.Eventually(t, func() bool {
			b, err := f.store.GetRuntimeBroker(context.Background(), f.brokerID)
			return err == nil && b.Health != nil && b.Health.Checks["control_channel"] == api.BrokerHealthUnhealthy
		}, heartbeatTimeout, 100*time.Millisecond, "the broker's heartbeat reports control_channel unhealthy")

		f.requireNetworkPeerRefused(t, srv)
	})

	t.Run("ExplicitLoopback", func(t *testing.T) {
		logs.reset()
		srv := f.startHubFromSettings(t, "  trusted_proxies:\n    - 127.0.0.1\n    - \"::1\"\n", true)
		f.startBroker(t)
		f.requireConnected(t)
		f.requireBrokerRoundTrip(t)
		require.Zero(t, logs.count(isDefaultWarning), "no default warning when the setting is set")
		require.Zero(t, logs.count(isHubRefusal))
		require.Zero(t, logs.count(isBrokerRefusal))
		require.NotContains(t, srv.GetHealthInfo(context.Background()).Checks, transportProxyHealthCheck)
		// No refusal: the broker's command path is never unhealthy. Its
		// control channel is healthy, or not yet reported while the broker
		// finishes the handshake the hub has already seen (the grace), and
		// its heartbeat tells the hub healthy (replacing the refused phase's
		// value).
		checks := f.broker.GetHealthInfo(context.Background()).Checks
		if v, ok := checks["control_channel"]; ok {
			require.Equal(t, api.BrokerHealthHealthy, v, "control_channel is healthy when reported")
		}
		require.NotEqual(t, api.BrokerHealthUnhealthy, checks["conduit"], "no conduit refusal")
		require.Eventually(t, func() bool {
			b, err := f.store.GetRuntimeBroker(context.Background(), f.brokerID)
			return err == nil && b.Health != nil && b.Health.Checks["control_channel"] == api.BrokerHealthHealthy
		}, heartbeatTimeout, 100*time.Millisecond, "the broker's heartbeat reports control_channel healthy")
		f.requireNetworkPeerRefused(t, srv)
	})
}

func isDefaultWarning(r logRecord) bool { return r.msg == config.TransportTrustedProxiesDefaultWarning }
func isHubRefusal(r logRecord) bool     { return strings.HasPrefix(r.msg, hubRefusalLogPrefix) }
func isBrokerRefusal(r logRecord) bool  { return strings.HasPrefix(r.msg, brokerRefusalLogPrefix) }

// fixture is a store shared by the hub instances of each phase, one
// registered Runtime Broker identity, and a TLS-terminating reverse proxy
// on loopback whose upstream is the current phase's hub.
type fixture struct {
	store     store.Store
	brokerID  string
	brokerKey []byte
	proxy     *tlsProxy

	// Per phase.
	ccMgr  *hub.ControlChannelManager
	broker *runtimebroker.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	testutil.ScrubHubEnv(t)
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	dbName := fmt.Sprintf("proxye2e_%d", dbSeq.Add(1))
	client, err := entc.OpenSQLite("file:"+dbName+"?mode=memory&cache=shared", entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(ctx, client))
	s := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = s.Close() })

	f := &fixture{store: s, brokerID: uuid.NewString(), brokerKey: make([]byte, 32)}
	_, err = rand.Read(f.brokerKey)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: f.brokerID, Name: "proxy-e2e-broker", Slug: "proxy-e2e-broker",
		Status: store.BrokerStatusOnline, Created: now, Updated: now,
	}))
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: f.brokerID, SecretKey: f.brokerKey, Status: store.BrokerSecretStatusActive,
	}))
	f.proxy = newTLSProxy(t)
	return f
}

// startHubFromSettings writes settings.yaml with the server block's
// trusted_proxies lines (none for not set), rewrites it through the
// settings struct as the hub's own settings PUTs do, loads the server
// config as the hub does at startup, and starts the hub with the result,
// so each phase models an upgrade of an existing settings file that the
// hub has rewritten. wantSet is the set state the load must give.
func (f *fixture) startHubFromSettings(t *testing.T, trustedProxiesLines string, wantSet bool) *hub.Server {
	t.Helper()
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(globalDir, 0o700))
	content := "schema_version: \"1\"\nserver:\n  log_level: info\n" + trustedProxiesLines
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(content), 0o600))
	require.NoError(t, config.LoadModifySaveVersionedSettings(globalDir, func(vs *config.VersionedSettings) error {
		vs.ImageRegistry = "rewrite.example"
		return nil
	}))
	gc, err := config.LoadGlobalConfig("")
	require.NoError(t, err)
	require.Equal(t, wantSet, gc.TrustedProxiesSet, "server.trusted_proxies set state after the rewrite")
	return f.startHub(t, gc.TrustedProxies, gc.TrustedProxiesSet)
}

// startHub starts a hub on the shared store with server.trusted_proxies
// as given (set reports whether the setting is set at all), served in
// plain HTTP on loopback behind the proxy. It is stopped at the end of the
// calling (sub)test.
func (f *fixture) startHub(t *testing.T, trusted []string, set bool) *hub.Server {
	t.Helper()
	ctx := context.Background()
	cfg := hub.DefaultServerConfig()
	cfg.DevAuthToken = devToken
	cfg.DisableCloudLogQuery = true
	cfg.DevUserConfig = hub.DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	cfg.TransportTrustedProxies = trusted
	cfg.TransportTrustedProxiesSet = set
	srv, err := hub.New(cfg, f.store)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ops := hub.NewOperationalSettings(f.store, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	f.ccMgr = srv.GetControlChannelManager()

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	f.proxy.setUpstream(t, ts.URL)
	return srv
}

// startBroker starts the Runtime Broker, reaching the hub through the
// proxy's https URL, with heartbeats on. It is stopped at the end of the
// calling (sub)test.
func (f *fixture) startBroker(t *testing.T) {
	t.Helper()
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return []api.AgentInfo{}, nil },
	}
	bcfg := runtimebroker.DefaultServerConfig()
	bcfg.Host = "127.0.0.1"
	bcfg.BrokerID = f.brokerID
	bcfg.BrokerName = "proxy-e2e-broker"
	bcfg.StateDir = filepath.Join(t.TempDir(), "broker-state")
	bcfg.HubEnabled = true
	bcfg.HubEndpoint = f.proxy.URL()
	bcfg.ControlChannelEnabled = true
	bcfg.HeartbeatEnabled = true
	bcfg.HeartbeatInterval = runtimebroker.MinHeartbeatInterval
	bcfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{
		BrokerID:    f.brokerID,
		SecretKey:   base64.StdEncoding.EncodeToString(f.brokerKey),
		HubEndpoint: f.proxy.URL(),
	}
	f.broker = runtimebroker.New(bcfg, agent.NewManager(rt), rt, config.RuntimeBuildRecord{})
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, f.broker.StartServices(ctx))
	broker := f.broker
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = broker.Shutdown(sctx)
	})
}

func (f *fixture) requireConnected(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return f.ccMgr.IsConnected(f.brokerID) },
		stepTimeout, 50*time.Millisecond, "the broker connects its control channel through the proxy")
}

// requireBrokerRoundTrip sends a request from the hub to the broker over
// the control channel and requires an answer.
func (f *fixture) requireBrokerRoundTrip(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	resp, err := f.ccMgr.TunnelRequest(ctx, f.brokerID,
		wsprotocol.NewRequestEnvelope(uuid.NewString(), http.MethodGet, "/healthz", "", nil, nil))
	require.NoError(t, err, "the hub reaches the broker over its control channel")
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", resp.Body)
}

// upgradeRequest is a control channel upgrade request signed with the
// broker's credentials.
func (f *fixture) upgradeRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target+"/api/v1/runtime-brokers/connect", nil)
	require.NoError(t, err)
	key := make([]byte, 16)
	_, err = rand.Read(key)
	require.NoError(t, err)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key))
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: f.brokerID, SecretKey: f.brokerKey}).ApplyAuth(req))
	return req
}

// connectThroughProxy sends a signed control channel upgrade through the
// proxy and returns the status and, for an error, its code and details.
func (f *fixture) connectThroughProxy(t *testing.T) (int, string, map[string]any) {
	t.Helper()
	resp, err := f.proxy.client().Do(f.upgradeRequest(t, f.proxy.URL()))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	code, details := decodeError(t, resp.Body)
	return resp.StatusCode, code, details
}

// requireNetworkPeerRefused sends a signed control channel upgrade from a
// non-loopback peer that claims https, straight to the hub's handler, and
// requires the transport refusal: a network peer's forwarding headers are
// never trusted, whatever server.trusted_proxies holds.
func (f *fixture) requireNetworkPeerRefused(t *testing.T, srv *hub.Server) {
	t.Helper()
	req := f.upgradeRequest(t, "http://hub.internal")
	req.RemoteAddr = networkPeer
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	code, details := decodeError(t, rec.Body)
	require.Equal(t, hub.ErrCodeTransportNotConfidential, code)
	require.Equal(t, reasonNotTLS, details["reason"])
}

func decodeError(t *testing.T, body io.Reader) (string, map[string]any) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.NewDecoder(body).Decode(&resp)
	return resp.Error.Code, resp.Error.Details
}

// tlsProxy is a TLS-terminating reverse proxy on loopback, as Caddy or
// nginx in front of a hub: it sets X-Forwarded-Proto (https) and
// X-Forwarded-For from its own connection, dropping any client value, and
// passes WebSocket upgrades through.
type tlsProxy struct {
	ts       *httptest.Server
	upstream atomic.Pointer[url.URL]
}

func newTLSProxy(t *testing.T) *tlsProxy {
	t.Helper()
	p := &tlsProxy{}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(p.upstream.Load())
			r.SetXForwarded()
		},
	}
	p.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.upstream.Load() == nil {
			http.Error(w, "no upstream", http.StatusBadGateway)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.ts.Close)
	return p
}

func (p *tlsProxy) URL() string { return p.ts.URL }

func (p *tlsProxy) client() *http.Client { return p.ts.Client() }

func (p *tlsProxy) setUpstream(t *testing.T, raw string) {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	p.upstream.Store(u)
}

// logRecord is one captured slog record with its attributes as strings.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// logCapture records every slog record logged through the default logger.
// The hub and the broker log there, so the test is not parallel.
type logCapture struct {
	mu      sync.Mutex
	records []logRecord
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(&captureHandler{c: c}))
	t.Cleanup(func() { slog.SetDefault(old) })
	return c
}

func (c *logCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = nil
}

func (c *logCapture) count(match func(logRecord) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.records {
		if match(r) {
			n++
		}
	}
	return n
}

func (c *logCapture) first(match func(logRecord) bool) logRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if match(r) {
			return r
		}
	}
	return logRecord{}
}

type captureHandler struct {
	c     *logCapture
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	for _, a := range h.attrs {
		rec.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.c.mu.Lock()
	h.c.records = append(h.c.records, rec)
	h.c.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{c: h.c, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

// WithGroup keeps the group's attributes flat; the test matches on keys
// that the hub and the broker log at the top level.
func (h *captureHandler) WithGroup(string) slog.Handler { return h }
