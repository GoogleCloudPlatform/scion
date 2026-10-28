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

// Legacy fallback for launch material delivery.
//
// Launch selection (material_delivery.go) admits each project, hub and
// broker item through the delivery admission walk (hubDeliveryChainWalk),
// which needs recorded delegation provenance at every hop. Agents written
// before provenance was recorded, or whose recorded ceiling cannot carry the
// deliver permissions for a legacy reason, would lose every such item at
// their next start or restart, although they received them before. The
// fallback keeps that delivery for them, with a rate-limited Warn per item,
// until their edges are re-recorded by a later backfill.
//
// The fallback is a second, legacy-tolerant run of the same walk
// (hubDeliveryWalk with a hubDeliveryLegacyNote). It applies only when:
//
//   - the credential is a launch selection credential for a purpose
//     legacyDeliveryFallbackPurpose names (start, restart, the availability
//     probe of those); no other caller of Decide gets it;
//   - every Decide step before step 10 allowed the item (the association,
//     relationship and restriction stages, explicit access constraints
//     included), and the step-10 gate conditions passed: the fallback runs
//     inside hubDeliveryAdmission, after both;
//   - the strict walk denied with a cause a legacy hop produces
//     (legacyFallbackCauses), and the tolerant walk passes having tolerated
//     at least one legacy hop.
//
// A legacy hop is exactly one of (the cohort names are logged and reported
// by the census, delivery_census.go):
//
//   - legacy_no_edge: the hop's agent has no active edge in the project;
//   - legacy_sentinel_edge: its only active edge is the delegation-edge
//     backfill's migration sentinel (user:system/migration);
//   - legacy_unadopted_edge: its only active edge has unrecorded provenance
//     (version 0, or no known effect ceiling kind), the shape older writers
//     left and that adoption excluded, legacy scheduled children with role
//     none included;
//   - legacy_adopted_baseline, legacy_adopted_readonly: its only active edge
//     was adopted (source system_migration) at role baseline or readonly,
//     whose compatibility policy V1 ceiling lacks the deliver permission;
//   - legacy_uat_created: its only active edge was recorded from a user
//     access token (source uat), whose bounded ceiling cannot carry the
//     deliver permission because no token scope maps to it.
//
// Everything else is enforced as before, on the strict and tolerant walk
// alike: a held agent at any hop, a deleted, missing or held delegator or
// ancestor agent, an inactive delegator or root user, more than one active
// edge, local-development provenance this server does not admit, the depth
// bound and cycles, and every recorded non-legacy hop's effect ceiling (an
// agent-recorded edge whose ceiling lacks the permission still denies). A
// chain with no legacy hop never takes the fallback, so its provenance
// root admission (ResolveProvenanceRoot) still applies.
//
// When the walk cannot follow edges any further (legacy_no_edge,
// legacy_sentinel_edge), the hop agent's stored ancestry stands in for the
// rest of the chain (hubDeliveryLegacyAncestry): its root user must exist
// and be active, and every ancestor agent must exist, not be deleted and
// not be held. An agent without stored ancestry is denied. The root user's
// project membership is not required on the fallback (it was not before);
// a chain with no legacy hop still requires it.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Legacy cohorts (see the file comment).
const (
	legacyDeliveryCohortNoEdge          = "legacy_no_edge"
	legacyDeliveryCohortSentinelEdge    = "legacy_sentinel_edge"
	legacyDeliveryCohortUnadoptedEdge   = "legacy_unadopted_edge"
	legacyDeliveryCohortAdoptedBaseline = "legacy_adopted_baseline"
	legacyDeliveryCohortAdoptedReadOnly = "legacy_adopted_readonly"
	legacyDeliveryCohortUATCreated      = "legacy_uat_created"
)

// Fallback Warn rate limit: one line per agent, cohort and item per window.
const (
	legacyDeliveryLogDedupWindow = time.Hour
	legacyDeliveryLogDedupSize   = 4096
)

// legacyFallbackCauses are the strict walk's deny causes a legacy hop
// produces. A strict deny with any other cause (held agent, ambiguous
// edges, local-development provenance, an inactive delegator, a lookup
// error, the depth bound) never takes the fallback.
var legacyFallbackCauses = map[DenyCause]bool{
	DenyCauseCeilingProvenanceMissing: true, // no edge
	DenyCauseCeilingOrphaned:          true, // sentinel edge (or a real orphan, which the tolerant walk denies again)
	DenyCauseCeilingUnrecorded:        true, // unadopted edge
	DenyCauseCeilingEffectExceeded:    true, // adopted baseline/readonly, UAT-created
}

// legacyDeliveryFallbackPurpose reports whether launch selection for
// purpose takes the legacy fallback: the start and restart selection, and
// the create-time availability probe, which must agree with what a start
// delivers.
func legacyDeliveryFallbackPurpose(p MaterialPurpose) bool {
	switch p {
	case PurposeDispatch, PurposeRestart, PurposeAvailabilityProbe:
		return true
	}
	return false
}

// hubDeliveryLegacyNote collects the first legacy hop of a tolerant walk.
type hubDeliveryLegacyNote struct {
	cohort string
	depth  int
}

