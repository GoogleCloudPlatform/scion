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
//   - the project policy (projectSettingTelemetryEnabled, projectTelemetryPolicy);
//   - the requester's telemetry, read from CreateInputs.InlineConfig (the
//     create request plus explicit edits), never from the stored
//     InlineConfig, which older hubs filled with a hub-default copy.
//
// Requester telemetry is on/off only, for every broker: its other fields
// are dropped with a create warning (warnRequesterTelemetryDropped). Every
// broker receives the same delivery, as separate tiers (ptone/scion#4224):
//
//   - the hub default in HubAgentDefaults.Telemetry (added by the
//     hub-defaults builder, see wireTelemetry);
//   - the policy in TelemetryPolicy;
//   - the requester's on/off as SCION_TELEMETRY_ENABLED (the requester flag
//     tier, below policy).
//
// InlineConfig.Telemetry is never sent. The ladder is broker settings < hub
// default < requester on/off < project policy. A broker older than the
// templateTiers capability (TS-2a) ignores the two unknown fields, so its
// agents get only the broker's own settings telemetry plus the requester's
// on/off (it maps SCION_TELEMETRY_ENABLED to its override); dispatch is not
// refused, and the heartbeat handler warns once per broker
// (warnBrokerWithoutTemplateTiers).
//
// Template telemetry reaches no broker by any hub path (ptone/scion#4524).

// envTelemetryEnabled is the requester's telemetry on/off flag.
const envTelemetryEnabled = "SCION_TELEMETRY_ENABLED"

// telemetryDispatch is the telemetry one dispatch delivers.
type telemetryDispatch struct {
	// hubDefault is the hub's settings telemetry; nil when none.
	hubDefault *api.TelemetryConfig
	// policy is the project's telemetry on/off; nil when none.
	policy *bool
	// requester is the requester's original telemetry; nil when none.
	requester *api.TelemetryConfig
}

// projectTelemetryPolicy returns the project's projectSettingTelemetryEnabled
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

// dispatchTelemetry resolves the telemetry for one dispatch of agent.
// policy is the project policy (projectDispatchInfo.telemetryPolicy).
func (d *HTTPAgentDispatcher) dispatchTelemetry(agent *store.Agent, policy *bool) telemetryDispatch {
	t := telemetryDispatch{
		policy:    policy,
		requester: requesterTelemetry(agent.AppliedConfig),
	}
	if d.hubTelemetryProvider != nil {
		t.hubDefault = d.hubTelemetryProvider()
	}
	return t
}

// inlineConfig returns the InlineConfig to send for stored (the agent's
// AppliedConfig.InlineConfig): a shallow copy with Telemetry cleared, since
// telemetry travels only as tiers. stored is never modified. Nil when stored
// is nil.
func (t telemetryDispatch) inlineConfig(stored *api.ScionConfig) *api.ScionConfig {
	if stored == nil {
		return nil
	}
	out := *stored
	out.Telemetry = nil
	return &out
}

// wireTelemetry is the hub telemetry default the hub-defaults builder
// (remoteHubAgentDefaults / startHubAgentDefaults) puts in
// HubAgentDefaults.Telemetry: a copy, nil when there is none.
func (t telemetryDispatch) wireTelemetry() *api.TelemetryConfig {
	if t.hubDefault == nil {
		return nil
	}
	c := *t.hubDefault
	return &c
}

// telemetryPolicy is the TelemetryPolicy wire value: a copy of the project
// policy, nil when there is none.
func (t telemetryDispatch) telemetryPolicy() *bool {
	if t.policy == nil {
		return nil
	}
	v := *t.policy
	return &v
}

// injectRequesterEnabled delivers the requester's telemetry on/off as
// SCION_TELEMETRY_ENABLED, unless env already sets the key (an explicit env
// flag from the requester). The broker ranks it below the project policy; a
// broker older than templateTiers maps it to its own telemetry override.
func (t telemetryDispatch) injectRequesterEnabled(env map[string]string, classifications *map[string]api.EnvKind) {
	if t.requester == nil || t.requester.Enabled == nil || env == nil {
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
// the on/off (injectRequesterEnabled).
func (s *Server) warnRequesterTelemetryDropped(ctx context.Context, agent *store.Agent) {
	if agent == nil {
		return
	}
	if fields := requesterTelemetryDroppedFields(requesterTelemetry(agent.AppliedConfig)); len(fields) > 0 {
		addDispatchWarnings(ctx, requesterTelemetryDroppedWarning(fields))
	}
}

// brokerWithoutTemplateTiersWarning is the WARN for a broker whose heartbeat
// capabilities lack templateTiers.
func brokerWithoutTemplateTiersWarning(name string) string {
	return fmt.Sprintf("runtime broker %s does not report the templateTiers capability; hub telemetry defaults and project telemetry policy are not applied to its agents. Upgrade the broker.", name)
}

// warnBrokerWithoutTemplateTiers logs, at WARN and once per broker per
// process, that the broker's heartbeat capabilities lack templateTiers: such
// a broker (older than TS-2a) ignores the telemetry tiers every dispatch
// sends, and its agents get only its own settings telemetry. It is called
// from the heartbeat handler, where the evidence arrives (on a multi-replica
// hub, heartbeats and dispatches can reach different replicas). A heartbeat
// without capabilities says nothing and is not warned about; neither is
// registration, which never carries templateTiers. Dispatch is never refused
// (ptone/scion#4224).
func (s *Server) warnBrokerWithoutTemplateTiers(brokerID, brokerName string, caps *store.BrokerCapabilities) {
	if caps == nil || caps.TemplateTiers || brokerID == "" {
		return
	}
	if _, warned := s.brokerTemplateTiersWarned.LoadOrStore(brokerID, struct{}{}); warned {
		return
	}
	name := brokerName
	if name == "" {
		name = brokerID
	}
	s.agentLifecycleLog.Warn(brokerWithoutTemplateTiersWarning(name), "broker_id", brokerID)
}
