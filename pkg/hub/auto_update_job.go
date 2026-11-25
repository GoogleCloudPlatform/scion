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
	"encoding/json"
	"errors"
	"hash/fnv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// HubSettingSectionAutoUpdateState is the hub_settings row that stores
// bookkeeping state for the release-auto-update scheduler job. Only the
// release-auto-update job writes this row.
const HubSettingSectionAutoUpdateState = "system.auto_update_state"

const (
	// releaseAutoUpdateIntervalMinutes is the tick interval for the
	// release-auto-update recurring singleton (≈10 minutes).
	releaseAutoUpdateIntervalMinutes = 10

	// autoUpdateWindowDuration is the length of the daily auto-update window
	// starting at WindowStartUTC:00 UTC.
	autoUpdateWindowDuration = 2 * time.Hour

	// autoUpdateMaxHubOffsetMinutes is the upper bound (exclusive) of the
	// stable per-hub offset into the daily window.
	autoUpdateMaxHubOffsetMinutes = 60
)

// AutoUpdateState.LastOutcome values.
const (
	AutoUpdateOutcomeStarted                = "started"
	AutoUpdateOutcomeVerified               = LastUpdateVerified
	AutoUpdateOutcomeFailed                 = LastUpdateFailed
	AutoUpdateOutcomeRolledBack             = LastUpdateRolledBack
	AutoUpdateOutcomeNoUpdate               = "no_update"
	AutoUpdateOutcomeSkippedMaintenanceMode = "skipped:maintenance_mode"
	AutoUpdateOutcomeSkippedCheckFailed     = "skipped:check_failed"
	AutoUpdateOutcomeSkippedSuppressed      = "skipped:suppressed"
	AutoUpdateOutcomeSkippedInFlight        = "skipped:in_flight"
	AutoUpdateOutcomeSkippedRefused         = "skipped:refused"
	AutoUpdateOutcomeSkippedError           = "skipped:error"
)

// AutoUpdateState is the system.auto_update_state hub_settings row.
type AutoUpdateState struct {
	LastCheckAt    string `json:"last_check_at,omitempty"`    // last tick that evaluated a check (RFC3339)
	LastAttemptDay string `json:"last_attempt_day,omitempty"` // UTC date "2026-11-24" of last window evaluation
	LastRunID      string `json:"last_run_id,omitempty"`      // update-binary run started by the job
	LastTarget     string `json:"last_target,omitempty"`      // tag the job targeted
	LastOutcome    string `json:"last_outcome,omitempty"`     // started|verified|failed|rolled_back|no_update|skipped:<reason>
	SuppressedTag  string `json:"suppressed_tag,omitempty"`   // tag not to retry automatically
}

// autoUpdateJobDeps are the inputs of a scheduled release auto-update tick
// that tests replace.
type autoUpdateJobDeps struct {
	now            func() time.Time
	running        string
	buildTime      time.Time
	check          func(context.Context, maintenance.ReleaseCheckOptions) (*maintenance.ReleaseUpdateCheckResult, error)
	updateExecutor func() (MaintenanceExecutor, error)
}

// autoUpdateWindowOffset returns a stable per-hub offset in [0, 60) minutes
// into the daily auto-update window so hubs spread their catalog checks.
func autoUpdateWindowOffset(hubID string) time.Duration {
	if hubID == "" {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(hubID))
	return time.Duration(h.Sum32()%autoUpdateMaxHubOffsetMinutes) * time.Minute
}

// normalizeAutoUpdateWindow clamps startHourUTC to [0, 23] (falling back to
// DefaultAutoUpdateWindowStartHourUTC) and offset to [0, autoUpdateWindowDuration).
func normalizeAutoUpdateWindow(startHourUTC int, offset time.Duration) (int, time.Duration) {
	if startHourUTC < 0 || startHourUTC > 23 {
		startHourUTC = DefaultAutoUpdateWindowStartHourUTC
	}
	if offset < 0 {
		offset = 0
	} else if offset >= autoUpdateWindowDuration {
		offset = autoUpdateWindowDuration - time.Minute
	}
	return startHourUTC, offset
}

