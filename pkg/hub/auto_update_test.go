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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const autoUpdateAdminPath = "/api/v1/admin/maintenance/auto-update"

func TestEffectiveAutoUpdate(t *testing.T) {
	cases := []struct {
		name        string
		mc          MaintenanceConfig
		setting     *AutoUpdateSetting
		wantEnabled bool
		wantSource  string
		wantLocked  bool
		wantHour    int
	}{
		{
			name:        "setting enabled overrides notify config",
			mc:          MaintenanceConfig{UpdatePolicy: "notify"},
			setting:     &AutoUpdateSetting{Enabled: true, WindowStartUTC: 6},
			wantEnabled: true,
			wantSource:  AutoUpdateSourceSetting,
			wantLocked:  false,
			wantHour:    6,
		},
		{
			name:        "setting disabled overrides auto config",
			mc:          MaintenanceConfig{UpdatePolicy: "auto", UpdatePolicyExplicit: true},
			setting:     &AutoUpdateSetting{Enabled: false, WindowStartUTC: 0},
			wantEnabled: false,
			wantSource:  AutoUpdateSourceSetting,
			wantLocked:  false,
			wantHour:    0,
		},
		{
			name:        "setting enabled overrides implicit disabled (source tier default)",
			mc:          MaintenanceConfig{UpdatePolicy: "disabled", UpdatePolicyExplicit: false},
			setting:     &AutoUpdateSetting{Enabled: true, WindowStartUTC: 23},
			wantEnabled: true,
			wantSource:  AutoUpdateSourceSetting,
			wantLocked:  false,
			wantHour:    23,
		},
		{
			name:        "explicit disabled locks even when setting is present and enabled",
			mc:          MaintenanceConfig{UpdatePolicy: "disabled", UpdatePolicyExplicit: true},
			setting:     &AutoUpdateSetting{Enabled: true, WindowStartUTC: 9},
			wantEnabled: false,
			wantSource:  AutoUpdateSourceConfigLocked,
			wantLocked:  true,
			wantHour:    9,
		},
		{
			name:        "absent setting with auto config enables with default window",
			mc:          MaintenanceConfig{UpdatePolicy: "auto", UpdatePolicyExplicit: true},
			setting:     nil,
			wantEnabled: true,
			wantSource:  AutoUpdateSourceConfig,
			wantLocked:  false,
			wantHour:    DefaultAutoUpdateWindowStartHourUTC,
		},
		{
			name:        "absent setting with notify config defaults off",
			mc:          MaintenanceConfig{UpdatePolicy: "notify"},
			setting:     nil,
			wantEnabled: false,
			wantSource:  AutoUpdateSourceDefault,
			wantLocked:  false,
			wantHour:    DefaultAutoUpdateWindowStartHourUTC,
		},
		{
			name:        "absent setting with empty config defaults off",
			mc:          MaintenanceConfig{},
			setting:     nil,
			wantEnabled: false,
			wantSource:  AutoUpdateSourceDefault,
			wantLocked:  false,
			wantHour:    DefaultAutoUpdateWindowStartHourUTC,
		},
		{
			name:        "absent setting with implicit disabled defaults off unlocked",
			mc:          MaintenanceConfig{UpdatePolicy: "disabled", UpdatePolicyExplicit: false},
			setting:     nil,
			wantEnabled: false,
			wantSource:  AutoUpdateSourceDefault,
			wantLocked:  false,
			wantHour:    DefaultAutoUpdateWindowStartHourUTC,
		},
		{
			name:        "absent setting with explicit disabled is config-locked",
			mc:          MaintenanceConfig{UpdatePolicy: "disabled", UpdatePolicyExplicit: true},
			setting:     nil,
			wantEnabled: false,
			wantSource:  AutoUpdateSourceConfigLocked,
			wantLocked:  true,
			wantHour:    DefaultAutoUpdateWindowStartHourUTC,
		},
		{
			name:        "setting with out-of-range hour falls back to default",
			mc:          MaintenanceConfig{UpdatePolicy: "notify"},
			setting:     &AutoUpdateSetting{Enabled: true, WindowStartUTC: 99},
			wantEnabled: true,
			wantSource:  AutoUpdateSourceSetting,
			wantLocked:  false,
			wantHour:    DefaultAutoUpdateWindowStartHourUTC,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveAutoUpdate(tc.mc, tc.setting)
			assert.Equal(t, tc.wantEnabled, got.Enabled)
			assert.Equal(t, tc.wantSource, got.Source)
			assert.Equal(t, tc.wantLocked, got.Locked)
			assert.Equal(t, tc.wantHour, got.WindowStartHourUTC)
			if tc.wantLocked {
				assert.NotEmpty(t, got.LockedReason)
			} else {
				assert.Empty(t, got.LockedReason)
			}
		})
	}
}

