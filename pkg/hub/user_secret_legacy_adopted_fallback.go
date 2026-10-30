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
	"log/slog"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Legacy adopted-agent fallback for an owner's user secrets.
//
// Delegation provenance adoption gives a legacy agent's edge the frozen
// compatibility ceiling of its role (permissions.CompatibilityPolicyV1).
// That table grants secret.use to full only, so an adopted baseline or
// readonly agent fails clause S3c of the user secret parent ceiling and
// lost its owner's progeny-shared user secrets at start and restart when
// the ceiling started to apply to the owner path (#4234). Before, the owner
// path delivered them with no check.
//
// The fallback lets S3c pass for exactly that case and nothing else:
//   - the hop is the agent's own edge (depth 0), and its delegator is a
//     user who is the agent's owner, so there is no agent above it;
//   - the edge was written by adoption (source credential
//     system_migration) and is bounded;
//   - its ceiling is exactly the baseline or the readonly
//     CompatibilityPolicyV1 row (with or without the assigned service
//     account permissions), so the missing secret.use is the policy's
//     doing and nothing narrower. Adoption ceilings the edge at the lower of
//     its role and the agent's applied role but keeps the edge's role, so
//     the cohort role is the row's role, which must not rank above the
//     edge's recorded role (an edge "full" adopted at the baseline row
//     because the agent's applied role was baseline qualifies; an edge
//     "readonly" carrying the baseline row does not);
//   - an adoption record under policy V1, adopted or recognized, points at
//     this edge for this agent.
//
// An edge written later by a scope re-issue keeps the system_migration
// source but has a new ID and no adoption record, so it drops out of the
// cohort and fails closed (S3c denies again).
//
// Every other clause still applies: S1, S2 (held and deleted agents),
// S3/S3a/S3b/S3f on the hop, S4 (terminal user active), S4a (admitted to
// the project), S4c, and the source check S5-S8 (the secret is the owner's
// own, shared with progeny, authored by the owner at the head of the
// ancestry). A use under the fallback is logged at Warn, rate limited per
// (agent, secret), naming the cohort and the secret, never the value.
//
// The runtime read and the agent secret list are not restored for this
// cohort: their precheck (materialRuntimePrecheck) has required the token
// scope project:secret:read for every runtime read since ptone/scion#2072,
// and these agents' tokens never carry it. That refusal predates the
// ceiling and is unchanged. The fallback still applies inside the ceiling
// evaluator, so a cohort agent whose token does carry the scope is judged
// like any other (Decide's step 10), but the read path adds no exception.

// legacyAdoptedFallback records that a chain passed S3c through the
// legacy adopted-agent fallback.
type legacyAdoptedFallback struct {
	Role   string
	EdgeID string
}

// legacyAdoptedFallbackWarnInterval is the shortest gap between two Warn
// lines for the same agent and secret.
const legacyAdoptedFallbackWarnInterval = 10 * time.Minute

// legacyAdoptedSecretUseFallback reports whether edge, the hop at depth of
// agent's chain that S3c refuses, qualifies for the legacy adopted-agent
// fallback. A store error is returned; the caller denies with ceiling_error.
func (a *AuthzService) legacyAdoptedSecretUseFallback(ctx context.Context, agent *store.Agent, edge *store.DelegationEdge, depth int) (*legacyAdoptedFallback, error) {
	if agent == nil || edge == nil || depth != 0 {
		return nil, nil
	}
	if edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID == "" || edge.DelegatorID != agent.OwnerID {
		return nil, nil
	}
	if edge.SourceCredentialKind != store.SourceCredentialSystemMigration || edge.Kind != store.EffectCeilingBounded {
		return nil, nil
	}
	rowRole, ok := legacyCompatibilityV1RowRole(edge.PermissionIDs)
	if !ok || roleOrdinal(rowRole) > roleOrdinal(AgentRole(edge.Role)) {
		return nil, nil
	}
	recs, _, err := a.store.ListDelegationAdoptions(ctx, store.DelegationAdoptionFilter{AdoptedEdgeID: edge.ID})
	if err != nil {
		return nil, fmt.Errorf("adoption record lookup: %w", err)
	}
	for _, rec := range recs {
		if rec == nil || rec.AdoptedEdgeID != edge.ID || rec.DelegateID != agent.ID {
			continue
		}
		if rec.PolicyVersion != int(permissions.CompatibilityPolicyV1) {
			continue
		}
		if rec.Status != store.DelegationAdoptionAdopted && rec.Status != store.DelegationAdoptionRecognized {
			continue
		}
		return &legacyAdoptedFallback{Role: string(rowRole), EdgeID: edge.ID}, nil
	}
	return nil, nil
}

