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
// for this type — every failed condition below is a terminal, named deny,
// and the function never calls walkDelegationChain itself. The five
// conditions below are the whole arm: the function performs no store read,
// and every request that passes all five reaches the terminal deny below —
// admission is a dedicated walk function gated on its own set membership
// (deliveryCredentialKinds), added together with that membership in one
// change.
//
// h may be a typed nil (the caller's type assertion against *hubDeliveryIdentity
// succeeds even when the underlying pointer is nil); the first condition
// below handles that case before any field of h is read.
//
// Every deny here returns a nil error with cause left at its zero value
// (""), so Decide's read-only allow-on-error for step-10 errors
// (authz.go, isReadOnlyOperation) can never apply to a hub_delivery deny.
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
	// already fail closed inside walkDelegationChain, but this type never
	// reaches that function, so the gate names the reason itself.
	if h.projectID == "" {
		return deny("delivery credential has no project")
	}

	// Terminal deny. Admission (the per-hop walk plus
	// ResolveProvenanceRoot) is a later change, gated on adding the kind to
	// deliveryCredentialKinds; every hub_delivery request that reaches step
	// 10 denies here, never reaching walkDelegationChain.
	return deny("delivery credential admission is not enabled")
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
