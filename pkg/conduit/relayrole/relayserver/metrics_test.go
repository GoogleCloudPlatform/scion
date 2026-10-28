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

package relayserver

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// collectRelayMetrics returns, by metric name, the observed values keyed by
// their cause attribute ("" when none).
func collectRelayMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			vals := map[string]int64{}
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					cause, _ := dp.Attributes.Value("cause")
					vals[cause.AsString()] = dp.Value
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					cause, _ := dp.Attributes.Value("cause")
					vals[cause.AsString()] = dp.Value
				}
			}
			out[m.Name] = vals
		}
	}
	return out
}

// TestRelayMetrics_RegisteredOnTheProvider (design §3.9, v2.21.55): with a
// MeterProvider, a singleton exports scion.relay.singleton_endpoint_peers,
// and every relay exports the ingress header counter with both causes
// (ingress_header_untrusted and ingress_cap_invalid); a relay that is not
// a singleton has no shared-endpoint gauge.
func TestRelayMetrics_RegisteredOnTheProvider(t *testing.T) {
	for _, singleton := range []bool{true, false} {
		w := relaytest.NewWorld(t)
		reader := sdkmetric.NewManualReader()
		cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
		cfg.Singleton = singleton
		cfg.MeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		startServer(t, cfg)
		got := collectRelayMetrics(t, reader)

		peers, hasGauge := got[metricSingletonEndpointPeers]
		if singleton && (!hasGauge || peers[""] != 0) {
			t.Fatalf("singleton: %s = %v (present %v), want 0", metricSingletonEndpointPeers, peers, hasGauge)
		}
		if !singleton && hasGauge {
			t.Fatalf("not a singleton: %s is registered", metricSingletonEndpointPeers)
		}
		refused, ok := got[metricIngressHeaderRefused]
		if !ok {
			t.Fatalf("singleton=%v: %s is not registered", singleton, metricIngressHeaderRefused)
		}
		for _, cause := range []string{"ingress_header_untrusted", "ingress_cap_invalid"} {
			if v, ok := refused[cause]; !ok || v != 0 {
				t.Fatalf("singleton=%v: %s{cause=%s} = %d (present %v), want 0", singleton, metricIngressHeaderRefused, cause, v, ok)
			}
		}
	}
}

// TestRelayMetrics_NoProviderRegistersNothing: without a MeterProvider the
// relay registers no instrument and starts as before.
func TestRelayMetrics_NoProviderRegistersNothing(t *testing.T) {
	if err := registerRelayMetrics(nil, nil, nil, true); err != nil {
		t.Fatalf("registerRelayMetrics(nil) = %v", err)
	}
}