// legacyCompatibilityV1RowRole returns the role whose CompatibilityPolicyV1
// row ids is exactly, with or without the assigned service account
// permissions, when that role is baseline or readonly (the rows without
// secret.use). Both sides are sorted and de-duplicated.
func legacyCompatibilityV1RowRole(ids []string) (AgentRole, bool) {
	for _, role := range []AgentRole{AgentRoleBaseline, AgentRoleReadOnly} {
		for _, withSA := range []bool{false, true} {
			want, ok := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, string(role), withSA)
			if ok && slices.Equal(want, ids) {
				return role, true
			}
		}
	}
	return "", false
}

// legacyFallbackPathStart is the path attribute of a fallback use at start
// or restart (the owner path of startSecretResolveOpts), the only path that
// records one.
const legacyFallbackPathStart = "start"

// legacyAdoptedFallbackMetrics counts uses of the fallback, so the policy V2
// backfill can show when the exception is no longer needed.
type legacyAdoptedFallbackMetrics interface {
	RecordLegacyAdoptedFallback(ctx context.Context, path, role string)
}

// OTelLegacyAdoptedFallbackMetrics counts fallback uses as
// scion.hub.user_secret.legacy_adopted_fallback (attributes path, role).
type OTelLegacyAdoptedFallbackMetrics struct {
	total metric.Int64Counter
}

// NewOTelLegacyAdoptedFallbackMetrics creates the counter.
func NewOTelLegacyAdoptedFallbackMetrics(mp metric.MeterProvider) (*OTelLegacyAdoptedFallbackMetrics, error) {
	if mp == nil {
		return nil, fmt.Errorf("otel legacy adopted fallback metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.user_secret.legacy_adopted_fallback",
		metric.WithUnit("{use}"))
	if err != nil {
		return nil, fmt.Errorf("creating user_secret.legacy_adopted_fallback counter: %w", err)
	}
	return &OTelLegacyAdoptedFallbackMetrics{total: c}, nil
}

// RecordLegacyAdoptedFallback implements legacyAdoptedFallbackMetrics.
func (m *OTelLegacyAdoptedFallbackMetrics) RecordLegacyAdoptedFallback(ctx context.Context, path, role string) {
	m.total.Add(ctx, 1, metric.WithAttributes(
		attribute.String("path", path),
		attribute.String("role", role),
	))
}

// SetLegacyAdoptedFallbackMetrics installs the fallback-use counter.
func (s *Server) SetLegacyAdoptedFallbackMetrics(m legacyAdoptedFallbackMetrics) {
	if s == nil || s.authzService == nil {
		return
	}
	s.authzService.legacyFallbackMetrics = m
}

// LegacyAdoptedFallbackUses returns the number of fallback uses since the
// process started.
func (a *AuthzService) LegacyAdoptedFallbackUses() int64 {
	if a == nil {
		return 0
	}
	return a.legacyFallbackUses.Load()
}

// recordLegacyAdoptedFallback counts one use of the fallback on path and
// logs it at Warn, rate limited per (agent, secret), naming the cohort and
// the secret, never the value.
func (a *AuthzService) recordLegacyAdoptedFallback(ctx context.Context, path string, fb *legacyAdoptedFallback, agentID, userID, secretID, secretName string) {
	if a == nil || fb == nil {
		return
	}
	total := a.legacyFallbackUses.Add(1)
	if m := a.legacyFallbackMetrics; m != nil {
		m.RecordLegacyAdoptedFallback(ctx, path, fb.Role)
	}
	a.legacyFallbackWarnOnce.Do(func() {
		a.legacyFallbackWarn = newHookRefusalLimiter(legacyAdoptedFallbackWarnInterval, time.Now)
	})
	record, suppressed := a.legacyFallbackWarn.admit(agentID+"/"+secretID, "legacy_adopted_fallback/"+fb.Role)
	if !record {
		return
	}
	slog.WarnContext(ctx, "user secret allowed under the legacy adopted-agent fallback: compatibility policy V1 grants secret.use to full only",
		"path", path, "agent_id", agentID, "cohort", "adopted_"+fb.Role, "edge_id", fb.EdgeID,
		"secret", secretName, "secret_id", secretID, "user_id", userID,
		"suppressed_since_last", suppressed, "total_uses", total)
}
