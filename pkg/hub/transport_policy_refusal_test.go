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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transportRefusalLogs records slog records for the refusal tests.
type transportRefusalLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *transportRefusalLogs) Enabled(context.Context, slog.Level) bool { return true }
func (h *transportRefusalLogs) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *transportRefusalLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *transportRefusalLogs) WithGroup(string) slog.Handler      { return h }

// matching returns the records at level whose message contains substr.
func (h *transportRefusalLogs) matching(level slog.Level, substr string) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			out = append(out, r)
		}
	}
	return out
}

func refusalAttr(r slog.Record, key string) slog.Value {
	var v slog.Value
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value
			return false
		}
		return true
	})
	return v
}

func captureTransportRefusalLogs(t *testing.T) *transportRefusalLogs {
	t.Helper()
	h := &transportRefusalLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func brokerConnectFrom(remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers/connect", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

type transportRefusalBody struct {
	Error struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Details map[string]interface{} `json:"details"`
	} `json:"error"`
}

func decodeTransportRefusal(t *testing.T, rec *httptest.ResponseRecorder) transportRefusalBody {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	var body transportRefusalBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body: %s", rec.Body.String())
	return body
}

// A loopback peer with forwarding headers and an empty trusted list is a
// local proxy the hub does not trust: the 403 keeps its code and names the
// reason and the setting, the hub logs one ERROR naming the setting (then
// suppresses repeats), and the health check reports it.
func TestRequireConfidentialArrival_LocalProxyRefusal(t *testing.T) {
	logs := captureTransportRefusalLogs(t)
	s := &Server{}
	xfp := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "198.51.100.7"}

	rec := httptest.NewRecorder()
	assert.False(t, s.requireConfidentialArrival(rec, brokerConnectFrom("127.0.0.1:40000", xfp)))
	body := decodeTransportRefusal(t, rec)
	assert.Equal(t, ErrCodeTransportNotConfidential, body.Error.Code, "the code is unchanged for existing brokers")
	assert.Equal(t, config.ArrivalRefusalUntrustedLocalProxy, body.Error.Details["reason"])
	assert.Equal(t, config.TrustedProxiesKey, body.Error.Details["setting"])
	assert.True(t, strings.HasPrefix(body.Error.Message, localProxyRefusalPrefix), "message: %s", body.Error.Message)
	assert.Contains(t, body.Error.Message, config.TrustedProxiesKey)

	for i := 0; i < 3; i++ {
		s.requireConfidentialArrival(httptest.NewRecorder(), brokerConnectFrom("127.0.0.1:40000", xfp))
	}
	errs := logs.matching(slog.LevelError, "local TLS-terminating proxy")
	require.Len(t, errs, 1, "the ERROR is rate-limited")
	assert.Contains(t, errs[0].Message, config.TrustedProxiesKey)
	assert.Equal(t, config.ArrivalRefusalUntrustedLocalProxy, refusalAttr(errs[0], "reason").String())
	assert.Equal(t, uint64(0), refusalAttr(errs[0], "suppressed").Uint64())
	assert.Empty(t, logs.matching(slog.LevelWarn, "Refused a broker credential request"), "a local proxy refusal is not also a WARN")

	checks := map[string]string{}
	s.checkTransportProxyHealth(checks)
	require.Contains(t, checks, transportProxyHealthCheck)
	assert.Contains(t, checks[transportProxyHealthCheck], config.TrustedProxiesKey)
	assert.Equal(t, HealthStatusDegraded, deriveHealthStatus(map[string]string{
		"database": HealthStatusHealthy, transportProxyHealthCheck: checks[transportProxyHealthCheck],
	}), "the check degrades the hub, never makes it unhealthy")
}

// A local proxy that a non-empty list does not name gets its own reason.
func TestRequireConfidentialArrival_LocalProxyNotInList(t *testing.T) {
	logs := captureTransportRefusalLogs(t)
	s := &Server{transportTrustedNets: mustTrustedNets(t, "10.20.0.0/16")}

	rec := httptest.NewRecorder()
	s.requireConfidentialArrival(rec, brokerConnectFrom("[::1]:40000", map[string]string{"X-Forwarded-Proto": "https"}))
	body := decodeTransportRefusal(t, rec)
	assert.Equal(t, config.ArrivalRefusalLocalProxyNotTrusted, body.Error.Details["reason"])
	errs := logs.matching(slog.LevelError, "does not list this proxy")
	require.Len(t, errs, 1)
}

