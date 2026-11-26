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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Agent limits in the persisted agent config (ptone/scion#623).
//
// max_turns, max_model_calls and max_duration are job keys: a template or the
// requester (inline config) may set them, and those values are the agent's
// own. The hub agent_defaults and this broker's settings.yaml defaults only
// fill a limit the agent does not set. Those defaults are applied on every
// start (applyDefaultLimits) and never persisted, so scion-agent.json holds
// only the agent's own limits: a persisted limit is the agent's own, and an
// absent one means "use the current default". A persisted count limit of 0 is
// the agent's own "no limit" (ptone/scion#5605): no default fills it, and the
// launch sets no SCION_MAX_TURNS / SCION_MAX_MODEL_CALLS for it. A later change to either
// default then reaches the agent on its next start or restart instead of
// being frozen at provision.
//
// An agent provisioned before this change has the defaults of that time in
// its scion-agent.json; they cannot be told apart from its own values and
// keep applying.

// agentLimits holds the limit fields of an agent config. A nil count limit
// is unset; 0 is an explicit "no limit" (ptone/scion#5605).
type agentLimits struct {
	MaxTurns      *int
	MaxModelCalls *int
	MaxDuration   string
}

// limitsOf returns cfg's limit fields.
func limitsOf(cfg *api.ScionConfig) agentLimits {
	if cfg == nil {
		return agentLimits{}
	}
	return agentLimits{
		MaxTurns:      api.CloneIntPtr(cfg.MaxTurns),
		MaxModelCalls: api.CloneIntPtr(cfg.MaxModelCalls),
		MaxDuration:   cfg.MaxDuration,
	}
}

// overlay returns l with every limit set in over replacing l's. A count
// limit of 0 ("no limit") is set, so it replaces.
func (l agentLimits) overlay(over agentLimits) agentLimits {
	if over.MaxTurns != nil {
		l.MaxTurns = api.CloneIntPtr(over.MaxTurns)
	}
	if over.MaxModelCalls != nil {
		l.MaxModelCalls = api.CloneIntPtr(over.MaxModelCalls)
	}
	if over.MaxDuration != "" {
		l.MaxDuration = over.MaxDuration
	}
	return l
}

// withLimits returns a shallow copy of cfg whose limit fields are l's. Nil
// cfg yields nil.
func withLimits(cfg *api.ScionConfig, l agentLimits) *api.ScionConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.MaxTurns = api.CloneIntPtr(l.MaxTurns)
	out.MaxModelCalls = api.CloneIntPtr(l.MaxModelCalls)
	out.MaxDuration = l.MaxDuration
	return &out
}

// readPersistedAgentLimits reads the limit fields of the agent's persisted
// scion-agent.json in agentDir. ok is false when the file cannot be read or
// parsed.
func readPersistedAgentLimits(agentDir string) (l agentLimits, ok bool) {
	if agentDir == "" {
		return agentLimits{}, false
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		return agentLimits{}, false
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return agentLimits{}, false
	}
	return limitsOf(&cfg), true
}

// applyDefaultLimits fills each limit cfg leaves unset, lowest tier last:
// the hub agent_defaults on ctx, then this broker's settings.yaml defaults,
// and then enforces any Hub agent_limits ceilings via config.ClampLimits.
// Precedence is therefore agent (inline) > template > hub agent_defaults >
// broker settings defaults, capped by Hub ceilings (which also act as the
// effective value when nothing was set anywhere).
//
// Returns the list of limit field names whose configured values were clamped
// to a Hub ceiling (empty when no clamping occurred).
//
// The hub tier sits above the broker settings tier on purpose. The hub sends
// nothing in file mode, so there a co-located broker applies the same
// settings.yaml values itself at the bottom tier, and file-mode behaviour is
// unchanged (design §3.2.4, rejected alternative A7). The hub defaults must
// never be stamped into InlineConfig instead: that is a top-of-chain slot and
// would outrank a template's explicit max_turns (rejected alternative A5).
func applyDefaultLimits(ctx context.Context, cfg *api.ScionConfig, settings *config.VersionedSettings) []string {
	if cfg == nil {
		return nil
	}
	var turns, calls int
	var duration string
	if settings != nil {
		turns, calls, duration = settings.DefaultMaxTurns, settings.DefaultMaxModelCalls, settings.DefaultMaxDuration
	}
	hubDefaults := api.HubAgentDefaultsFromContext(ctx)
	config.FillDefaultLimits(cfg, hubDefaults, turns, calls, duration)
	if hubDefaults != nil && !hubDefaults.LimitCeilings.IsEmpty() {
		return config.ClampLimits(cfg, hubDefaults.LimitCeilings)
	}
	return nil
}

// effectiveLimitsFromConfig returns the effective positive run limits from cfg
// for reporting in AgentInfo (0 / "" when unlimited or unset).
func effectiveLimitsFromConfig(cfg *api.ScionConfig) (maxTurns, maxModelCalls int, maxDuration string) {
	if cfg == nil {
		return 0, 0, ""
	}
	if n, ok := cfg.TurnLimit(); ok {
		maxTurns = n
	}
	if n, ok := cfg.ModelCallLimit(); ok {
		maxModelCalls = n
	}
	if d := strings.TrimSpace(cfg.MaxDuration); d != "" && api.ParseDuration(d) > 0 {
		maxDuration = d
	}
	return maxTurns, maxModelCalls, maxDuration
}

// formatLimitsClampedWarning builds a user-facing warning naming each run
// limit that was clamped to a Hub ceiling and its effective value.
func formatLimitsClampedWarning(clamped []string, cfg *api.ScionConfig) string {
	if len(clamped) == 0 {
		return ""
	}
	maxTurns, maxModelCalls, maxDuration := effectiveLimitsFromConfig(cfg)
	parts := make([]string, 0, len(clamped))
	for _, field := range clamped {
		switch field {
		case "max_turns":
			if maxTurns > 0 {
				parts = append(parts, fmt.Sprintf("max_turns=%d", maxTurns))
			} else {
				parts = append(parts, field)
			}
		case "max_model_calls":
			if maxModelCalls > 0 {
				parts = append(parts, fmt.Sprintf("max_model_calls=%d", maxModelCalls))
			} else {
				parts = append(parts, field)
			}
		case "max_duration":
			if maxDuration != "" {
				parts = append(parts, fmt.Sprintf("max_duration=%s", maxDuration))
			} else {
				parts = append(parts, field)
			}
		default:
			parts = append(parts, field)
		}
	}
	return "Run limits were clamped to hub ceiling: " + strings.Join(parts, ", ")
}
