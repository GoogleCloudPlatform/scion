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
	"encoding/json"
	"log/slog"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// What an agent's Runtime Broker applies at a start beyond merging the
// hub's inline config into the agent's persisted config, and how that
// changes when an edit takes effect (ptone/scion#3982).
//
// A broker that advertises changedFields re-renders, at start, the files
// provisioning wrote for the provision-rendered keys the hub lists in the
// inline config's changed_fields (api.RerenderableConfigKeys). For such a
// broker an edit of those keys on an agent with no container takes effect
// at the next start, like a container (T1) key, instead of waiting for a
// reincarnation. brokerStartSupport is consulted wherever the hub decides
// that: the editability the GET reports, and the PATCH's dispositions and
// warnings.

// brokerStartSupport is what an agent's broker applies at a start beyond
// the inline-config merge. The zero value is an older broker (or none):
// edits it cannot apply at a start wait for a reincarnation.
type brokerStartSupport struct {
	// ChangedFields: the broker re-renders the provision-rendered keys
	// listed in changed_fields at start.
	ChangedFields bool
	// ClearedFields: the broker applies cleared_fields at start, so a
	// cleared key api.ClearableConfigPath accepts, and a removed env key,
	// take effect at the next start (ptone/scion#4890).
	ClearedFields bool
}

// brokerStartSupportFrom reads a broker's capabilities.
func brokerStartSupportFrom(caps *store.BrokerCapabilities) brokerStartSupport {
	if caps == nil {
		return brokerStartSupport{}
	}
	return brokerStartSupport{ChangedFields: caps.ChangedFields, ClearedFields: caps.ClearedFields}
}

// agentBrokerStartSupport is brokerStartSupportFrom for the agent's
// assigned broker. An agent with no broker, or a broker lookup that fails,
// gets the zero value, so edits are reported as waiting for a
// reincarnation rather than promised at the next start.
func (s *Server) agentBrokerStartSupport(ctx context.Context, agent *store.Agent) brokerStartSupport {
	if agent == nil || agent.RuntimeBrokerID == "" || s.store == nil {
		return brokerStartSupport{}
	}
	broker, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || broker == nil {
		if err != nil {
			slog.DebugContext(ctx, "agent edit: broker lookup failed; edits of provision-rendered keys reported as held for reincarnation",
				"agent", agent.ID, "broker", agent.RuntimeBrokerID, "error", err)
		}
		return brokerStartSupport{}
	}
	return brokerStartSupportFrom(broker.Capabilities)
}

// appliesAtStart reports whether the broker applies an edit of the field
// with wire key key at the next start although its tier alone would hold
// it for a reincarnation: a provision-rendered key the broker re-renders.
func (sup brokerStartSupport) appliesAtStart(key string) bool {
	k, ok := strings.CutPrefix(key, agentConfigKeyPrefix)
	return ok && sup.ChangedFields && api.RerenderableConfigKey(k)
}

// heldProvisionEdits returns the entries of provision (wire keys of
// provision-rendered edits, from reincarnateOnlyConfigEdits) that still
// wait for a reincarnation with this broker.
func (sup brokerStartSupport) heldProvisionEdits(provision []string) []string {
	var out []string
	for _, k := range provision {
		if !sup.appliesAtStart(k) {
			out = append(out, k)
		}
	}
	return out
}

// clearAppliesAtStart reports whether the broker applies a clear or zero
// of the config key with wire key key at the next start: with
// ClearedFields, a key api.ClearableConfigPath accepts, and env, whose
// removed keys are sent as env.KEY entries. The count limits are left out:
// their 0 (Unlimited) is not applied by any start or reincarnation over an
// inherited limit (ptone/scion#5605), so the field keeps
// clearNeedsReincarnate; their null clear is handled by heldClearEdits.
func (sup brokerStartSupport) clearAppliesAtStart(key string) bool {
	k, ok := strings.CutPrefix(key, agentConfigKeyPrefix)
	return ok && sup.ClearedFields && !countLimitKeys[k] && (k == "env" || api.ClearableConfigPath(k))
}

// countLimitKeys are the count limits, whose 0 is the Edit page's
// Unlimited rather than a clear.
var countLimitKeys = map[string]bool{"max_turns": true, "max_model_calls": true}

