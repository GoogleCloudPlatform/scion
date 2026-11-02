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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hubDeliveryPermissionIDs are the only permissions a hub_delivery principal
// may ever request, with ActionDeliver (ptone/scion#2228 part 2). This is a
// fixed, named set rather than a derivation from the permissions registry:
// exactly these three IDs are in scope, and a future registry change that
// adds another *.deliver permission must not be admitted under hub_delivery
// without a deliberate edit here. deliverPermissionIDs
// (authz_delivery_gate.go), by contrast, is derived from the registry and
// drives the Step 0 gate for every credential kind.
var hubDeliveryPermissionIDs = map[string]struct{}{
	"secret.deliver":          {},
	"env_var.deliver":         {},
	"skill_injection.deliver": {},
}

// hubDeliveryIdentity is the internal credential a hub-side material
// delivery caller presents to request secret.deliver, env_var.deliver or
// skill_injection.deliver on behalf of one agent (ptone/scion#2228 part 2).
// It is produced only by newHubDeliveryIdentity, never appears in a request
// context, and carries no request-supplied data: every field is copied from
// the stored agent record at construction.
//
// evidence is a NAMED field, not an embedded one. Embedding
// *storedAgentIdentity would make hubDeliveryIdentity inherit its
// localAncestryProvenance marker method, which
// TestIdentityClassification_EveryTypeHasExplicitOutcome's SourceScan
// detects through embedded fields — and which would make
// AncestryIsHubAttested treat this credential as hub-attested. The
// credential attests nothing about how the agent came to exist (see
// AncestryIsHubAttested's doc comment); ancestry/progeny evidence for a
// deliver request is instead read explicitly from evidence, scoped to the
// three deliver permissions, by relationshipStageAncestryAttested and the
// progeny candidate builder (authz_relationship_rules.go).
type hubDeliveryIdentity struct {
	agentID      string
	projectID    string
	ancestry     []string
	originUserID string
	// boundAgentID names the agent this credential may act for. It equals
	// agentID by construction; the two are compared separately wherever the
	// bound check runs (Step 0b, authz.go; the step-10 gate arm,
	// authz_delegation_ceiling.go) so that a test-only literal built with a
	// mismatched boundAgentID exercises the same deny path a corrupted
	// credential would.
	boundAgentID string
	// evidence is the stored-agent-backed AgentIdentity used as the fact
	// source for stage-2 ancestry attestation and the progeny candidate,
	// instead of this type's own (always non-attested) identity.
	evidence *storedAgentIdentity
	// admission, when set, memoizes the step-10 admission walk per
	// permission for the life of this credential (hubDeliveryAdmission).
	// It is hub-internal state, not request data. Only launch selection
	// sets it (deliverySelector.mint), on a credential it mints for one
	// start or restart and then drops; a credential without it re-runs the
	// walk on every decision.
	admission *hubDeliveryAdmissionMemo
	// legacyFallback, when set, lets the admission admit a chain the walk
	// refuses only because of legacy provenance (hubDeliveryLegacyFallback,
	// material_delivery_legacy.go). It is hub-internal state, not request
	// data. Only launch selection sets it (deliverySelector.mint), for the
	// purposes legacyDeliveryFallbackPurpose names; every other credential
	// enforces the walk strictly.
	legacyFallback bool
}

// hubDeliveryAdmissionMemo holds the outcome of hubDeliveryAdmission's
// chain walk and provenance-root admission, per permission, for one
// delivery credential. The outcome depends only on the bound agent, the
// credential's project and the permission, not on the item, so every
// item a launch selection decides under one permission shares it. An
// allow and a deny (including a fail-closed lookup error) are both kept,
// so a selection judges every item against one reading of the chain.
type hubDeliveryAdmissionMemo struct {
	mu       sync.Mutex
	outcomes map[string]hubDeliveryAdmissionOutcome
}

type hubDeliveryAdmissionOutcome struct {
	ok     bool
	reason string
	cause  DenyCause
	// legacyCohort is set when the outcome is an allow by the legacy
	// fallback: the cohort (legacyDeliveryCohort*) of the first legacy hop.
	legacyCohort string
	// deniedCohort is set on a deny the fallback returned for a chain of
	// that cohort (uat_descendant_legacy whose root user is not admitted).
	deniedCohort string
	// uatIntrinsic is set on an allow whose walk passed a hop recorded from
	// a user access token only through the intrinsic rule
	// (uatIntrinsicDecides).
	uatIntrinsic bool
}

func newHubDeliveryAdmissionMemo() *hubDeliveryAdmissionMemo {
	return &hubDeliveryAdmissionMemo{outcomes: make(map[string]hubDeliveryAdmissionOutcome)}
}