func TestBuildAutoUpdateStatus_Eligibility(t *testing.T) {
	enabledSetting := &AutoUpdateSetting{Enabled: true, WindowStartUTC: 4}
	notifyMC := MaintenanceConfig{UpdatePolicy: "notify"}
	now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)

	t.Run("eligible and enabled is active with nextRunAt", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, AutoUpdateState{}, UpdateModeRelease, "nightly", maintenance.PinLatest, true, false, "", now)
		assert.True(t, got.Enabled)
		assert.True(t, got.Active)
		assert.Empty(t, got.InactiveReason)
		require.NotNil(t, got.NextRunAt)
		assert.Equal(t, "2026-11-24T04:00:00Z", *got.NextRunAt)
	})

	t.Run("eligible and disabled is inactive with empty reason and nil nextRunAt", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, nil, AutoUpdateState{}, UpdateModeRelease, "nightly", maintenance.PinLatest, true, false, "", now)
		assert.False(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Empty(t, got.InactiveReason)
		assert.Nil(t, got.NextRunAt)
	})

	t.Run("config locked", func(t *testing.T) {
		lockedMC := MaintenanceConfig{UpdatePolicy: "disabled", UpdatePolicyExplicit: true}
		got := buildAutoUpdateStatus(lockedMC, enabledSetting, AutoUpdateState{}, UpdateModeRelease, "nightly", maintenance.PinLatest, true, false, "", now)
		assert.False(t, got.Enabled)
		assert.True(t, got.Locked)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveConfigLocked, got.InactiveReason)
		assert.Nil(t, got.NextRunAt)
	})

	t.Run("git mode", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, AutoUpdateState{}, UpdateModeGit, "", maintenance.PinLatest, true, false, "", now)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveGitMode, got.InactiveReason)
		assert.Nil(t, got.NextRunAt)
	})

	t.Run("pinned", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, AutoUpdateState{}, UpdateModeRelease, "nightly", "nightly-20261124", true, false, "", now)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactivePinned, got.InactiveReason)
		assert.Nil(t, got.NextRunAt)
	})

	t.Run("installer unsupported", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, AutoUpdateState{}, UpdateModeRelease, "nightly", maintenance.PinLatest, false, false, "", now)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveInstallerUnsupported, got.InactiveReason)
		assert.Nil(t, got.NextRunAt)
	})

	t.Run("no channel", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, AutoUpdateState{}, UpdateModeRelease, "", maintenance.PinLatest, true, false, "", now)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveNoChannel, got.InactiveReason)
		assert.Nil(t, got.NextRunAt)
	})

	t.Run("maintenance mode", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, AutoUpdateState{}, UpdateModeRelease, "nightly", maintenance.PinLatest, true, true, "", now)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveMaintenanceMode, got.InactiveReason)
		assert.Nil(t, got.NextRunAt)
	})

	t.Run("propagates state fields and per-hub offset into nextRunAt", func(t *testing.T) {
		hubID := "hub-alpha-123"
		offset := autoUpdateWindowOffset(hubID)
		state := AutoUpdateState{
			LastCheckAt:    "2026-11-23T04:17:00Z",
			LastAttemptDay: "2026-11-24",
			LastRunID:      "run-42",
			LastTarget:     "nightly-20261124",
			LastOutcome:    AutoUpdateOutcomeFailed,
			SuppressedTag:  "nightly-20261124",
		}
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, state, UpdateModeRelease, "nightly", maintenance.PinLatest, true, false, hubID, now)
		assert.True(t, got.Active)
		require.NotNil(t, got.NextRunAt)
		// LastAttemptDay is already 2026-11-24, so nextRunAt advances to 2026-11-25.
		wantNext := time.Date(2026, 11, 25, 4, 0, 0, 0, time.UTC).Add(offset).Format(time.RFC3339)
		assert.Equal(t, wantNext, *got.NextRunAt)
		assert.Equal(t, state.LastCheckAt, got.LastCheckAt)
		assert.Equal(t, state.LastRunID, got.LastRunID)
		assert.Equal(t, state.LastTarget, got.LastTarget)
		assert.Equal(t, state.LastOutcome, got.LastOutcome)
		assert.Equal(t, state.SuppressedTag, got.SuppressedTag)
	})
}