func (n *hubDeliveryLegacyNote) note(cohort string, depth int) {
	if n.cohort == "" {
		n.cohort, n.depth = cohort, depth
	}
}

// legacyBoundedEdgeCohort returns the legacy cohort of a recorded bounded
// edge whose ceiling does not allow permissionID, or "" when the edge is
// not legacy (its deny stands).
func legacyBoundedEdgeCohort(edge *store.DelegationEdge, permissionID string) string {
	switch edge.SourceCredentialKind {
	case store.SourceCredentialSystemMigration:
		switch AgentRole(edge.Role) {
		case AgentRoleBaseline:
			return legacyDeliveryCohortAdoptedBaseline
		case AgentRoleReadOnly:
			return legacyDeliveryCohortAdoptedReadOnly
		}
	case store.SourceCredentialUAT:
		if !uatCarriesPermission(permissionID) {
			return legacyDeliveryCohortUATCreated
		}
	}
	return ""
}

// uatPermissionIDs is every permission some user access token scope or
// alias maps to (permissions.ResolveSelector).
var uatPermissionIDs = sync.OnceValue(func() map[string]bool {
	out := make(map[string]bool)
	for scope := range permissions.UATValidScopes() {
		if m, ok := permissions.ResolveSelector(scope); ok {
			for _, id := range m.PermissionIDs {
				out[id] = true
			}
		}
	}
	return out
})

// uatCarriesPermission reports whether a user access token can carry
// permissionID. The UAT-created cohort exists only while it cannot: once a
// scope maps a deliver permission, a UAT-created edge that lacks it is an
// ordinary ceiling deny.
func uatCarriesPermission(permissionID string) bool {
	return uatPermissionIDs()[permissionID]
}

// hubDeliveryLegacyAncestry ends a tolerant walk at agentID, whose chain
// cannot be followed by edges: the agent's stored ancestry stands in for
// the rest of the chain. The root user (position 0) must exist and be
// active; every ancestor agent (later positions) must exist, not be
// deleted and not be held. deny and failClosed are the walk's.
func (a *AuthzService) hubDeliveryLegacyAncestry(
	ctx context.Context,
	agentID string,
	deny func(string, DenyCause) (bool, string, DenyCause),
	failClosed func(error) (bool, string, DenyCause),
) (bool, string, DenyCause) {
	ag, err := a.store.GetAgent(ctx, agentID)
	if err != nil {
		return failClosed(fmt.Errorf("agent %s lookup: %w", agentID, err))
	}
	if ag == nil || !ag.DeletedAt.IsZero() {
		return deny(fmt.Sprintf("agent %s is not live", agentID), DenyCauseCeilingOrphaned)
	}
	if len(ag.Ancestry) == 0 || ag.Ancestry[0] == "" {
		return deny(fmt.Sprintf("legacy fallback: agent %s has no stored ancestry", agentID), DenyCauseCeilingProvenanceMissing)
	}
	for i, id := range ag.Ancestry {
		typ := store.DelegationPrincipalAgent
		if i == 0 {
			typ = store.DelegationPrincipalUser
		}
		live, status, err := a.hubDeliveryDelegatorLiveness(ctx, typ, id)
		if err != nil {
			return failClosed(err)
		}
		switch live {
		case hdDelegatorLive:
		case hdDelegatorHeld:
			return deny(fmt.Sprintf("ancestor agent %s is held", id), DenyCauseCeilingOrphaned)
		case hdDelegatorInactive:
			return deny(fmt.Sprintf("root user %s is %s", id, status), DenyCauseCeilingDelegatorLacksPermission)
		default:
			return deny(fmt.Sprintf("ancestor %s is not live", id), DenyCauseCeilingOrphaned)
		}
	}
	return true, "delivery legacy ancestry passed", ""
}

// hubDeliveryLegacyFallback is the legacy fallback for one permission (see
// the file comment). strict is the strict admission's deny; it is returned
// unchanged unless the tolerant walk passes with a legacy hop.
func (a *AuthzService) hubDeliveryLegacyFallback(
	ctx context.Context,
	h *hubDeliveryIdentity,
	permissionID string,
	agentID string,
	strict hubDeliveryAdmissionOutcome,
	explain *[]DecisionStep,
) hubDeliveryAdmissionOutcome {
	if !legacyFallbackCauses[strict.cause] {
		return strict
	}
	var note hubDeliveryLegacyNote
	ok, _, _ := a.hubDeliveryWalk(ctx, agentID, h.projectID, permissionID, nil, &note)
	if !ok || note.cohort == "" {
		return strict
	}
	reason := "delivery legacy fallback: " + note.cohort
	if note.depth > 0 {
		reason += fmt.Sprintf(" at depth %d", note.depth)
	}
	if explain != nil {
		*explain = append(*explain, DecisionStep{Step: "delegation_ceiling_delivery_legacy_fallback", Detail: reason})
	}
	return hubDeliveryAdmissionOutcome{ok: true, reason: reason, legacyCohort: note.cohort}
}
