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
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

const usageCapabilitiesProbeTimeout = 5 * time.Second

// probeAndConfigureUsageIngest probes GET /api/v1/agents/{id}/usage/capabilities
// at sciontool init. When the Hub reports usage_ingest_enabled=true, it wires
// the Hub usage sink onto pipeline (enabling the analytics outbox even without
// a GCP exporter) and sets SCION_USAGE_HUB_INGEST=1 so hook subprocesses also
// record to the outbox. On 404, disabled, or error, it leaves legacy behavior
// unchanged and sets SCION_USAGE_HUB_INGEST=0.
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

	probeCtx, cancel := context.WithTimeout(ctx, usageCapabilitiesProbeTimeout)
	caps, err := c.ProbeUsageCapabilities(probeCtx)
	cancel()
	if err != nil {
		if errors.Is(err, hub.ErrUsageIngestUnsupported) {
			log.Debug("Hub usage ingest capabilities not supported (404); keeping legacy telemetry behavior")
		} else {
			log.Debug("Hub usage capabilities probe failed (%v); keeping legacy telemetry behavior", err)
		}
		_ = os.Setenv(telemetry.EnvUsageHubIngest, "0")
		return false
	}
	if caps == nil || !caps.UsageIngestEnabled {
		log.Debug("Hub usage ingest disabled by capabilities probe; keeping legacy telemetry behavior")
		_ = os.Setenv(telemetry.EnvUsageHubIngest, "0")
		return false
	}

	runID := caps.RunID
	if runID == "" {
		runID = os.Getenv(hub.EnvLaunchID)
	}

	var drainInterval time.Duration
	if caps.DrainIntervalSec > 0 {
		drainInterval = time.Duration(caps.DrainIntervalSec) * time.Second
	}
	var heartbeatInterval time.Duration
	if caps.HeartbeatIntervalSec > 0 {
		heartbeatInterval = time.Duration(caps.HeartbeatIntervalSec) * time.Second
	}

	pipeline.SetHubUsageSink(telemetry.HubUsageSinkConfig{
		Post: func(postCtx context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			client := newClient()
			if client == nil || !client.IsConfigured() {
				return nil, errors.New("hub client not configured")
			}
			resp, postErr := client.PostUsageEvents(postCtx, req)
			if postErr != nil {
				var httpErr *hub.UsageIngestHTTPError
				if errors.As(postErr, &httpErr) && httpErr.IsPermanent() {
					return nil, telemetry.PermanentSendError(postErr)
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
	})
	_ = os.Setenv(telemetry.EnvUsageHubIngest, "1")
	log.Info("Hub usage ingest sink enabled (run_id=%s, max_events=%d, max_bytes=%d)", runID, caps.MaxBatchEvents, caps.MaxBatchBytes)
	return true
}
