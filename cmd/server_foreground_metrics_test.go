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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestWireHubCoreMetrics_LaunchReaperTicksExported verifies the production
// metrics wiring (wireHubCoreMetrics, called from runServerStart's
// OTel-metrics block) makes the launch reaper's ticks observable. Deleting
// the SetReaperMetrics call inside wireHubCoreMetrics makes this test fail,
// since the reaper's own dedicated ticker (started by
// StartBackgroundServices) then has no recorder to report through.
func TestWireHubCoreMetrics_LaunchReaperTicksExported(t *testing.T) {
	ctx := context.Background()
	inner := newTestStore(t)

	srv, err := hub.New(hub.ServerConfig{}, inner)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	wireHubCoreMetrics(srv, mp)

	// StartBackgroundServices starts the scheduler, which registers and
	// starts the launch reaper's own goroutine; that goroutine's first
	// ("tick-zero") run happens asynchronously, so poll for it rather than
	// assuming it has completed once StartBackgroundServices returns.
	srv.StartBackgroundServices(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Fatalf("collecting metrics: %v", err)
		}
		if metricExported(&rm, "scion.launch_reaper.ticks") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for scion.launch_reaper.ticks to be exported after StartBackgroundServices; is SetReaperMetrics still wired in wireHubCoreMetrics?")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func metricExported(rm *metricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}
