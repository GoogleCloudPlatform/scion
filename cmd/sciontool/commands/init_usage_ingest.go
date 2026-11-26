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

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

const usageCapabilitiesProbeTimeout = 2 * time.Second

// Bounded init-time retry settings for transient capability probe failures
// (e.g. Hub restarting or returning 503). Vars rather than consts so unit
// tests can run without multi-second sleeps.
var (
	usageCapabilitiesProbeMaxAttempts = 3
	usageCapabilitiesProbeBaseBackoff = 500 * time.Millisecond
)

func buildHubUsageSinkConfig(caps *telemetrycontract.UsageCapabilitiesResponse, newClient func() *hub.Client) telemetry.HubUsageSinkConfig {
	// Prefer the container's own run ID (SCION_LAUNCH_ID, which matches the
	// runtime's scion.run_id label and X-Scion-Run-Id header) so native
	// appenders, hook subprocesses, and heartbeats all share one run identity
	// and never oscillate the outbox epoch if the probe sees a different run.
	envRunID := strings.TrimSpace(os.Getenv(hub.EnvLaunchID))
	probeRunID := strings.TrimSpace(caps.RunID)
	runID := envRunID
	if runID == "" {
		runID = probeRunID
	} else if probeRunID != "" && probeRunID != envRunID {
		log.Warn("Hub usage capabilities probe returned run_id=%q differing from %s=%q; using %s", probeRunID, hub.EnvLaunchID, envRunID, hub.EnvLaunchID)
	}
	var drainInterval time.Duration
	if caps.DrainIntervalSec > 0 {
		drainInterval = time.Duration(caps.DrainIntervalSec) * time.Second
	}
	var heartbeatInterval time.Duration
	if caps.HeartbeatIntervalSec > 0 {
		heartbeatInterval = time.Duration(caps.HeartbeatIntervalSec) * time.Second
	}
	return telemetry.HubUsageSinkConfig{
		Post: func(postCtx context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			client := newClient()
			if client == nil || !client.IsConfigured() {
				return nil, errors.New("hub client not configured")
			}
			resp, postErr := client.PostUsageEvents(postCtx, req)
			if postErr != nil {
				var httpErr *hub.UsageIngestHTTPError
				if errors.As(postErr, &httpErr) {
					if httpErr.IsNotFound() {
						return nil, fmt.Errorf("%w: %v", telemetry.ErrUsageIngestRevoked, postErr)
					}
					if httpErr.IsPayloadTooLarge() {
						return nil, fmt.Errorf("%w: %v", telemetry.ErrPayloadTooLarge, postErr)
					}
					if httpErr.IsPermanent() {
						return nil, telemetry.PermanentSendError(postErr)
					}
				}
				return nil, postErr
			}
			return resp, nil
		},
		RunID:             runID,
		HubID:             caps.HubID,
		MaxBatchEvents:    caps.MaxBatchEvents,
		MaxBatchBytes:     caps.MaxBatchBytes,
		DrainInterval:     drainInterval,
		HeartbeatInterval: heartbeatInterval,
	}
}

// probeUsageOnce performs a single capability probe attempt against the Hub.
// Returns (cfg, false, nil) on definitive negative (404 or disabled),
// (nil, true, err) on transient failure, or (cfg, false, nil) when enabled.
func probeUsageOnce(ctx context.Context, newClient func() *hub.Client) (cfg *telemetry.HubUsageSinkConfig, transient bool, err error) {
	c := newClient()
	if c == nil || !c.IsConfigured() {
		return nil, false, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, usageCapabilitiesProbeTimeout)
	caps, err := c.ProbeUsageCapabilities(probeCtx)
	cancel()
	if err != nil {
		if errors.Is(err, hub.ErrUsageIngestUnsupported) {
			log.Debug("Hub usage ingest capabilities not supported (404); keeping legacy telemetry behavior")
			return nil, false, nil
		}
		log.Debug("Hub usage capabilities probe failed transiently (%v)", err)
		return nil, true, err
	}
	if caps == nil || !caps.UsageIngestEnabled {
		log.Debug("Hub usage ingest disabled by capabilities probe; keeping legacy telemetry behavior")
		return nil, false, nil
	}
	built := buildHubUsageSinkConfig(caps, newClient)
	return &built, false, nil
}

