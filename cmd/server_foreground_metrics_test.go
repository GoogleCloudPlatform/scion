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
	"bytes"
	"context"
	"log"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestWireHubCoreMetrics_LaunchReaperTicksExported verifies the production
// metrics wiring (wireHubCoreMetrics, called from runServerStart's
// OTel-metrics block) makes the launch reaper's ticks observable, and that
// tearing down the test leaves no background Hub or scheduler jobs running
// after it returns. Deleting the SetReaperMetrics call inside
// wireHubCoreMetrics makes this test fail, since the reaper's own dedicated
// ticker (started by StartBackgroundServices) then has no recorder to report
// through.
func TestWireHubCoreMetrics_LaunchReaperTicksExported(t *testing.T) {
	beforeGoroutines := snapshotGoroutineIDs()

	t.Run("export", func(t *testing.T) {
		ctx := context.Background()
		servicesCtx, cancelServices := context.WithCancel(ctx)
		t.Cleanup(cancelServices)
		inner := newTestStore(t)

		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

		srv, err := hub.New(hub.ServerConfig{}, inner)
		if err != nil {
			t.Fatalf("hub.New: %v", err)
		}
		// Registered after newTestStore and MeterProvider so LIFO cleanup
		// cancels background services and waits for Shutdown before closing
		// the meter provider or SQLite database.
		t.Cleanup(func() {
			cancelServices()
			_ = srv.Shutdown(context.Background())
		})

		wireHubCoreMetrics(srv, mp)

		// StartBackgroundServices starts the scheduler, which registers and
		// starts the launch reaper's own goroutine; that goroutine's first
		// ("tick-zero") run happens asynchronously, so poll for it rather than
		// assuming it has completed once StartBackgroundServices returns.
		srv.StartBackgroundServices(servicesCtx)

		deadline := time.Now().Add(5 * time.Second)
		for {
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(ctx, &rm); err != nil {
				t.Fatalf("collecting metrics: %v", err)
			}
			if metricExported(&rm, "scion.launch_reaper.ticks") {
				assertNoDecisionWriterInstruments(t, &rm)
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for scion.launch_reaper.ticks to be exported after StartBackgroundServices; is SetReaperMetrics still wired in wireHubCoreMetrics?")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	var logBuf bytes.Buffer
	savedOut, savedFlags := log.Writer(), log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(savedOut)
		log.SetFlags(savedFlags)
	})

	// Scheduler jobs must already be stopped and joined before the subtest's
	// database closed, with zero polling needed.
	if stacks := leakedHubGoroutines(beforeGoroutines, "pkg/hub.(*Scheduler)"); len(stacks) > 0 {
		t.Fatalf("scheduler goroutines still running after test teardown:\n%s", strings.Join(stacks, "\n\n"))
	}

	// Context-driven background loops started by StartBackgroundServices wake
	// on context cancellation and exit promptly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		stacks := leakedHubGoroutines(beforeGoroutines, "github.com/GoogleCloudPlatform/scion/pkg/hub")
		if len(stacks) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background hub goroutines still running after test teardown:\n%s", strings.Join(stacks, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := logBuf.String(); got != "" {
		t.Fatalf("unexpected global log output after test teardown:\n%s", got)
	}
}

func snapshotGoroutineIDs() map[string]struct{} {
	ids := make(map[string]struct{})
	for _, block := range allGoroutineStacks() {
		if id := goroutineID(block); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func leakedHubGoroutines(before map[string]struct{}, substr string) []string {
	var leaked []string
	for _, block := range allGoroutineStacks() {
		id := goroutineID(block)
		if _, existed := before[id]; existed {
			continue
		}
		if strings.Contains(block, substr) {
			leaked = append(leaked, block)
		}
	}
	return leaked
}

func allGoroutineStacks() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Split(strings.TrimSpace(string(buf[:n])), "\n\n")
		}
		buf = make([]byte, len(buf)*2)
	}
}

func goroutineID(block string) string {
	line, _, _ := strings.Cut(block, "\n")
	rest, ok := strings.CutPrefix(line, "goroutine ")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, " ")
	return id
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

// The retained reaper metric proves collection is active while retired instruments stay absent.
func assertNoDecisionWriterInstruments(t *testing.T, rm *metricdata.ResourceMetrics) {
	t.Helper()
	for _, name := range []string{"scion.hub.decision_audit.queue_depth", "scion.hub.decision_audit.write.duration", "scion.hub.decision_audit.dropped"} {
		if metricExported(rm, name) {
			t.Fatalf("retired writer instrument exported: %s", name)
		}
	}
}