func (m *hubDeliveryAdmissionMemo) load(permissionID string) (hubDeliveryAdmissionOutcome, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outcomes[permissionID]
	return o, ok
}

func (m *hubDeliveryAdmissionMemo) save(permissionID string, o hubDeliveryAdmissionOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes[permissionID] = o
}

// Compile-time interface assertion.
var _ AgentIdentity = (*hubDeliveryIdentity)(nil)

func (h *hubDeliveryIdentity) ID() string        { return h.agentID }
func (h *hubDeliveryIdentity) Type() string      { return "agent" }
func (h *hubDeliveryIdentity) ProjectID() string { return h.projectID }

// Scopes is always nil rather than the stored role: the 7b
// delivery_credential restriction replaces agentScopeRestriction for this
// type, so the credential's authority is fixed to hubDeliveryPermissionIDs.
func (h *hubDeliveryIdentity) Scopes() []AgentTokenScope     { return nil }
func (h *hubDeliveryIdentity) HasScope(AgentTokenScope) bool { return false }
func (h *hubDeliveryIdentity) Ancestry() []string            { return append([]string(nil), h.ancestry...) }
func (h *hubDeliveryIdentity) OriginUserID() string          { return h.originUserID }
func (h *hubDeliveryIdentity) TokenID() string               { return "" }
func (h *hubDeliveryIdentity) BoundAgentID() string          { return h.boundAgentID }

// newHubDeliveryIdentity is the only producer of hubDeliveryIdentity. It
// loads the agent from the store and fails for a missing, deleted or
// project-less agent, so a hubDeliveryIdentity returned with a nil error
// always has a non-empty boundAgentID/agentID and projectID — the typed-nil
// and empty-project cases that Step 0b and the step-10 gate arm each deny
// are reached only through a test-only struct literal, never through this
// constructor.
func (a *AuthzService) newHubDeliveryIdentity(ctx context.Context, agentID string) (*hubDeliveryIdentity, error) {
	if a.store == nil {
		return nil, fmt.Errorf("hub delivery identity for agent %s: store not available", agentID)
	}
	agent, err := a.store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("hub delivery identity for agent %s: %w", agentID, err)
	}
	if agent == nil {
		return nil, fmt.Errorf("hub delivery identity for agent %s: agent not found", agentID)
	}
	if !agent.DeletedAt.IsZero() {
		return nil, fmt.Errorf("hub delivery identity for agent %s: agent is deleted", agentID)
	}
	if agent.ProjectID == "" {
		return nil, fmt.Errorf("hub delivery identity for agent %s: agent has no project", agentID)
	}
	originUserID := ""
	if len(agent.Ancestry) > 0 {
		originUserID = agent.Ancestry[0]
	}
	return &hubDeliveryIdentity{
		agentID:      agent.ID,
		projectID:    agent.ProjectID,
		ancestry:     append([]string(nil), agent.Ancestry...),
		originUserID: originUserID,
		boundAgentID: agent.ID,
		evidence:     &storedAgentIdentity{agent: agent},
	}, nil
}

// deliveryCredentialRestriction is the 7b restriction for a hub_delivery
// principal: it allows exactly hubDeliveryPermissionIDs, in place of
// agentScopeRestriction. agentScopeRestriction would deny every permission
// for this type (hubDeliveryIdentity.Scopes() is always nil, and an agent
// JWT with no scopes denies everything by design), but the delivery
// credential's authority is the fixed deliver permission set, not a JWT
// scope grant.
func deliveryCredentialRestriction() Restriction {
	return Restriction{
		Kind:        "delivery_credential",
		Description: "delivery credential restriction",
		Check: func(permissionID string) bool {
			_, ok := hubDeliveryPermissionIDs[permissionID]
			return ok
		},
	}
}

