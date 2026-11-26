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

package opsettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
)

// DecisionRetentionSectionName is the name of the operational settings section
// that bounds how long the hub keeps settled decisions (hub.decision_retention).
const DecisionRetentionSectionName = "decision_retention"

// Per-project decision retention override modes (design-8984 §3.1).
const (
	DecisionRetentionOverrideModeDays        = "days"
	DecisionRetentionOverrideModeKeepForever = "keep_forever"
)

// decisionRetentionUUIDPattern matches canonical lowercase UUIDs used as
// project_overrides keys.
const decisionRetentionUUIDPattern = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// Compiled defaults and bounds of the decision_retention section (design-8984 §3.1).
const (
	// DecisionRetentionMinDays is the shortest retention_days a hub admin may set.
	DecisionRetentionMinDays = 30
	// DecisionRetentionMaxDays bounds retention_days (100 years).
	DecisionRetentionMaxDays = 36500

	DecisionRetentionDefaultPageSize = 200
	DecisionRetentionMinPageSize     = 50
	DecisionRetentionMaxPageSize     = 1000

	// TickBudgetSeconds keeps a tick inside the hub scheduler's 55 s handler
	// context, leaving room for a final page transaction and end-of-tick writes.
	DecisionRetentionDefaultTickBudgetSeconds = 30
	DecisionRetentionMinTickBudgetSeconds     = 1
	DecisionRetentionMaxTickBudgetSeconds     = 40

	DecisionRetentionDefaultTickIntervalMinutes = 60
	DecisionRetentionMinTickIntervalMinutes     = 1
	DecisionRetentionMaxTickIntervalMinutes     = 1440
)

// DecisionRetentionOverride overrides the hub-wide retention window for one
// project (design-8984 §3.1).
type DecisionRetentionOverride struct {
	// Mode is "days" or "keep_forever".
	Mode string `json:"mode"`
	// Days is required iff Mode == "days" (30..36500); must be nil for "keep_forever".
	Days *int `json:"days,omitempty"`
}

// Equal reports whether o equals other.
func (o DecisionRetentionOverride) Equal(other DecisionRetentionOverride) bool {
	if o.Mode != other.Mode {
		return false
	}
	if (o.Days == nil) != (other.Days == nil) {
		return false
	}
	if o.Days != nil && *o.Days != *other.Days {
		return false
	}
	return true
}

// Clone returns a deep copy of o.
func (o DecisionRetentionOverride) Clone() DecisionRetentionOverride {
	out := DecisionRetentionOverride{Mode: o.Mode}
	if o.Days != nil {
		d := *o.Days
		out.Days = &d
	}
	return out
}

// DecisionRetentionSettings is the stored decision_retention section. DB-only
// (runtime state), no settings.yaml representation, written through PUT
// /api/v1/admin/decision-retention. Absent row = compiled defaults: disabled,
// no window (keep forever).
type DecisionRetentionSettings struct {
	// Enabled turns the retention sweep on. Default false. It doubles as
	// the hub-wide legal hold: while it is false nothing is purged.
	Enabled *bool `json:"enabled,omitempty"`
	// RetentionDays is the hub-wide age window in days. Absent = keep forever.
	// When set it is between DecisionRetentionMinDays (30) and
	// DecisionRetentionMaxDays (36500).
	RetentionDays *int `json:"retention_days,omitempty"`
	// ProjectOverrides maps project UUID -> per-project override.
	ProjectOverrides map[string]DecisionRetentionOverride `json:"project_overrides,omitempty"`
	// Sweep holds the sweep tuning; absent fields take their defaults.
	Sweep *DecisionRetentionSweepSettings `json:"sweep,omitempty"`
}

// DecisionRetentionSweepSettings is the sweep tuning of the decision_retention
// section. Every field is optional.
type DecisionRetentionSweepSettings struct {
	// PageSize is the largest number of candidate decision rows scanned per
	// page (50..1000, default 200).
	PageSize *int `json:"page_size,omitempty"`
	// TickBudgetSeconds is the wall time one tick may run (1..40, default 30).
	TickBudgetSeconds *int `json:"tick_budget_seconds,omitempty"`
	// TickIntervalMinutes is the time between ticks (1..1440, default 60).
	TickIntervalMinutes *int `json:"tick_interval_minutes,omitempty"`
}

// DecisionRetentionConfig is the resolved decision_retention section.
type DecisionRetentionConfig struct {
	Enabled bool
	// RetentionDays is 0 when no hub-wide window is set (keep forever).
	RetentionDays int
	// ProjectOverrides maps project UUID -> validated per-project override.
	// Nil when no overrides are configured.
	ProjectOverrides    map[string]DecisionRetentionOverride
	PageSize            int
	TickBudgetSeconds   int
	TickIntervalMinutes int
}

// DefaultDecisionRetentionConfig is the resolved value of an absent section.
func DefaultDecisionRetentionConfig() DecisionRetentionConfig {
	return DecisionRetentionConfig{
		PageSize:            DecisionRetentionDefaultPageSize,
		TickBudgetSeconds:   DecisionRetentionDefaultTickBudgetSeconds,
		TickIntervalMinutes: DecisionRetentionDefaultTickIntervalMinutes,
	}
}

