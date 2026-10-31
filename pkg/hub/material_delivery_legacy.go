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
//     legacyDeliveryFallbackPurpose names (start, restart, their provision
//     credentials, the availability probe of those); no other caller of
//     Decide gets it;
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
//   - legacy_adopted_readonly, legacy_adopted_baseline, legacy_adopted_capped:
//     its only active edge was written by adoption (source system_migration)
//     with a bounded ceiling that lacks the deliver permission only because
//     of the compatibility policy V1 cap or intersection: the ceiling is
//     within the V1 row of the edge's own role (legacyAdoptedCohort). The
//     cohort is keyed on the ceiling, never on the edge role: adoption
//     keeps the edge role and caps the ceiling at the lower of the edge and
//     applied roles, and intersects a child's ceiling with its adopted
//     parent's, so a full edge can carry the baseline or readonly row. The
//     cohort names the lowest V1 row the ceiling fits in (readonly, then
//     baseline); legacy_adopted_capped is a ceiling within the edge role's
//     row that fits in neither.
//
//   - uat_descendant_legacy: an agent-recorded edge (agent delegator, agent
//     credential) that lacks the deliver permission only because its
//     parent chain did not allow it when the edge was recorded: an ancestor
//     was created from a user access token, before the deliver permissions
//     were intrinsic to such a hop (uatHopEffectCeiling), so the parent
//     could not pass them down (legacyInheritsUATGap). It is let through
//     when every hop above it, up to a hop recorded from a user access token
//     whose uatHopEffectCeiling allows the permission, is of the same kind;
//     a descendant hop whose parent's ceiling does allow the permission is a
//     real narrowing and still denies. Edges recorded since carry the
//     deliver permissions (parentDeliverEligibility) and need no exception.
//     Unlike the other cohorts, it still requires the provenance root
//     admission (uatDescendantRootDenied), as its token-created ancestor
//     does on the strict walk.
//     The census counts this cohort until it reads zero, and then it is
//     removed.
//
// A hop recorded from a user access token is not a legacy hop: the deliver
// permissions are intrinsic to it (uatIntrinsicPermissionIDs), so the strict
// walk admits it, exactly as for a session-created agent.
//
// Everything else is enforced as before, on the strict and tolerant walk
// alike: a held agent at any hop, a deleted, missing or held delegator or
// ancestor agent, an inactive delegator or root user, more than one active
// edge (withheld for now, with the Warn naming ceiling_provenance_ambiguous,
// and counted by the census), local-development provenance this server does
// not admit, the depth bound and cycles, and every recorded non-legacy hop's
// effect ceiling (an agent-recorded edge whose ceiling lacks the permission
// still denies unless it only inherits a UAT-created ancestor's gap). A
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
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Fallback cohorts (see the file comment).
const (
	legacyDeliveryCohortNoEdge          = "legacy_no_edge"
	legacyDeliveryCohortSentinelEdge    = "legacy_sentinel_edge"
	legacyDeliveryCohortUnadoptedEdge   = "legacy_unadopted_edge"
	legacyDeliveryCohortAdoptedBaseline = "legacy_adopted_baseline"
	legacyDeliveryCohortAdoptedReadOnly = "legacy_adopted_readonly"
	legacyDeliveryCohortAdoptedCapped   = "legacy_adopted_capped"
	legacyDeliveryCohortUATDescendant   = "uat_descendant_legacy"
)

// Fallback Warn rate limit: one line per agent, cohort and item per window,
// remembering at most legacyDeliveryLogDedupSize keys (least recently
// logged evicted first).
const (
	legacyDeliveryLogDedupWindow = time.Hour
	legacyDeliveryLogDedupSize   = 16384
)

// legacyFallbackCauses are the strict walk's deny causes a legacy hop
// produces. A strict deny with any other cause (held agent, ambiguous
// edges, local-development provenance, an inactive delegator, a lookup
// error, the depth bound) never takes the fallback.
var legacyFallbackCauses = map[DenyCause]bool{
	DenyCauseCeilingProvenanceMissing: true, // no edge
	DenyCauseCeilingOrphaned:          true, // sentinel edge (or a real orphan, which the tolerant walk denies again)
	DenyCauseCeilingUnrecorded:        true, // unadopted edge
	DenyCauseCeilingEffectExceeded:    true, // adopted baseline/readonly, UAT descendant
}