// heldClearEdits returns the entries of cleared (wire keys of cleared
// container keys, from reincarnateOnlyConfigEdits) that still wait for a
// reincarnation with this broker. rawConfig is the request's raw config
// object.
//
// A count limit sent as a number (0, Unlimited) is excluded from the
// applied path and stays held, with today's warning: the broker would
// restore the inherited limit rather than remove it, and no start or
// reincarnation can yet express "no limit" over an inherited one
// (ptone/scion#5605). Only a null value is reported as a clear the broker
// applies.
func (sup brokerStartSupport) heldClearEdits(cleared []string, rawConfig map[string]json.RawMessage) []string {
	var out []string
	for _, k := range cleared {
		countLimitClear := sup.ClearedFields && countLimitKeys[strings.TrimPrefix(k, agentConfigKeyPrefix)] && !countLimitSentAsNumber(k, rawConfig)
		if !sup.clearAppliesAtStart(k) && !countLimitClear {
			out = append(out, k)
		}
	}
	return out
}

// countLimitSentAsNumber reports whether the wire key key is a count limit
// the request's raw config sends as a number rather than null.
func countLimitSentAsNumber(key string, rawConfig map[string]json.RawMessage) bool {
	k := strings.TrimPrefix(key, agentConfigKeyPrefix)
	if !countLimitKeys[k] {
		return false
	}
	for rk, v := range rawConfig {
		if strings.EqualFold(rk, k) {
			s := strings.TrimSpace(string(v))
			return s != "" && s != "null"
		}
	}
	return false
}

// heldRemovals returns removed (the entries removedConfigEntries reports
// per config key) without the keys whose removals this broker applies at
// the next start: the env keys, which nextClearedFields records as
// env.KEY entries under the same rule.
func (sup brokerStartSupport) heldRemovals(removed map[string][]string) map[string][]string {
	if len(removed) == 0 {
		return removed
	}
	out := make(map[string][]string, len(removed))
	for k, v := range removed {
		if !sup.clearAppliesAtStart(agentConfigKeyPrefix + k) {
			out[k] = v
		}
	}
	return out
}

// nextChangedFields returns the agent's live inline config's ChangedFields
// after a config PATCH: old's list plus every provision-rendered key in
// provision (the wire keys reincarnateOnlyConfigEdits reports changed) that
// api.RerenderableConfigKey accepts. Nothing else can enter it: the
// request's own changed_fields is never read (a fixed key, dropped before
// this runs), and old's entries are filtered the same way.
//
// The list is sticky, like ClearedFields: it is sent with every start and
// reset only by a reincarnation. The broker re-renders a listed key only
// when its rendered result differs from its last render, so a later start
// with no new change writes nothing. It is recorded whatever the agent's
// broker supports, so a broker upgraded later applies it. The result is
// sorted, and nil when empty.
func nextChangedFields(old *api.ScionConfig, provision []string) []string {
	set := map[string]bool{}
	if old != nil {
		for _, k := range old.ChangedFields {
			if api.RerenderableConfigKey(k) {
				set[k] = true
			}
		}
	}
	for _, key := range provision {
		if k := strings.TrimPrefix(key, agentConfigKeyPrefix); api.RerenderableConfigKey(k) {
			set[k] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rerenderUnsupportedWarning is the start warning for a start whose inline
// config lists provision-rendered keys to re-render, dispatched to a broker
// that does not advertise changedFields; nil when there is nothing to say.
func rerenderUnsupportedWarning(inline *api.ScionConfig, caps *store.BrokerCapabilities) string {
	keys := api.RerenderConfigKeys(inline)
	if len(keys) == 0 || brokerStartSupportFrom(caps).ChangedFields {
		return ""
	}
	return strings.Join(keys, ", ") + ": changed since the agent was provisioned; this agent's runtime broker does not re-render them at start, so the change takes effect at the next reincarnation"
}

// warnRerenderUnsupported adds rerenderUnsupportedWarning to the start's
// dispatch warnings, for the agent's broker. A failed broker lookup adds
// nothing.
func (d *HTTPAgentDispatcher) warnRerenderUnsupported(ctx context.Context, agent *store.Agent, inline *api.ScionConfig) {
	if len(api.RerenderConfigKeys(inline)) == 0 || agent.RuntimeBrokerID == "" || d.store == nil {
		return
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || broker == nil {
		return
	}
	addDispatchWarnings(ctx, rerenderUnsupportedWarning(inline, broker.Capabilities))
}
