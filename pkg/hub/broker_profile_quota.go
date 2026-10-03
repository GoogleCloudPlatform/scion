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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Per-profile broker capacity (ptone/scion#2728).
//
// A settings profile, or a runtime entry, may set max_agents. Agents on a
// broker whose recorded profile resolves such a limit (profile first, then
// its runtime entry; see config.ResolveAgentLimit) hold their
// max_agents_per_broker reservation in a scope of their own,
// store.QuotaScopeBrokerProfile, and count only against that limit. Every
// other agent keeps its reservation at store.QuotaScopeBroker and counts
// toward the broker-wide max_agents_per_broker total.
//
// Both kinds of reservation use the same limit definition, so an agent
// holds at most one active max_agents_per_broker reservation (the store's
// one-active-reservation-per-resource index), and release finds it by agent
// ID whatever its scope: a settings change between reserve and release can
// never leave a reservation behind.

// brokerQuotaScope is where an agent's max_agents_per_broker reservation
// belongs under the current settings.
type brokerQuotaScope struct {
	ScopeType string
	ScopeID   string
	// Limit is the profile or runtime entry's own max_agents. 0 for the
	// broker-wide scope, whose limit is resolved by QuotaService.
	Limit int64
	// Source is the settings key the limit came from, e.g.
	// "profiles.gke.max_agents". Empty for the broker-wide scope.
	Source string
}

// ownLimit reports whether the scope is a profile or runtime entry scope.
func (sc brokerQuotaScope) ownLimit() bool {
	return sc.ScopeType == store.QuotaScopeBrokerProfile
}

// describe names the scope for error and log messages, for example
// "profile gke" or "runtime entry k8s".
func (sc brokerQuotaScope) describe() string {
	kind, name, ok := strings.Cut(strings.TrimSuffix(sc.Source, ".max_agents"), ".")
	if !ok {
		return "this broker"
	}
	switch kind {
	case "profiles":
		return "profile " + name
	case "runtimes":
		return "runtime entry " + name
	}
	return sc.Source
}

// brokerQuotaScopeFor returns the scope for an agent on brokerID whose
// recorded profile is profile. vs may be nil (no own limits).
func brokerQuotaScopeFor(vs *config.VersionedSettings, brokerID, profile string) brokerQuotaScope {
	limit, source := vs.ResolveAgentLimit(profile)
	if limit <= 0 || source == "" {
		return brokerQuotaScope{ScopeType: store.QuotaScopeBroker, ScopeID: brokerID}
	}
	kind, name, _ := strings.Cut(strings.TrimSuffix(source, ".max_agents"), ".")
	return brokerQuotaScope{
		ScopeType: store.QuotaScopeBrokerProfile,
		ScopeID:   brokerID + "/" + kind + "/" + name,
		Limit:     int64(limit),
		Source:    source,
	}
}

// agentQuotaProfile is the profile an agent's broker capacity is counted
// against: the one recorded at create (AppliedConfig.QuotaProfile), else,
// for agents created before it was recorded, AppliedConfig.Profile. Empty
// means the broker-wide total.
func agentQuotaProfile(agent *store.Agent) string {
	if agent == nil || agent.AppliedConfig == nil {
		return ""
	}
	if agent.AppliedConfig.QuotaProfile != "" {
		return agent.AppliedConfig.QuotaProfile
	}
	return agent.AppliedConfig.Profile
}

// agentLimitSettings returns the settings max_agents is read from: the
// hub's global settings file plus the DB settings overlay, never a
// project's settings. When the hub keeps its settings in the database, the
// runtimes and profiles sections come from the current DB snapshot, which
// is what the overlay carries in co-located mode. A load failure is logged
// and yields nil, so every agent counts toward the broker-wide total
// (which stays enforced) rather than going uncounted; ok is false so the
// reconcile pass knows not to move reservations on that basis.
func (s *Server) agentLimitSettings(ctx context.Context) (vs *config.VersionedSettings, ok bool) {
	if s.agentLimitSettingsFn != nil {
		return s.agentLimitSettingsFn(), true
	}
	ok = true
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		s.agentLifecycleLog.WarnContext(ctx, "quota: failed to load global settings for per-profile max_agents; using the broker-wide limit", "error", err)
		vs, ok = nil, false
	}
	if ops := s.GetOperationalSettings(); ops != nil {
		snap := ops.Snapshot()
		if snap.Runtimes != nil || snap.Profiles != nil {
			if vs == nil {
				vs = &config.VersionedSettings{}
			}
			if snap.Runtimes != nil {
				vs.Runtimes = snap.Runtimes
			}
			if snap.Profiles != nil {
				vs.Profiles = snap.Profiles
			}
		}
	}
	return vs, ok
}

// agentBrokerQuotaScope returns the scope agent's reservation belongs in
// under the current settings.
func (s *Server) agentBrokerQuotaScope(ctx context.Context, agent *store.Agent) brokerQuotaScope {
	profile := agentQuotaProfile(agent)
	if profile == "" {
		return brokerQuotaScope{ScopeType: store.QuotaScopeBroker, ScopeID: agent.RuntimeBrokerID}
	}
	vs, _ := s.agentLimitSettings(ctx)
	return brokerQuotaScopeFor(vs, agent.RuntimeBrokerID, profile)
}