// autoUpdateWindowFor reports whether now falls inside the daily auto-update
// window [startHourUTC:00 + offset, startHourUTC:00 + 2h) in UTC, and if so
// returns the window's start date ("YYYY-MM-DD") as attemptDay. Checking both
// today's and yesterday's window start handles windows that cross midnight
// (startHourUTC == 23) without permitting a second attempt after 00:00 UTC.
func autoUpdateWindowFor(now time.Time, startHourUTC int, offset time.Duration) (inWindow bool, attemptDay string) {
	startHourUTC, offset = normalizeAutoUpdateWindow(startHourUTC, offset)
	utc := now.UTC()
	y, m, d := utc.Date()
	todayStart := time.Date(y, m, d, startHourUTC, 0, 0, 0, time.UTC)
	for _, windowStart := range []time.Time{todayStart, todayStart.AddDate(0, 0, -1)} {
		effectiveStart := windowStart.Add(offset)
		windowEnd := windowStart.Add(autoUpdateWindowDuration)
		if !utc.Before(effectiveStart) && utc.Before(windowEnd) {
			return true, windowStart.UTC().Format("2006-01-02")
		}
	}
	return false, ""
}

// nextAutoUpdateRunAt returns the next UTC instant at which the daily
// auto-update job will evaluate an update check, using the same window and
// per-hub offset rules as autoUpdateWindowFor. When now already lies inside an
// unattempted window, it returns now truncated to seconds (imminent next tick).
func nextAutoUpdateRunAt(now time.Time, startHourUTC int, offset time.Duration, lastAttemptDay string) time.Time {
	startHourUTC, offset = normalizeAutoUpdateWindow(startHourUTC, offset)
	utc := now.UTC()
	y, m, d := utc.Date()
	todayStart := time.Date(y, m, d, startHourUTC, 0, 0, 0, time.UTC)
	for _, windowStart := range []time.Time{
		todayStart.AddDate(0, 0, -1),
		todayStart,
		todayStart.AddDate(0, 0, 1),
	} {
		day := windowStart.UTC().Format("2006-01-02")
		if day == lastAttemptDay {
			continue
		}
		effectiveStart := windowStart.Add(offset)
		windowEnd := windowStart.Add(autoUpdateWindowDuration)
		if !utc.Before(windowEnd) {
			continue
		}
		if utc.Before(effectiveStart) {
			return effectiveStart
		}
		return utc.Truncate(time.Second)
	}
	return todayStart.AddDate(0, 0, 1).Add(offset)
}

// setAutoUpdateNonRunOutcome records a non-run outcome (no_update or
// skipped:*) and clears LastRunID/LastTarget so the UI never pairs a skip
// outcome with an unrelated earlier run.
func setAutoUpdateNonRunOutcome(state *AutoUpdateState, outcome string) {
	state.LastRunID = ""
	state.LastTarget = ""
	state.LastOutcome = outcome
}

// readAutoUpdateState reads system.auto_update_state. Missing or unreadable
// rows return an empty state.
func (s *Server) readAutoUpdateState(ctx context.Context) (AutoUpdateState, error) {
	var state AutoUpdateState
	found, err := s.readHubSettingJSON(ctx, HubSettingSectionAutoUpdateState, &state)
	if err != nil {
		if found {
			s.maintenanceLog.Warn("Ignoring unreadable auto_update_state setting", "error", err)
			return AutoUpdateState{}, nil
		}
		return AutoUpdateState{}, err
	}
	return state, nil
}

// writeAutoUpdateState persists system.auto_update_state.
func (s *Server) writeAutoUpdateState(ctx context.Context, state AutoUpdateState) error {
	return s.writeHubSettingJSON(ctx, HubSettingSectionAutoUpdateState, state)
}

// releaseAutoUpdateHandler returns the handler of the "release-auto-update"
// recurring singleton.
func (s *Server) releaseAutoUpdateHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		deps := autoUpdateJobDeps{
			now:     time.Now,
			running: version.Version,
			check:   maintenance.CheckForReleaseUpdates,
			updateExecutor: func() (MaintenanceExecutor, error) {
				return s.resolveMaintenanceExecutor("update-binary")
			},
		}
		if t, err := time.Parse(time.RFC3339, version.BuildTime); err == nil {
			deps.buildTime = t
		}
		s.runReleaseAutoUpdate(ctx, deps)
	}
}

// reconcileAutoUpdateState inspects the run named by state.LastRunID when
// state.LastOutcome is "started" and maps terminal runs to verified, failed,
// or rolled_back. On failed or rolled_back, state.SuppressedTag is set to
// state.LastTarget so the same release is not retried automatically.
// inFlight is true when the run is still pending, running, or restarting.
func (s *Server) reconcileAutoUpdateState(ctx context.Context, state *AutoUpdateState) (changed bool, inFlight bool, err error) {
	if state.LastRunID == "" || state.LastOutcome != AutoUpdateOutcomeStarted {
		return false, false, nil
	}

	run, err := s.store.GetMaintenanceRun(ctx, state.LastRunID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return false, false, err
		}
		run = nil
	}

	var outcome string
	switch {
	case run != nil && (run.Status == store.MaintenanceStatusRunning ||
		run.Status == store.MaintenanceStatusRestarting ||
		run.Status == store.MaintenanceStatusPending):
		// Still in flight.
		return false, true, nil
	case run != nil && run.Status == store.MaintenanceStatusCompleted:
		outcome = AutoUpdateOutcomeVerified
	default:
		outcome = s.resolveFailedAutoUpdateOutcome(ctx, state.LastRunID, run)
	}

	state.LastOutcome = outcome
	if (outcome == AutoUpdateOutcomeFailed || outcome == AutoUpdateOutcomeRolledBack) && state.LastTarget != "" {
		state.SuppressedTag = state.LastTarget
	}
	return true, false, nil
}

