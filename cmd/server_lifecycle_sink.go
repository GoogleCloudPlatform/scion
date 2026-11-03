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

package cmd

import (
	"log"
	"os"
	"strings"

	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// lifecycleCloudHandler is the server's ResilientCloudHandler, recorded by
// initServerLogging when server Cloud Logging is enabled, so the lifecycle
// sink shares its client and circuit breaker (WP1 design 5.3).
var lifecycleCloudHandler *logging.ResilientCloudHandler

// lifecycleMeterProvider is the hub's MeterProvider when metrics export is
// enabled, for the lifecycle counters and gauges.
var lifecycleMeterProvider metric.MeterProvider

// newLifecycleCloudSink builds the Cloud Logging lifecycle sink; a test seam,
// replaced only by tests (with a t.Cleanup restore).
var newLifecycleCloudSink = telemetryemit.NewCloudLoggingSink

// lifecycleSinkEnvironment gathers the sink selection inputs (design
// section 22 item 4).
func lifecycleSinkEnvironment(cfg *config.GlobalConfig, devAuth bool) lifecycleevents.SinkEnvironment {
	loopback := config.IsLoopbackHost(cfg.Hub.Host)
	if enableRuntimeBroker && !config.IsLoopbackHost(cfg.RuntimeBroker.Host) {
		loopback = false
	}
	return lifecycleevents.SinkEnvironment{
		CloudLogging:    lifecycleCloudHandler != nil,
		LoopbackOnly:    loopback,
		SQLite:          strings.EqualFold(cfg.Database.Driver, "sqlite"),
		DevAuth:         devAuth,
		CloudRunService: cloudrunenv.IsService(),
		Kubernetes:      os.Getenv("KUBERNETES_SERVICE_HOST") != "",
	}
}

// wireLifecycleEventsSink gives the hub its lifecycle sink and counters
// (design section 22 item 4). It must run before StartBackgroundServices,
// which installs the observer when hub.lifecycle_events is on. With no
// sink the observer refuses (disabled_no_sink); the choice is exported as
// lifecycle_sink_kind.
func wireLifecycleEventsSink(hubSrv *hub.Server, cfg *config.GlobalConfig, devAuth bool) {
	if hubSrv == nil {
		return
	}
	var counters telemetryemit.LifecycleCounters = telemetryemit.NopCounters{}
	if lifecycleMeterProvider != nil {
		c, err := telemetryemit.NewOTelCounters(lifecycleMeterProvider, "github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents")
		if err != nil {
			log.Printf("WARNING: lifecycle event metrics disabled: %v", err)
		} else {
			counters = c
		}
	}
	var sink telemetryemit.Sink
	switch lifecycleevents.SelectSinkKind(lifecycleSinkEnvironment(cfg, devAuth)) {
	case telemetryemit.SinkKindCloudLogging:
		sink = newLifecycleCloudSink(lifecycleCloudHandler.Client(), lifecycleCloudHandler.CircuitOpen)
	case telemetryemit.SinkKindStdout:
		sink = telemetryemit.NewStdoutSink(os.Stdout)
	}
	hubSrv.SetLifecycleEventsSink(sink, counters)
}
