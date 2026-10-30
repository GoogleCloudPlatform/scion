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
	"log/slog"
	"slices"
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
	// UnlimitedCountLimits: the broker keeps an explicit max_turns or
	// max_model_calls of 0 ("no limit") over an inherited limit at start
	// (ptone/scion#5605). Without it the hub refuses a change of a count
	// limit to 0 (unlimitedCountRefusal).
	UnlimitedCountLimits bool
}

// brokerStartSupportFrom reads a broker's capabilities.
func brokerStartSupportFrom(caps *store.BrokerCapabilities) brokerStartSupport {
	if caps == nil {
		return brokerStartSupport{}
	}
	return brokerStartSupport{ChangedFields: caps.ChangedFields, ClearedFields: caps.ClearedFields, UnlimitedCountLimits: caps.UnlimitedCountLimits}
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
// wait for a reincarnation with this broker. old is the agent's inline
// config before the PATCH, req the decoded request and recorded the
// cleared paths the PATCH recorded (the new ClearedFields).
//
// The broker renders the agent's persisted config after merging the inline
// config into it, so an edit is applied at the next start only when the
// broker re-renders the key and the new value reaches the persisted config:
//   - a non-empty value the start merge applies (startMergeKeepsBase is
//     false), or a clear recorded in cleared_fields that the broker
//     applies (system_prompt, with clearedFields). Any other clear -- an
//     empty agent_instructions, skills or services value -- is held: the
//     broker would render the previous value again.
//   - for skills, in addition, no reference is removed: the merge appends
//     skill references, and a re-render installs the listed skills without
//     uninstalling others, so a removed skill stays installed.
func (sup brokerStartSupport) heldProvisionEdits(provision []string, old, req *api.ScionConfig, recorded []string) []string {
	var out []string
	for _, key := range provision {
		k := strings.TrimPrefix(key, agentConfigKeyPrefix)
		// An empty value is a clear whatever the merge would do with it:
		// an empty list is omitted on the wire and reaches the broker as
		// nothing. Only a recorded clear the broker applies takes effect.
		var applied bool
		switch {
		case !sup.appliesAtStart(key):
		case configFieldZero(k, req):
			applied = sup.ClearedFields && slices.Contains(recorded, k)
		default:
			applied = !startMergeKeepsBase(k, req)
		}
		if applied && k == "skills" && skillReferencesRemoved(old, req) {
			applied = false
		}
		if !applied {
			out = append(out, key)
		}
	}
	return out
}

// skillReferencesRemoved reports whether req leaves out a skill reference
// that old lists, compared by base URI (without a version specifier) and
// install name, as the broker deduplicates references: a version change of
// a skill replaces it at the same destination, so it is not a removal.
func skillReferencesRemoved(old, req *api.ScionConfig) bool {
	if old == nil {
		return false
	}
	kept := map[[2]string]bool{}
	if req != nil {
		for _, r := range req.Skills {
			kept[[2]string{api.SkillBaseURI(r.URI), r.As}] = true
		}
	}
	for _, r := range old.Skills {
		if !kept[[2]string{api.SkillBaseURI(r.URI), r.As}] {
			return true
		}
	}
	return false
}

// clearAppliesAtStart reports whether the broker can apply a clear of the
// config key with wire key key at the next start: with ClearedFields, a key
// api.ClearableConfigPath accepts, and env, whose removed keys are sent as
// env.KEY entries. Whether a given PATCH's clear is applied also depends on
// what it recorded (heldClearEdits). A count limit's clear is its null; its
// 0 is Unlimited, a value, not a clear (ptone/scion#5605).
func (sup brokerStartSupport) clearAppliesAtStart(key string) bool {
	k, ok := strings.CutPrefix(key, agentConfigKeyPrefix)
	return ok && sup.ClearedFields && (k == "env" || api.ClearableConfigPath(k))
}

// heldClearEdits returns the entries of cleared (wire keys of cleared
// container keys, from reincarnateOnlyConfigEdits) that still wait for a
// reincarnation with this broker. recorded is the cleared paths the PATCH
// recorded (the new ClearedFields), and envDropped the env keys the request
// drops (droppedEnvKeys).
//
// A clear is reported applied only when the broker applies cleared_fields
// and the PATCH actually recorded it: the key itself, or, for env, an
// env.KEY entry for every dropped key. A caller who cannot view the
// agent's env records no env entries, and the auto-expose and TZ keys are
// never recorded, so such an env edit stays held.
//
// A count limit's 0 (Unlimited) never reaches here: it is a set value the
// start merge applies, so reincarnateOnlyConfigEdits does not report it as
// cleared, and the hub accepts it only for a broker with
// UnlimitedCountLimits (unlimitedCountRefusal, ptone/scion#5605).
func (sup brokerStartSupport) heldClearEdits(cleared, recorded, envDropped []string) []string {
	var out []string
	for _, key := range cleared {
		k := strings.TrimPrefix(key, agentConfigKeyPrefix)
		var applied bool
		switch {
		case !sup.ClearedFields:
		case k == "env":
			applied = envKeysRecorded(envDropped, recorded)
		default:
			applied = sup.clearAppliesAtStart(key) && slices.Contains(recorded, k)
		}
		if !applied {
			out = append(out, key)
		}
	}
	return out
}

// envKeysRecorded reports whether every key in keys has an env.KEY entry
// in recorded.
func envKeysRecorded(keys, recorded []string) bool {
	for _, k := range keys {
		if !slices.Contains(recorded, api.ClearedEnvPathPrefix+k) {
			return false
		}
	}
	return true
}

// droppedEnvKeys returns the keys of old's env that a request presenting
// env (present["env"]) leaves out, sorted; nil when env is not present.
// The auto-expose keys and TZ are left out, as removedConfigEntries leaves
// them out: the hub derives them for each start rather than taking them
// from the inline env.
func droppedEnvKeys(old, req *api.ScionConfig, present map[string]bool) []string {
	if !present["env"] || old == nil {
		return nil
	}
	var out []string
	for k := range old.Env {
		if autoExposeEnvKeys[k] || k == agentTZEnvKey {
			continue
		}
		if req == nil {
			out = append(out, k)
			continue
		}
		if _, kept := req.Env[k]; !kept {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// heldRemovals returns removed (the entries removedConfigEntries reports
// per config key) without the env removals this broker applies at the next
// start: those whose every key the PATCH recorded as an env.KEY entry in
// recorded (the new ClearedFields).
func (sup brokerStartSupport) heldRemovals(removed map[string][]string, recorded []string) map[string][]string {
	if len(removed) == 0 {
		return removed
	}
	out := make(map[string][]string, len(removed))
	for k, v := range removed {
		if k == "env" && sup.ClearedFields && envKeysRecorded(v, recorded) {
			continue
		}
		out[k] = v
	}
	return out
}

// reincarnateOnlyEditWarnings is the package's reincarnateOnlyEditWarnings
// for this broker: with changedFields, the provision-rendered keys still
// held are secrets and clears or removals a start does not apply, and the
// warning says so instead of saying a start never re-renders.
func (sup brokerStartSupport) reincarnateOnlyEditWarnings(provision, cleared []string) []string {
	if !sup.ChangedFields || len(provision) == 0 {
		return reincarnateOnlyEditWarnings(provision, cleared)
	}
	out := []string{strings.Join(provision, ", ") + ": stored now; rendered at the next reincarnation (a start does not render secrets, and does not apply a clear the broker cannot apply or a removed skill)"}
	return append(out, reincarnateOnlyEditWarnings(nil, cleared)...)
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