func TestNextAutoUpdateRunAt(t *testing.T) {
	offset := 17 * time.Minute

	cases := []struct {
		name           string
		now            time.Time
		startHourUTC   int
		offset         time.Duration
		lastAttemptDay string
		want           time.Time
	}{
		{
			name:         "before today window",
			now:          time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC),
			startHourUTC: 4,
			offset:       offset,
			want:         time.Date(2026, 11, 24, 4, 17, 0, 0, time.UTC),
		},
		{
			name:         "inside unattempted window returns now truncated to seconds",
			now:          time.Date(2026, 11, 24, 4, 25, 30, 500, time.UTC),
			startHourUTC: 4,
			offset:       offset,
			want:         time.Date(2026, 11, 24, 4, 25, 30, 0, time.UTC),
		},
		{
			name:           "inside already-attempted window advances to tomorrow",
			now:            time.Date(2026, 11, 24, 4, 25, 0, 0, time.UTC),
			startHourUTC:   4,
			offset:         offset,
			lastAttemptDay: "2026-11-24",
			want:           time.Date(2026, 11, 25, 4, 17, 0, 0, time.UTC),
		},
		{
			name:         "after today window ended advances to tomorrow",
			now:          time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC),
			startHourUTC: 4,
			offset:       offset,
			want:         time.Date(2026, 11, 25, 4, 17, 0, 0, time.UTC),
		},
		{
			name:         "midnight-crossing window unattempted after 00:00",
			now:          time.Date(2026, 11, 25, 0, 30, 0, 0, time.UTC),
			startHourUTC: 23,
			offset:       15 * time.Minute,
			want:         time.Date(2026, 11, 25, 0, 30, 0, 0, time.UTC),
		},
		{
			name:           "midnight-crossing window already attempted advances to same-day 23:15",
			now:            time.Date(2026, 11, 25, 0, 30, 0, 0, time.UTC),
			startHourUTC:   23,
			offset:         15 * time.Minute,
			lastAttemptDay: "2026-11-24",
			want:           time.Date(2026, 11, 25, 23, 15, 0, 0, time.UTC),
		},
		{
			name:           "window moved later on same day after attempt already ran",
			now:            time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC),
			startHourUTC:   10,
			offset:         offset,
			lastAttemptDay: "2026-11-24",
			want:           time.Date(2026, 11, 25, 10, 17, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextAutoUpdateRunAt(tc.now, tc.startHourUTC, tc.offset, tc.lastAttemptDay)
			assert.Equal(t, tc.want, got)
		})
	}
}

func autoUpdateTestServer(t *testing.T, mc MaintenanceConfig) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()
	srv, s := testServer(t)
	srv.config.MaintenanceConfig = mc
	ctx := context.Background()
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "au-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	member, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "au-member", store.SystemRoleHubMember))
	require.NoError(t, err)
	return srv, s, admin, member
}

