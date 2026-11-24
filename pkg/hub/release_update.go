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

package hub

import (
	"context"
	"errors"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// UpdateAvailableInfo is the JSON payload stored in the
// system.update_available hub setting.
type UpdateAvailableInfo struct {
	Version    string `json:"version"`
	Channel    string `json:"channel"`
	ReleaseURL string `json:"release_url,omitempty"`
	DetectedAt string `json:"detected_at"`
}

// autoUpdateStartedBy is the started_by of update-binary runs created by
// the auto update policy.
const autoUpdateStartedBy = "system:auto-update"

// shouldRegisterReleaseUpdateCheck reports whether the scheduled release
// update check runs: whenever the update policy is not disabled, whatever
// the deployment tier (the handler skips hubs whose effective mode is git,
// and never auto-updates a pinned hub).
func shouldRegisterReleaseUpdateCheck(mc MaintenanceConfig) bool {
	return mc.UpdatePolicy != "disabled"
}

// releaseCheckDeps are the inputs of a scheduled release update check that
// tests replace.
type releaseCheckDeps struct {
	// running and buildTime identify the running binary.
	running   string
	buildTime time.Time
	// check runs the release check (maintenance.CheckForReleaseUpdates).
	check func(context.Context, maintenance.ReleaseCheckOptions) (*maintenance.ReleaseUpdateCheckResult, error)
}

// releaseUpdateCheckHandler returns the handler of the
// "release-update-check" recurring singleton.
func (s *Server) releaseUpdateCheckHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		deps := releaseCheckDeps{
			running: version.Version,
			check:   maintenance.CheckForReleaseUpdates,
		}
		if t, err := time.Parse(time.RFC3339, version.BuildTime); err == nil {
			deps.buildTime = t
		}
		s.runReleaseUpdateCheck(ctx, deps)
	}
}

// runReleaseUpdateCheck checks the effective channel (release selection,
// else configuration, else the running version's) for a newer release in
// the release catalog and stores any available update in
// system.update_available for the admin UI banner. Hubs whose effective
// mode is git are skipped. Automatic installation is handled separately by
// the release-auto-update job.
func (s *Server) runReleaseUpdateCheck(ctx context.Context, deps releaseCheckDeps) {
	log := logging.Subsystem("hub.maintenance.release-update-check")
	mc := s.config.MaintenanceConfig

	sel, err := s.readReleaseSelection(ctx)
	if err != nil {
		log.Error("Failed to read the release selection, skipping scheduled update check", "error", err)
		return
	}
	if mode := effectiveUpdateMode(mc, sel); mode != UpdateModeRelease {
		log.Debug("Effective update mode is not release, skipping scheduled update check", "mode", mode)
		return
	}
	channel, source := effectiveReleaseChannel(mc, deps.running, sel)
	pin := effectivePin(sel)
	if channel == "" {
		log.Debug("No release channel (git build without release_channel), skipping scheduled update check",
			"version", deps.running)
		return
	}

	log.Debug("Running scheduled release update check",
		"current_version", deps.running, "channel", channel, "channel_source", source,
		"pin", pin, "repo", mc.GitHubRepo, "policy", mc.UpdatePolicy)

	result, err := deps.check(ctx, maintenance.ReleaseCheckOptions{
		CurrentVersion:   deps.running,
		CurrentBuildTime: deps.buildTime,
		Channel:          channel,
		Repo:             mc.GitHubRepo,
		Catalog:          s.updateReleaseCatalog(),
		Releases:         s.updateReleaseSource(),
	})
	if err != nil {
		// A transient GitHub failure must not crash the scheduler; the next
		// scheduled run retries.
		log.Error("Scheduled release update check failed", "error", err)
		return
	}
	if !result.UpdateAvailable {
		log.Debug("No update available",
			"current", result.CurrentVersion, "latest", result.LatestVersion,
			"channel", result.Channel, "class", result.Class)
		return
	}

	log.Info("Update available",
		"current", result.CurrentVersion, "latest", result.LatestVersion,
		"channel", result.Channel, "class", result.Class)

	s.storeUpdateAvailable(ctx, result)
}

