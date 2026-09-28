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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// HubSettingSectionUpdateAvailable is the hub_settings section key used to
// store the latest detected release update for the admin UI.
const HubSettingSectionUpdateAvailable = "system.update_available"

// UpdateAvailableInfo is the JSON payload stored in the
// system.update_available hub setting.
type UpdateAvailableInfo struct {
	Version     string `json:"version"`
	Channel     string `json:"channel"`
	DownloadURL string `json:"download_url,omitempty"`
	ReleaseURL  string `json:"release_url,omitempty"`
	DetectedAt  string `json:"detected_at"`
}

// releaseUpdateCheckHandler returns a handler function for the
// "release-update-check" recurring singleton. The handler checks for a new
// binary release and either stores a notification (notify policy) or triggers
// the BinaryUpdateExecutor (auto policy).
func (s *Server) releaseUpdateCheckHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		log := logging.Subsystem("hub.maintenance.release-update-check")

		mc := s.config.MaintenanceConfig

		// Determine channel: explicit config > detect from version string.
		channel := mc.ReleaseChannel
		if channel == "" {
			channel = update.DetectChannel(version.Version)
		}
		if channel == "" {
			log.Debug("No release channel detected (dev build), skipping scheduled update check",
				"version", version.Version)
			return
		}

		repo := mc.GitHubRepo
		if repo == "" {
			repo = "GoogleCloudPlatform/scion"
		}

		log.Debug("Running scheduled release update check",
			"current_version", version.Version,
			"channel", channel,
			"repo", repo,
			"policy", mc.UpdatePolicy)

		result, err := CheckForReleaseUpdates(ctx, version.Version, channel, repo)
		if err != nil {
			// Log and swallow — a transient GitHub API failure should not crash
			// the scheduler. The next scheduled run will retry.
			log.Error("Scheduled release update check failed", "error", err)
			return
		}

		if !result.UpdateAvailable {
			log.Debug("No update available",
				"current", result.CurrentVersion,
				"latest", result.LatestVersion,
				"channel", result.Channel)
			return
		}

		// Defense in depth: verify the detected version is actually newer, not equal.
		if result.LatestVersion == result.CurrentVersion {
			log.Debug("Latest version equals current version, skipping",
				"version", result.CurrentVersion)
			return
		}

		log.Info("Update available",
			"current", result.CurrentVersion,
			"latest", result.LatestVersion,
			"channel", result.Channel)

		switch mc.UpdatePolicy {
		case "notify":
			s.storeUpdateAvailable(ctx, result)
		case "auto":
			s.autoApplyUpdate(ctx, result)
		default:
			log.Warn("Unknown update policy, treating as notify", "policy", mc.UpdatePolicy)
			s.storeUpdateAvailable(ctx, result)
		}
	}
}

// storeUpdateAvailable writes the update metadata to the system.update_available
// hub setting so the admin UI can display a banner.
func (s *Server) storeUpdateAvailable(ctx context.Context, result *ReleaseUpdateCheckResult) {
	log := logging.Subsystem("hub.maintenance.release-update-check")

	info := UpdateAvailableInfo{
		Version:     result.LatestVersion,
		Channel:     result.Channel,
		DownloadURL: result.DownloadURL,
		ReleaseURL:  result.ReleaseURL,
		DetectedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	value, err := json.Marshal(info)
	if err != nil {
		log.Error("Failed to marshal update info", "error", err)
		return
	}

	// Use expectedRevision=-1 for unconditional upsert.
	_, err = s.store.UpsertHubSetting(ctx, HubSettingSectionUpdateAvailable,
		json.RawMessage(value), "system", -1, "managed")
	if err != nil {
		log.Error("Failed to store update_available setting", "error", err)
		return
	}

	log.Info("Stored update notification",
		"version", result.LatestVersion,
		"channel", result.Channel)
}

// autoApplyUpdate creates a maintenance operation run for "update-binary" and
// executes the BinaryUpdateExecutor. This follows the same pattern as manually
// triggering an operation run via the API.
func (s *Server) autoApplyUpdate(ctx context.Context, result *ReleaseUpdateCheckResult) {
	log := logging.Subsystem("hub.maintenance.release-update-check")

	log.Info("Auto-applying update",
		"version", result.LatestVersion,
		"download_url", result.DownloadURL)

	// Check for an already-running update operation.
	recentRuns, err := s.store.ListMaintenanceRuns(ctx, "update-binary", 1)
	if err != nil {
		log.Error("Failed to check for running update operations", "error", err)
		return
	}
	if len(recentRuns) > 0 && recentRuns[0].Status == store.MaintenanceStatusRunning {
		log.Info("Update operation already running, skipping auto-apply",
			"run_id", recentRuns[0].ID)
		return
	}

	// Resolve the executor.
	executor, err := s.resolveMaintenanceExecutor("update-binary")
	if err != nil {
		log.Error("Failed to resolve update-binary executor", "error", err)
		return
	}

	// Create a run record.
	runID := api.NewUUID()
	now := time.Now()
	run := &store.MaintenanceOperationRun{
		ID:           runID,
		OperationKey: "update-binary",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    now,
		StartedBy:    "system:auto-update",
	}
	if err := s.store.CreateMaintenanceRun(ctx, run); err != nil {
		log.Error("Failed to create run record", "error", err)
		return
	}

	log.Info("Auto-update operation started",
		"run_id", runID,
		"target_version", result.LatestVersion)

	// Execute asynchronously (same pattern as executeOperation in admin_maintenance.go).
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Auto-update executor panicked",
					"run_id", runID, "panic", fmt.Sprint(r))
				finishedAt := time.Now()
				run.CompletedAt = &finishedAt
				run.Status = store.MaintenanceStatusFailed
				panicResult := map[string]interface{}{
					"error": fmt.Sprintf("executor panic: %v", r),
				}
				resultJSON, _ := json.Marshal(panicResult)
				run.Result = string(resultJSON)
				if err := s.store.UpdateMaintenanceRun(context.Background(), run); err != nil {
					log.Error("Failed to update run record after panic",
						"run_id", runID, "error", err)
				}
			}
		}()

		params := map[string]string{
			"target_version": result.LatestVersion,
			"download_url":   result.DownloadURL,
		}

		var buf bytes.Buffer
		execErr := executor.Run(context.Background(), &buf, params)

		finishedAt := time.Now()
		run.CompletedAt = &finishedAt
		run.Log = buf.String()

		if execErr != nil {
			run.Status = store.MaintenanceStatusFailed
			failResult := map[string]interface{}{
				"error": execErr.Error(),
			}
			resultJSON, _ := json.Marshal(failResult)
			run.Result = string(resultJSON)
			log.Error("Auto-update operation failed",
				"run_id", runID, "error", execErr,
				"duration", finishedAt.Sub(run.StartedAt))
		} else {
			run.Status = store.MaintenanceStatusCompleted

			// Clear the update_available notification on success.
			if delErr := s.store.DeleteHubSetting(context.Background(),
				HubSettingSectionUpdateAvailable); delErr != nil && delErr != store.ErrNotFound {
				log.Warn("Failed to clear update_available setting after successful update",
					"error", delErr)
			}

			log.Info("Auto-update operation completed",
				"run_id", runID,
				"duration", finishedAt.Sub(run.StartedAt))
		}

		if err := s.store.UpdateMaintenanceRun(context.Background(), run); err != nil {
			log.Error("Failed to update run record",
				"run_id", runID, "error", err)
		}
	}()
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