func TestHandleAdminMaintenanceAutoUpdate_AuthzAndMethod(t *testing.T) {
	mc := MaintenanceConfig{
		DeploymentTier: "binary",
		ReleaseChannel: "nightly",
		UpdatePolicy:   "notify",
		Installer:      newTestInstaller(maintenance.StrategySymlink),
	}
	srv, _, admin, member := autoUpdateTestServer(t, mc)

	// Non-admin receives 403 Forbidden.
	rec := doRequestAsUser(t, srv, member, http.MethodPut, autoUpdateAdminPath, map[string]any{"enabled": true})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Non-PUT method receives 405 Method Not Allowed.
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, autoUpdateAdminPath, nil)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}

func TestHandleAdminMaintenanceAutoUpdate_ValidationAndLocked(t *testing.T) {
	t.Run("missing enabled returns 400", func(t *testing.T) {
		mc := MaintenanceConfig{DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "notify", Installer: newTestInstaller(maintenance.StrategySymlink)}
		srv, _, admin, _ := autoUpdateTestServer(t, mc)
		rec := doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{"windowStartHourUtc": 5})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("bad hour returns 400", func(t *testing.T) {
		mc := MaintenanceConfig{DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "notify", Installer: newTestInstaller(maintenance.StrategySymlink)}
		srv, _, admin, _ := autoUpdateTestServer(t, mc)
		for _, bad := range []int{-1, 24, 100} {
			rec := doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{
				"enabled":            true,
				"windowStartHourUtc": bad,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "hour=%d body=%s", bad, rec.Body.String())
		}
	})

	t.Run("explicit disabled returns 409 auto_update_locked", func(t *testing.T) {
		mc := MaintenanceConfig{
			DeploymentTier:       "binary",
			ReleaseChannel:       "nightly",
			UpdatePolicy:         "disabled",
			UpdatePolicyExplicit: true,
			Installer:            newTestInstaller(maintenance.StrategySymlink),
		}
		srv, _, admin, _ := autoUpdateTestServer(t, mc)
		rec := doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{"enabled": true})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		var errBody struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
		assert.Equal(t, "auto_update_locked", errBody.Error.Code)
	})

	t.Run("implicit disabled is not locked", func(t *testing.T) {
		mc := MaintenanceConfig{
			DeploymentTier:       "source",
			ReleaseChannel:       "nightly",
			UpdatePolicy:         "disabled",
			UpdatePolicyExplicit: false,
			Installer:            newTestInstaller(maintenance.StrategySymlink),
		}
		srv, _, admin, _ := autoUpdateTestServer(t, mc)
		rec := doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{"enabled": true})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp updateStatusAutoUpdate
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.True(t, resp.Enabled)
		assert.Equal(t, AutoUpdateSourceSetting, resp.Source)
		assert.False(t, resp.Active)
		assert.Equal(t, AutoUpdateInactiveGitMode, resp.InactiveReason)
	})
}

type autoUpdateConflictStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (c *autoUpdateConflictStore) UpsertHubSetting(
	ctx context.Context, section string, value json.RawMessage,
	updatedBy string, expectedRevision int64, origin string,
) (*store.HubSetting, error) {
	if c.fault.Active() && section == HubSettingSectionAutoUpdate {
		return nil, store.ErrRevisionConflict
	}
	return c.Store.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
}

func TestHandleAdminMaintenanceAutoUpdate_RevisionConflict(t *testing.T) {
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *autoUpdateConflictStore {
		return &autoUpdateConflictStore{Store: inner, fault: f}
	})
	srv.config.MaintenanceConfig = MaintenanceConfig{
		DeploymentTier: "binary",
		ReleaseChannel: "nightly",
		UpdatePolicy:   "notify",
		Installer:      newTestInstaller(maintenance.StrategySymlink),
	}
	ctx := context.Background()
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "au-conflict-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	fault.Arm()
	rec := doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{"enabled": true})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Equal(t, "revision_conflict", errBody.Error.Code)
}

