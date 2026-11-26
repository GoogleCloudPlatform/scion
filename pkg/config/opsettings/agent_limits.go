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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// AgentLimitsSectionName is the name of the operational settings section
// holding Hub-level run-limit ceilings.
const AgentLimitsSectionName = "agent_limits"

// AgentLimitsSettings holds Hub-level upper bounds on agent run limits.
// DB-only (runtime state), no settings.yaml representation. Absent row or
// zero/empty fields mean no ceiling for that limit.
//
// When a ceiling is set to N (> 0), effective limits are always in [1, N];
// "Unlimited" (0 / "0") is unavailable.
type AgentLimitsSettings struct {
	MaxTurns      int    `json:"max_turns,omitempty"`       // 0 = no ceiling
	MaxModelCalls int    `json:"max_model_calls,omitempty"` // 0 = no ceiling
	MaxDuration   string `json:"max_duration,omitempty"`    // "" = no ceiling; api.ParseDuration, > 0
}

// IsEmpty reports whether no ceiling is configured.
func (s AgentLimitsSettings) IsEmpty() bool {
	return s.MaxTurns <= 0 && s.MaxModelCalls <= 0 && strings.TrimSpace(s.MaxDuration) == ""
}

// ToAPI converts s into the wire LimitCeilings form, returning nil when no
// ceiling is configured.
func (s AgentLimitsSettings) ToAPI() *api.LimitCeilings {
	if s.IsEmpty() {
		return nil
	}
	return &api.LimitCeilings{
		MaxTurns:      s.MaxTurns,
		MaxModelCalls: s.MaxModelCalls,
		MaxDuration:   strings.TrimSpace(s.MaxDuration),
	}
}

// ParseAgentLimitsDoc parses and validates a stored agent_limits document.
// Unknown fields, negative counts, and unparsable or non-positive durations
// are errors.
func ParseAgentLimitsDoc(raw json.RawMessage) (AgentLimitsSettings, error) {
	var doc AgentLimitsSettings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return AgentLimitsSettings{}, fmt.Errorf("agent_limits settings: %w", err)
	}
	if dec.More() {
		return AgentLimitsSettings{}, fmt.Errorf("agent_limits settings: trailing data")
	}
	if doc.MaxTurns < 0 {
		return AgentLimitsSettings{}, fmt.Errorf("agent_limits settings: max_turns must be >= 0, got %d", doc.MaxTurns)
	}
	if doc.MaxModelCalls < 0 {
		return AgentLimitsSettings{}, fmt.Errorf("agent_limits settings: max_model_calls must be >= 0, got %d", doc.MaxModelCalls)
	}
	doc.MaxDuration = strings.TrimSpace(doc.MaxDuration)
	if doc.MaxDuration != "" {
		if d := api.ParseDuration(doc.MaxDuration); d <= 0 {
			return AgentLimitsSettings{}, fmt.Errorf("agent_limits settings: max_duration must be a positive duration (leave empty for no ceiling), got %q", doc.MaxDuration)
		}
	}
	return doc, nil
}

// ValidateAgentDefaultsAgainstLimits checks that agent_defaults limit defaults
// do not exceed configured agent_limits ceilings. Errors point at both the
// agent_defaults key and the agent_limits ceiling key.
func ValidateAgentDefaultsAgainstLimits(defaults AgentDefaultsSettings, limits AgentLimitsSettings) []config.ValidationError {
	return validateAgentDefaultsAgainstLimits(defaults, limits, false, false, false)
}

// ValidateAgentDefaultsDocAgainstLimits checks a raw JSON agent_defaults
// document against limits, additionally refusing explicit 0 / "" (unlimited)
// defaults when the corresponding ceiling is configured.
func ValidateAgentDefaultsDocAgainstLimits(defaultsDoc json.RawMessage, limits AgentLimitsSettings) []config.ValidationError {
	if limits.IsEmpty() || len(bytes.TrimSpace(defaultsDoc)) == 0 {
		return nil
	}
	var defaults AgentDefaultsSettings
	if err := json.Unmarshal(defaultsDoc, &defaults); err != nil {
		return nil // schema validation reports malformed JSON
	}
	var rawFields map[string]json.RawMessage
	_ = json.Unmarshal(defaultsDoc, &rawFields)
	hasKey := func(k string) bool {
		v, ok := rawFields[k]
		if !ok {
			return false
		}
		return string(bytes.TrimSpace(v)) != "null"
	}
	return validateAgentDefaultsAgainstLimits(
		defaults,
		limits,
		hasKey("default_max_turns"),
		hasKey("default_max_model_calls"),
		hasKey("default_max_duration"),
	)
}

