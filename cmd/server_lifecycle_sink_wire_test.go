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
	"net"
	"reflect"
	"testing"
	"time"

	gcplog "cloud.google.com/go/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

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

// fakeLoggingServer is an in-process gRPC server on loopback that answers
// every call with PermissionDenied (not retried by the Cloud Logging
// client), so a flush fails at once and nothing leaves the machine.
func fakeLoggingServer(t *testing.T) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
		return status.Error(codes.PermissionDenied, "lifecycle sink test: no writes")
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// offlineCloudHandler is a real ResilientCloudHandler over a Cloud Logging
// client connected to fakeLoggingServer (no authentication, no network).
// cfg tunes its circuit breaker.
func offlineCloudHandler(t *testing.T, cfg logging.ResilientCloudHandlerConfig) *logging.ResilientCloudHandler {
	t.Helper()
	client, err := gcplog.NewClient(context.Background(), "projects/scion-test",
		option.WithGRPCConn(fakeLoggingServer(t)), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	inner := logging.NewCloudHandlerFromClient(client, "scion-test", "hub", "", "", slog.LevelInfo)
	h, cleanup := logging.NewResilientCloudHandler(inner, cfg)
	t.Cleanup(cleanup)
	return h
}

// openCircuit drives h's circuit breaker open offline: one buffered entry
// makes the next health probe flush to fakeLoggingServer, which refuses it;
// with MaxFailures 1 the circuit opens.
func openCircuit(t *testing.T, h *logging.ResilientCloudHandler) {
	t.Helper()
	slog.New(h).Info("lifecycle sink test entry")
	require.Eventually(t, h.CircuitOpen, 30*time.Second, 20*time.Millisecond, "the failing flush opens the circuit")
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
// The circuit func is proved bound to the wired handler by behaviour
// (upmerge2 r2 R2-4): two handlers in different circuit states, and the
// func the sink receives reports its own handler's state each time.
func TestWireLifecycleEventsSink_CloudLogging(t *testing.T) {
	sinkTestGlobals(t)
	open := offlineCloudHandler(t, logging.ResilientCloudHandlerConfig{
		MaxFailures: 1, ProbeInterval: 20 * time.Millisecond, ProbeTimeout: 2 * time.Second, OpenDuration: time.Hour,
	})
	closed := offlineCloudHandler(t, logging.ResilientCloudHandlerConfig{ProbeInterval: time.Hour})
	openCircuit(t, open)
	require.False(t, closed.CircuitOpen(), "the second handler's circuit stays closed")

	for _, tt := range []struct {
		name string
		h    *logging.ResilientCloudHandler
		want bool
	}{
		{"handler with an open circuit", open, true},
		{"handler with a closed circuit", closed, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lifecycleCloudHandler = tt.h
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
			assert.Same(t, tt.h.Client(), gotClient, "the wired handler's client is passed through")
			require.NotNil(t, gotCircuit, "the circuit-open func is passed through")
			assert.Equal(t, tt.want, gotCircuit(), "the func reports the wired handler's own circuit state")
			assert.Equal(t, wantKind(telemetryemit.SinkKindCloudLogging), sinkKinds(t, reader))
		})
	}
}

// TestWireLifecycleEventsSink_DefaultSeam: the seam's default is the
// production constructor (telemetryemit.NewCloudLoggingSink).
func TestWireLifecycleEventsSink_DefaultSeam(t *testing.T) {
	assert.Equal(t, reflect.ValueOf(telemetryemit.NewCloudLoggingSink).Pointer(), reflect.ValueOf(newLifecycleCloudSink).Pointer())
}
