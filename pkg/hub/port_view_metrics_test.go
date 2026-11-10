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
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// portViewPoint is one data point of a page view counter: the counter
// name, its attributes and its value.
type portViewPoint struct {
	name  string
	attrs map[string]string
	value int64
}

// portViewCounters wires the page view counters on s and returns a reader
// of their data points.
func portViewCounters(t *testing.T, s *Server) func() []portViewPoint {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelPortViewMetrics(mp)
	require.NoError(t, err)
	s.SetPortViewMetrics(rec)
	return func() []portViewPoint {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var out []portViewPoint
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if !strings.HasPrefix(m.Name, "scion.hub.port_view.") {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					p := portViewPoint{name: m.Name, attrs: map[string]string{}, value: dp.Value}
					for _, kv := range dp.Attributes.ToSlice() {
						p.attrs[string(kv.Key)] = kv.Value.Emit()
					}
					out = append(out, p)
				}
			}
		}
		return out
	}
}

// portViewCount sums the points of counter name whose attributes are attrs
// exactly.
func portViewCount(points []portViewPoint, name string, attrs map[string]string) int64 {
	var n int64
	for _, p := range points {
		if p.name != name || len(p.attrs) != len(attrs) {
			continue
		}
		match := true
		for k, v := range attrs {
			if p.attrs[k] != v {
				match = false
			}
		}
		if match {
			n += p.value
		}
	}
	return n
}

// portViewMetricLabels are the only attribute values the page view
// counters may record.
var portViewMetricLabels = map[string]map[string][]string{
	"scion.hub.port_view.redirect": {
		"outcome": {portViewRedirectMinted, portViewRedirectNotEligible, portViewRedirectIssuanceClosed,
			portViewRedirectNoTransport, portViewRedirectUserRefused},
	},
	"scion.hub.port_view.request": {
		"transport": {portViewTransportConduit, portViewTransportLegacy},
		"outcome":   {portViewRequestServed, portViewRequestInvalid, portViewRequestOffline, portViewRequestRefused},
	},
}

// TestPortViewMetrics_Outcomes: a port link navigation that creates a page
// link counts minted; page view requests count served with their
// transport, and a failed link counts invalid with no transport. Every
// recorded attribute is an enumerated value: no link, user, agent, path
// or query appears in any label.
func TestPortViewMetrics_Outcomes(t *testing.T) {
	var seen []string
	for _, transport := range portViewTransports {
		t.Run(transport, func(t *testing.T) {
			f := newPortViewFixture(t, nil)
			points := portViewCounters(t, f.srv)
			f.startOver(t, transport)

			link := f.portLink("dir/index.html?a=1")
			res := f.send(t, http.MethodGet, link, withBearer(navHeader(), f.userToken), nil)
			require.Equal(t, http.StatusFound, res.status, res.body)
			view := resolveLocation(t, link, res)
			served := f.send(t, http.MethodGet, view.String(), subHeader(), nil)
			require.Equal(t, http.StatusOK, served.status, served.body)
			expired := f.capFor(f.launched.OwnerID, f.app.port, time.Now().Add(-time.Minute))
			requirePortViewInvalid(t, f.send(t, http.MethodGet, f.viewURL(expired, "a.js?q=1"), subHeader(), nil))

			wantTransport := map[string]string{"conduit": portViewTransportConduit, "tunnel": portViewTransportLegacy}[transport]
			got := points()
			assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.redirect",
				map[string]string{"outcome": portViewRedirectMinted}), "%+v", got)
			assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.request",
				map[string]string{"transport": wantTransport, "outcome": portViewRequestServed}), "%+v", got)
			assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.request",
				map[string]string{"outcome": portViewRequestInvalid}), "%+v", got)
			// An answer given before any transport is chosen carries no
			// transport attribute.
			for _, p := range got {
				if p.name == "scion.hub.port_view.request" && p.attrs["outcome"] == portViewRequestInvalid {
					_, has := p.attrs["transport"]
					assert.False(t, has, "an invalid answer names a transport: %+v", p)
				}
			}

			// The app stops answering: the chosen transport counts offline.
			f.app.srv.Close()
			gone := f.send(t, http.MethodGet, f.viewURL(f.ownerCap(), "gone.js"), subHeader(), nil)
			require.NotEqual(t, http.StatusOK, gone.status, gone.body)
			got = points()
			assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.request",
				map[string]string{"transport": wantTransport, "outcome": portViewRequestOffline}), "%+v", got)

			// Values that must never reach a label.
			private := []string{view.Path, view.RawQuery, expired, f.launched.ID, f.launched.OwnerID, "dir/index.html", "a.js"}
			for _, p := range got {
				allowed, ok := portViewMetricLabels[p.name]
				require.True(t, ok, "unexpected counter %s", p.name)
				for k, v := range p.attrs {
					assert.Contains(t, allowed[k], v, "%s: attribute %s=%q is not an enumerated value", p.name, k, v)
					for _, pv := range private {
						assert.NotContains(t, v, pv, "%s: attribute %s holds a request value", p.name, k)
					}
					seen = append(seen, p.name+" "+k+"="+v)
				}
			}
		})
	}
	sort.Strings(seen)
	t.Logf("attribute values recorded: %v", seen)
}