// checkHubDeliveryCeiling is the step-10 arm for a hubDeliveryIdentity
// principal (ptone/scion#2228 part 2). checkDelegationCeiling routes every
// hubDeliveryIdentity here before its own AgentIdentity assertion, so the
// ordinary delegator-permission proof (walkDelegationChain) is never reached
// for this type: every failed condition below is a terminal, named deny,
// and neither this function nor hubDeliveryAdmission calls
// walkDelegationChain.
//
// The five gate conditions run first and perform no store read. A request
// that passes all five goes to hubDeliveryAdmission, the admission walk.
//
// h may be a typed nil (the caller's type assertion against *hubDeliveryIdentity
// succeeds even when the underlying pointer is nil); the first condition
// below handles that case before any field of h is read.
//
// Every deny here, and every deny from hubDeliveryAdmission, returns a nil
// error: a lookup error inside the walk is itself a deny with cause
// ceiling_error. So Decide's step-10 error branch never applies to a
// hub_delivery request, and nothing here can fall open.
func (a *AuthzService) checkHubDeliveryCeiling(
	ctx context.Context,
	req AuthzRequest,
	h *hubDeliveryIdentity,
	permissionID string,
	agentID string,
	explain *[]DecisionStep,
	cause *DenyCause,
) (bool, string, error) {
	deny := func(reason string) (bool, string, error) {
		if explain != nil {
			*explain = append(*explain, DecisionStep{
				Step:   "delegation_ceiling_delivery_denied",
				Detail: reason,
			})
		}
		return false, reason, nil
	}

	// A missing (typed-nil) credential is never bound to anything.
	if h == nil {
		return deny("delivery credential is missing")
	}

	// Keyed on the agentID argument actually passed to step 10, never on
	// req.Principal.ID. With Decide's own call this is agent.ID(), i.e.
	// h.agentID, so this reduces to "the bound ID is non-empty and equals
	// the identity's own ID" — but a direct call with a different agentID,
	// or a test-only literal whose boundAgentID disagrees with its own
	// agentID, denies here independently of Step 0b.
	if h.boundAgentID == "" || h.boundAgentID != agentID || h.agentID != agentID {
		return deny("delivery credential is bound to a different agent")
	}

	// Keyed on the permissionID argument checkDelegationCeiling receives,
	// the same permission its ordinary walk evaluates. An empty permission
	// is not one of the three deliver permissions, so it denies here.
	if _, ok := hubDeliveryPermissionIDs[permissionID]; !ok {
		return deny("delivery credential is limited to deliver permissions")
	}

	// No read allowance. ActionRead, ActionUse and every other action
	// deny — there is no "evaluate the ceiling for a non-deliver action"
	// case for this type.
	if req.Action != ActionDeliver {
		return deny("delivery credential is limited to deliver permissions")
	}

	// An empty project is a named deny, before any store read. The
	// constructor never returns a project-less agent, so this is reachable
	// only through a test-only literal; without it, an empty scope would
	// fail closed in the admission walk (no edge matches it), but the gate
	// names the reason itself.
	if h.projectID == "" {
		return deny("delivery credential has no project")
	}

	return a.hubDeliveryAdmission(ctx, req, h, permissionID, agentID, explain, cause)
}

// hubDeliveryAdmission is the step-10 admission walk for a hub_delivery
// request that passed every gate condition (ptone/scion#2228 part 2b). It
// does not require any delegator to hold the deliver
// permission through a role. Instead, in order:
//
//   - the resource must not be in another project than the credential's
//     (cause ceiling_provenance_missing: no edge of the agent exists there);
//   - hubDeliveryChainWalk: every hop of the target's delegation chain in the
//     credential's project has exactly one active edge, is not the migration
//     sentinel, passes the local-development source check, has known
//     recorded provenance, has an effect ceiling that allows the exact
//     permission with no self-operation exception, and names a live
//     delegator;
//   - ResolveProvenanceRoot(target, {PermissionID}) succeeds: the recorded
//     provenance is consistent, and the root user is active and admitted to
//     the target's project for the exact permission
//     (ProjectAdmissionForClass).
//
// The item grant itself (progeny today) is authorized separately, by the
// relationship stages that ran before step 10. Every failure is a deny
// with a named reason and cause and a nil error.
func (a *AuthzService) hubDeliveryAdmission(
	ctx context.Context,
	req AuthzRequest,
	h *hubDeliveryIdentity,
	permissionID string,
	agentID string,
	explain *[]DecisionStep,
	cause *DenyCause,
) (bool, string, error) {
	addStep := func(step, detail string) {
		if explain != nil {
			*explain = append(*explain, DecisionStep{Step: step, Detail: detail})
		}
	}
	deny := func(reason string, c DenyCause) (bool, string, error) {
		addStep("delegation_ceiling_delivery_denied", reason)
		if cause != nil {
			*cause = c
		}
		return false, reason, nil
	}

	if rp := resourceProjectScope(req.Resource); rp != "" && rp != h.projectID {
		return deny("delivery resource is in a different project", DenyCauseCeilingProvenanceMissing)
	}

	// The walk and the root admission do not depend on the resource, so a
	// credential carrying a memo (launch selection) runs them once per
	// permission. An explained decision always runs them, so its trace
	// carries every step.
	memo := h.admission
	if explain != nil {
		memo = nil
	}
	var o hubDeliveryAdmissionOutcome
	cached := false
	if memo != nil {
		o, cached = memo.load(permissionID)
	}
	if !cached {
		o = a.hubDeliveryAdmissionOutcomeFor(ctx, h, permissionID, agentID, explain)
		if memo != nil {
			memo.save(permissionID, o)
		}
		if o.ok && o.uatIntrinsic {
			a.recordUATIntrinsic(ctx, permissionID, uatIntrinsicPathLaunchDelivery)
		}
	}
	if !o.ok {
		if cause != nil {
			*cause = o.cause
		}
		return false, o.reason, nil
	}
	return true, o.reason, nil
}