// storeUpdateAvailable writes the update metadata to the system.update_available
// hub setting so the admin UI can display a banner.
func (s *Server) storeUpdateAvailable(ctx context.Context, result *maintenance.ReleaseUpdateCheckResult) {
	log := logging.Subsystem("hub.maintenance.release-update-check")

	info := UpdateAvailableInfo{
		Version:    result.LatestVersion,
		Channel:    result.Channel,
		ReleaseURL: result.ReleaseURL,
		DetectedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.writeHubSettingJSON(ctx, HubSettingSectionUpdateAvailable, info); err != nil {
		log.Error("Failed to store update_available setting", "error", err)
		return
	}
	log.Info("Stored update notification",
		"version", result.LatestVersion,
		"channel", result.Channel)
}

// autoApplyUpdate creates an update-binary run for an explicit release tag,
// started by system:auto-update, and runs executor on it with the same runner
// and parameter validation as a run requested through the API. It never passes
// confirm_downgrade. Returns the created run ID and outcome.
func (s *Server) autoApplyUpdate(ctx context.Context, channel, targetTag string, executor MaintenanceExecutor) (runID string, outcome string) {
	log := logging.Subsystem("hub.maintenance.release-auto-update")

	// Check for an already-running update or rebuild (one exclusive group).
	blocking, err := s.maintenanceGroupInFlight(ctx, "update-binary")
	if err != nil {
		log.Error("Failed to check for running update operations", "error", err)
		return "", AutoUpdateOutcomeSkippedError
	}
	if blocking != nil {
		log.Info("Update or rebuild already in flight, skipping auto update",
			"run_id", blocking.ID, "operation_key", blocking.OperationKey, "status", blocking.Status)
		return "", AutoUpdateOutcomeSkippedInFlight
	}

	params := map[string]string{
		maintenance.ParamTargetVersion: targetTag,
	}
	if channel != "" {
		params[maintenance.ParamChannel] = channel
	}
	if v, ok := executor.(maintenance.ParamValidator); ok {
		if err := v.ValidateParams(params); err != nil {
			log.Error("Auto update refused", "target_version", targetTag, "channel", channel, "error", err)
			return "", AutoUpdateOutcomeSkippedRefused
		}
	}

	run := &store.MaintenanceOperationRun{
		ID:           api.NewUUID(),
		OperationKey: "update-binary",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    time.Now(),
		StartedBy:    autoUpdateStartedBy,
	}
	if blocking, err := s.createExclusiveMaintenanceRun(ctx, run); errors.Is(err, errHubRestartPending) {
		log.Info("Hub restart in progress, skipping auto update")
		return "", AutoUpdateOutcomeSkippedInFlight
	} else if err != nil {
		log.Error("Failed to create run record", "error", err)
		return "", AutoUpdateOutcomeSkippedError
	} else if blocking != nil {
		log.Info("Update or rebuild started meanwhile, skipping auto-apply",
			"run_id", blocking.ID, "operation_key", blocking.OperationKey)
		return "", AutoUpdateOutcomeSkippedInFlight
	}
	log.Info("Auto update started", "run_id", run.ID, "target_version", targetTag, "channel", channel)

	// Same runner as executeOperation: the run is recorded as "restarting",
	// with the pending update, before the hub restarts. update_available
	// stays until boot reconciliation verifies the new version.
	s.launchMaintenanceRun(run, executor, params)
	return run.ID, AutoUpdateOutcomeStarted
}

// ClearUpdateAvailable removes the system.update_available hub setting.
// It is called after a successful binary update to prevent showing a stale
// notification. It is safe to call even if the setting does not exist.
func (s *Server) ClearUpdateAvailable(ctx context.Context) {
	if err := s.store.DeleteHubSetting(ctx, HubSettingSectionUpdateAvailable); err != nil && err != store.ErrNotFound {
		log := logging.Subsystem("hub.maintenance.release-update-check")
		log.Warn("Failed to clear update_available setting", "error", err)
	}
}
