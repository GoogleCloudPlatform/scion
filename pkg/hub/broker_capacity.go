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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Broker capacity precedence sources (ptone/scion#2061 P2, design.md §5.9).
// "not_enforced" is reserved for PR ptone/scion#2270 (P1b, the enforcement
// switch): effectiveBrokerLimit does not produce it yet, and it is added
// here as documentation for that future rebase, not because anything
// returns it today.
const (
	BrokerLimitSourceBroker      = "broker"
	BrokerLimitSourceEntitlement = "entitlement"
	BrokerLimitSourceHubDefault  = "hub_default"
	BrokerLimitSourceUnlimited   = "unlimited"
	BrokerLimitSourceNotEnforced = "not_enforced"
)

// effectiveBrokerLimit resolves the effective max_agents_per_broker limit for
// brokerID and reports which precedence step produced it (design.md §5.2):
//
//  1. (not yet wired — P1b/PR ptone/scion#2270) the enforcement switch is
//     off: "not_enforced".
//  2. The broker's own settings.maxAgents, if set: "broker". It can be
//     lower than the hub-wide default and lower than any system-scoped
//     entitlement binding.
//  3. An entitlement binding (bindings, most generous wins): "entitlement".
//  4. The limit definition's hub-wide default value: "hub_default".
//
// limitDef is looked up once by the caller and shared across a whole
// listing (lookupAgentLimitDefinition, handlers_env_secrets.go), the same
// convention resolveBrokerCapacity uses. A nil limitDef, or no configured
// quota service, means no limit exists at all: (0, "unlimited", nil).
//
// This is the effective-limit half of the one read model shared by
// enforcement and every read path (AC-P2-10): Reserve reaches the same
// broker-settings override through QuotaService.limitOverride, wired in
// server.go to brokerSettingLimitOverride below, so Reserve and every read
// path (this function, the providers listing, the broker settings GET,
// `scion hub projects info`) always agree.
func (s *Server) effectiveBrokerLimit(ctx context.Context, brokerID string, limitDef *store.LimitDefinition) (value int64, source string, err error) {
	if limitDef == nil || s.quotaService == nil {
		return 0, BrokerLimitSourceUnlimited, nil
	}

	overrideValue, ok, err := s.brokerSettingLimitOverride(ctx, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, brokerID)
	if err != nil {
		return 0, "", fmt.Errorf("effective broker limit: broker settings: %w", err)
	}
	if ok {
		return overrideValue, BrokerLimitSourceBroker, nil
	}

	value, fromBinding, err := s.quotaService.resolveEffectiveLimitWithSource(ctx, limitDef.ID, brokerID, store.QuotaScopeBroker, brokerID)
	if err != nil {
		return 0, "", fmt.Errorf("effective broker limit: resolve: %w", err)
	}
	if fromBinding {
		return value, BrokerLimitSourceEntitlement, nil
	}
	return value, BrokerLimitSourceHubDefault, nil
}

// brokerSettingLimitOverride implements QuotaService.limitOverride for
// max_agents_per_broker (design.md §5.2): a broker's own settings.maxAgents,
// if set, beats every entitlement binding, including a system-scoped one,
// and may be lower than the hub-wide default. Wired into QuotaService in
// server.go so Reserve enforces exactly what effectiveBrokerLimit reports
// (AC-P2-10). ok=false for any other limit/scope, or when the broker has no
// settings row, or has one with maxAgents unset (inherit).
func (s *Server) brokerSettingLimitOverride(ctx context.Context, limitName, scopeType, scopeID string) (value int64, ok bool, err error) {
	if limitName != store.LimitMaxAgentsPerBroker || scopeType != store.QuotaScopeBroker {
		return 0, false, nil
	}
	rec, err := s.store.GetBrokerSettings(ctx, scopeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if rec.Settings.MaxAgents == nil {
		return 0, false, nil
	}
	return *rec.Settings.MaxAgents, true, nil
}

// BrokerCapacity is the one read model shared by enforcement and every read
// path for a single broker (design.md §5.9, AC-P2-10): the broker settings
// GET and the broker detail page build their response from it directly; the
// providers listing (handlers_env_secrets.go) uses the same underlying
// effectiveBrokerLimit but keeps its own agentLimit/agentCount field shape
// for backward compatibility (ptone/scion#2161).
type BrokerCapacity struct {
	// Limit is nil when unlimited.
	Limit *int64
	// Count is nil when resolution failed.
	Count *int64
	// Source is one of the BrokerLimitSource* constants above.
	Source string
}

// brokerCapacity computes brokerID's current BrokerCapacity. limitDef is
// looked up once per listing by the caller (lookupAgentLimitDefinition),
// matching effectiveBrokerLimit's convention. Failures are logged here —
// callers such as resolveBrokerCapacity must not need their own copy of
// this logging (ptone/scion#2061 P2 review round 1, F5).
func (s *Server) brokerCapacity(ctx context.Context, brokerID string, limitDef *store.LimitDefinition) BrokerCapacity {
	value, source, err := s.effectiveBrokerLimit(ctx, brokerID, limitDef)
	if err != nil {
		slog.WarnContext(ctx, "broker capacity: failed to resolve effective agent limit",
			"broker_id", brokerID, "error", err)
		return BrokerCapacity{}
	}

	bc := BrokerCapacity{Source: source}
	if value > 0 {
		v := value
		bc.Limit = &v
	}

	if limitDef == nil || s.quotaService == nil {
		return bc
	}
	count, err := s.store.CountActiveReservations(ctx, limitDef.ID, brokerID, store.QuotaScopeBroker, brokerID)
	if err != nil {
		slog.WarnContext(ctx, "broker capacity: failed to count active reservations",
			"broker_id", brokerID, "error", err)
		return bc
	}
	bc.Count = &count
	return bc
}
