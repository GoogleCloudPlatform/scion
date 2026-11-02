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
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// An agent's count limits (max_turns, max_model_calls) are optional typed
// values: nil inherits, 0 is "no limit", n >= 1 is a limit
// (ptone/scion#5605). Only a Runtime Broker that advertises
// unlimitedCountLimits keeps a 0 over an inherited limit at start; an older
// one decodes it as "unset" and the agent keeps the inherited limit, at a
// start and at a reincarnation alike. So the hub refuses a change of a count
// limit to 0 for such an agent, rather than report an edit as applied that
// no start applies.

// Reasons for refusing a change of a count limit to Unlimited, as the
// validation error's per-field detail.
const (
	unlimitedNotSupportedByBroker = "unlimited_not_supported_by_broker: this agent's runtime broker cannot apply Unlimited over an inherited limit; reload the page, or upgrade the broker"
	unlimitedBrokerUnknown        = "broker_unknown: the agent has no registered runtime broker, so no start can promise Unlimited"
)

// unlimitedCountFieldKeys are the edit keys of the count limits, whose 0 is
// Unlimited.
var unlimitedCountFieldKeys = map[string]bool{
	agentConfigKeyPrefix + "max_turns":       true,
	agentConfigKeyPrefix + "max_model_calls": true,
}

// errBrokerLookup is returned by agentBrokerStartSupportStrict when reading
// the agent's broker record fails (a store error, which a retry may fix);
// the caller answers 503.
var errBrokerLookup = errors.New("runtime broker lookup failed")

// unlimitedCountChanges returns the wire keys (config.max_turns,
// config.max_model_calls) whose value req changes to 0 ("no limit") from
// base, the value the agent's next run would otherwise use. A key req
// leaves unset (nil) or sends with base's own 0 (an echo) is not a change.
func unlimitedCountChanges(req, base *api.ScionConfig) []string {
	if req == nil {
		return nil
	}
	var baseTurns, baseCalls *int
	if base != nil {
		baseTurns, baseCalls = base.MaxTurns, base.MaxModelCalls
	}
	var out []string
	if api.IsUnlimitedCount(req.MaxTurns) && !api.IsUnlimitedCount(baseTurns) {
		out = append(out, agentConfigKeyPrefix+"max_turns")
	}
	if api.IsUnlimitedCount(req.MaxModelCalls) && !api.IsUnlimitedCount(baseCalls) {
		out = append(out, agentConfigKeyPrefix+"max_model_calls")
	}
	return out
}

// agentBrokerStartSupportStrict is agentBrokerStartSupport for a decision
// that must not guess: assigned is false when the agent has no broker or its
// broker record no longer exists (a retry would not help), and err is
// errBrokerLookup when reading the record fails, instead of folding both
// into the zero value.
func (s *Server) agentBrokerStartSupportStrict(ctx context.Context, agent *store.Agent) (sup brokerStartSupport, assigned bool, err error) {
	if agent == nil || agent.RuntimeBrokerID == "" || s.store == nil {
		return brokerStartSupport{}, false, nil
	}
	broker, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && broker == nil:
		return brokerStartSupport{}, false, nil
	case err != nil:
		return brokerStartSupport{}, true, errBrokerLookup
	}
	return brokerStartSupportFrom(broker.Capabilities), true, nil
}

// unlimitedCountRefusal checks a config edit of agent whose decoded request
// is req against base, the value the agent's next run would otherwise use
// (the applied inline config, overlaid by any held value when held edits
// are enabled). It returns the per-field reasons for refusing a change of a
// count limit to Unlimited, nil when there is nothing to refuse, or
// errBrokerLookup (answer 503) when the broker record cannot be read.
//
// The broker is looked up only when the request does change a count limit
// to 0, so an ordinary edit never depends on that lookup.
func (s *Server) unlimitedCountRefusal(ctx context.Context, agent *store.Agent, req, base *api.ScionConfig) (map[string]string, error) {
	changed := unlimitedCountChanges(req, base)
	if len(changed) == 0 {
		return nil, nil
	}
	sup, assigned, err := s.agentBrokerStartSupportStrict(ctx, agent)
	if err != nil {
		return nil, err
	}
	var reason string
	switch {
	case !assigned:
		reason = unlimitedBrokerUnknown
	case !sup.UnlimitedCountLimits:
		reason = unlimitedNotSupportedByBroker
	default:
		return nil, nil
	}
	out := make(map[string]string, len(changed))
	for _, k := range changed {
		out[k] = reason
	}
	return out, nil
}