// Clone returns a deep copy of c so callers may mutate ProjectOverrides safely.
func (c DecisionRetentionConfig) Clone() DecisionRetentionConfig {
	out := c
	if len(c.ProjectOverrides) > 0 {
		out.ProjectOverrides = make(map[string]DecisionRetentionOverride, len(c.ProjectOverrides))
		for k, v := range c.ProjectOverrides {
			out.ProjectOverrides[k] = v.Clone()
		}
	} else {
		out.ProjectOverrides = nil
	}
	return out
}

// Equal reports whether c and other have identical resolved values.
func (c DecisionRetentionConfig) Equal(other DecisionRetentionConfig) bool {
	if c.Enabled != other.Enabled ||
		c.RetentionDays != other.RetentionDays ||
		c.PageSize != other.PageSize ||
		c.TickBudgetSeconds != other.TickBudgetSeconds ||
		c.TickIntervalMinutes != other.TickIntervalMinutes {
		return false
	}
	return maps.EqualFunc(c.ProjectOverrides, other.ProjectOverrides, func(a, b DecisionRetentionOverride) bool {
		return a.Equal(b)
	})
}

// HasWindow reports whether a hub-wide retention window is set.
func (c DecisionRetentionConfig) HasWindow() bool { return c.RetentionDays > 0 }

// HasAnyWindow reports whether the hub default or any project override sets a
// positive retention window.
func (c DecisionRetentionConfig) HasAnyWindow() bool {
	_, ok := c.MinEffectiveDays()
	return ok
}

// EffectiveDays returns the effective retention window in days for projectID:
// override if present (keep_forever returns 0, false), else hub default, else
// none (0, false).
func (c DecisionRetentionConfig) EffectiveDays(projectID string) (int, bool) {
	if ov, ok := c.ProjectOverrides[projectID]; ok {
		if ov.Mode == DecisionRetentionOverrideModeDays && ov.Days != nil && *ov.Days > 0 {
			return *ov.Days, true
		}
		return 0, false
	}
	if c.RetentionDays > 0 {
		return c.RetentionDays, true
	}
	return 0, false
}

// MinEffectiveDays returns the smallest positive retention window in use across
// the hub default and all project overrides, or (0, false) if none exists.
func (c DecisionRetentionConfig) MinEffectiveDays() (int, bool) {
	minDays := 0
	if c.RetentionDays > 0 {
		minDays = c.RetentionDays
	}
	for _, ov := range c.ProjectOverrides {
		if ov.Mode == DecisionRetentionOverrideModeDays && ov.Days != nil && *ov.Days > 0 {
			if minDays == 0 || *ov.Days < minDays {
				minDays = *ov.Days
			}
		}
	}
	if minDays <= 0 {
		return 0, false
	}
	return minDays, true
}

// TickBudget is TickBudgetSeconds as a duration.
func (c DecisionRetentionConfig) TickBudget() time.Duration {
	return time.Duration(c.TickBudgetSeconds) * time.Second
}

// TickInterval is TickIntervalMinutes as a duration.
func (c DecisionRetentionConfig) TickInterval() time.Duration {
	return time.Duration(c.TickIntervalMinutes) * time.Minute
}

// Settings returns the stored document form of c, with every field set.
// A config without a window stores no retention_days; empty overrides are omitted.
func (c DecisionRetentionConfig) Settings() DecisionRetentionSettings {
	enabled := c.Enabled
	pageSize, budget, interval := c.PageSize, c.TickBudgetSeconds, c.TickIntervalMinutes
	s := DecisionRetentionSettings{
		Enabled: &enabled,
		Sweep: &DecisionRetentionSweepSettings{
			PageSize:            &pageSize,
			TickBudgetSeconds:   &budget,
			TickIntervalMinutes: &interval,
		},
	}
	if c.HasWindow() {
		days := c.RetentionDays
		s.RetentionDays = &days
	}
	if len(c.ProjectOverrides) > 0 {
		s.ProjectOverrides = make(map[string]DecisionRetentionOverride, len(c.ProjectOverrides))
		for k, v := range c.ProjectOverrides {
			s.ProjectOverrides[k] = v.Clone()
		}
	}
	return s
}

// ParseDecisionRetentionDoc resolves a stored decision_retention document.
// Unknown fields and out-of-range values are errors: callers treat an error as
// "retention off" (fail closed), never as a default window.
func ParseDecisionRetentionDoc(raw json.RawMessage) (DecisionRetentionConfig, error) {
	var s DecisionRetentionSettings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return DecisionRetentionConfig{}, fmt.Errorf("decision_retention settings: %w", err)
	}
	return s.Resolve()
}

