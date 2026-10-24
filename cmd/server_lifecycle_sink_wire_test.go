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

package cmd

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	gcplog "cloud.google.com/go/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/api/option"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// sinkWireHub is a hub on a fresh SQLite test store.
func sinkWireHub(t *testing.T) *hub.Server {
	t.Helper()
	cfg := hub.DefaultServerConfig()
	cfg.DisableCloudLogQuery = true
	srv, err := hub.New(cfg, newTestStore(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

// sinkKindReader installs an OTel SDK meter provider with a manual reader
// as lifecycleMeterProvider and returns the reader.
func sinkKindReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	lifecycleMeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return reader
}

// sinkKinds reads lifecycle_sink_kind by kind attribute; nil when the gauge
// was never recorded.
func sinkKinds(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var out map[string]int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetryemit.MetricSinkKind {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			require.True(t, ok, "%s is an int64 gauge, got %T", m.Name, m.Data)
			out = map[string]int64{}
			for _, dp := range g.DataPoints {
				kind, _ := dp.Attributes.Value("kind")
				out[kind.AsString()] = dp.Value
			}
		}
	}
	return out
}

func wantKind(kind string) map[string]int64 {
	out := map[string]int64{telemetryemit.SinkKindCloudLogging: 0, telemetryemit.SinkKindStdout: 0, telemetryemit.SinkKindNone: 0}
	out[kind] = 1
	return out
}

// offlineCloudHandler is a real ResilientCloudHandler over a Cloud Logging
// client that never authenticates and points at an unused local endpoint;
// the client dials lazily, so nothing reaches the network unless a write
// is flushed.
func offlineCloudHandler(t *testing.T) *logging.ResilientCloudHandler {
	t.Helper()
	client, err := gcplog.NewClient(context.Background(), "projects/scion-test",
		option.WithoutAuthentication(), option.WithEndpoint("127.0.0.1:1"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	inner := logging.NewCloudHandlerFromClient(client, "scion-test", "hub", "", "", slog.LevelInfo)
	h, cleanup := logging.NewResilientCloudHandler(inner, logging.ResilientCloudHandlerConfig{})
	t.Cleanup(cleanup)
	return h
}

// TestWireLifecycleEventsSink_NilServer: no hub, nothing wired or
// recorded.
func TestWireLifecycleEventsSink_NilServer(t *testing.T) {
	sinkTestGlobals(t)
	reader := sinkKindReader(t)
	wireLifecycleEventsSink(nil, sinkTestConfig("127.0.0.1", "", "sqlite"), true)
	assert.Nil(t, sinkKinds(t, reader), "a nil server records no sink kind")
}

// TestWireLifecycleEventsSink_Kinds: the environment decides the sink, and
// the choice is published as lifecycle_sink_kind (stdout on a loopback
// SQLite dev-auth workstation, none elsewhere; a Cloud Run service or
// Kubernetes never gets stdout).
func TestWireLifecycleEventsSink_Kinds(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cfg     func() (hubHost, driver string, devAuth bool)
		k8s     string
		service string
		want    string
	}{
		{"workstation gets stdout", func() (string, string, bool) { return "127.0.0.1", "sqlite", true }, "", "", telemetryemit.SinkKindStdout},
		{"no dev auth gets none", func() (string, string, bool) { return "127.0.0.1", "sqlite", false }, "", "", telemetryemit.SinkKindNone},
		{"public host gets none", func() (string, string, bool) { return "0.0.0.0", "sqlite", true }, "", "", telemetryemit.SinkKindNone},
		{"postgres gets none", func() (string, string, bool) { return "127.0.0.1", "postgres", true }, "", "", telemetryemit.SinkKindNone},
		{"kubernetes gets none", func() (string, string, bool) { return "127.0.0.1", "sqlite", true }, "10.0.0.1", "", telemetryemit.SinkKindNone},
		{"cloud run gets none", func() (string, string, bool) { return "127.0.0.1", "sqlite", true }, "", "scion-hub", telemetryemit.SinkKindNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sinkTestGlobals(t)
			lifecycleCloudHandler = nil
			enableRuntimeBroker = false
			t.Setenv("KUBERNETES_SERVICE_HOST", tt.k8s)
			t.Setenv("K_SERVICE", tt.service)
			newLifecycleCloudSink = func(*gcplog.Client, func() bool) *telemetryemit.CloudLoggingSink {
				t.Fatal("the cloud sink must not be built without a Cloud Logging handler")
				return nil
			}
			reader := sinkKindReader(t)
			host, driver, devAuth := tt.cfg()
			wireLifecycleEventsSink(sinkWireHub(t), sinkTestConfig(host, "", driver), devAuth)
			assert.Equal(t, wantKind(tt.want), sinkKinds(t, reader))
		})
	}
}

// TestWireLifecycleEventsSink_CloudLogging: with server Cloud Logging on,
// the sink is built from the shared handler, with its client and its
// circuit breaker passed through, and lifecycle_sink_kind=cloud_logging.
func TestWireLifecycleEventsSink_CloudLogging(t *testing.T) {
	sinkTestGlobals(t)
	h := offlineCloudHandler(t)
	lifecycleCloudHandler = h
	var gotClient *gcplog.Client
	var gotCircuit func() bool
	calls := 0
	newLifecycleCloudSink = func(c *gcplog.Client, circuitOpen func() bool) *telemetryemit.CloudLoggingSink {
		calls++
		gotClient, gotCircuit = c, circuitOpen
		return telemetryemit.NewCloudLoggingSink(c, circuitOpen)
	}
	reader := sinkKindReader(t)
	wireLifecycleEventsSink(sinkWireHub(t), sinkTestConfig("0.0.0.0", "", "postgres"), false)

	require.Equal(t, 1, calls, "the cloud sink is built once")
	assert.Same(t, h.Client(), gotClient, "the shared handler's client is passed through")
	require.NotNil(t, gotCircuit, "the circuit-open func is passed through")
	assert.Equal(t, reflect.ValueOf(h.CircuitOpen).Pointer(), reflect.ValueOf(gotCircuit).Pointer(),
		"it is the handler's CircuitOpen method")
	assert.Equal(t, h.CircuitOpen(), gotCircuit(), "and reports the handler's circuit state")
	assert.Equal(t, wantKind(telemetryemit.SinkKindCloudLogging), sinkKinds(t, reader))
}

// TestWireLifecycleEventsSink_DefaultSeam: the seam's default is the
// production constructor (telemetryemit.NewCloudLoggingSink).
func TestWireLifecycleEventsSink_DefaultSeam(t *testing.T) {
	assert.Equal(t, reflect.ValueOf(telemetryemit.NewCloudLoggingSink).Pointer(), reflect.ValueOf(newLifecycleCloudSink).Pointer())
}
