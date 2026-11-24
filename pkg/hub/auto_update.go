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
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// HubSettingSectionAutoUpdate is the hub_settings row that stores the
// operator's daily automatic release update intent. Absent means fall back to
// server.maintenance.update_policy.
const HubSettingSectionAutoUpdate = "system.auto_update"

// DefaultAutoUpdateWindowStartHourUTC is the default UTC hour (04:00 UTC) for
// the daily auto-update window, just after the ~03:00 UTC nightly cut.
const DefaultAutoUpdateWindowStartHourUTC = 4

// Auto-update effective sources reported in update-status.
const (
	AutoUpdateSourceSetting      = "setting"
	AutoUpdateSourceConfig       = "config"
	AutoUpdateSourceDefault      = "default"
	AutoUpdateSourceConfigLocked = "config-locked"
)

// Auto-update inactiveReason values reported in update-status.
const (
	AutoUpdateInactivePinned               = "pinned"
	AutoUpdateInactiveGitMode              = "git_mode"
	AutoUpdateInactiveInstallerUnsupported = "installer_unsupported"
	AutoUpdateInactiveMaintenanceMode      = "maintenance_mode"
	AutoUpdateInactiveNoChannel            = "no_channel"
	AutoUpdateInactiveConfigLocked         = "config_locked"
)

const autoUpdateLockedReason = "Automatic updates are disabled by server configuration (server.maintenance.update_policy: disabled)."

// AutoUpdateSetting is the system.auto_update hub_settings row.
type AutoUpdateSetting struct {
	Enabled        bool   `json:"enabled"`
	WindowStartUTC int    `json:"window_start_hour_utc"` // 0..23, default 4
	SetBy          string `json:"set_by"`                // user id/email of the admin
	SetAt          string `json:"set_at"`                // RFC3339
}

// updateStatusAutoUpdate is the autoUpdate block returned by
// GET /api/v1/admin/maintenance/update-status and
// PUT /api/v1/admin/maintenance/auto-update.
type updateStatusAutoUpdate struct {
	Enabled            bool   `json:"enabled"`
	Source             string `json:"source"`
	Locked             bool   `json:"locked"`
	LockedReason       string `json:"lockedReason"`
	Active             bool   `json:"active"`
	InactiveReason     string `json:"inactiveReason"`
	WindowStartHourUTC int    `json:"windowStartHourUtc"`
}

// effectiveAutoUpdateResult is the precedence-resolved auto-update intent
// before eligibility checks (mode, pin, installer, channel, maintenance mode).
type effectiveAutoUpdateResult struct {
	Enabled            bool
	Source             string
	Locked             bool
	LockedReason       string
	WindowStartHourUTC int
}

func validAutoUpdateWindowHour(setting *AutoUpdateSetting) int {
	if setting != nil && setting.WindowStartUTC >= 0 && setting.WindowStartUTC <= 23 {
		return setting.WindowStartUTC
	}
	return DefaultAutoUpdateWindowStartHourUTC
}

// effectiveAutoUpdate resolves operator intent from the stored
// system.auto_update row and the startup MaintenanceConfig:
//   - An explicit server.maintenance.update_policy: disabled locks auto-update
//     off regardless of any stored row (source: "config-locked").
//   - Otherwise, a stored system.auto_update row wins (source: "setting").
//   - Without a stored row, update_policy: auto enables auto-update with the
//     default daily window (source: "config").
//   - Otherwise (notify, implicit disabled, or unset), auto-update is off
//     (source: "default").
func effectiveAutoUpdate(mc MaintenanceConfig, setting *AutoUpdateSetting) effectiveAutoUpdateResult {
	if mc.UpdatePolicyExplicit && mc.UpdatePolicy == "disabled" {
		return effectiveAutoUpdateResult{
			Enabled:            false,
			Source:             AutoUpdateSourceConfigLocked,
			Locked:             true,
			LockedReason:       autoUpdateLockedReason,
			WindowStartHourUTC: validAutoUpdateWindowHour(setting),
		}
	}
	if setting != nil {
		return effectiveAutoUpdateResult{
			Enabled:            setting.Enabled,
			Source:             AutoUpdateSourceSetting,
			WindowStartHourUTC: validAutoUpdateWindowHour(setting),
		}
	}
	if mc.UpdatePolicy == "auto" {
		return effectiveAutoUpdateResult{
			Enabled:            true,
			Source:             AutoUpdateSourceConfig,
			WindowStartHourUTC: DefaultAutoUpdateWindowStartHourUTC,
		}
	}
	return effectiveAutoUpdateResult{
		Enabled:            false,
		Source:             AutoUpdateSourceDefault,
		WindowStartHourUTC: DefaultAutoUpdateWindowStartHourUTC,
	}
}