func TestHandleAdminMaintenanceAutoUpdate_RoundTripAndStates(t *testing.T) {
	ctx := context.Background()
	mc := MaintenanceConfig{
		DeploymentTier: "binary",
		ReleaseChannel: "nightly",
		UpdatePolicy:   "notify",
		Installer:      newTestInstaller(maintenance.StrategySymlink),
	}
	srv, s, admin, _ := autoUpdateTestServer(t, mc)

	// Initial GET update-status: default off, window 4.
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/update-status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var st updateStatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.False(t, st.AutoUpdate.Enabled)
	assert.Equal(t, AutoUpdateSourceDefault, st.AutoUpdate.Source)
	assert.False(t, st.AutoUpdate.Locked)
	assert.False(t, st.AutoUpdate.Active)
	assert.Empty(t, st.AutoUpdate.InactiveReason)
	assert.Equal(t, DefaultAutoUpdateWindowStartHourUTC, st.AutoUpdate.WindowStartHourUTC)

	// Enable with explicit hour 6.
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{
		"enabled":            true,
		"windowStartHourUtc": 6,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var putResp updateStatusAutoUpdate
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &putResp))
	assert.True(t, putResp.Enabled)
	assert.Equal(t, AutoUpdateSourceSetting, putResp.Source)
	assert.True(t, putResp.Active)
	assert.Empty(t, putResp.InactiveReason)
	assert.Equal(t, 6, putResp.WindowStartHourUTC)

	// Verify stored row records set_by (email), set_at, and updated_by (user ID).
	stored, rev, err := srv.readAutoUpdateSetting(ctx)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, int64(1), rev)
	assert.True(t, stored.Enabled)
	assert.Equal(t, 6, stored.WindowStartUTC)
	assert.Equal(t, admin.Email, stored.SetBy)
	assert.NotEmpty(t, stored.SetAt)
	rawRow, err := s.GetHubSetting(ctx, HubSettingSectionAutoUpdate)
	require.NoError(t, err)
	assert.Equal(t, admin.ID, rawRow.UpdatedBy)

	// Toggle off without windowStartHourUtc: preserves existing hour (6).
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &putResp))
	assert.False(t, putResp.Enabled)
	assert.False(t, putResp.Active)
	assert.Equal(t, 6, putResp.WindowStartHourUTC)

	// Enable while pinned: stored as enabled, but active:false with inactiveReason:"pinned".
	require.NoError(t, srv.writeHubSettingJSON(ctx, HubSettingSectionReleaseSelection, ReleaseSelection{
		Mode:    UpdateModeRelease,
		Channel: "nightly",
		Pin:     "nightly-20261120",
		Version: "nightly-20261120",
	}))
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &putResp))
	assert.True(t, putResp.Enabled)
	assert.False(t, putResp.Active)
	assert.Equal(t, AutoUpdateInactivePinned, putResp.InactiveReason)

	// GET update-status reflects the persisted pinned state.
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/update-status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.Equal(t, putResp, st.AutoUpdate)

	// Switch release selection to git mode: stored stays enabled, active:false with inactiveReason:"git_mode".
	require.NoError(t, srv.writeHubSettingJSON(ctx, HubSettingSectionReleaseSelection, ReleaseSelection{
		Mode:    UpdateModeGit,
		Version: "git-abc123def456",
	}))
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/update-status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.True(t, st.AutoUpdate.Enabled)
	assert.False(t, st.AutoUpdate.Active)
	assert.Equal(t, AutoUpdateInactiveGitMode, st.AutoUpdate.InactiveReason)

	// Clear release selection (returns to release+latest): auto-update becomes active again.
	require.NoError(t, s.DeleteHubSetting(ctx, HubSettingSectionReleaseSelection))
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/update-status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.True(t, st.AutoUpdate.Enabled)
	assert.True(t, st.AutoUpdate.Active)
	assert.Empty(t, st.AutoUpdate.InactiveReason)
	require.NotNil(t, st.AutoUpdate.NextRunAt)
	_, err = time.Parse(time.RFC3339, *st.AutoUpdate.NextRunAt)
	assert.NoError(t, err)
}

