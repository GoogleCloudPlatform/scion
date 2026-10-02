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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentTZEnvKey is the container environment variable the agent TZ chain
// resolves.
const agentTZEnvKey = "TZ"

// gatherAnswerTZ is the value the resolver returns, when asked to answer a
// broker's env-gather need for TZ, in place of "" (no TZ). An older broker
// that reports TZ as needed would otherwise block on, or fall back to its
// host value for, an empty answer.
const gatherAnswerTZ = "UTC"

// Timezone sources reported by resolveAgentTZ. They name the rung of the
// agent TZ chain that supplied the value.
const (
	// TZSourceExplicit: AgentAppliedConfig.ExplicitTimezone, set by an
	// explicit act (create-time TZ or a PATCH pin).
	TZSourceExplicit = "explicit"
	// TZSourceLegacy: ExplicitTimezone adopted from a TZ that an older hub
	// persisted in AgentAppliedConfig.Env.
	TZSourceLegacy = "legacy"
	// TZSourceUser, TZSourceProject, TZSourceHub and TZSourceBroker: a hub
	// env-var storage entry (injectionMode always) at that scope.
	TZSourceUser    = "user"
	TZSourceProject = "project"
	TZSourceHub     = "hub"
	TZSourceBroker  = "broker"
	// TZSourceProgeny: an ancestor's user-scope env var shared with
	// allowProgeny.
	TZSourceProgeny = "progeny"
	// TZSourceHubDefault: the hub's agent_defaults.default_timezone.
	TZSourceHubDefault = "hub-default"
	// TZSourceNone: no rung supplied a value. No TZ is sent, so a
	// hub-dispatched container runs the image default (UTC).
	TZSourceNone = "none"
)

// agentTZ is the outcome of the agent TZ chain: the value to put in the
// container's TZ ("" means send no TZ) and the rung that supplied it.
type agentTZ struct {
	TZ     string
	Source string
}

// chooseAgentTZ applies the agent TZ chain to already-gathered inputs. The
// first non-empty rung wins:
//
//  1. ac.ExplicitTimezone (source "explicit", or "legacy" when it was
//     adopted from a persisted Env TZ);
//  2. storage, the winning hub env-var storage TZ with its scope source
//     (user > project > hub > broker > progeny);
//  3. hubDefault, the hub's agent_defaults.default_timezone ("hub-default");
//  4. nothing ("" with source "none").
//
// There is deliberately no runtime-profile rung, and the agent's own Env is
// not consulted: TZ is not an env record once ExplicitTimezone exists, and an
// unadopted Env TZ must be adopted into ExplicitTimezone before resolving.
//
// forGatherAnswer turns rung 4 into "UTC" (source still "none"). Use it only
// to answer a broker that reported TZ as an env-gather need.
func chooseAgentTZ(ac *store.AgentAppliedConfig, storage agentTZ, hubDefault string, forGatherAnswer bool) agentTZ {
	if ac != nil && ac.ExplicitTimezone != "" {
		source := TZSourceExplicit
		if ac.ExplicitTimezoneLegacy {
			source = TZSourceLegacy
		}
		return agentTZ{TZ: ac.ExplicitTimezone, Source: source}
	}
	if storage.TZ != "" {
		return storage
	}
	if hubDefault != "" {
		return agentTZ{TZ: hubDefault, Source: TZSourceHubDefault}
	}
	if forGatherAnswer {
		return agentTZ{TZ: gatherAnswerTZ, Source: TZSourceNone}
	}
	return agentTZ{TZ: "", Source: TZSourceNone}
}

// resolveAgentTZ resolves the agent's container TZ and reports which rung
// supplied it. It is the single source of the TZ value the hub sends to a
// broker on create, start and restart, and of the timezone source shown to
// users. See chooseAgentTZ for the chain. The storage rung is read with
// resolveStorageTZ and the hub default is read live from
// hubAgentDefaultsProvider, so an edit to either reaches every unpinned
// agent at its next start.
func (d *HTTPAgentDispatcher) resolveAgentTZ(ctx context.Context, agent *store.Agent, forGatherAnswer bool) agentTZ {
	var ac *store.AgentAppliedConfig
	if agent != nil {
		ac = agent.AppliedConfig
	}
	if ac != nil && ac.ExplicitTimezone != "" {
		// Rung 1 wins outright; skip the storage and settings reads.
		return chooseAgentTZ(ac, agentTZ{}, "", forGatherAnswer)
	}
	if legacy := legacyEnvTZ(ac); legacy != "" && d.log != nil {
		// Every caller must adopt a legacy env TZ first. The result is
		// unchanged (the agent's Env is not a rung); the log makes a missed
		// adoption visible instead of silently dropping the agent's zone.
		d.log.Warn("unadopted legacy TZ in agent env; adoptLegacyTZ must run before resolveAgentTZ",
			"agentID", agent.ID, "tz", legacy)
	}
	storage := d.resolveStorageTZ(ctx, agent)
	hubDefault := ""
	if storage.TZ == "" && d.hubAgentDefaultsProvider != nil {
		hubDefault = d.hubAgentDefaultsProvider().DefaultTimezone
	}
	return chooseAgentTZ(ac, storage, hubDefault, forGatherAnswer)
}

// legacyEnvTZ returns the TZ an older hub persisted in the agent's env
// records (AppliedConfig.Env first, then InlineConfig.Env), or "".
func legacyEnvTZ(ac *store.AgentAppliedConfig) string {
	if ac == nil {
		return ""
	}
	if v := ac.Env[agentTZEnvKey]; v != "" {
		return v
	}
	if ac.InlineConfig != nil {
		if v := ac.InlineConfig.Env[agentTZEnvKey]; v != "" {
			return v
		}
	}
	return ""
}

// resolveStorageTZ returns the winning hub env-var storage TZ for the agent
// and its scope source, or a zero agentTZ if no scope supplies a non-empty
// value.
//
// It walks the same scopes in the same order as resolveEnvFromStorage
// (envScopePrecedence, lowest first, so a later scope overwrites an earlier
// one), then falls back to progeny vars. It differs in one way: an
// empty-valued entry never wins, because the TZ chain takes the first
// non-empty rung. as_needed entries are skipped: they are not a rung of the
// TZ chain.
func (d *HTTPAgentDispatcher) resolveStorageTZ(ctx context.Context, agent *store.Agent) agentTZ {
	var result agentTZ
	if agent == nil || d.store == nil {
		return result
	}

	for _, filter := range d.envScopesInPrecedenceOrder(agent) {
		filter.Key = agentTZEnvKey
		vars, err := d.store.ListEnvVars(ctx, filter)
		if err != nil {
			d.log.Warn("resolveStorageTZ: failed to list env vars", "scope", filter.Scope, "scope_id", filter.ScopeID, "error", err)
			continue
		}
		for _, v := range vars {
			if v.Key != agentTZEnvKey || v.Value == "" || v.InjectionMode == store.InjectionModeAsNeeded {
				continue
			}
			result = agentTZ{TZ: v.Value, Source: envScopeSourceLabel(filter.Scope)}
		}
	}
	if result.TZ != "" {
		return result
	}

	if len(agent.Ancestry) > 1 {
		progenyVars, err := d.store.ListProgenyEnvVars(ctx, agent.Ancestry)
		if err != nil {
			d.log.Warn("resolveStorageTZ: failed to list progeny env vars", "agent_id", agent.ID, "error", err)
			return result
		}
		for _, v := range progenyVars {
			if v.Key == agentTZEnvKey && v.Value != "" {
				return agentTZ{TZ: v.Value, Source: TZSourceProgeny}
			}
		}
	}
	return result
}
