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
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// configEnvTZIgnoredWarning is returned by the agent PATCH when config.env
// carries a non-empty TZ. The env editor never sets the agent timezone; the
// top-level explicitTimezone field does.
const configEnvTZIgnoredWarning = "TZ in config.env is ignored; use explicitTimezone"

// explicitTimezoneNextStartWarning is returned by the agent PATCH when it
// changes explicitTimezone on an agent whose container is already running.
const explicitTimezoneNextStartWarning = "explicitTimezone applies at the agent's next start"

// stripAgentEnvTZ removes TZ from the agent's env records, AppliedConfig.Env
// and AppliedConfig.InlineConfig.Env (which alias on create). It reports
// whether either held the key. CreateInputs is not touched: it is the
// as-requested record that reincarnate replays.
func stripAgentEnvTZ(ac *store.AgentAppliedConfig) bool {
	if ac == nil {
		return false
	}
	_, had := ac.Env[agentTZEnvKey]
	delete(ac.Env, agentTZEnvKey)
	if ac.InlineConfig != nil {
		if _, ok := ac.InlineConfig.Env[agentTZEnvKey]; ok {
			had = true
		}
		delete(ac.InlineConfig.Env, agentTZEnvKey)
	}
	return had
}

// adoptLegacyTZ classifies an agent written before ExplicitTimezone existed.
// Such an agent may hold a TZ in AppliedConfig.Env, or only in
// AppliedConfig.InlineConfig.Env when the two copies diverged. When
// ExplicitTimezone is empty, the non-empty value (Env first) moves into
// ExplicitTimezone with ExplicitTimezoneLegacy set, so the agent keeps the
// zone it had. Both env copies are stripped either way; an empty value is a
// "no value" marker and is just stripped.
//
// An agent with ExplicitTimezoneUnpinned set was unpinned explicitly, so a TZ
// that reappeared in its env (written by an older hub during a rolling
// deploy) is stripped without re-pinning.
//
// adoptLegacyTZ is deterministic and idempotent. It reports whether it
// changed the config. Callers run it before anything reads or writes the
// agent's TZ: the resolver's callers, the agent PATCH, the agent GET and
// reincarnate.
func adoptLegacyTZ(ac *store.AgentAppliedConfig) bool {
	if ac == nil {
		return false
	}
	value := legacyEnvTZ(ac)
	if !stripAgentEnvTZ(ac) {
		return false
	}
	if value != "" && ac.ExplicitTimezone == "" && !ac.ExplicitTimezoneUnpinned {
		ac.ExplicitTimezone = value
		ac.ExplicitTimezoneLegacy = true
	}
	return true
}

// captureCreateTZ is writer (a) of ExplicitTimezone: the create pipeline. A
// TZ from the request config.env or a hub-resolved template (merged into
// AppliedConfig.Env before this runs), else the hub harness config's TZ
// (harnessConfigTZ), is an explicit act and becomes the agent's pin. It
// never overwrites a non-empty ExplicitTimezone, which is how reincarnate
// carries a pin forward, and it moves nothing when ExplicitTimezoneUnpinned
// is set, so an unpin survives reincarnate. TZ is always stripped from the
// env records.
func captureCreateTZ(ac *store.AgentAppliedConfig, harnessConfigTZ string) {
	if ac == nil {
		return
	}
	value := legacyEnvTZ(ac)
	if value == "" {
		value = harnessConfigTZ
	}
	stripAgentEnvTZ(ac)
	if value == "" || ac.ExplicitTimezone != "" || ac.ExplicitTimezoneUnpinned {
		return
	}
	ac.ExplicitTimezone = value
}

// captureCreateTimezone runs captureCreateTZ at the end of
// resolveDerivedConfig. hc is the hub harness config that
// resolveDerivedConfig resolved by name, or nil; when it is nil and the
// agent already carries a HarnessConfigID, the config is loaded by ID. The
// harness config is read only when the env records supply no TZ.
func (s *Server) captureCreateTimezone(ctx context.Context, agent *store.Agent, hc *store.HarnessConfig) {
	ac := agent.AppliedConfig
	if ac == nil {
		return
	}
	hcTZ := ""
	if legacyEnvTZ(ac) == "" && ac.ExplicitTimezone == "" && !ac.ExplicitTimezoneUnpinned {
		if hc == nil && ac.HarnessConfigID != "" {
			loaded, err := s.store.GetHarnessConfig(ctx, ac.HarnessConfigID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.agentLifecycleLog.Warn("failed to load harness config for timezone capture",
					"agent_id", agent.ID, "harness_config_id", ac.HarnessConfigID, "error", err)
			}
			hc = loaded
		}
		if hc != nil && hc.Config != nil {
			hcTZ = hc.Config.Env[agentTZEnvKey]
		}
	}
	captureCreateTZ(ac, hcTZ)
}

// phaseHasLiveContainer reports whether an agent in the given phase has a
// container that is coming up or running, and so keeps its current TZ until
// the next start.
func phaseHasLiveContainer(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseCloning, state.PhaseStarting, state.PhaseRunning:
		return true
	}
	return false
}

// applyExplicitTimezoneEdit is writer (b) of ExplicitTimezone: the agent
// PATCH's top-level explicitTimezone. "" unpins (and records the unpin so
// reincarnate keeps it); any other value must be a valid IANA zone name and
// pins it. Either write clears ExplicitTimezoneLegacy. It reports whether
// the stored pin changed.
func applyExplicitTimezoneEdit(ac *store.AgentAppliedConfig, value string) (bool, error) {
	if value != "" {
		// "Local" loads in Go but names the hub's own zone, not a zone the
		// container can be given.
		if _, err := time.LoadLocation(value); err != nil || value == "Local" {
			return false, fmt.Errorf("explicitTimezone %q is not a valid IANA time zone name", value)
		}
	}
	before := *ac
	ac.ExplicitTimezone = value
	ac.ExplicitTimezoneUnpinned = value == ""
	ac.ExplicitTimezoneLegacy = false
	changed := before.ExplicitTimezone != ac.ExplicitTimezone ||
		before.ExplicitTimezoneUnpinned != ac.ExplicitTimezoneUnpinned ||
		before.ExplicitTimezoneLegacy != ac.ExplicitTimezoneLegacy
	return changed, nil
}