func TestHandleAdminMaintenanceUpdateStatus_AutoUpdateStateAndNextRunAt(t *testing.T) {
	ctx := context.Background()
	mc := MaintenanceConfig{
		DeploymentTier: "binary",
		ReleaseChannel: "nightly",
		UpdatePolicy:   "notify",
		Installer:      newTestInstaller(maintenance.StrategySymlink),
	}
	srv, _, admin, _ := autoUpdateTestServer(t, mc)
	srv.config.HubID = "hub-status-test-01"

	require.NoError(t, srv.writeAutoUpdateState(ctx, AutoUpdateState{
		LastCheckAt:    "2026-11-24T04:17:00Z",
		LastAttemptDay: "2026-11-24",
		LastRunID:      "run-abc",
		LastTarget:     "nightly-20261124",
		LastOutcome:    AutoUpdateOutcomeRolledBack,
		SuppressedTag:  "nightly-20261124",
	}))

	// Inactive (default off): nextRunAt must serialize as explicit JSON null,
	// while persisted state fields are still surfaced.
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/update-status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	auRaw, ok := raw["autoUpdate"].(map[string]any)
	require.True(t, ok)
	val, hasKey := auRaw["nextRunAt"]
	assert.True(t, hasKey, "nextRunAt key must be present")
	assert.Nil(t, val, "nextRunAt must be JSON null when inactive")
	assert.Equal(t, "2026-11-24T04:17:00Z", auRaw["lastCheckAt"])
	assert.Equal(t, "run-abc", auRaw["lastRunId"])
	assert.Equal(t, "nightly-20261124", auRaw["lastTarget"])
	assert.Equal(t, AutoUpdateOutcomeRolledBack, auRaw["lastOutcome"])
	assert.Equal(t, "nightly-20261124", auRaw["suppressedTag"])

	// Enable via PUT with a window hour 6h ahead (guaranteed outside the current 2h window):
	// response includes non-null RFC3339 nextRunAt and preserves state fields.
	windowHour := (time.Now().UTC().Hour() + 6) % 24
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{
		"enabled":            true,
		"windowStartHourUtc": windowHour,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var putResp updateStatusAutoUpdate
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &putResp))
	assert.True(t, putResp.Active)
	require.NotNil(t, putResp.NextRunAt)
	parsedNext, err := time.Parse(time.RFC3339, *putResp.NextRunAt)
	require.NoError(t, err)
	assert.Equal(t, windowHour, parsedNext.UTC().Hour())
	assert.Equal(t, int(autoUpdateWindowOffset(srv.config.HubID)/time.Minute), parsedNext.UTC().Minute())
	assert.Equal(t, "run-abc", putResp.LastRunID)
	assert.Equal(t, "nightly-20261124", putResp.LastTarget)
	assert.Equal(t, AutoUpdateOutcomeRolledBack, putResp.LastOutcome)
	assert.Equal(t, "nightly-20261124", putResp.SuppressedTag)
}