// hubDeliveryAdmissionOutcomeFor is the admission outcome for one
// permission: hubDeliveryChainAdmission, and, when that denies and the
// credential carries legacyFallback, the legacy fallback
// (hubDeliveryLegacyFallback).
func (a *AuthzService) hubDeliveryAdmissionOutcomeFor(
	ctx context.Context,
	h *hubDeliveryIdentity,
	permissionID string,
	agentID string,
	explain *[]DecisionStep,
) hubDeliveryAdmissionOutcome {
	o := a.hubDeliveryChainAdmission(ctx, h, permissionID, agentID, explain)
	if o.ok || !h.legacyFallback {
		return o
	}
	return a.hubDeliveryLegacyFallback(ctx, h, permissionID, agentID, o, explain)
}

// hubDeliveryChainAdmission runs hubDeliveryChainWalk and then
// ResolveProvenanceRoot for the target, as described on
// hubDeliveryAdmission, and returns the outcome.
func (a *AuthzService) hubDeliveryChainAdmission(
	ctx context.Context,
	h *hubDeliveryIdentity,
	permissionID string,
	agentID string,
	explain *[]DecisionStep,
) hubDeliveryAdmissionOutcome {
	addStep := func(step, detail string) {
		if explain != nil {
			*explain = append(*explain, DecisionStep{Step: step, Detail: detail})
		}
	}

	var obs hubDeliveryWalkObservation
	if ok, reason, c := a.hubDeliveryWalkObserved(ctx, agentID, h.projectID, permissionID, explain, nil, &obs); !ok {
		return hubDeliveryAdmissionOutcome{reason: reason, cause: c}
	}

	root, err := a.ResolveProvenanceRoot(ctx, agentID, ResolveProvenanceOptions{PermissionID: permissionID})
	if err != nil {
		c := DenyCauseForProvenanceError(err)
		reason := "delivery provenance root denied: " + err.Error()
		if errors.Is(err, ErrProvenanceNotAdmitted) {
			// Named for the launch withheld Warn and the census: the chain
			// passed, and the root user is not admitted to the agent's project.
			reason = deliveryRootNotAdmittedReason + ": " + err.Error()
		}
		if c == DenyCauseCeilingError {
			reason = "delivery ceiling check failed (fail-closed): " + err.Error()
		}
		addStep("delegation_ceiling_delivery_denied", reason)
		return hubDeliveryAdmissionOutcome{reason: reason, cause: c}
	}
	// ResolveProvenanceRoot evaluates admission against the stored agent's
	// project, the scope of the target's own edge (root.Edge.ScopeID). The
	// step names that project rather than the credential's, so the trace
	// shows what was evaluated even if the two ever differed.
	addStep("delegation_ceiling_delivery_root",
		fmt.Sprintf("root user %s admitted to project %s for %s", root.RootUser.ID, root.Edge.ScopeID, permissionID))
	return hubDeliveryAdmissionOutcome{ok: true, reason: "delivery ceiling passed", uatIntrinsic: obs.uatIntrinsic}
}

// deliveryRootNotAdmittedReason begins the reason of a delivery admission
// whose chain passed and whose root user is not admitted to the agent's
// project. It keeps the "delivery provenance root denied" prefix the census
// reads (launchDeliveryOutcomeCohort).
const deliveryRootNotAdmittedReason = "delivery provenance root denied: root user not admitted to the project"

// hdDelegatorLiveness is one hop's delegator state, as seen by
// hubDeliveryDelegatorLiveness.
type hdDelegatorLiveness int

const (
	hdDelegatorLive        hdDelegatorLiveness = iota
	hdDelegatorNotFound                        // no record
	hdDelegatorDeleted                         // agent with DeletedAt set
	hdDelegatorHeld                            // agent with an active hold (ptone/scion#3433)
	hdDelegatorInactive                        // user whose status is not active
	hdDelegatorUnsupported                     // delegator type is neither user nor agent
)