// TestPortViewMetrics_NoTransport: a port link navigation by a user who
// could open the page, for an agent no transport reaches, counts
// no_transport and creates no page link.
func TestPortViewMetrics_NoTransport(t *testing.T) {
	f := newPortViewFixture(t, nil)
	setPortViewExperiments(t, f.srv, false, true)
	points := portViewCounters(t, f.srv)
	res := f.send(t, http.MethodGet, f.portLink("index.html"), withBearer(navHeader(), f.userToken), nil)
	assert.NotEqual(t, http.StatusFound, res.status)
	got := points()
	assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.redirect",
		map[string]string{"outcome": portViewRedirectNoTransport}), "%+v", got)
}

// TestPortViewMetrics_ExperimentOff: with hub.port_view off, port link
// navigations and requests on the page view prefix record nothing.
func TestPortViewMetrics_ExperimentOff(t *testing.T) {
	f := newPortViewFixture(t, nil)
	setPortViewExperiments(t, f.srv, true, false)
	points := portViewCounters(t, f.srv)
	f.startAgent(t)
	res := f.send(t, http.MethodGet, f.portLink("index.html"), withBearer(navHeader(), f.userToken), nil)
	require.Equal(t, http.StatusOK, res.status, res.body)
	f.send(t, http.MethodGet, f.viewURL(f.ownerCap(), "a.js"), subHeader(), nil)
	assert.Empty(t, points())
}

// TestPortViewMetrics_TunnelRefusals: on the tunnel, a WebSocket (501) and
// a body over the tunnel's limit (413) are requests the transport does not
// carry, counted refused with transport legacy (portViewRequestRefused).
func TestPortViewMetrics_TunnelRefusals(t *testing.T) {
	f := newPortViewFixture(t, nil)
	points := portViewCounters(t, f.srv)
	startLegacyTunnel(t, f.conduitProxyFixture, f.srv)

	ws := f.send(t, http.MethodGet, f.viewURL(f.ownerCap(), "ws"), http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
		"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}, nil)
	require.Equal(t, http.StatusNotImplemented, ws.status, ws.body)
	big := f.send(t, http.MethodPost, f.viewURL(f.ownerCap(), "upload"), http.Header{"Content-Type": {"text/plain"}},
		bytes.NewReader(bytes.Repeat([]byte("a"), maxProxyRequestBody+1)))
	require.Equal(t, http.StatusRequestEntityTooLarge, big.status, big.body)
	assert.Zero(t, len(f.app.got), "neither request reached the app")

	got := points()
	assert.Equal(t, int64(2), portViewCount(got, "scion.hub.port_view.request",
		map[string]string{"transport": portViewTransportLegacy, "outcome": portViewRequestRefused}), "%+v", got)
}
