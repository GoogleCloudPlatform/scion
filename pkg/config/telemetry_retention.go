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
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Default and minimum Hub DB usage-ledger retention windows (Tokenomics §4.2).
// Explicit 0 means opt-in unlimited retention for any *_days field.
const (
	DefaultRetentionUsageEventsDays   = 45
	MinRetentionUsageEventsDays       = 35
	DefaultRetentionHourlyRollupsDays = 14
	MinRetentionHourlyRollupsDays     = 1
	DefaultRetentionDailyRollupsDays  = 365
	MinRetentionDailyRollupsDays      = 90
	DefaultRetentionScopeCountersDays = 90
	MinRetentionScopeCountersDays     = 65
	DefaultRetentionMeterHealthDays   = 30
	MinRetentionMeterHealthDays       = 1
	MinRetentionRunAttributionDays    = 365

	DefaultRetentionSweepInterval = 24 * time.Hour
	MinRetentionSweepInterval     = time.Minute
	DefaultRetentionBatchSize     = 5000
	MinRetentionBatchSize         = 1
	MaxRetentionBatchSize         = 50000
)

// ResolvedTelemetryRetention holds validated, defaulted Hub DB retention
// parameters ready for the retention sweeper and usage read paths.
// A 0 value on any *_Days field means unlimited retention (no deletion).
type ResolvedTelemetryRetention struct {
	UsageEventsDays    int
	HourlyRollupsDays  int
	DailyRollupsDays   int
	ScopeCountersDays  int
	MeterHealthDays    int
	RunAttributionDays int // derived: 0 if DailyRollupsDays == 0, else max(365, DailyRollupsDays)
	SweepInterval      time.Duration
	BatchSize          int
}

// DefaultResolvedTelemetryRetention returns the compiled default retention settings.
func DefaultResolvedTelemetryRetention() ResolvedTelemetryRetention {
	return ResolvedTelemetryRetention{
		UsageEventsDays:    DefaultRetentionUsageEventsDays,
		HourlyRollupsDays:  DefaultRetentionHourlyRollupsDays,
		DailyRollupsDays:   DefaultRetentionDailyRollupsDays,
		ScopeCountersDays:  DefaultRetentionScopeCountersDays,
		MeterHealthDays:    DefaultRetentionMeterHealthDays,
		RunAttributionDays: MinRetentionRunAttributionDays,
		SweepInterval:      DefaultRetentionSweepInterval,
		BatchSize:          DefaultRetentionBatchSize,
	}
}

// ResolveTelemetryRetention applies defaults to cfg (which may be nil) and
// derives RunAttributionDays. Values below minimum (other than 0 = unlimited)
// are clamped to their minimums defensively; callers validating user input
// should call ValidateTelemetryRetention first.
func ResolveTelemetryRetention(cfg *api.TelemetryRetentionConfig) ResolvedTelemetryRetention {
	out := DefaultResolvedTelemetryRetention()
	if cfg == nil {
		return out
	}
	out.UsageEventsDays = resolveRetentionDays(cfg.UsageEventsDays, DefaultRetentionUsageEventsDays, MinRetentionUsageEventsDays)
	out.HourlyRollupsDays = resolveRetentionDays(cfg.HourlyRollupsDays, DefaultRetentionHourlyRollupsDays, MinRetentionHourlyRollupsDays)
	out.DailyRollupsDays = resolveRetentionDays(cfg.DailyRollupsDays, DefaultRetentionDailyRollupsDays, MinRetentionDailyRollupsDays)
	out.ScopeCountersDays = resolveRetentionDays(cfg.ScopeCountersDays, DefaultRetentionScopeCountersDays, MinRetentionScopeCountersDays)
	out.MeterHealthDays = resolveRetentionDays(cfg.MeterHealthDays, DefaultRetentionMeterHealthDays, MinRetentionMeterHealthDays)

	if out.DailyRollupsDays == 0 {
		out.RunAttributionDays = 0
	} else {
		out.RunAttributionDays = max(MinRetentionRunAttributionDays, out.DailyRollupsDays)
	}

	if s := strings.TrimSpace(cfg.SweepInterval); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= MinRetentionSweepInterval {
			out.SweepInterval = d
		}
	}
	if cfg.BatchSize != nil {
		bs := *cfg.BatchSize
		if bs < MinRetentionBatchSize {
			bs = DefaultRetentionBatchSize
		} else if bs > MaxRetentionBatchSize {
			bs = MaxRetentionBatchSize
		}
		out.BatchSize = bs
	}
	return out
}

