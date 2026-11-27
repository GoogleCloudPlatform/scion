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
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/trajectory"
	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
)

// startTrajectoryRunner starts the Track 2 raw trajectory shipper runner in
// sciontool init when a Hub client is configured, a run ID is available, and
// the active harness has a built-in shipper spec (currently "claude").
//
// The runner is self-gating against the Hub: when the
// tokenomics.trajectory_capture experiment is off, segments:prepare returns
// 404 and the runner backs off cleanly without uploading or corrupting state.
func startTrajectoryRunner(
	ctx context.Context,
	agentHome string,
	targetUID, targetGID int,
	newClient func() *hub.Client,
) *trajectory.Runner {
	if agentHome == "" || newClient == nil {
		return nil
	}
	client := newClient()
	if client == nil || !client.IsConfigured() {
		return nil
	}

	harnessName := strings.TrimSpace(os.Getenv("SCION_HARNESS"))
	if harnessName == "" {
		log.Debug("Trajectory shipper disabled: SCION_HARNESS is unset")
		return nil
	}
	if _, ok := shipper.HarnessSpecFor(harnessName); !ok {
		log.Debug("Trajectory shipper disabled: unsupported harness %q", harnessName)
		return nil
	}

	runID := trajectory.ResolveRunID(client)
	if runID == "" {
		log.Debug("Trajectory shipper disabled: no run_id available from env or agent token")
		return nil
	}

	var periodicInterval time.Duration
	if raw := strings.TrimSpace(os.Getenv(trajectory.EnvPeriodicInterval)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			periodicInterval = d
		} else {
			log.Warn("Ignoring invalid %s=%q", trajectory.EnvPeriodicInterval, raw)
		}
	}

	runner, err := trajectory.NewRunner(trajectory.Config{
		HomeDir:          agentHome,
		RunID:            runID,
		Harness:          harnessName,
		Uploader:         hub.NewTrajectoryUploaderWithProvider(newClient),
		PeriodicInterval: periodicInterval,
		OwnerUID:         targetUID,
		OwnerGID:         targetGID,
	})
	if err != nil {
		log.Warn("Failed to initialize trajectory shipper: %v", err)
		return nil
	}
	if err := runner.Start(ctx); err != nil {
		log.Warn("Failed to start trajectory shipper: %v", err)
		return nil
	}
	log.Info("Trajectory shipper started (harness=%s, run_id=%s)", harnessName, runID)
	return runner
}