// hubDeliveryDelegatorLiveness reports one hop's delegator liveness for
// the hub_delivery admission walk. It never consults roles or super-admin
// status (a suspended super-admin is inactive like any other user), and it
// never reads or writes the request-scoped delegator-authority cache. A
// stopped agent is live; a held agent is not (ptone/scion#3433), as in
// checkAgentHoldsPermission and agentStanding. The returned string is the
// user's status for hdDelegatorInactive. Any lookup error other than
// not-found, including a hold lookup error, is returned.
func (a *AuthzService) hubDeliveryDelegatorLiveness(ctx context.Context, delegatorType, delegatorID string) (hdDelegatorLiveness, string, error) {
	switch delegatorType {
	case store.DelegationPrincipalUser:
		u, err := a.store.GetUser(ctx, delegatorID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && u == nil) {
			return hdDelegatorNotFound, "", nil
		}
		if err != nil {
			return hdDelegatorLive, "", err
		}
		if u.Status != store.UserStatusActive {
			return hdDelegatorInactive, u.Status, nil
		}
		return hdDelegatorLive, "", nil
	case store.DelegationPrincipalAgent:
		ag, err := a.store.GetAgent(ctx, delegatorID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && ag == nil) {
			return hdDelegatorNotFound, "", nil
		}
		if err != nil {
			return hdDelegatorLive, "", err
		}
		if !ag.DeletedAt.IsZero() {
			return hdDelegatorDeleted, "", nil
		}
		held, err := a.store.HasActiveAgentHold(ctx, delegatorID)
		if err != nil {
			return hdDelegatorLive, "", fmt.Errorf("agent %s hold lookup: %w", delegatorID, err)
		}
		if held {
			return hdDelegatorHeld, "", nil
		}
		return hdDelegatorLive, "", nil
	default:
		return hdDelegatorUnsupported, "", nil
	}
}

// hubDeliveryChainWalk walks the delegation chain upward from startAgentID
// in project scopeProjectID and applies the delivery admission per-hop
// checks (ptone/scion#2228 part 2b) to every hop, for the exact
// permissionID. It returns whether
// every hop passed, the deny reason and the deny cause. It never returns an
// error: a lookup error is a deny with cause ceiling_error. It shares only
// the edge-data cache with the ordinary walk (getCachedDelegationEdges); it
// has no pre-backfill or read allowance, and never calls
// migrationSentinelCeiling.
//
// Per hop, in order:
//  1. exactly one active edge in scope (none: ceiling_provenance_missing;
//     more than one: ceiling_provenance_ambiguous);
//  2. not the migration sentinel (ceiling_orphaned);
//  3. local-development provenance needs dev auth on this server and the
//     delegator user:DevUserID (ceiling_source_not_allowed);
//  4. known provenance version (ceiling_unrecorded);
//  5. effect ceiling: principal passes, bounded must allow permissionID
//     with no self-operation exception (ceiling_effect_exceeded), read
//     through uatHopEffectCeiling, so the deliver permissions are intrinsic
//     to a hop recorded from a user access token; any other kind denies
//     (ceiling_unrecorded);
//  6. live delegator (hubDeliveryDelegatorLiveness); a held agent
//     delegator is not live;
//  7. a user delegator ends the chain; an agent delegator is the next hop;
//     the depth bound and a repeated agent deny with no cause, like the
//     ordinary walk.
//
// Before step 1 of every hop, the hop's delegate agent must not be held
// (ptone/scion#3433): at depth 0 that is the target agent itself, and at
// every later depth it is the agent delegator of the hop below, so a held
// agent anywhere on the chain denies, matching agentStanding. A held agent
// denies with cause ceiling_orphaned, the cause the ordinary walk records
// for a non-live agent link (checkAgentHoldsPermission); a hold lookup
// error denies with cause ceiling_error.
func (a *AuthzService) hubDeliveryChainWalk(ctx context.Context, startAgentID, scopeProjectID, permissionID string, explain *[]DecisionStep) (bool, string, DenyCause) {
	return a.hubDeliveryWalk(ctx, startAgentID, scopeProjectID, permissionID, explain, nil)
}

// hubDeliveryWalk is hubDeliveryChainWalk. With legacy nil it is exactly
// the strict walk. With legacy set it is the legacy-tolerant walk of the
// fallback (material_delivery_legacy.go): at the legacy points named
// there it records the hop's cohort in legacy and goes on instead of
// denying; every other check is unchanged.
func (a *AuthzService) hubDeliveryWalk(ctx context.Context, startAgentID, scopeProjectID, permissionID string, explain *[]DecisionStep, legacy *hubDeliveryLegacyNote) (bool, string, DenyCause) {
	return a.hubDeliveryWalkObserved(ctx, startAgentID, scopeProjectID, permissionID, explain, legacy, nil)
}

// hubDeliveryWalkObservation is what a walk reports besides its verdict.
type hubDeliveryWalkObservation struct {
	// uatIntrinsic: a hop recorded from a user access token passed only
	// through the intrinsic rule (uatIntrinsicDecides).
	uatIntrinsic bool
}