// Resolve applies the compiled defaults to s and checks every bound.
func (s DecisionRetentionSettings) Resolve() (DecisionRetentionConfig, error) {
	c := DefaultDecisionRetentionConfig()
	if s.Enabled != nil {
		c.Enabled = *s.Enabled
	}
	if s.RetentionDays != nil {
		d := *s.RetentionDays
		if d < DecisionRetentionMinDays || d > DecisionRetentionMaxDays {
			return DecisionRetentionConfig{}, fmt.Errorf("decision_retention settings: retention_days must be between %d and %d", DecisionRetentionMinDays, DecisionRetentionMaxDays)
		}
		c.RetentionDays = d
	}
	if len(s.ProjectOverrides) > 0 {
		overrides := make(map[string]DecisionRetentionOverride, len(s.ProjectOverrides))
		for pid, ov := range s.ProjectOverrides {
			u, err := uuid.Parse(pid)
			if err != nil || u.String() != pid {
				return DecisionRetentionConfig{}, fmt.Errorf("decision_retention settings: project_overrides key %q must be a canonical lowercase project UUID", pid)
			}
			switch ov.Mode {
			case DecisionRetentionOverrideModeDays:
				if ov.Days == nil {
					return DecisionRetentionConfig{}, fmt.Errorf("decision_retention settings: project_overrides[%s].days is required when mode is %q", pid, DecisionRetentionOverrideModeDays)
				}
				d := *ov.Days
				if d < DecisionRetentionMinDays || d > DecisionRetentionMaxDays {
					return DecisionRetentionConfig{}, fmt.Errorf("decision_retention settings: project_overrides[%s].days must be between %d and %d", pid, DecisionRetentionMinDays, DecisionRetentionMaxDays)
				}
				overrides[pid] = DecisionRetentionOverride{Mode: DecisionRetentionOverrideModeDays, Days: &d}
			case DecisionRetentionOverrideModeKeepForever:
				if ov.Days != nil {
					return DecisionRetentionConfig{}, fmt.Errorf("decision_retention settings: project_overrides[%s].days must be omitted when mode is %q", pid, DecisionRetentionOverrideModeKeepForever)
				}
				overrides[pid] = DecisionRetentionOverride{Mode: DecisionRetentionOverrideModeKeepForever}
			default:
				return DecisionRetentionConfig{}, fmt.Errorf("decision_retention settings: project_overrides[%s].mode must be %q or %q", pid, DecisionRetentionOverrideModeDays, DecisionRetentionOverrideModeKeepForever)
			}
		}
		c.ProjectOverrides = overrides
	}
	if sw := s.Sweep; sw != nil {
		for _, f := range []struct {
			name     string
			v        *int
			min, max int
			dst      *int
		}{
			{"page_size", sw.PageSize, DecisionRetentionMinPageSize, DecisionRetentionMaxPageSize, &c.PageSize},
			{"tick_budget_seconds", sw.TickBudgetSeconds, DecisionRetentionMinTickBudgetSeconds, DecisionRetentionMaxTickBudgetSeconds, &c.TickBudgetSeconds},
			{"tick_interval_minutes", sw.TickIntervalMinutes, DecisionRetentionMinTickIntervalMinutes, DecisionRetentionMaxTickIntervalMinutes, &c.TickIntervalMinutes},
		} {
			if f.v == nil {
				continue
			}
			if *f.v < f.min || *f.v > f.max {
				return DecisionRetentionConfig{}, fmt.Errorf("decision_retention settings: sweep.%s must be between %d and %d", f.name, f.min, f.max)
			}
			*f.dst = *f.v
		}
	}
	return c, nil
}

// decisionRetentionSchema is the hand-written JSON schema of the section; it
// is runtime/API-owned state with no $defs in settings-v1.schema.json. Its
// bounds match Resolve.
func decisionRetentionSchema() map[string]interface{} {
	intRange := func(min, max int) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "minimum": min, "maximum": max}
	}
	overrideSchema := map[string]interface{}{
		"oneOf": []interface{}{
			map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"mode": map[string]interface{}{"const": DecisionRetentionOverrideModeDays},
					"days": intRange(DecisionRetentionMinDays, DecisionRetentionMaxDays),
				},
				"required":             []interface{}{"mode", "days"},
				"additionalProperties": false,
			},
			map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"mode": map[string]interface{}{"const": DecisionRetentionOverrideModeKeepForever},
				},
				"required":             []interface{}{"mode"},
				"additionalProperties": false,
			},
		},
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"enabled":        map[string]interface{}{"type": "boolean"},
			"retention_days": intRange(DecisionRetentionMinDays, DecisionRetentionMaxDays),
			"project_overrides": map[string]interface{}{
				"type": "object",
				"propertyNames": map[string]interface{}{
					"pattern": decisionRetentionUUIDPattern,
				},
				"additionalProperties": overrideSchema,
			},
			"sweep": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"page_size":             intRange(DecisionRetentionMinPageSize, DecisionRetentionMaxPageSize),
					"tick_budget_seconds":   intRange(DecisionRetentionMinTickBudgetSeconds, DecisionRetentionMaxTickBudgetSeconds),
					"tick_interval_minutes": intRange(DecisionRetentionMinTickIntervalMinutes, DecisionRetentionMaxTickIntervalMinutes),
				},
				"additionalProperties": false,
			},
		},
		"additionalProperties": false,
	}
}