// probeAndConfigureUsageIngest probes GET /api/v1/agents/{id}/usage/capabilities
// at sciontool init with bounded retry and exponential backoff on transient
// errors (e.g. 503 while Hub restarts). When the Hub reports
// usage_ingest_enabled=true, it wires the Hub usage sink onto pipeline
// (enabling the analytics outbox even without a GCP exporter) and sets
// SCION_USAGE_HUB_INGEST=1 so hook subprocesses also record to the outbox.
// When the init probe ends negative (404, disabled, or transient retries
// exhausted), it leaves legacy behavior unchanged, sets
// SCION_USAGE_HUB_INGEST=0, and registers a periodic re-probe on pipeline so
// the Hub sink activates automatically if capabilities turn positive later.
func probeAndConfigureUsageIngest(ctx context.Context, pipeline *telemetry.Pipeline, newClient func() *hub.Client) bool {
	if pipeline == nil || newClient == nil {
		_ = os.Setenv(telemetry.EnvUsageHubIngest, "0")
		return false
	}
	c := newClient()
	if c == nil || !c.IsConfigured() {
		_ = os.Setenv(telemetry.EnvUsageHubIngest, "0")
		return false
	}

	attempts := usageCapabilitiesProbeMaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	backoff := usageCapabilitiesProbeBaseBackoff
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				_ = os.Setenv(telemetry.EnvUsageHubIngest, "0")
				return false
			case <-timer.C:
			}
			backoff *= 2
		}
		cfg, transient, _ := probeUsageOnce(ctx, newClient)
		if cfg != nil {
			pipeline.SetHubUsageSink(*cfg)
			_ = os.Setenv(telemetry.EnvUsageHubIngest, "1")
			log.Info("Hub usage ingest sink enabled (run_id=%s, max_events=%d, max_bytes=%d)", cfg.RunID, cfg.MaxBatchEvents, cfg.MaxBatchBytes)
			return true
		}
		if !transient {
			break
		}
	}

	// Probe ended negative: keep legacy behavior and register periodic re-probe
	// for dual-run mode (when a GCP exporter is active). Hub-only mode does not
	// late-activate after a negative init probe so the Hub reads unmeasured
	// rather than receiving empty heartbeats for unwired producers.
	_ = os.Setenv(telemetry.EnvUsageHubIngest, "0")
	pipeline.SetHubUsageProbe(func(pctx context.Context) (*telemetry.HubUsageSinkConfig, error) {
		cfg, _, err := probeUsageOnce(pctx, newClient)
		return cfg, err
	})
	return false
}

// startTelemetryWithUsageIngest probes Hub usage capabilities, wires the Hub
// usage sink onto pipeline when active, and starts the pipeline. On success,
// it exports SCION_NATIVE_METERING_SUPPORTED=1 so harness provisioners know
// this sciontool understands SCION_NATIVE_TELEMETRY_POLICY=metering (older
// P1-era sciontool binaries omit the marker, so provisioners stay on
// enabled/disabled). If Start fails (e.g. loopback receiver port bind error),
// it resets both SCION_USAGE_HUB_INGEST=0 and SCION_NATIVE_METERING_SUPPORTED=0
// so harness provisioners do not point native OTel exporters at a dead loopback
// receiver and hook subprocesses do not write to an undrained outbox.
func startTelemetryWithUsageIngest(ctx context.Context, pipeline *telemetry.Pipeline, newClient func() *hub.Client) error {
	probeAndConfigureUsageIngest(ctx, pipeline, newClient)
	if err := pipeline.Start(ctx); err != nil {
		_ = os.Setenv(telemetry.EnvUsageHubIngest, "0")
		_ = os.Setenv(telemetry.EnvNativeMeteringSupported, "0")
		return err
	}
	_ = os.Setenv(telemetry.EnvNativeMeteringSupported, "1")
	return nil
}