func TestHandleAdminMaintenanceAutoUpdate_AuditEvents(t *testing.T) {
	t.Run("PUT emits setting_changed with old and new values", func(t *testing.T) {
		mc := MaintenanceConfig{
			DeploymentTier: "binary",
			ReleaseChannel: "nightly",
			UpdatePolicy:   "notify",
			Installer:      newTestInstaller(maintenance.StrategySymlink),
		}
		srv, _, admin, _ := autoUpdateTestServer(t, mc)
		audit := &mockAuditLogger{}
		srv.SetAuditLogger(audit)

		// First write: default (false, 4) -> (true, 6).
		rec := doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{
			"enabled":            true,
			"windowStartHourUtc": 6,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Len(t, audit.maintenanceAutoUpdateEvents, 1)
		ev := audit.maintenanceAutoUpdateEvents[0]
		assert.Equal(t, MaintenanceAutoUpdateKindSettingChanged, ev.Kind)
		assert.Equal(t, admin.Email, ev.Actor)
		assert.Equal(t, admin.ID, ev.ActorID)
		assert.False(t, ev.OldEnabled)
		assert.True(t, ev.NewEnabled)
		assert.Equal(t, DefaultAutoUpdateWindowStartHourUTC, ev.OldHourUTC)
		assert.Equal(t, 6, ev.NewHourUTC)
		assert.False(t, ev.Timestamp.IsZero())

		// Second write: (true, 6) -> (false, 0).
		rec = doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{
			"enabled":            false,
			"windowStartHourUtc": 0,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Len(t, audit.maintenanceAutoUpdateEvents, 2)
		ev2 := audit.maintenanceAutoUpdateEvents[1]
		assert.Equal(t, MaintenanceAutoUpdateKindSettingChanged, ev2.Kind)
		assert.Equal(t, admin.Email, ev2.Actor)
		assert.True(t, ev2.OldEnabled)
		assert.False(t, ev2.NewEnabled)
		assert.Equal(t, 6, ev2.OldHourUTC)
		assert.Equal(t, 0, ev2.NewHourUTC)

		// Validation error emits no audit event.
		rec = doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{
			"enabled":            true,
			"windowStartHourUtc": 99,
		})
		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Len(t, audit.maintenanceAutoUpdateEvents, 2)
	})

	t.Run("first write from config auto reports oldEnabled true", func(t *testing.T) {
		mc := MaintenanceConfig{
			DeploymentTier: "binary",
			ReleaseChannel: "nightly",
			UpdatePolicy:   "auto",
			Installer:      newTestInstaller(maintenance.StrategySymlink),
		}
		srv, _, admin, _ := autoUpdateTestServer(t, mc)
		audit := &mockAuditLogger{}
		srv.SetAuditLogger(audit)

		rec := doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{
			"enabled": false,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Len(t, audit.maintenanceAutoUpdateEvents, 1)
		ev := audit.maintenanceAutoUpdateEvents[0]
		assert.True(t, ev.OldEnabled)
		assert.False(t, ev.NewEnabled)
		assert.Equal(t, DefaultAutoUpdateWindowStartHourUTC, ev.OldHourUTC)
		assert.Equal(t, DefaultAutoUpdateWindowStartHourUTC, ev.NewHourUTC)
	})

	t.Run("config-locked PUT emits no audit event", func(t *testing.T) {
		mc := MaintenanceConfig{
			DeploymentTier:       "binary",
			ReleaseChannel:       "nightly",
			UpdatePolicy:         "disabled",
			UpdatePolicyExplicit: true,
			Installer:            newTestInstaller(maintenance.StrategySymlink),
		}
		srv, _, admin, _ := autoUpdateTestServer(t, mc)
		audit := &mockAuditLogger{}
		srv.SetAuditLogger(audit)

		rec := doRequestAsUser(t, srv, admin, http.MethodPut, autoUpdateAdminPath, map[string]any{"enabled": true})
		require.Equal(t, http.StatusConflict, rec.Code)
		assert.Empty(t, audit.maintenanceAutoUpdateEvents)
	})
}

func TestLogAuditLogger_MaintenanceAutoUpdateEvent(t *testing.T) {
	ts := time.Date(2026, 11, 24, 4, 15, 0, 0, time.UTC)

	t.Run("setting_changed emits INFO with old and new fields", func(t *testing.T) {
		buf := captureAuditLogs(t)
		l := NewLogAuditLogger("[Test]", false)
		err := l.LogMaintenanceAutoUpdateEvent(context.Background(), &MaintenanceAutoUpdateEvent{
			Kind:       MaintenanceAutoUpdateKindSettingChanged,
			Actor:      "admin@example.com",
			ActorID:    "user-1",
			OldEnabled: false,
			NewEnabled: true,
			OldHourUTC: 0,
			NewHourUTC: 4,
			Timestamp:  ts,
		})
		require.NoError(t, err)
		rec := auditRecordWithMsg(t, buf, "maintenance auto-update audit event")
		require.NotNil(t, rec, "missing audit record in:\n%s", buf.String())
		assert.Equal(t, "INFO", rec["level"])
		assert.Equal(t, "maintenance_auto_update", rec["event_type"])
		assert.Equal(t, "setting_changed", rec["kind"])
		assert.Equal(t, "admin@example.com", rec["actor"])
		assert.Equal(t, "user-1", rec["actor_id"])
		assert.Equal(t, false, rec["old_enabled"])
		assert.Equal(t, true, rec["new_enabled"])
		assert.Equal(t, float64(0), rec["old_hour_utc"])
		assert.Equal(t, float64(4), rec["new_hour_utc"])
		assert.Equal(t, "2026-11-24T04:15:00Z", rec["timestamp"])
	})

	t.Run("run_started emits INFO with run fields", func(t *testing.T) {
		buf := captureAuditLogs(t)
		l := NewLogAuditLogger("[Test]", false)
		err := l.LogMaintenanceAutoUpdateEvent(context.Background(), &MaintenanceAutoUpdateEvent{
			Kind:      MaintenanceAutoUpdateKindRunStarted,
			Actor:     autoUpdateStartedBy,
			From:      "nightly-20261120",
			Target:    "nightly-20261124",
			RunID:     "run-1",
			Outcome:   AutoUpdateOutcomeStarted,
			Timestamp: ts,
		})
		require.NoError(t, err)
		rec := auditRecordWithMsg(t, buf, "maintenance auto-update audit event")
		require.NotNil(t, rec)
		assert.Equal(t, "INFO", rec["level"])
		assert.Equal(t, "run_started", rec["kind"])
		assert.Equal(t, autoUpdateStartedBy, rec["actor"])
		assert.Equal(t, "run-1", rec["run_id"])
		assert.Equal(t, "nightly-20261120", rec["from"])
		assert.Equal(t, "nightly-20261124", rec["target"])
		assert.Equal(t, AutoUpdateOutcomeStarted, rec["outcome"])
	})

	t.Run("run_outcome uses INFO for verified and WARN for failed/rolled_back", func(t *testing.T) {
		for _, tc := range []struct {
			outcome   string
			wantLevel string
		}{
			{AutoUpdateOutcomeVerified, "INFO"},
			{AutoUpdateOutcomeFailed, "WARN"},
			{AutoUpdateOutcomeRolledBack, "WARN"},
		} {
			t.Run(tc.outcome, func(t *testing.T) {
				buf := captureAuditLogs(t)
				l := NewLogAuditLogger("[Test]", false)
				err := l.LogMaintenanceAutoUpdateEvent(context.Background(), &MaintenanceAutoUpdateEvent{
					Kind:      MaintenanceAutoUpdateKindRunOutcome,
					Actor:     autoUpdateStartedBy,
					From:      "nightly-20261120",
					Target:    "nightly-20261124",
					RunID:     "run-2",
					Outcome:   tc.outcome,
					Timestamp: ts,
				})
				require.NoError(t, err)
				rec := auditRecordWithMsg(t, buf, "maintenance auto-update audit event")
				require.NotNil(t, rec)
				assert.Equal(t, tc.wantLevel, rec["level"])
				assert.Equal(t, "run_outcome", rec["kind"])
				assert.Equal(t, tc.outcome, rec["outcome"])
				assert.Equal(t, "run-2", rec["run_id"])
				assert.Equal(t, "nightly-20261120", rec["from"])
				assert.Equal(t, "nightly-20261124", rec["target"])
			})
		}
	})

	t.Run("nil receiver and nil event are safe no-ops", func(t *testing.T) {
		var nilLogger *LogAuditLogger
		assert.NoError(t, nilLogger.LogMaintenanceAutoUpdateEvent(context.Background(), &MaintenanceAutoUpdateEvent{}))
		assert.NoError(t, NewLogAuditLogger("[Test]", false).LogMaintenanceAutoUpdateEvent(context.Background(), nil))
	})
}