func validateAgentDefaultsAgainstLimits(
	defaults AgentDefaultsSettings,
	limits AgentLimitsSettings,
	explicitTurns, explicitCalls, explicitDuration bool,
) []config.ValidationError {
	if limits.IsEmpty() {
		return nil
	}
	var errs []config.ValidationError

	if limits.MaxTurns > 0 {
		switch {
		case defaults.DefaultMaxTurns > limits.MaxTurns:
			errs = append(errs, config.ValidationError{
				Path: "default_max_turns",
				Message: fmt.Sprintf(
					"agent_defaults.default_max_turns (%d) exceeds agent_limits.max_turns ceiling (%d)",
					defaults.DefaultMaxTurns, limits.MaxTurns,
				),
			})
		case explicitTurns && defaults.DefaultMaxTurns == 0:
			errs = append(errs, config.ValidationError{
				Path: "default_max_turns",
				Message: fmt.Sprintf(
					"agent_defaults.default_max_turns cannot be 0 (unlimited) while agent_limits.max_turns ceiling (%d) is set",
					limits.MaxTurns,
				),
			})
		}
	}

	if limits.MaxModelCalls > 0 {
		switch {
		case defaults.DefaultMaxModelCalls > limits.MaxModelCalls:
			errs = append(errs, config.ValidationError{
				Path: "default_max_model_calls",
				Message: fmt.Sprintf(
					"agent_defaults.default_max_model_calls (%d) exceeds agent_limits.max_model_calls ceiling (%d)",
					defaults.DefaultMaxModelCalls, limits.MaxModelCalls,
				),
			})
		case explicitCalls && defaults.DefaultMaxModelCalls == 0:
			errs = append(errs, config.ValidationError{
				Path: "default_max_model_calls",
				Message: fmt.Sprintf(
					"agent_defaults.default_max_model_calls cannot be 0 (unlimited) while agent_limits.max_model_calls ceiling (%d) is set",
					limits.MaxModelCalls,
				),
			})
		}
	}

	if ceilingStr := strings.TrimSpace(limits.MaxDuration); ceilingStr != "" {
		if ceilingDur := api.ParseDuration(ceilingStr); ceilingDur > 0 {
			defStr := strings.TrimSpace(defaults.DefaultMaxDuration)
			switch {
			case defStr != "":
				defDur := api.ParseDuration(defStr)
				if defDur <= 0 {
					errs = append(errs, config.ValidationError{
						Path: "default_max_duration",
						Message: fmt.Sprintf(
							"agent_defaults.default_max_duration (%q) must be a positive duration while agent_limits.max_duration ceiling (%s) is set",
							defStr, ceilingStr,
						),
					})
				} else if defDur > ceilingDur {
					errs = append(errs, config.ValidationError{
						Path: "default_max_duration",
						Message: fmt.Sprintf(
							"agent_defaults.default_max_duration (%s) exceeds agent_limits.max_duration ceiling (%s)",
							defStr, ceilingStr,
						),
					})
				}
			case explicitDuration:
				errs = append(errs, config.ValidationError{
					Path: "default_max_duration",
					Message: fmt.Sprintf(
						"agent_defaults.default_max_duration cannot be empty (unlimited) while agent_limits.max_duration ceiling (%s) is set",
						ceilingStr,
					),
				})
			}
		}
	}

	return errs
}

// agentLimitsSchema is the hand-written JSON schema of the agent_limits
// section (runtime state, no $defs in settings-v1.schema.json).
func agentLimitsSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"max_turns":       map[string]interface{}{"type": "integer", "minimum": 0},
			"max_model_calls": map[string]interface{}{"type": "integer", "minimum": 0},
			"max_duration":    map[string]interface{}{"type": "string"},
		},
		"additionalProperties": false,
	}
}