// recordAgentQuotaProfile records on a new agent the profile its broker
// capacity is counted against: AppliedConfig.Profile, else the broker's
// default profile (what the broker dispatches with when none is named).
// Called once at create, before the first reservation; the value is kept
// for the agent's lifetime so reserve, release and reconcile key on the
// same profile across restarts even if the broker's default changes.
func (s *Server) recordAgentQuotaProfile(ctx context.Context, agent *store.Agent) {
	if agent == nil || agent.RuntimeBrokerID == "" {
		return
	}
	if agent.AppliedConfig == nil {
		agent.AppliedConfig = &store.AgentAppliedConfig{}
	}
	if agent.AppliedConfig.QuotaProfile != "" {
		return
	}
	if agent.AppliedConfig.Profile != "" {
		agent.AppliedConfig.QuotaProfile = agent.AppliedConfig.Profile
		return
	}
	broker, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.WarnContext(ctx, "quota: could not look up broker default profile", "broker_id", agent.RuntimeBrokerID, "error", err)
		}
		return
	}
	agent.AppliedConfig.QuotaProfile = broker.DefaultProfile
}

// reserveBrokerQuota reserves agent's max_agents_per_broker slot in the
// scope its recorded profile resolves to. It returns the scope used, so a
// refusal can name it.
func (s *Server) reserveBrokerQuota(ctx context.Context, agent *store.Agent) (created bool, scope brokerQuotaScope, err error) {
	scope = s.agentBrokerQuotaScope(ctx, agent)
	if scope.ownLimit() {
		created, err = s.quotaService.ReserveWithLimit(ctx, store.LimitMaxAgentsPerBroker, agent.RuntimeBrokerID, scope.ScopeType, scope.ScopeID, agent.ID, scope.Limit)
	} else {
		created, err = s.quotaService.Reserve(ctx, store.LimitMaxAgentsPerBroker, agent.RuntimeBrokerID, scope.ScopeType, scope.ScopeID, agent.ID)
	}
	if errors.Is(err, store.ErrQuotaExceeded) {
		err = &brokerQuotaExceededError{scope: scope}
	}
	return created, scope, err
}

// brokerQuotaExceededError is store.ErrQuotaExceeded for a broker capacity
// refusal, carrying the scope that is full.
type brokerQuotaExceededError struct {
	scope brokerQuotaScope
}

func (e *brokerQuotaExceededError) Error() string { return brokerQuotaExceededMessage(e) }

func (e *brokerQuotaExceededError) Unwrap() error { return store.ErrQuotaExceeded }

// brokerQuotaExceededMessage is the error text for a start refused by
// broker capacity. A profile or runtime entry limit is named with its
// value; the broker-wide total keeps the generic quota message.
func brokerQuotaExceededMessage(err error) string {
	var qe *brokerQuotaExceededError
	if errors.As(err, &qe) && qe.scope.ownLimit() {
		return fmt.Sprintf("quota exceeded: max_agents for %s on this broker (limit %d)", qe.scope.describe(), qe.scope.Limit)
	}
	return quotaExceededMessage(store.LimitMaxAgentsPerBroker)
}

// brokerQuotaLaunchInFlight reports whether agent has a launch in flight:
// an async launch that is still active, or a phase on the way to running.
// The reconcile pass never moves such an agent's reservation between
// scopes; it is handled on a later pass once the launch settles.
func brokerQuotaLaunchInFlight(agent *store.Agent) bool {
	return agent.LaunchState == "active" || store.InFlightPhases[agent.Phase]
}

// moveBrokerQuotaReservation moves agent's active reservation res to scope
// to. Accounting only, like the reconcile backfill: the agent is already
// running, so no cap is checked. Returns whether the move happened.
func (s *Server) moveBrokerQuotaReservation(ctx context.Context, limitDefID string, agent *store.Agent, res *store.UsageReservation, to brokerQuotaScope) bool {
	if err := s.store.ReleaseReservation(ctx, limitDefID, agent.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.agentLifecycleLog.WarnContext(ctx, "quota reconcile: failed to release reservation for scope move",
			"agent_id", agent.ID, "error", err)
		return false
	}
	if _, err := s.store.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: limitDefID,
		SubjectID:         agent.RuntimeBrokerID,
		ScopeType:         to.ScopeType,
		ScopeID:           to.ScopeID,
		ResourceID:        agent.ID,
		Reserved:          1,
	}); err != nil {
		s.agentLifecycleLog.WarnContext(ctx, "quota reconcile: failed to recreate reservation after scope move",
			"agent_id", agent.ID, "to_scope_type", to.ScopeType, "to_scope_id", to.ScopeID, "error", err)
		return false
	}
	s.agentLifecycleLog.InfoContext(ctx, "quota reconcile: moved agent reservation to its current capacity scope",
		"agent_id", agent.ID, "broker_id", agent.RuntimeBrokerID,
		"from_scope_type", res.ScopeType, "from_scope_id", res.ScopeID,
		"to_scope_type", to.ScopeType, "to_scope_id", to.ScopeID, "source", to.Source)
	return true
}
