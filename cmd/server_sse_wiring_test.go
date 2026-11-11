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
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/eventstreams"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/ssemetrics"
)

// TestWireWebEventStreamMetrics: the production wiring (wireHubCoreMetrics,
// then wireWebEventStreamMetrics as initWebServer calls it) makes the web
// server's events stream registry report scion.hub.sse.* (phase 0 review of
// ptone/scion#5628). Without the second call the registry reports nothing.
func TestWireWebEventStreamMetrics(t *testing.T) {
	collect := func(t *testing.T, reader *sdkmetric.ManualReader) map[string]bool {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collecting metrics: %v", err)
		}
		out := map[string]bool{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				out[m.Name] = true
			}
		}
		return out
	}
	run := func(t *testing.T, wire bool) map[string]bool {
		t.Helper()
		srv, err := hub.New(hub.ServerConfig{}, newTestStore(t))
		if err != nil {
			t.Fatalf("hub.New: %v", err)
		}
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
		wireHubCoreMetrics(srv, mp)

		webSrv := hub.NewWebServer(hub.WebServerConfig{Host: "127.0.0.1"})
		if wire {
			wireWebEventStreamMetrics(webSrv, srv)
		}
		s := webSrv.EventStreams().Register(eventstreams.Key{UserID: "u1", ConnID: "c1"}, 1)
		s.Close(eventstreams.CloseClient, nil)
		return collect(t, reader)
	}

	got := run(t, true)
	for _, name := range []string{ssemetrics.MetricConnects, ssemetrics.MetricCloses, ssemetrics.MetricStreamDuration} {
		if !got[name] {
			t.Errorf("%s not exported after wireWebEventStreamMetrics; got %v", name, got)
		}
	}
	if got := run(t, false); got[ssemetrics.MetricConnects] {
		t.Errorf("%s exported without wireWebEventStreamMetrics: the test would not catch a missing call", ssemetrics.MetricConnects)
	}
}
