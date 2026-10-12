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
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Telemetry delivery (ptone/scion#4222).
//
// The agent record holds only what the requester asked for: create no longer
// writes the hub telemetry default, the project telemetry policy or any
// template telemetry into InlineConfig. Each dispatch (create, provision,
// start, restart) builds the telemetry it sends from three live sources and
// the requester's original input:
//
//   - the hub telemetry default (settings telemetry, hubTelemetryProvider);
//   - the project policy (scion.io/telemetry-enabled, projectTelemetryPolicy);
//   - the requester's telemetry, read from CreateInputs.InlineConfig (the
//     create request plus explicit edits), never from the stored
//     InlineConfig, which older hubs filled with a hub-default copy.
//
// How they travel depends on the broker's templateTiers capability:
//
// Requester telemetry is on/off only, for every broker: its other fields
// are dropped with a create warning (warnRequesterTelemetryDropped). Only the
// delivery channel depends on the broker's templateTiers capability:
//
//   - capable broker: the hub default in HubAgentDefaults.Telemetry, the
//     policy in TelemetryPolicy, and the requester's on/off as
//     SCION_TELEMETRY_ENABLED (the requester flag tier, below policy).
//     InlineConfig.Telemetry is not sent. The ladder is broker settings <
//     hub default < requester on/off < project policy.
//   - legacy broker: the same ladder folded into InlineConfig.Telemetry: a
//     copy of the hub default, the requester's Enabled over it, then the
//     policy over Enabled. Removed in ptone/scion#4224.
//
// Template telemetry reaches no broker by any hub path (ptone/scion#4524).

// envTelemetryEnabled is the requester's telemetry on/off flag.
const envTelemetryEnabled = "SCION_TELEMETRY_ENABLED"

// telemetryDispatch is the telemetry one dispatch delivers.
type telemetryDispatch struct {
	// templateTiers reports that the target broker takes the hub default
	// and the policy as separate tiers.
	templateTiers bool
	// hubDefault is the hub's settings telemetry; nil when none.
	hubDefault *api.TelemetryConfig
	// policy is the project's telemetry on/off; nil when none.
	policy *bool
	// requester is the requester's original telemetry; nil when none.
	requester *api.TelemetryConfig
}

// projectTelemetryPolicy returns the project's scion.io/telemetry-enabled
// annotation as a policy, or nil when it is unset or not a valid bool.
func projectTelemetryPolicy(project *store.Project) *bool {
	if project == nil || project.Annotations == nil {
		return nil
	}
	val, ok := project.Annotations[projectSettingTelemetryEnabled]
	if !ok {
		return nil
	}
	b, err := strconv.ParseBool(val)
	if err != nil {
		return nil
	}
	return &b
}

// requesterTelemetry returns the requester's own telemetry for ac: the
// create request's (or an explicit edit's) InlineConfig.Telemetry from
// CreateInputs. An agent with no CreateInputs has no trustworthy record of
// it (its stored InlineConfig.Telemetry may be a hub, project or template
// copy), so it has none.
func requesterTelemetry(ac *store.AgentAppliedConfig) *api.TelemetryConfig {
	if ac == nil || ac.CreateInputs == nil || ac.CreateInputs.InlineConfig == nil {
		return nil
	}
	return ac.CreateInputs.InlineConfig.Telemetry
}

// requesterTelemetryDroppedFields names the fields of a requester telemetry
// block, other than enabled, that no broker receives.
func requesterTelemetryDroppedFields(t *api.TelemetryConfig) []string {
	if t == nil {
		return nil
	}
	var out []string
	if t.Cloud != nil {
		out = append(out, "cloud")
	}
	if t.Hub != nil {
		out = append(out, "hub")
	}
	if t.Local != nil {
		out = append(out, "local")
	}
	if t.Filter != nil {
		out = append(out, "filter")
	}
	if len(t.Resource) > 0 {
		out = append(out, "resource")
	}
	sort.Strings(out)
	return out
}

// requesterTelemetryDroppedWarning is the create warning for requester
// telemetry fields other than enabled.
func requesterTelemetryDroppedWarning(fields []string) string {
	return fmt.Sprintf("telemetry %s in the agent config ignored: an agent sets telemetry on or off only (the rest comes from settings)",
		strings.Join(fields, ", "))
}

// brokerTemplateTiers reports whether brokerID reports the templateTiers
// capability. A lookup failure reads as a legacy broker: the legacy payload
// is still understood by a capable broker.
func (d *HTTPAgentDispatcher) brokerTemplateTiers(ctx context.Context, brokerID string) bool {
	if brokerID == "" || d.store == nil {
		return false
	}
	broker, err := d.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil || broker == nil || broker.Capabilities == nil {
		return false
	}
	return broker.Capabilities.TemplateTiers
}

// dispatchTelemetry resolves the telemetry for one dispatch of agent.
// policy is the project policy (projectDispatchInfo.telemetryPolicy).
func (d *HTTPAgentDispatcher) dispatchTelemetry(ctx context.Context, agent *store.Agent, policy *bool) telemetryDispatch {
	t := telemetryDispatch{
		templateTiers: d.brokerTemplateTiers(ctx, agent.RuntimeBrokerID),
		policy:        policy,
		requester:     requesterTelemetry(agent.AppliedConfig),
	}
	if d.hubTelemetryProvider != nil {
		t.hubDefault = d.hubTelemetryProvider()
	}
	return t
}

// inlineTelemetry is the InlineConfig.Telemetry this dispatch sends: nil for
// a templateTiers broker; for a legacy broker the hub default, the
// requester's Enabled over it, then the policy over Enabled. The requester's
// other fields are never sent. The result never aliases the hub default or
// the requester's record.
func (t telemetryDispatch) inlineTelemetry() *api.TelemetryConfig {
	if t.templateTiers {
		return nil
	}
	var out *api.TelemetryConfig
	if t.hubDefault != nil {
		c := *t.hubDefault
		out = &c
	}
	if t.requester != nil && t.requester.Enabled != nil {
		if out == nil {
			out = &api.TelemetryConfig{}
		}
		v := *t.requester.Enabled
		out.Enabled = &v
	}
	if t.policy != nil {
		if out == nil {
			out = &api.TelemetryConfig{}
		}
		v := *t.policy
		out.Enabled = &v
	}
	return out
}

// inlineConfig returns the InlineConfig to send for stored (the agent's
// AppliedConfig.InlineConfig): a shallow copy with Telemetry replaced by
// inlineTelemetry. stored is never modified. Nil when stored is nil and
// there is no telemetry to send.
func (t telemetryDispatch) inlineConfig(stored *api.ScionConfig) *api.ScionConfig {
	tel := t.inlineTelemetry()
	if stored == nil && tel == nil {
		return nil
	}
	out := &api.ScionConfig{}
	if stored != nil {
		c := *stored
		out = &c
	}
	out.Telemetry = tel
	return out
}

// hubAgentDefaults adds the hub telemetry default to h for a templateTiers
// broker, allocating h when needed. A legacy broker's h is returned as is.
func (t telemetryDispatch) hubAgentDefaults(h *RemoteHubAgentDefaults) *RemoteHubAgentDefaults {
	if !t.templateTiers || t.hubDefault == nil {
		return h
	}
	if h == nil {
		h = &RemoteHubAgentDefaults{}
	}
	c := *t.hubDefault
	h.Telemetry = &c
	return h
}

// telemetryPolicy is the TelemetryPolicy wire value: the project policy for
// a templateTiers broker, nil otherwise.
func (t telemetryDispatch) telemetryPolicy() *bool {
	if !t.templateTiers || t.policy == nil {
		return nil
	}
	v := *t.policy
	return &v
}

// injectRequesterEnabled delivers the requester's telemetry on/off to a
// templateTiers broker as SCION_TELEMETRY_ENABLED, unless env already sets
// the key (an explicit env flag from the requester). The broker ranks it
// below the project policy.
func (t telemetryDispatch) injectRequesterEnabled(env map[string]string, classifications *map[string]api.EnvKind) {
	if !t.templateTiers || t.requester == nil || t.requester.Enabled == nil || env == nil {
		return
	}
	if _, ok := env[envTelemetryEnabled]; ok {
		return
	}
	env[envTelemetryEnabled] = strconv.FormatBool(*t.requester.Enabled)
	classifyEnv(classifications, envTelemetryEnabled, api.EnvKindPlain)
}

// hubTelemetryDefault returns a copy of the hub's settings telemetry config,
// or nil when there is none. It is the hub telemetry default dispatch sends
// (hubTelemetryProvider).
func (s *Server) hubTelemetryDefault() *api.TelemetryConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.config.TelemetryConfig == nil {
		return nil
	}
	c := *s.config.TelemetryConfig
	return &c
}

// warnRequesterTelemetryDropped adds a create warning when the requester's
// telemetry sets fields other than enabled: no broker receives them, only
// the on/off (inlineTelemetry, injectRequesterEnabled).
func (s *Server) warnRequesterTelemetryDropped(ctx context.Context, agent *store.Agent) {
	if agent == nil {
		return
	}
	if fields := requesterTelemetryDroppedFields(requesterTelemetry(agent.AppliedConfig)); len(fields) > 0 {
		addDispatchWarnings(ctx, requesterTelemetryDroppedWarning(fields))
	}
}
