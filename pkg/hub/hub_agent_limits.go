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
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ErrCodeLimitExceedsHubCeiling is the HTTP 400 error code returned when a
// requested run limit exceeds (or sets unlimited while a ceiling exists for)
// a Hub agent_limits ceiling.
const ErrCodeLimitExceedsHubCeiling = "limit_exceeds_hub_ceiling"

func writeAgentLimitsExperimentDisabled(w http.ResponseWriter) {
	writeError(w, http.StatusPreconditionFailed, ErrCodeExperimentDisabled,
		"agent_limits ceilings are not enabled on this Hub (experiment "+experiments.AgentPolicyLimitCeilings+" is off)",
		map[string]interface{}{"experiment": experiments.AgentPolicyLimitCeilings})
}

// agentEffectiveConfigView projects the effective run limits from a stored
// AgentEffectiveConfig onto the single-agent GET response. Returns nil when
// eff is nil or carries no effective limit fields.
func agentEffectiveConfigView(eff *store.AgentEffectiveConfig) *AgentEffectiveConfigView {
	if eff == nil {
		return nil
	}
	if eff.MaxTurns <= 0 && eff.MaxModelCalls <= 0 && eff.MaxDuration == "" && len(eff.LimitsClampedBy) == 0 {
		return nil
	}
	return &AgentEffectiveConfigView{
		MaxTurns:        eff.MaxTurns,
		MaxModelCalls:   eff.MaxModelCalls,
		MaxDuration:     eff.MaxDuration,
		LimitsClampedBy: slices.Clone(eff.LimitsClampedBy),
	}
}

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

// validateAgentLimitsSectionDocs validates agent_defaults and agent_limits
// against each other using the effective post-request values of sectionDocs
// (incoming doc when present, falling back to stored operational settings).
// Called in the upfront validation pass of PUT /api/v1/admin/server-config
// before any section is written, preventing partial writes across sections.
func validateAgentLimitsSectionDocs(
	ops *OperationalSettings,
	sectionDocs map[string]json.RawMessage,
	allValidationErrors map[string][]config.ValidationError,
) {
	defaultsDoc, hasDefaults := sectionDocs["agent_defaults"]
	limitsDoc, hasLimits := sectionDocs[opsettings.AgentLimitsSectionName]
	if !hasDefaults && !hasLimits {
		return
	}
	if len(allValidationErrors["agent_defaults"]) > 0 || len(allValidationErrors[opsettings.AgentLimitsSectionName]) > 0 {
		return
	}

	limits := ops.AgentLimits()
	if hasLimits {
		parsed, err := opsettings.ParseAgentLimitsDoc(limitsDoc)
		if err != nil {
			return // already reported by per-section opsettings.Validate
		}
		limits = parsed
	}
	if limits.IsEmpty() {
		return
	}

	if hasDefaults {
		if errs := opsettings.ValidateAgentDefaultsDocAgainstLimits(defaultsDoc, limits); len(errs) > 0 {
			allValidationErrors["agent_defaults"] = append(allValidationErrors["agent_defaults"], errs...)
		}
		return
	}

	if rawDefaults, ok := ops.rawSection("agent_defaults"); ok {
		if errs := opsettings.ValidateAgentDefaultsDocAgainstLimits(rawDefaults, limits); len(errs) > 0 {
			allValidationErrors[opsettings.AgentLimitsSectionName] = append(allValidationErrors[opsettings.AgentLimitsSectionName], errs...)
			return
		}
	}
	snap := ops.Snapshot()
	if errs := opsettings.ValidateAgentDefaultsAgainstLimits(opsettings.AgentDefaultsSettings{
		DefaultMaxTurns:      snap.DefaultMaxTurns,
		DefaultMaxModelCalls: snap.DefaultMaxModelCalls,
		DefaultMaxDuration:   snap.DefaultMaxDuration,
	}, limits); len(errs) > 0 {
		allValidationErrors[opsettings.AgentLimitsSectionName] = append(allValidationErrors[opsettings.AgentLimitsSectionName], errs...)
	}
}