// legacyFallbackPermission reports whether permissionID is one the fallback
// may admit: exactly the three deliver permissions (hubDeliveryPermissionIDs).
// The step-10 gate already limits the delivery credential to them; this
// bound keeps the fallback itself from admitting any other permission
// whatever its caller.
func legacyFallbackPermission(permissionID string) bool {
	_, ok := hubDeliveryPermissionIDs[permissionID]
	return ok
}

// legacyDeliveryFallbackPurpose reports whether launch selection for
// purpose takes the legacy fallback: the start and restart selection, the
// provision credentials sent with them for the broker's skill fetch, and
// the create-time availability probe, which must agree with what a start
// delivers.
func legacyDeliveryFallbackPurpose(p MaterialPurpose) bool {
	switch p {
	case PurposeDispatch, PurposeRestart, PurposeProvisionCredential, PurposeAvailabilityProbe:
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
//
// Only the three deliver permissions are ever tolerated
// (legacyFallbackPermission): the adopted cohort deliberately serves them
// beyond the V1 baseline and readonly rows, which never carry them (that is
// the ruled legacy gap), and nothing else may ride on it.
func legacyBoundedEdgeCohort(edge *store.DelegationEdge, permissionID string) string {
	if !legacyFallbackPermission(permissionID) {
		return ""
	}
	if edge.SourceCredentialKind == store.SourceCredentialSystemMigration {
		return legacyAdoptedCohort(edge)
	}
	return ""
}

// legacyAdoptedCohort returns the adopted cohort of a system_migration
// bounded edge whose ceiling lacks a deliver permission, or "" when its
// ceiling is not within the compatibility policy V1 row of the edge's own
// role (it was not capped or intersected by V1, so its deny stands).
func legacyAdoptedCohort(edge *store.DelegationEdge) string {
	ids := edge.PermissionIDs
	if !withinCompatibilityV1Row(ids, AgentRole(edge.Role)) {
		return ""
	}
	switch {
	case withinCompatibilityV1Row(ids, AgentRoleReadOnly):
		return legacyDeliveryCohortAdoptedReadOnly
	case withinCompatibilityV1Row(ids, AgentRoleBaseline):
		return legacyDeliveryCohortAdoptedBaseline
	}
	return legacyDeliveryCohortAdoptedCapped
}

// withinCompatibilityV1Row reports whether every ID in ids is in the
// compatibility policy V1 row of role, assigned service account permissions
// included. An unknown role (none included) has no row.
func withinCompatibilityV1Row(ids []string, role AgentRole) bool {
	row, ok := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, string(role), true)
	if !ok {
		return false
	}
	in := make(map[string]bool, len(row))
	for _, id := range row {
		in[id] = true
	}
	for _, id := range ids {
		if !in[id] {
			return false
		}
	}
	return true
}

// legacyInheritsUATGap reports whether a recorded bounded edge whose ceiling
// does not allow permissionID, one of the deliver permissions, may only
// inherit the gap a token-created ancestor left before the deliver
// permissions were intrinsic to its hop (uat_descendant_legacy): it was
// recorded by an agent delegator from an agent credential. The tolerant
// walk admits such a hop only when the hops above reach a hop recorded from
// a user access token whose uatHopEffectCeiling allows the permission,
// through hops of the same kind (hubDeliveryWalk, pendingUAT); otherwise
// its own deny stands.
func legacyInheritsUATGap(edge *store.DelegationEdge, permissionID string) bool {
	return legacyFallbackPermission(permissionID) &&
		edge.SourceCredentialKind == store.SourceCredentialAgent &&
		edge.DelegatorType == store.DelegationPrincipalAgent
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
	if !legacyFallbackCauses[strict.cause] || !legacyFallbackPermission(permissionID) {
		return strict
	}
	var (
		note hubDeliveryLegacyNote
		obs  hubDeliveryWalkObservation
	)
	ok, walkReason, walkCause := a.hubDeliveryWalkObserved(ctx, agentID, h.projectID, permissionID, nil, &note, &obs)
	if !ok && note.cohort == legacyDeliveryCohortUATDescendant {
		// The tolerant walk reached the token-created ancestor and refused a
		// later check there (for example an inactive creating user). Return
		// that deny, which names the cause, as the strict walk does for the
		// token-created agent itself.
		if explain != nil {
			*explain = append(*explain, DecisionStep{Step: "delegation_ceiling_delivery_denied", Detail: walkReason})
		}
		return hubDeliveryAdmissionOutcome{reason: walkReason, cause: walkCause, deniedCohort: legacyDeliveryCohortUATDescendant}
	}
	if !ok || note.cohort == "" {
		return strict
	}
	if note.cohort == legacyDeliveryCohortUATDescendant {
		// A descendant of a token-created agent is admitted as its
		// token-created ancestor is: the root user must still be admitted to
		// the project (ResolveProvenanceRoot), as on the strict walk. The
		// other legacy cohorts keep their behaviour from before the check.
		if denied, ok := a.uatDescendantRootDenied(ctx, agentID, permissionID, explain); ok {
			return denied
		}
	}
	reason := "delivery legacy fallback: " + note.cohort
	if note.depth > 0 {
		reason += fmt.Sprintf(" at depth %d", note.depth)
	}
	if explain != nil {
		*explain = append(*explain, DecisionStep{Step: "delegation_ceiling_delivery_legacy_fallback", Detail: reason})
	}
	return hubDeliveryAdmissionOutcome{ok: true, reason: reason, legacyCohort: note.cohort, uatIntrinsic: obs.uatIntrinsic}
}

// uatDescendantRootDenied applies the provenance root admission to a chain
// the tolerant walk passed under uat_descendant_legacy, exactly as the strict
// walk applies it to the token-created ancestor: any ResolveProvenanceRoot
// error denies, with its cause (root user not admitted, inactive, a chain
// mismatch, a lookup fault failing closed, and so on). It returns the deny and
// true, or false when the admission passes.
func (a *AuthzService) uatDescendantRootDenied(ctx context.Context, agentID, permissionID string, explain *[]DecisionStep) (hubDeliveryAdmissionOutcome, bool) {
	_, err := a.ResolveProvenanceRoot(ctx, agentID, ResolveProvenanceOptions{PermissionID: permissionID})
	if err == nil {
		return hubDeliveryAdmissionOutcome{}, false
	}
	cause := DenyCauseForProvenanceError(err)
	reason := "delivery provenance root denied: " + err.Error()
	switch {
	case errors.Is(err, ErrProvenanceNotAdmitted):
		reason = deliveryRootNotAdmittedReason + ": " + err.Error()
	case cause == DenyCauseCeilingError:
		reason = "delivery ceiling check failed (fail-closed): " + err.Error()
	}
	if explain != nil {
		*explain = append(*explain, DecisionStep{Step: "delegation_ceiling_delivery_denied", Detail: reason})
	}
	return hubDeliveryAdmissionOutcome{reason: reason, cause: cause, deniedCohort: legacyDeliveryCohortUATDescendant}, true
}

// deliveryLogDedup remembers recently logged keys for the fallback Warn: a
// key logged within window is not logged again. It holds at most max keys;
// when full it evicts the least recently logged key, so a hub with more
// keys than max degrades to re-logging only its oldest items, never to
// clearing everything.
type deliveryLogDedup struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	order  *list.List // front = most recently logged; values are *deliveryLogEntry
	byKey  map[string]*list.Element
}

type deliveryLogEntry struct {
	key string
	at  time.Time
}

func newDeliveryLogDedup(max int, window time.Duration) *deliveryLogDedup {
	return &deliveryLogDedup{max: max, window: window, order: list.New(), byKey: make(map[string]*list.Element)}
}

// first reports whether key was not logged within the window, and records
// it as logged at now when so.
func (d *deliveryLogDedup) first(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.byKey[key]; ok {
		e := el.Value.(*deliveryLogEntry)
		if now.Sub(e.at) < d.window {
			return false
		}
		e.at = now
		d.order.MoveToFront(el)
		return true
	}
	d.byKey[key] = d.order.PushFront(&deliveryLogEntry{key: key, at: now})
	for d.order.Len() > d.max {
		oldest := d.order.Back()
		d.order.Remove(oldest)
		delete(d.byKey, oldest.Value.(*deliveryLogEntry).key)
	}
	return true
}
