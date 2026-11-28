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

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveTelemetryRetention_Defaults(t *testing.T) {
	got := ResolveTelemetryRetention(nil)
	assert.Equal(t, 45, got.UsageEventsDays)
	assert.Equal(t, 14, got.HourlyRollupsDays)
	assert.Equal(t, 365, got.DailyRollupsDays)
	assert.Equal(t, 90, got.ScopeCountersDays)
	assert.Equal(t, 365, got.GroupRollupsDays)
	assert.Equal(t, 90, got.BudgetStateDays)
	assert.Equal(t, 30, got.MeterHealthDays)
	assert.Equal(t, 365, got.RunAttributionDays)
	assert.Equal(t, 24*time.Hour, got.SweepInterval)
	assert.Equal(t, 5000, got.BatchSize)
}

func TestResolveTelemetryRetention_UnlimitedAndDerivedRunAttribution(t *testing.T) {
	t.Run("explicit zero means unlimited including derived run_attribution", func(t *testing.T) {
		got := ResolveTelemetryRetention(&api.TelemetryRetentionConfig{
			UsageEventsDays:   intPtr(0),
			HourlyRollupsDays: intPtr(0),
			DailyRollupsDays:  intPtr(0),
			ScopeCountersDays: intPtr(0),
			GroupRollupsDays:  intPtr(0),
			BudgetStateDays:   intPtr(0),
			MeterHealthDays:   intPtr(0),
		})
		assert.Equal(t, 0, got.UsageEventsDays)
		assert.Equal(t, 0, got.HourlyRollupsDays)
		assert.Equal(t, 0, got.DailyRollupsDays)
		assert.Equal(t, 0, got.ScopeCountersDays)
		assert.Equal(t, 0, got.GroupRollupsDays)
		assert.Equal(t, 0, got.BudgetStateDays)
		assert.Equal(t, 0, got.MeterHealthDays)
		assert.Equal(t, 0, got.RunAttributionDays)
	})

	t.Run("run_attribution derives max(365, daily_rollups_days)", func(t *testing.T) {
		short := ResolveTelemetryRetention(&api.TelemetryRetentionConfig{DailyRollupsDays: intPtr(90)})
		assert.Equal(t, 90, short.DailyRollupsDays)
		assert.Equal(t, 365, short.RunAttributionDays)

		long := ResolveTelemetryRetention(&api.TelemetryRetentionConfig{DailyRollupsDays: intPtr(500)})
		assert.Equal(t, 500, long.DailyRollupsDays)
		assert.Equal(t, 500, long.RunAttributionDays)
	})

	t.Run("custom sweep_interval and batch_size", func(t *testing.T) {
		got := ResolveTelemetryRetention(&api.TelemetryRetentionConfig{
			SweepInterval: "6h",
			BatchSize:     intPtr(2000),
		})
		assert.Equal(t, 6*time.Hour, got.SweepInterval)
		assert.Equal(t, 2000, got.BatchSize)
	})
}

