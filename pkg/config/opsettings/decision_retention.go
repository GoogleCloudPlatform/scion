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
	"time"
)

// DecisionRetentionSectionName is the name of the operational settings section
// that bounds how long the hub keeps settled decisions (hub.decision_retention).
const DecisionRetentionSectionName = "decision_retention"

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
	// RetentionDays is 0 when no window is set (keep forever).
	RetentionDays       int
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

// HasWindow reports whether a hub-wide retention window is set.
func (c DecisionRetentionConfig) HasWindow() bool { return c.RetentionDays > 0 }

// TickBudget is TickBudgetSeconds as a duration.
func (c DecisionRetentionConfig) TickBudget() time.Duration {
	return time.Duration(c.TickBudgetSeconds) * time.Second
}

// TickInterval is TickIntervalMinutes as a duration.
func (c DecisionRetentionConfig) TickInterval() time.Duration {
	return time.Duration(c.TickIntervalMinutes) * time.Minute
}

// Settings returns the stored document form of c, with every field set.
// A config without a window stores no retention_days.
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
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"enabled":        map[string]interface{}{"type": "boolean"},
			"retention_days": intRange(DecisionRetentionMinDays, DecisionRetentionMaxDays),
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