func resolveRetentionDays(ptr *int, def, minVal int) int {
	if ptr == nil {
		return def
	}
	v := *ptr
	if v == 0 {
		return 0
	}
	if v < minVal {
		return minVal
	}
	return v
}

// ValidateTelemetryRetention validates telemetry.retention.* values, rejecting
// negative values, non-zero values below each table's minimum, invalid sweep
// intervals, and out-of-range batch sizes. Explicit 0 on *_days fields is valid
// (opt-in unlimited retention).
func ValidateTelemetryRetention(cfg *api.TelemetryRetentionConfig) error {
	if cfg == nil {
		return nil
	}
	if err := validateRetentionDaysField("usage_events_days", cfg.UsageEventsDays, MinRetentionUsageEventsDays); err != nil {
		return err
	}
	if err := validateRetentionDaysField("hourly_rollups_days", cfg.HourlyRollupsDays, MinRetentionHourlyRollupsDays); err != nil {
		return err
	}
	if err := validateRetentionDaysField("daily_rollups_days", cfg.DailyRollupsDays, MinRetentionDailyRollupsDays); err != nil {
		return err
	}
	if err := validateRetentionDaysField("scope_counters_days", cfg.ScopeCountersDays, MinRetentionScopeCountersDays); err != nil {
		return err
	}
	if err := validateRetentionDaysField("meter_health_days", cfg.MeterHealthDays, MinRetentionMeterHealthDays); err != nil {
		return err
	}
	if s := strings.TrimSpace(cfg.SweepInterval); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("telemetry.retention.sweep_interval: invalid duration %q: %w", cfg.SweepInterval, err)
		}
		if d < MinRetentionSweepInterval {
			return fmt.Errorf("telemetry.retention.sweep_interval: must be at least %s, got %s", MinRetentionSweepInterval, d)
		}
	}
	if cfg.BatchSize != nil {
		bs := *cfg.BatchSize
		if bs < MinRetentionBatchSize || bs > MaxRetentionBatchSize {
			return fmt.Errorf("telemetry.retention.batch_size: must be between %d and %d, got %d",
				MinRetentionBatchSize, MaxRetentionBatchSize, bs)
		}
	}
	return nil
}

func validateRetentionDaysField(name string, ptr *int, minVal int) error {
	if ptr == nil {
		return nil
	}
	v := *ptr
	if v < 0 {
		return fmt.Errorf("telemetry.retention.%s: must be 0 (unlimited) or >= %d, got %d", name, minVal, v)
	}
	if v > 0 && v < minVal {
		return fmt.Errorf("telemetry.retention.%s: must be 0 (unlimited) or >= %d, got %d", name, minVal, v)
	}
	return nil
}

// ValidateV1TelemetryRetention applies ValidateTelemetryRetention to a
// settings-file (or hub telemetry section) V1TelemetryRetentionConfig.
func ValidateV1TelemetryRetention(cfg *V1TelemetryRetentionConfig) error {
	if cfg == nil {
		return nil
	}
	return ValidateTelemetryRetention(&api.TelemetryRetentionConfig{
		UsageEventsDays:   cfg.UsageEventsDays,
		HourlyRollupsDays: cfg.HourlyRollupsDays,
		DailyRollupsDays:  cfg.DailyRollupsDays,
		ScopeCountersDays: cfg.ScopeCountersDays,
		MeterHealthDays:   cfg.MeterHealthDays,
		SweepInterval:     cfg.SweepInterval,
		BatchSize:         cfg.BatchSize,
	})
}
