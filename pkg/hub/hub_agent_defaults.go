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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hubAgentDefaults returns the hub's operational agent_defaults section.
//
// Thread-safe: ApplySnapshot writes s.config.AgentDefaults under s.mu from the
// settings-propagation goroutine (operational_settings.go, refreshAndApply)
// while request paths read it, so every read must take the lock. Mirrors
// (*Server).HubName, which is the same shape for the same reason.
//
// The returned value is a deep copy — DefaultResources is a pointer, and
// handing callers the live pointer would let a downstream merge mutate the
// server's config out from under the lock.
//
// In file mode this always returns the zero value: BuildLayer1SnapshotFromFile
// deliberately leaves the agent-defaults fields empty (design §3.2.4), so
// callers that gate on "non-empty" never fire in file mode. That is what keeps
// file-mode dispatch byte-identical to the pre-change behaviour.
func (s *Server) hubAgentDefaults() opsettings.AgentDefaultsSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.config.AgentDefaults
	if d.DefaultResources != nil {
		rs := *d.DefaultResources
		d.DefaultResources = &rs
	}
	return d
}

// agentDefaultsEqual reports whether two agent_defaults sections carry the same
// values, comparing DefaultResources by pointee rather than by pointer. Used by
// ApplySnapshot to decide whether "agent_defaults" belongs in the applied-fields
// list; a pointer comparison would report a change on every refresh, because
// Snapshot() builds a fresh *api.ResourceSpec each time.
func agentDefaultsEqual(a, b opsettings.AgentDefaultsSettings) bool {
	if a.DefaultTemplate != b.DefaultTemplate ||
		a.DefaultHarnessConfig != b.DefaultHarnessConfig ||
		a.DefaultMaxTurns != b.DefaultMaxTurns ||
		a.DefaultMaxModelCalls != b.DefaultMaxModelCalls ||
		a.DefaultMaxDuration != b.DefaultMaxDuration {
		return false
	}
	return resourceSpecEqual(a.DefaultResources, b.DefaultResources)
}

// resourceSpecEqual compares two resource specs by value, treating nil and a
// zero-valued spec as different (nil means "unset").
func resourceSpecEqual(a, b *api.ResourceSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// applyHubAgentDefaults stamps the hub operational agent_defaults that must be
// resolved Hub-side, and reports whether it supplied the harness config.
//
// PLACEMENT IS THE WHOLE POINT. Call it AFTER applyProjectDefaults, so that the
// request, the project annotation and the template have all had their chance at
// the slot — only-if-empty then places the hub tier correctly BENEATH them.
// Call it BEFORE populateAgentConfig, so the harness-config ID/hash stamping
// there sees the name and the broker gets something to hydrate from. Called
// before applyProjectDefaults it would beat the project; called before the
// template rungs it would beat the template (design §5.2 risk (b)). There are
// exactly two call sites — the agent-create path and the scheduler-dispatch
// path — and each sits strictly between those two functions.
//
// Only default_harness_config is handled here. default_template joins the
// template ladder further up each path, because it has to be in place before
// the template is resolved. The other four agent_defaults fields
// (default_max_turns, default_max_model_calls, default_max_duration,
// default_resources) deliberately do NOT belong here: writing them into
// AppliedConfig/InlineConfig would send them to the broker as top-of-chain and
// let a hub-wide floor override a template's explicit value — the inversion
// this workstream exists to remove (design §3.2.1, rejected alternative A5).
// They travel by a separate low-rank channel and are applied broker-side.
//
// ACCEPTED CONSEQUENCE, so the next reader does not file it as a bug: a
// harness config resolved here reaches the broker as a CLIFlag-rank value and
// therefore outranks the broker's own profile default_harness_config and its
// settings.yaml default_harness_config (resolve_harness_config.go, ranks 6 and
// 7). That is unavoidable if we want the hub to stamp HarnessConfigID/Hash for
// remote-broker hydration, and it is consistent with "hub values win".
func applyHubAgentDefaults(ac *store.AgentAppliedConfig, d opsettings.AgentDefaultsSettings) bool {
	if ac == nil {
		return false
	}
	if ac.HarnessConfig == "" && d.DefaultHarnessConfig != "" {
		ac.HarnessConfig = d.DefaultHarnessConfig
		return true
	}
	return false
}

// warnHubDefaultTemplateUnusable logs the degradation described in
// applyHubAgentDefaults' sibling rung: the hub operational default_template
// named a template this hub cannot use, so the agent is created with no
// template rather than the create being failed.
//
// Deliberately loud. This is an operator-fixable misconfiguration whose only
// other symptom is agents quietly starting without the template the operator
// believes they set.
func (s *Server) warnHubDefaultTemplateUnusable(ctx context.Context, name, projectID, reason string) {
	s.agentLifecycleLog.WarnContext(ctx,
		"hub operational default_template is unusable; creating the agent with no template. "+
			"Fix or clear default_template in the hub agent_defaults settings",
		"template", name, "project_id", projectID, "reason", reason)
}

// hubDefaultHarnessConfigCtxKey marks a request context in which
// AppliedConfig.HarnessConfig was supplied by the hub operational default.
type hubDefaultHarnessConfigCtxKey struct{}

// withHubDefaultHarnessConfig records that applyHubAgentDefaults supplied the
// harness-config name, so populateAgentConfig can report the right provenance
// when the name does not resolve.
//
// Provenance is carried, not inferred. Comparing the name back against
// s.hubAgentDefaults().DefaultHarnessConfig would misreport the case where a
// user, a project annotation or a template happens to name the same harness
// config the hub defaults to — the same trap the templateFromHubDefault flag
// avoids on the template ladder.
func withHubDefaultHarnessConfig(ctx context.Context) context.Context {
	return context.WithValue(ctx, hubDefaultHarnessConfigCtxKey{}, true)
}

// hubDefaultHarnessConfigFromContext reports whether the agent's harness-config
// name came from the hub operational default.
func hubDefaultHarnessConfigFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(hubDefaultHarnessConfigCtxKey{}).(bool)
	return v
}