// buildAutoUpdateStatus combines the precedence-resolved auto-update intent
// with the hub's runtime eligibility state.
func buildAutoUpdateStatus(
	mc MaintenanceConfig,
	setting *AutoUpdateSetting,
	mode, channel, pin string,
	installerSupported, maintenanceMode bool,
) updateStatusAutoUpdate {
	eff := effectiveAutoUpdate(mc, setting)

	var inactiveReason string
	switch {
	case eff.Locked:
		inactiveReason = AutoUpdateInactiveConfigLocked
	case mode != UpdateModeRelease:
		inactiveReason = AutoUpdateInactiveGitMode
	case pin != maintenance.PinLatest:
		inactiveReason = AutoUpdateInactivePinned
	case !installerSupported:
		inactiveReason = AutoUpdateInactiveInstallerUnsupported
	case channel == "":
		inactiveReason = AutoUpdateInactiveNoChannel
	case maintenanceMode:
		inactiveReason = AutoUpdateInactiveMaintenanceMode
	}

	return updateStatusAutoUpdate{
		Enabled:            eff.Enabled,
		Source:             eff.Source,
		Locked:             eff.Locked,
		LockedReason:       eff.LockedReason,
		Active:             eff.Enabled && inactiveReason == "",
		InactiveReason:     inactiveReason,
		WindowStartHourUTC: eff.WindowStartHourUTC,
	}
}

// readAutoUpdateSetting returns system.auto_update and its current revision.
// When the row does not exist, it returns (nil, 0, nil). An unreadable row is
// ignored with a warning while preserving its revision so a subsequent PUT can
// replace it.
func (s *Server) readAutoUpdateSetting(ctx context.Context) (*AutoUpdateSetting, int64, error) {
	row, err := s.store.GetHubSetting(ctx, HubSettingSectionAutoUpdate)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	var setting AutoUpdateSetting
	if err := json.Unmarshal(row.Value, &setting); err != nil {
		s.maintenanceLog.Warn("Ignoring an unreadable auto_update setting", "error", err)
		return nil, row.Revision, nil
	}
	return &setting, row.Revision, nil
}

// writeAutoUpdateSetting persists system.auto_update with optimistic
// concurrency against expectedRevision (0 for create-only, >0 for CAS update).
func (s *Server) writeAutoUpdateSetting(ctx context.Context, setting AutoUpdateSetting, updatedBy string, expectedRevision int64) error {
	raw, err := json.Marshal(setting)
	if err != nil {
		return fmt.Errorf("encode %s: %w", HubSettingSectionAutoUpdate, err)
	}
	if _, err := s.store.UpsertHubSetting(ctx, HubSettingSectionAutoUpdate, json.RawMessage(raw), updatedBy, expectedRevision, "managed"); err != nil {
		return err
	}
	return nil
}

type putAutoUpdateRequest struct {
	Enabled            *bool `json:"enabled"`
	WindowStartHourUTC *int  `json:"windowStartHourUtc,omitempty"`
}

// handleAdminMaintenanceAutoUpdate handles PUT /api/v1/admin/maintenance/auto-update.
// Authorization: enforced by routeGuard via hub.maintenance.execute permission.
func (s *Server) handleAdminMaintenanceAutoUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		MethodNotAllowed(w, http.MethodPut)
		return
	}

	var body putAutoUpdateRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body: "+err.Error(), nil)
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "enabled is required", nil)
		return
	}
	if body.WindowStartHourUTC != nil && (*body.WindowStartHourUTC < 0 || *body.WindowStartHourUTC > 23) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "windowStartHourUtc must be between 0 and 23", nil)
		return
	}

	mc := s.config.MaintenanceConfig
	if mc.UpdatePolicyExplicit && mc.UpdatePolicy == "disabled" {
		writeError(w, http.StatusConflict, "auto_update_locked", autoUpdateLockedReason, nil)
		return
	}

	ctx := r.Context()
	existing, rev, err := s.readAutoUpdateSetting(ctx)
	if err != nil {
		s.maintenanceLog.Error("Failed to read auto_update setting", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read auto-update setting", nil)
		return
	}

	windowHour := validAutoUpdateWindowHour(existing)
	if body.WindowStartHourUTC != nil {
		windowHour = *body.WindowStartHourUTC
	}

	actor := ""
	updatedBy := ""
	if user := GetUserIdentityFromContext(ctx); user != nil {
		updatedBy = user.ID()
		actor = user.Email()
		if actor == "" {
			actor = user.ID()
		}
	}

	next := AutoUpdateSetting{
		Enabled:        *body.Enabled,
		WindowStartUTC: windowHour,
		SetBy:          actor,
		SetAt:          time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.writeAutoUpdateSetting(ctx, next, updatedBy, rev); err != nil {
		if errors.Is(err, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, "revision_conflict",
				"The auto-update setting was modified concurrently. Please refresh and retry.", nil)
			return
		}
		s.maintenanceLog.Error("Failed to write auto_update setting", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to update auto-update setting", nil)
		return
	}

	sel, err := s.readReleaseSelection(ctx)
	if err != nil {
		s.maintenanceLog.Error("Failed to read release selection after auto_update write", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read update status", nil)
		return
	}
	cur := currentRunningBuild()
	channel, _ := effectiveReleaseChannel(mc, cur.Version, sel)
	resp := buildAutoUpdateStatus(
		mc,
		&next,
		effectiveUpdateMode(mc, sel),
		channel,
		effectivePin(sel),
		s.updateInstaller().Status().Supported,
		s.maintenance != nil && s.maintenance.IsEnabled(),
	)
	writeJSON(w, http.StatusOK, resp)
}
