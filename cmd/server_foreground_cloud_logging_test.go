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
	"sort"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// useServerCloudWriter substitutes standalone writer=cloud counters for the
// test, so nothing touches the process-wide logging.CloudWriter().
func useServerCloudWriter(t *testing.T) *logging.CloudWriteStats {
	t.Helper()
	cs := &logging.CloudWriteStats{}
	prev := serverCloudWriter
	serverCloudWriter = func() *logging.CloudWriteStats { return cs }
	t.Cleanup(func() { serverCloudWriter = prev })
	return cs
}

// P2-4: with a ManualReader MeterProvider through the real
// wireHubCoreMetrics, the writer=cloud series export with only the declared
// attributes, and counts recorded before the provider was attached are
// exported in full (the atomics are read at collection).
func TestWireHubCoreMetrics_CloudWriterSeries(t *testing.T) {
	ctx := context.Background()
	cs := useServerCloudWriter(t)

	// Before any MeterProvider exists (logging initializes first).
	cs.RecordFailure(logging.CloudReasonError)
	cs.RecordFailure(logging.CloudReasonCircuitOpen)
	cs.RecordFailure(logging.CloudReasonCircuitOpen)
	var open atomic.Bool
	open.Store(true)
	cs.RegisterCircuitSource(open.Load)

	srv, err := hub.New(hub.ServerConfig{}, newTestStore(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	wireHubCoreMetrics(srv, mp)

	// After attach.
	cs.RecordFailure(logging.CloudReasonQueueFull)
	cs.RecordFailure(logging.CloudReasonFlushError)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	failures := map[string]int64{}
	var circuit []int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				if m.Name != logging.MetricWriteFailures {
					continue
				}
				require.True(t, d.IsMonotonic, "failures stays a monotonic counter")
				require.Equal(t, metricdata.CumulativeTemporality, d.Temporality)
				for _, dp := range d.DataPoints {
					attrs := map[string]string{}
					var keys []string
					for _, kv := range dp.Attributes.ToSlice() {
						attrs[string(kv.Key)] = kv.Value.String()
						keys = append(keys, string(kv.Key))
					}
					sort.Strings(keys)
					require.Equal(t, []string{"reason", "writer"}, keys)
					if attrs["writer"] == logging.CloudWriterName {
						failures[attrs["reason"]] = dp.Value
					}
				}
			case metricdata.Gauge[int64]:
				if m.Name != logging.MetricWriterCircuitOpen {
					continue
				}
				for _, dp := range d.DataPoints {
					attrs := map[string]string{}
					for _, kv := range dp.Attributes.ToSlice() {
						attrs[string(kv.Key)] = kv.Value.String()
					}
					require.Equal(t, map[string]string{"writer": "cloud"}, attrs)
					circuit = append(circuit, dp.Value)
				}
			}
		}
	}
	require.Equal(t, map[string]int64{
		"error":        int64(cs.Failures(logging.CloudReasonError)),
		"queue_full":   int64(cs.Failures(logging.CloudReasonQueueFull)),
		"circuit_open": int64(cs.Failures(logging.CloudReasonCircuitOpen)),
		"flush_error":  int64(cs.Failures(logging.CloudReasonFlushError)),
	}, failures)
	require.Equal(t, int64(2), failures["circuit_open"], "pre-attach counts exported")
	require.Equal(t, []int64{1}, circuit)
}

// P2-5 wiring: cmd injects the cloud_logging provider only when a Cloud
// Logging handler registered its circuit; otherwise the key is absent.
func TestWireCloudLoggingHealth_OnlyWhenConfigured(t *testing.T) {
	ctx := context.Background()
	srv, err := hub.New(hub.ServerConfig{}, newTestStore(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	cs := &logging.CloudWriteStats{}
	wireCloudLoggingHealth(srv, cs)
	_, present := srv.GetHealthInfo(ctx).Checks["cloud_logging"]
	require.False(t, present, "cloud_logging wired without a Cloud handler")

	var open atomic.Bool
	cs.RegisterCircuitSource(open.Load)
	wireCloudLoggingHealth(srv, cs)
	require.Equal(t, "healthy", srv.GetHealthInfo(ctx).Checks["cloud_logging"])
	open.Store(true)
	info := srv.GetHealthInfo(ctx)
	require.Equal(t, "degraded: circuit open", info.Checks["cloud_logging"])
	require.NotEqual(t, hub.HealthStatusUnhealthy, info.Status)

	wireCloudLoggingHealth(nil, cs) // no hub: no panic
}