// unlimitedCountCreateWarning returns the create warning for an agent whose
// dispatched inline config cfg (the request after the project stamp) sets a
// count limit to 0 while its broker, with capabilities caps, does not
// advertise unlimitedCountLimits: the agent keeps the inherited limit. It
// returns "" when there is nothing to say. Template zeros are not seen here
// (the hub's template snapshot may predate them); the broker logs those.
func unlimitedCountCreateWarning(cfg *api.ScionConfig, caps *store.BrokerCapabilities) string {
	if cfg == nil || brokerStartSupportFrom(caps).UnlimitedCountLimits {
		return ""
	}
	var keys []string
	if api.IsUnlimitedCount(cfg.MaxTurns) {
		keys = append(keys, agentConfigKeyPrefix+"max_turns")
	}
	if api.IsUnlimitedCount(cfg.MaxModelCalls) {
		keys = append(keys, agentConfigKeyPrefix+"max_model_calls")
	}
	if len(keys) == 0 {
		return ""
	}
	msg := keys[0]
	if len(keys) == 2 {
		msg += ", " + keys[1]
	}
	return msg + ": this agent's runtime broker keeps the inherited limit; Unlimited applies only on a broker that supports it"
}

// foldKeep returns the keep rule a fold of the pending config pc applies
// with this broker: the keys a start applies (startApplies), except a count
// limit pending as 0 (Unlimited) when the broker does not advertise
// unlimitedCountLimits, which a start would not apply over an inherited
// limit (ptone/scion#5605). Such keys stay pending and are returned in
// waiting, sorted wire keys, for unlimitedWaitsWarning. A broker can lose
// the capability after the edit was saved (a downgrade or a re-registration).
func (sup brokerStartSupport) foldKeep(pc *store.AgentPendingConfig) (keep func(agentEditField) bool, waiting []string) {
	keep = sup.startApplies
	if sup.UnlimitedCountLimits || pc.IsEmpty() {
		return keep, nil
	}
	p := pendingConfigPatch(pc)
	held := map[string]bool{}
	if p.present["max_turns"] && api.IsUnlimitedCount(p.cfg.MaxTurns) {
		held[agentConfigKeyPrefix+"max_turns"] = true
	}
	if p.present["max_model_calls"] && api.IsUnlimitedCount(p.cfg.MaxModelCalls) {
		held[agentConfigKeyPrefix+"max_model_calls"] = true
	}
	if len(held) == 0 {
		return keep, nil
	}
	for k := range held {
		waiting = append(waiting, k)
	}
	sort.Strings(waiting)
	return func(f agentEditField) bool { return !held[f.Key] && sup.startApplies(f) }, waiting
}

// unlimitedWaitsWarning is the warning for pending count limits left pending
// because the agent's broker cannot apply Unlimited (foldKeep): a start
// warning at the start fold, and a PATCH response warning at the fold of a
// PATCH to an agent with no container. "" when there are none.
func unlimitedWaitsWarning(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return strings.Join(keys, ", ") + ": Unlimited waits for a runtime broker that supports it; discard it with DELETE /api/v1/agents/{id}/pending-config"
}

// addUnlimitedWaitsWarning adds unlimitedWaitsWarning for keys to the
// start's dispatch warnings.
func addUnlimitedWaitsWarning(ctx context.Context, keys []string) {
	if w := unlimitedWaitsWarning(keys); w != "" {
		addDispatchWarnings(ctx, w)
	}
}

// stringsWithout returns list without the entries in drop, in order; nil
// when nothing is left.
func stringsWithout(list, drop []string) []string {
	if len(drop) == 0 {
		return list
	}
	var out []string
	for _, s := range list {
		if !slices.Contains(drop, s) {
			out = append(out, s)
		}
	}
	return out
}

// warnUnlimitedUnsupported adds unlimitedCountCreateWarning to the create's
// or start's dispatch warnings, for the agent's broker: the dispatched
// inline config sets a count limit to 0 that the broker would not apply. A
// failed broker lookup adds nothing.
func (d *HTTPAgentDispatcher) warnUnlimitedUnsupported(ctx context.Context, agent *store.Agent, inline *api.ScionConfig) {
	if inline == nil || (!api.IsUnlimitedCount(inline.MaxTurns) && !api.IsUnlimitedCount(inline.MaxModelCalls)) ||
		agent == nil || agent.RuntimeBrokerID == "" || d.store == nil {
		return
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || broker == nil {
		return
	}
	addDispatchWarnings(ctx, unlimitedCountCreateWarning(inline, broker.Capabilities))
}

// waitingKeysNamed returns the lower-cased config keys of waiting (wire keys
// foldKeep left pending) that present (a request's lower-cased config keys)
// names, as the presence map dropPendingKeysNamed takes; nil when none.
func waitingKeysNamed(waiting []string, present map[string]bool) map[string]bool {
	var out map[string]bool
	for _, k := range waiting {
		key := strings.ToLower(strings.TrimPrefix(k, agentConfigKeyPrefix))
		if present[key] {
			if out == nil {
				out = map[string]bool{}
			}
			out[key] = true
		}
	}
	return out
}