// A plaintext request from another host keeps today's WARN, names its
// reason, and raises no local-proxy health check.
func TestRequireConfidentialArrival_NetworkPeerRefusal(t *testing.T) {
	logs := captureTransportRefusalLogs(t)
	s := &Server{}

	rec := httptest.NewRecorder()
	assert.False(t, s.requireConfidentialArrival(rec, brokerConnectFrom("192.0.2.10:40000", map[string]string{"X-Forwarded-Proto": "https"})))
	body := decodeTransportRefusal(t, rec)
	assert.Equal(t, ErrCodeTransportNotConfidential, body.Error.Code)
	assert.Equal(t, config.ArrivalRefusalNotTLS, body.Error.Details["reason"])
	assert.Equal(t, config.TrustedProxiesKey, body.Error.Details["setting"])
	assert.False(t, strings.HasPrefix(body.Error.Message, localProxyRefusalPrefix))

	assert.Len(t, logs.matching(slog.LevelWarn, "Refused a broker credential request"), 1)
	// A proxy container on a bridge network is refused this way too, so
	// the WARN names the setting.
	assert.Len(t, logs.matching(slog.LevelWarn, "list that address in "+config.TrustedProxiesKey), 1)
	assert.Empty(t, logs.matching(slog.LevelError, ""))
	checks := map[string]string{}
	s.checkTransportProxyHealth(checks)
	assert.NotContains(t, checks, transportProxyHealthCheck)
}

// An accepted request writes nothing.
func TestRequireConfidentialArrival_AcceptedWritesNothing(t *testing.T) {
	s := &Server{transportTrustedNets: mustTrustedNets(t, "127.0.0.1", "::1")}
	rec := httptest.NewRecorder()
	assert.True(t, s.requireConfidentialArrival(rec, brokerConnectFrom("127.0.0.1:40000", map[string]string{"X-Forwarded-Proto": "https"})))
	assert.Equal(t, 0, rec.Body.Len())
}

// The ERROR for a reason is logged at most once per interval; the next line
// reports how many were suppressed. Reasons are limited separately.
func TestTransportProxyRefusals_RateLimit(t *testing.T) {
	var st transportProxyRefusals
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a, b := config.ArrivalRefusalUntrustedLocalProxy, config.ArrivalRefusalLocalProxyNotTrusted

	logIt, n := st.record(a, t0)
	assert.True(t, logIt)
	assert.Zero(t, n)
	for i := 1; i <= 4; i++ {
		logIt, _ = st.record(a, t0.Add(time.Duration(i)*time.Minute))
		assert.False(t, logIt, "within the interval")
	}
	logIt, _ = st.record(b, t0.Add(time.Minute))
	assert.True(t, logIt, "another reason has its own limit")

	logIt, n = st.record(a, t0.Add(transportProxyRefusalLogInterval))
	assert.True(t, logIt)
	assert.Equal(t, uint64(4), n)
	logIt, n = st.record(a, t0.Add(2*transportProxyRefusalLogInterval))
	assert.True(t, logIt)
	assert.Zero(t, n, "the count resets after it is reported")
}

// The health check lasts for the window after the last refusal.
func TestTransportProxyRefusals_HealthWindow(t *testing.T) {
	var st transportProxyRefusals
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, ok := st.recent(t0)
	assert.False(t, ok, "nothing refused yet")

	st.record(config.ArrivalRefusalUntrustedLocalProxy, t0)
	reason, ok := st.recent(t0.Add(transportProxyRefusalHealthWindow - time.Second))
	assert.True(t, ok)
	assert.Equal(t, config.ArrivalRefusalUntrustedLocalProxy, reason)
	_, ok = st.recent(t0.Add(transportProxyRefusalHealthWindow))
	assert.False(t, ok, "expired")
}