func (s *Server) resolveFailedAutoUpdateOutcome(ctx context.Context, runID string, run *store.MaintenanceOperationRun) string {
	var last LastUpdate
	if found, err := s.readHubSettingJSON(ctx, HubSettingSectionLastUpdate, &last); err == nil && found && last.RunID == runID {
		switch last.Status {
		case LastUpdateVerified:
			return AutoUpdateOutcomeVerified
		case LastUpdateRolledBack:
			return AutoUpdateOutcomeRolledBack
		case LastUpdateFailed:
			return AutoUpdateOutcomeFailed
		}
	}
	if run != nil && run.Result != "" {
		var res map[string]any
		if err := json.Unmarshal([]byte(run.Result), &res); err == nil {
			if v, ok := res["verification"].(string); ok && v == LastUpdateRolledBack {
				return AutoUpdateOutcomeRolledBack
			}
		}
	}
	return AutoUpdateOutcomeFailed
}

// runReleaseAutoUpdate evaluates one tick of the daily release auto-update
// job:
//  1. Read system.auto_update; return immediately if not effectively enabled.
//  2. Reconcile any previously started run (recording SuppressedTag on failure/rollback).
//  3. Check eligibility (release mode, pin=latest, channel, supported installer, unlocked).
//  4. Check daily window [WindowStartUTC+offset, WindowStartUTC+2h) and once-per-day guard.
//     Skip while hub maintenance mode is enabled (recording skipped:maintenance_mode).
//  5. Persist LastAttemptDay before calling the catalog (at most one check per day).
//  6. Refresh and check the release catalog.
//  7. Skip when no upgrade is available or the latest tag is suppressed.
//  8. Clear suppression if newer, and start an explicit-tag update-binary run.
func (s *Server) runReleaseAutoUpdate(ctx context.Context, deps autoUpdateJobDeps) {
	log := logging.Subsystem("hub.maintenance.release-auto-update")
	mc := s.config.MaintenanceConfig

	// Step 1: read operator intent; return after a single DB read when off.
	setting, _, err := s.readAutoUpdateSetting(ctx)
	if err != nil {
		log.Error("Failed to read auto_update setting, skipping tick", "error", err)
		return
	}
	eff := effectiveAutoUpdate(mc, setting)
	if !eff.Enabled {
		return
	}

	state, err := s.readAutoUpdateState(ctx)
	if err != nil {
		log.Error("Failed to read auto_update_state, skipping tick", "error", err)
		return
	}

	// Step 2: reconcile the previous run before checking eligibility or window
	// so outcomes and retry suppression are recorded promptly after restart.
	reconciled, inFlight, err := s.reconcileAutoUpdateState(ctx, &state)
	if err != nil {
		log.Error("Failed to reconcile previous auto-update run, skipping tick", "run_id", state.LastRunID, "error", err)
		return
	}
	if inFlight {
		log.Debug("Previous auto-update run still in flight, skipping tick", "run_id", state.LastRunID)
		return
	}
	if reconciled {
		if err := s.writeAutoUpdateState(ctx, state); err != nil {
			log.Error("Failed to persist reconciled auto_update_state", "run_id", state.LastRunID, "error", err)
			return
		}
		log.Info("Reconciled previous auto-update run",
			"run_id", state.LastRunID, "target", state.LastTarget,
			"outcome", state.LastOutcome, "suppressed_tag", state.SuppressedTag)
	}

	// Step 3: structural eligibility.
	sel, err := s.readReleaseSelection(ctx)
	if err != nil {
		log.Error("Failed to read release selection, skipping tick", "error", err)
		return
	}
	if mode := effectiveUpdateMode(mc, sel); mode != UpdateModeRelease {
		log.Debug("Auto-update skipped: effective mode is not release", "mode", mode)
		return
	}
	if pin := effectivePin(sel); pin != maintenance.PinLatest {
		log.Debug("Auto-update skipped: hub is pinned", "pin", pin)
		return
	}
	channel, _ := effectiveReleaseChannel(mc, deps.running, sel)
	if channel == "" {
		log.Debug("Auto-update skipped: no effective release channel", "version", deps.running)
		return
	}
	if st := s.updateInstaller().Status(); !st.Supported {
		log.Debug("Auto-update skipped: installer unsupported", "installer", st.Strategy, "reason", st.Reason)
		return
	}

	// Step 4: daily window + once-per-UTC-day guard.
	nowFn := deps.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	offset := autoUpdateWindowOffset(s.config.HubID)
	inWindow, attemptDay := autoUpdateWindowFor(now, eff.WindowStartHourUTC, offset)
	if !inWindow || state.LastAttemptDay == attemptDay {
		return
	}

	// Maintenance mode check: skip while the operator has maintenance mode on
	// without consuming the day's attempt, and record skipped:maintenance_mode.
	if s.maintenance != nil && s.maintenance.IsEnabled() {
		log.Info("Auto-update skipped: hub maintenance mode is enabled")
		state.LastCheckAt = now.UTC().Format(time.RFC3339)
		setAutoUpdateNonRunOutcome(&state, AutoUpdateOutcomeSkippedMaintenanceMode)
		if err := s.writeAutoUpdateState(ctx, state); err != nil {
			log.Error("Failed to persist maintenance-mode skip state", "error", err)
		}
		return
	}

	// Step 5: mark today's attempt before contacting the catalog so at most
	// one evaluation runs per day even if later steps fail or the hub restarts.
	state.LastAttemptDay = attemptDay
	state.LastCheckAt = now.Format(time.RFC3339)
	if err := s.writeAutoUpdateState(ctx, state); err != nil {
		log.Error("Failed to persist auto_update_state attempt day, aborting tick", "error", err)
		return
	}

	// Step 6: fresh catalog check on the effective channel.
	result, err := deps.check(ctx, maintenance.ReleaseCheckOptions{
		CurrentVersion:   deps.running,
		CurrentBuildTime: deps.buildTime,
		Channel:          channel,
		Repo:             mc.GitHubRepo,
		Catalog:          s.updateReleaseCatalog(),
		Releases:         s.updateReleaseSource(),
		Refresh:          true,
	})
	if err != nil {
		log.Error("Scheduled auto-update catalog check failed; will retry next day", "channel", channel, "error", err)
		setAutoUpdateNonRunOutcome(&state, AutoUpdateOutcomeSkippedCheckFailed)
		if werr := s.writeAutoUpdateState(ctx, state); werr != nil {
			log.Error("Failed to persist check_failed outcome", "error", werr)
		}
		return
	}

	// Step 7: no upgrade available, or latest tag is suppressed.
	if !result.UpdateAvailable || result.LatestVersion == "" {
		log.Debug("Auto-update check found no newer release",
			"current", result.CurrentVersion, "latest", result.LatestVersion,
			"channel", result.Channel, "class", result.Class)
		setAutoUpdateNonRunOutcome(&state, AutoUpdateOutcomeNoUpdate)
		if werr := s.writeAutoUpdateState(ctx, state); werr != nil {
			log.Error("Failed to persist no_update outcome", "error", werr)
		}
		return
	}
	if state.SuppressedTag != "" && result.LatestVersion == state.SuppressedTag {
		log.Info("Auto-update skipped: latest release is suppressed after a previous failure",
			"tag", result.LatestVersion, "channel", channel)
		setAutoUpdateNonRunOutcome(&state, AutoUpdateOutcomeSkippedSuppressed)
		if werr := s.writeAutoUpdateState(ctx, state); werr != nil {
			log.Error("Failed to persist suppressed outcome", "error", werr)
		}
		return
	}

	// Step 8: newer than suppressed — clear suppression and apply explicit tag.
	state.SuppressedTag = ""
	executor, err := deps.updateExecutor()
	if err != nil {
		log.Error("Failed to resolve update-binary executor for auto-update", "error", err)
		setAutoUpdateNonRunOutcome(&state, AutoUpdateOutcomeSkippedError)
		if werr := s.writeAutoUpdateState(ctx, state); werr != nil {
			log.Error("Failed to persist executor error outcome", "error", werr)
		}
		return
	}

	runID, outcome := s.autoApplyUpdate(ctx, channel, result.LatestVersion, executor)
	if runID != "" {
		state.LastRunID = runID
		state.LastTarget = result.LatestVersion
		state.LastOutcome = AutoUpdateOutcomeStarted
	} else {
		setAutoUpdateNonRunOutcome(&state, outcome)
	}
	if werr := s.writeAutoUpdateState(ctx, state); werr != nil {
		log.Error("Failed to persist auto-update apply outcome", "run_id", runID, "outcome", state.LastOutcome, "error", werr)
	}
}