// hubDeliveryWalkObserved is hubDeliveryWalk that also fills obs when it is
// not nil. obs never changes the verdict.
func (a *AuthzService) hubDeliveryWalkObserved(ctx context.Context, startAgentID, scopeProjectID, permissionID string, explain *[]DecisionStep, legacy *hubDeliveryLegacyNote, obs *hubDeliveryWalkObservation) (bool, string, DenyCause) {
	// The tolerant walk applies only to the delivery permissions
	// (legacyFallbackPermission); for any other permission it is the strict
	// walk, whatever the caller passed.
	if legacy != nil && !legacyFallbackPermission(permissionID) {
		legacy = nil
	}
	addStep := func(step, detail string) {
		if explain != nil {
			*explain = append(*explain, DecisionStep{Step: step, Detail: detail})
		}
	}
	deny := func(reason string, c DenyCause) (bool, string, DenyCause) {
		addStep("delegation_ceiling_delivery_denied", reason)
		return false, reason, c
	}
	failClosed := func(err error) (bool, string, DenyCause) {
		return deny("delivery ceiling check failed (fail-closed): "+err.Error(), DenyCauseCeilingError)
	}
	if a.store == nil {
		return failClosed(errors.New("store not available"))
	}
	if permissionID == "" || scopeProjectID == "" {
		return deny("delivery ceiling: no permission or project to evaluate", "")
	}

	// The delegator side is evaluated for principals other than the
	// requester, so it must never read the requester's memoized inputs.
	ctx = maskAuthzInputs(ctx)

	visited := make(map[string]bool, maxDelegationDepth+1)
	delegateID := startAgentID
	// pendingUAT (tolerant walk only) is set by an agent-recorded hop whose
	// ceiling lacks the permission because an ancestor was created from a
	// user access token (legacyInheritsUATGap): the uat_descendant_legacy
	// cohort. The next hops must reach a hop recorded from a user access
	// token whose uatHopEffectCeiling allows the permission, through hops
	// of the same kind, or the walk denies with pendingReason, the first
	// such hop's own deny.
	pendingUAT, pendingReason := false, ""
	intrinsic := false
	for depth := 0; ; depth++ {
		if depth > maxDelegationDepth {
			return deny("delegation chain exceeded maximum depth", "")
		}
		if visited[delegateID] {
			return deny("delegation chain cycle", "")
		}
		visited[delegateID] = true

		held, err := a.store.HasActiveAgentHold(ctx, delegateID)
		if err != nil {
			return failClosed(fmt.Errorf("agent %s hold lookup: %w", delegateID, err))
		}
		if held {
			return deny(fmt.Sprintf("agent %s is held", delegateID), DenyCauseCeilingOrphaned)
		}

		allEdges, err := a.getCachedDelegationEdges(ctx, store.DelegationPrincipalAgent, delegateID)
		if err != nil {
			return failClosed(err)
		}
		var active []*store.DelegationEdge
		for _, e := range filterEdgesByScope(allEdges, store.RoleScopeProject, scopeProjectID) {
			if e.Active {
				active = append(active, e)
			}
		}
		if len(active) == 0 {
			addStep("delegation_ceiling_delivery_hop", fmt.Sprintf("agent:%s has no active edge in project %s", delegateID, scopeProjectID))
			if legacy != nil {
				if pendingUAT {
					return deny(pendingReason, DenyCauseCeilingEffectExceeded)
				}
				legacy.note(legacyDeliveryCohortNoEdge, depth)
				return a.hubDeliveryLegacyAncestry(ctx, delegateID, deny, failClosed)
			}
			return deny("no active delegation edge", DenyCauseCeilingProvenanceMissing)
		}
		if len(active) > 1 {
			addStep("delegation_ceiling_delivery_hop", fmt.Sprintf("agent:%s has %d active edges in project %s", delegateID, len(active), scopeProjectID))
			return deny("multiple active delegation edges", DenyCauseCeilingProvenanceAmbiguous)
		}
		edge := active[0]
		addStep("delegation_ceiling_delivery_hop",
			fmt.Sprintf("edge %s: %s:%s -> agent:%s, ceiling %q, provenance version %d",
				edge.ID, edge.DelegatorType, edge.DelegatorID, delegateID, edge.Kind, edge.ProvenanceVersion))

		if isMigrationSentinel(edge) {
			if legacy != nil {
				if pendingUAT {
					return deny(pendingReason, DenyCauseCeilingEffectExceeded)
				}
				legacy.note(legacyDeliveryCohortSentinelEdge, depth)
				return a.hubDeliveryLegacyAncestry(ctx, delegateID, deny, failClosed)
			}
			return deny("migration-provenance delegation supplies no delivery authority", DenyCauseCeilingOrphaned)
		}
		if edgeHasDevLocalProvenance(edge) {
			if !a.devLocalAuthorityEnabled() {
				return deny("delegation ceiling: "+reasonDevLocalDisabled, DenyCauseCeilingSourceNotAllowed)
			}
			if edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID != DevUserID {
				return deny("delegation ceiling: local development provenance with another delegator", DenyCauseCeilingSourceNotAllowed)
			}
		}
		// hopCohort is the legacy cohort the tolerant walk lets this hop
		// through under; inherits marks an agent-recorded hop that only
		// inherits a UAT-created ancestor's gap. Both stay unset on the
		// strict walk. uatHop marks a hop recorded from a user access token
		// whose uatHopEffectCeiling allows the permission.
		hopCohort, inherits, uatHop := "", false, false
		switch {
		case !knownProvenanceVersion(edge.ProvenanceVersion) || (edge.Kind != store.EffectCeilingPrincipal && edge.Kind != store.EffectCeilingBounded):
			if legacy == nil {
				return deny(fmt.Sprintf("delegation ceiling: %s requires recorded provenance", permissionID), DenyCauseCeilingUnrecorded)
			}
			// An unrecorded edge has no ceiling to apply; its delegator is
			// still checked below and the walk goes on.
			hopCohort = legacyDeliveryCohortUnadoptedEdge
		case edge.Kind == store.EffectCeilingBounded && !EffectCeilingAllows(uatHopEffectCeiling(edge.DelegatorType, edge.DelegatorID, edge.AuthorityProvenance, edge.EffectCeiling), permissionID, false):
			exceeded := fmt.Sprintf("delegation ceiling: %s is outside the frozen effect ceiling", permissionID)
			if legacy != nil {
				hopCohort = legacyBoundedEdgeCohort(edge, permissionID)
				inherits = hopCohort == "" && legacyInheritsUATGap(edge, permissionID)
			}
			if hopCohort == "" && !inherits {
				return deny(exceeded, DenyCauseCeilingEffectExceeded)
			}
			if inherits && !pendingUAT {
				pendingReason = exceeded
				hopCohort = legacyDeliveryCohortUATDescendant
			}
		case uatRecordedHop(edge.DelegatorType, edge.DelegatorID, edge.AuthorityProvenance, edge.EffectCeiling):
			uatHop = true
			if uatIntrinsicDecides(edge.DelegatorType, edge.DelegatorID, edge.AuthorityProvenance, edge.EffectCeiling, permissionID) {
				addStep("delegation_ceiling_delivery_uat_intrinsic",
					fmt.Sprintf("edge %s: recorded from a user access token; %s is intrinsic to the hop", edge.ID, permissionID))
				intrinsic = true
			}
		}
		if pendingUAT {
			switch {
			case uatHop:
				pendingUAT = false
			case inherits:
			default:
				// The ancestor that should explain the missing permission
				// is not a UAT-created hop: the hop below narrowed it.
				return deny(pendingReason, DenyCauseCeilingEffectExceeded)
			}
		} else if inherits {
			pendingUAT = true
		}
		if hopCohort != "" {
			legacy.note(hopCohort, depth)
		}

		live, status, err := a.hubDeliveryDelegatorLiveness(ctx, edge.DelegatorType, edge.DelegatorID)
		if err != nil {
			return failClosed(err)
		}
		switch live {
		case hdDelegatorLive:
		case hdDelegatorNotFound, hdDelegatorDeleted:
			if edge.DelegatorType == store.DelegationPrincipalAgent {
				return deny(fmt.Sprintf("delegator agent %s is not live", edge.DelegatorID), DenyCauseCeilingOrphaned)
			}
			return deny(fmt.Sprintf("delegator %s is not live", edge.DelegatorID), DenyCauseCeilingOrphaned)
		case hdDelegatorHeld:
			return deny(fmt.Sprintf("delegator agent %s is held", edge.DelegatorID), DenyCauseCeilingOrphaned)
		case hdDelegatorInactive:
			return deny(fmt.Sprintf("delegator %s is %s", edge.DelegatorID, status), DenyCauseCeilingDelegatorLacksPermission)
		default:
			return deny("unsupported delegator type", "")
		}

		if edge.DelegatorType == store.DelegationPrincipalUser {
			if pendingUAT {
				// Unreachable: an inheriting hop has an agent delegator.
				return deny(pendingReason, DenyCauseCeilingEffectExceeded)
			}
			if obs != nil {
				obs.uatIntrinsic = intrinsic
			}
			return true, "delivery ceiling chain passed", ""
		}
		delegateID = edge.DelegatorID
	}
}

