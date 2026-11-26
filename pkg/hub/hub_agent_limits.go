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

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
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
