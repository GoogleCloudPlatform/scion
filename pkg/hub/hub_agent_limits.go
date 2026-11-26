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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ErrCodeLimitExceedsHubCeiling is the HTTP 400 error code returned when a
// requested run limit exceeds (or sets unlimited while a ceiling exists for)
// a Hub agent_limits ceiling.
const ErrCodeLimitExceedsHubCeiling = "limit_exceeds_hub_ceiling"

// agentLimitCeilings returns the active Hub agent_limits ceilings when the
// agent_policy.limit_ceilings experiment is enabled, or the zero value
// otherwise. While the experiment is off, Hub create validation and dispatch
// wire payloads are identical to pre-change behavior.
func (s *Server) agentLimitCeilings() opsettings.AgentLimitsSettings {
	if s == nil || !s.experimentEnabled(experiments.AgentPolicyLimitCeilings) {
		return opsettings.AgentLimitsSettings{}
	}
	ops := s.operationalSettings.Load()
	if ops == nil {
		return opsettings.AgentLimitsSettings{}
	}
	return ops.AgentLimits()
}

// validateCreateMaxTurnsAgainstCeiling checks an agent create request's
// max_turns against ceilings.MaxTurns. Nil (unset) is allowed and will be
// resolved/filled by the broker. When a ceiling is set (> 0), explicit 0
// (unlimited) and values above the ceiling are refused.
func validateCreateMaxTurnsAgainstCeiling(maxTurns *int, ceilings opsettings.AgentLimitsSettings) error {
	if ceilings.MaxTurns <= 0 || maxTurns == nil {
		return nil
	}
	if *maxTurns == 0 {
		return fmt.Errorf("max_turns cannot be 0 (unlimited) while hub ceiling (%d) is set", ceilings.MaxTurns)
	}
	if *maxTurns > ceilings.MaxTurns {
		return fmt.Errorf("max_turns (%d) exceeds hub ceiling (%d)", *maxTurns, ceilings.MaxTurns)
	}
	return nil
}

// validLimitClampedName reports whether name is one of the canonical run-limit
// field names that a broker may echo in LimitsClampedBy.
func validLimitClampedName(name string) bool {
	switch name {
	case "max_turns", "max_model_calls", "max_duration":
		return true
	default:
		return false
	}
}

// limitsClampedWarning formats a create/start warning when the broker reports
// that one or more run limits were clamped to Hub ceilings.
func limitsClampedWarning(clamped []string, eff store.AgentEffectiveConfig) string {
	if len(clamped) == 0 {
		return ""
	}
	parts := make([]string, 0, len(clamped))
	for _, field := range clamped {
		switch field {
		case "max_turns":
			if eff.MaxTurns > 0 {
				parts = append(parts, fmt.Sprintf("max_turns=%d", eff.MaxTurns))
			} else {
				parts = append(parts, field)
			}
		case "max_model_calls":
			if eff.MaxModelCalls > 0 {
				parts = append(parts, fmt.Sprintf("max_model_calls=%d", eff.MaxModelCalls))
			} else {
				parts = append(parts, field)
			}
		case "max_duration":
			if eff.MaxDuration != "" {
				parts = append(parts, fmt.Sprintf("max_duration=%s", eff.MaxDuration))
			} else {
				parts = append(parts, field)
			}
		default:
			parts = append(parts, field)
		}
	}
	return "Run limits were clamped to hub ceiling: " + strings.Join(parts, ", ")
}

func warningsMentionClamped(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), "clamped") {
			return true
		}
	}
	return false
}
