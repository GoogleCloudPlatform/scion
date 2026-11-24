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

	t.Run("eligible and enabled is active", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, UpdateModeRelease, "nightly", maintenance.PinLatest, true, false)
		assert.True(t, got.Enabled)
		assert.True(t, got.Active)
		assert.Empty(t, got.InactiveReason)
	})

	t.Run("eligible and disabled is inactive with empty reason", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, nil, UpdateModeRelease, "nightly", maintenance.PinLatest, true, false)
		assert.False(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Empty(t, got.InactiveReason)
	})

	t.Run("config locked", func(t *testing.T) {
		lockedMC := MaintenanceConfig{UpdatePolicy: "disabled", UpdatePolicyExplicit: true}
		got := buildAutoUpdateStatus(lockedMC, enabledSetting, UpdateModeRelease, "nightly", maintenance.PinLatest, true, false)
		assert.False(t, got.Enabled)
		assert.True(t, got.Locked)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveConfigLocked, got.InactiveReason)
	})

	t.Run("git mode", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, UpdateModeGit, "", maintenance.PinLatest, true, false)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveGitMode, got.InactiveReason)
	})

	t.Run("pinned", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, UpdateModeRelease, "nightly", "nightly-20261124", true, false)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactivePinned, got.InactiveReason)
	})

	t.Run("installer unsupported", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, UpdateModeRelease, "nightly", maintenance.PinLatest, false, false)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveInstallerUnsupported, got.InactiveReason)
	})

	t.Run("no channel", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, UpdateModeRelease, "", maintenance.PinLatest, true, false)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveNoChannel, got.InactiveReason)
	})

	t.Run("maintenance mode", func(t *testing.T) {
		got := buildAutoUpdateStatus(notifyMC, enabledSetting, UpdateModeRelease, "nightly", maintenance.PinLatest, true, true)
		assert.True(t, got.Enabled)
		assert.False(t, got.Active)
		assert.Equal(t, AutoUpdateInactiveMaintenanceMode, got.InactiveReason)
	})
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
}