func TestValidateTelemetryRetention(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *api.TelemetryRetentionConfig
		wantErr string
	}{
		{name: "nil", cfg: nil},
		{name: "empty", cfg: &api.TelemetryRetentionConfig{}},
		{name: "all explicit zero unlimited", cfg: &api.TelemetryRetentionConfig{
			UsageEventsDays:   intPtr(0),
			HourlyRollupsDays: intPtr(0),
			DailyRollupsDays:  intPtr(0),
			ScopeCountersDays: intPtr(0),
			MeterHealthDays:   intPtr(0),
		}},
		{name: "exact minimums", cfg: &api.TelemetryRetentionConfig{
			UsageEventsDays:   intPtr(35),
			HourlyRollupsDays: intPtr(1),
			DailyRollupsDays:  intPtr(90),
			ScopeCountersDays: intPtr(65),
			MeterHealthDays:   intPtr(1),
			SweepInterval:     "1m",
			BatchSize:         intPtr(1),
		}},
		{name: "usage_events below min", cfg: &api.TelemetryRetentionConfig{UsageEventsDays: intPtr(34)}, wantErr: "usage_events_days"},
		{name: "usage_events negative", cfg: &api.TelemetryRetentionConfig{UsageEventsDays: intPtr(-1)}, wantErr: "usage_events_days"},
		{name: "hourly_rollups negative", cfg: &api.TelemetryRetentionConfig{HourlyRollupsDays: intPtr(-1)}, wantErr: "hourly_rollups_days"},
		{name: "daily_rollups below min", cfg: &api.TelemetryRetentionConfig{DailyRollupsDays: intPtr(89)}, wantErr: "daily_rollups_days"},
		{name: "scope_counters below min", cfg: &api.TelemetryRetentionConfig{ScopeCountersDays: intPtr(64)}, wantErr: "scope_counters_days"},
		{name: "group_rollups below min", cfg: &api.TelemetryRetentionConfig{GroupRollupsDays: intPtr(89)}, wantErr: "group_rollups_days"},
		{name: "group_rollups negative", cfg: &api.TelemetryRetentionConfig{GroupRollupsDays: intPtr(-1)}, wantErr: "group_rollups_days"},
		{name: "budget_state negative", cfg: &api.TelemetryRetentionConfig{BudgetStateDays: intPtr(-1)}, wantErr: "budget_state_days"},
		{name: "meter_health negative", cfg: &api.TelemetryRetentionConfig{MeterHealthDays: intPtr(-5)}, wantErr: "meter_health_days"},
		{name: "sweep_interval too short", cfg: &api.TelemetryRetentionConfig{SweepInterval: "30s"}, wantErr: "sweep_interval"},
		{name: "sweep_interval invalid", cfg: &api.TelemetryRetentionConfig{SweepInterval: "nightly"}, wantErr: "sweep_interval"},
		{name: "batch_size zero", cfg: &api.TelemetryRetentionConfig{BatchSize: intPtr(0)}, wantErr: "batch_size"},
		{name: "batch_size too large", cfg: &api.TelemetryRetentionConfig{BatchSize: intPtr(50001)}, wantErr: "batch_size"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTelemetryRetention(tc.cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestConvertV1TelemetryToAPI_PreservesRetentionNotEmittedToEnv(t *testing.T) {
	v1 := &V1TelemetryConfig{
		Retention: &V1TelemetryRetentionConfig{
			UsageEventsDays:   intPtr(40),
			HourlyRollupsDays: intPtr(7),
			SweepInterval:     "12h",
			BatchSize:         intPtr(1000),
		},
	}
	apiCfg := ConvertV1TelemetryToAPI(v1)
	require.NotNil(t, apiCfg)
	require.NotNil(t, apiCfg.Retention)
	assert.Equal(t, 40, *apiCfg.Retention.UsageEventsDays)
	assert.Equal(t, 7, *apiCfg.Retention.HourlyRollupsDays)
	assert.Equal(t, "12h", apiCfg.Retention.SweepInterval)
	assert.Equal(t, 1000, *apiCfg.Retention.BatchSize)

	// Hub-only retention config must never be emitted to agent container env.
	env := TelemetryConfigToEnv(apiCfg)
	assert.Empty(t, env)
}

func TestLoadVersionedSettings_TelemetryRetentionValidation(t *testing.T) {
	t.Run("below-minimum value is rejected on load", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(
			"schema_version: \"1\"\ntelemetry:\n  retention:\n    usage_events_days: 10\n",
		), 0644))
		_, err := LoadVersionedSettings(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "usage_events_days")
	})

	t.Run("explicit zero loads and resolves to unlimited", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(
			"schema_version: \"1\"\ntelemetry:\n  retention:\n    usage_events_days: 0\n",
		), 0644))
		settings, err := LoadVersionedSettings(dir)
		require.NoError(t, err)
		require.NotNil(t, settings.Telemetry)
		require.NotNil(t, settings.Telemetry.Retention)
		require.NotNil(t, settings.Telemetry.Retention.UsageEventsDays)
		assert.Equal(t, 0, *settings.Telemetry.Retention.UsageEventsDays)

		resolved := ResolveTelemetryRetention(ConvertV1TelemetryToAPI(settings.Telemetry).Retention)
		assert.Equal(t, 0, resolved.UsageEventsDays)
	})
}