// hubDeliverySourceHolds is relationship stage 4 for a hub_delivery
// principal (ptone/scion#2228 part 2b): it decides
// whether the sharing source's owner may supply the item. It replaces the
// owner-activity and owner-delegation checks other principals get
// (relationshipSourceStage), which type the owner by probing the store and
// would require an agent owner to hold the deliver permission through a
// role. It returns the candidate detail on failure and never an error.
//
// The owner is typed by its position in the target agent's stored
// ancestry (the credential's stored-agent evidence), never by probing an
// ID: position 0 is the root user, later positions are creator agents.
//
//   - S1: the tuple is a hub_delivery credential with evidence, bound to its
//     own agent, for one of the three deliver permissions with ActionDeliver.
//   - S2: the owner is non-empty and occurs exactly once in the ancestry.
//   - S3: a user source must not also resolve as an agent.
//   - S4: a user source must exist and be active.
//
// An agent source (position 1 or later) is refused. Admitting one is the
// agent-owned source arm, which waits on an open product decision
// (ptone/scion#2228): an agent creator differs from the owner of the
// user-scope row it shares, and whether that sharing needs the owner's
// authorization is not yet decided. Until then no agent-owned source is
// delivered.
func (a *AuthzService) hubDeliverySourceHolds(ctx context.Context, h *hubDeliveryIdentity, src *SharingSource, resource Resource, action Action, permissionID string) (bool, string) {
	// resource is unused by the user-source checks. It is kept in the
	// signature for the agent-owned source arm, which evaluates the
	// source agent's chain for this resource once that arm is decided.
	_ = resource
	// S1: the step-10 gate texts.
	if h == nil {
		return false, "delivery credential is missing"
	}
	if h.evidence == nil {
		return false, "delivery credential has no stored-agent evidence"
	}
	if h.boundAgentID == "" || h.boundAgentID != h.agentID {
		return false, "delivery credential is bound to a different agent"
	}
	if _, ok := hubDeliveryPermissionIDs[permissionID]; !ok || action != ActionDeliver {
		return false, "delivery credential is limited to deliver permissions"
	}
	if src == nil {
		return false, "delivery source has no owner"
	}

	// S2: typed position in the stored ancestry.
	owner := src.OwnerID
	if owner == "" {
		return false, "delivery source has no owner"
	}
	pos, count := -1, 0
	for i, id := range h.evidence.Ancestry() {
		if id == owner {
			if pos < 0 {
				pos = i
			}
			count++
		}
	}
	switch {
	case count == 0:
		return false, "delivery source is not in the target's ancestry"
	case count > 1:
		return false, "delivery source appears more than once in the target's ancestry"
	}
	if pos > 0 {
		// The agent-owned source arm is not enabled: it waits on the open
		// decision on agent-shared user-scope rows (ptone/scion#2228). Deny
		// before any store read.
		return false, "delivery source is an agent; agent-owned sources are not admitted"
	}

	if a.store == nil {
		return false, "delivery source check failed (fail-closed): store not available"
	}
	// S3: the user source must not also name an agent.
	ag, err := a.store.GetAgent(ctx, owner)
	switch {
	case err == nil && ag != nil:
		return false, "delivery source owner resolves to both a user and an agent"
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return false, "delivery source check failed (fail-closed): " + err.Error()
	}
	// S4: the user source is live.
	u, err := a.store.GetUser(ctx, owner)
	if errors.Is(err, store.ErrNotFound) || (err == nil && u == nil) {
		return false, fmt.Sprintf("delivery source user %s is not live", owner)
	}
	if err != nil {
		return false, "delivery source check failed (fail-closed): " + err.Error()
	}
	if u.Status != store.UserStatusActive {
		return false, fmt.Sprintf("delivery source user %s is %s", owner, u.Status)
	}
	return true, ""
}

// relationshipStageAncestryAttested is runRelationshipStages' stage-2 check.
// For every principal other than hubDeliveryIdentity it is exactly
// relationshipAncestryAttested, which is also what ProgenyListPredicate and
// EvaluateProgeny call directly outside Decide. For a hub_delivery principal
// requesting one of the three deliver permissions, it reads attestation from
// the stored-agent evidence instead of the (always non-attested) credential
// itself — the credential attests nothing on its own; the stored agent
// record is the evidence. For any other permission it reports not attested,
// so a hub_delivery principal can never ride this exception into a
// non-deliver relationship grant (Step 0b already denies a non-deliver
// permission in Decide; this guard keeps the rule true for a direct
// runRelationshipStages call as well).
func relationshipStageAncestryAttested(principal PrincipalContext, permissionID string) bool {
	if h, ok := principal.Identity.(*hubDeliveryIdentity); ok {
		if h == nil || h.evidence == nil {
			return false
		}
		if _, deliver := hubDeliveryPermissionIDs[permissionID]; !deliver {
			return false
		}
		return AncestryIsHubAttested(h.evidence)
	}
	return relationshipAncestryAttested(principal)
}
