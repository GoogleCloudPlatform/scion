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

// Relationship candidates (ptone/scion#2119).
//
// A named relationship (owner, ancestor, progeny, hub-member service-account
// assign, creator user skill) produces a typed candidate for the requested
// permission. Every candidate passes the same ordered stages before it may
// grant anything:
//
//  1. relationship_policy  — the permission is listed for the relationship,
//     principal kind and resource type in permissions.RelationshipPolicies;
//  2. untrusted_ancestry   — a relationship derived from the ancestry chain
//     requires hub-attested ancestry, for every principal kind;
//  3. relationship_fact    — the rule's store fact holds (hub membership, a
//     progeny sharing source); a lookup failure rejects the candidate;
//  4. source_inactive      — the sharing source's owner is still active;
//  5. the request's restrictions (credential scope, agent token scope,
//     access constraints) — the same slice the kernel applied.
//
// The first accepted candidate becomes the decision. Every evaluated
// candidate is recorded in DecisionProvenance.Relationships when the
// request asks for an explanation.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RelationshipRuleID names a relationship rule. The values are the
// Relationship strings used in permissions.RelationshipPolicies.
type RelationshipRuleID string

const (
	RelationshipRuleOwner             RelationshipRuleID = "owner"
	RelationshipRuleAncestor          RelationshipRuleID = "ancestor"
	RelationshipRuleProgeny           RelationshipRuleID = "progeny"
	RelationshipRuleHubMemberSAAssign RelationshipRuleID = "hub_member_sa_assign"
	RelationshipRuleCreatorUserSkill  RelationshipRuleID = "creator_user_skill"

	// Association relationships are typed so delivery rules can name them.
	// They have no policy rows until their permissions are registered, so
	// they currently admit nothing.
	RelationshipRuleProjectAssociation RelationshipRuleID = "project_association"
	RelationshipRuleHubAssociation     RelationshipRuleID = "hub_association"
	RelationshipRuleBrokerAssociation  RelationshipRuleID = "broker_association"
)

// Rejection kinds recorded on relationship candidates. Restriction kinds
// from the request's restriction slice (for example "credential_scope",
// "access_constraint") are recorded unchanged.
const (
	RelationshipRejectPolicy            = "relationship_policy"
	RelationshipRejectUntrustedAncestry = "untrusted_ancestry"
	RelationshipRejectFact              = "relationship_fact"
	RelationshipRejectSourceInactive    = "source_inactive"
)

// relationshipRejectKeepsKernelReason reports whether a rejection kind
// leaves the kernel's deny reason in place. A permission outside the
// relationship's policy, or a relationship fact that does not hold, means
// the relationship simply does not apply; every other kind is a restriction
// on a relationship that does apply and is named in the deny reason.
func relationshipRejectKeepsKernelReason(kind string) bool {
	return kind == RelationshipRejectPolicy || kind == RelationshipRejectFact
}

// relationshipAncestryAttested is the single check every ancestry-derived
// relationship uses for hub attestation.
func relationshipAncestryAttested(principal PrincipalContext) bool {
	return AncestryIsHubAttested(principal.Identity)
}

// RelationshipSource identifies the record a relationship derives from.
// It carries identifiers only, never material.
type RelationshipSource struct {
	// Kind is the source kind, e.g. "secret", "user_skill".
	Kind string `json:"kind"`
	// ID is the source record ID. Recorded only for accepted candidates.
	ID string `json:"id,omitempty"`
	// OwnerID is the sharing-source owner. Recorded only for accepted
	// candidates.
	OwnerID string `json:"ownerId,omitempty"`
}

// RelationshipCandidateResult is the provenance for one relationship
// candidate.
type RelationshipCandidateResult struct {
	Rule       RelationshipRuleID  `json:"rule"`
	Permission string              `json:"permission"`
	Accepted   bool                `json:"accepted"`
	RejectedBy string              `json:"rejectedBy,omitempty"`
	Detail     string              `json:"detail,omitempty"`
	Source     *RelationshipSource `json:"source,omitempty"`
}

// relationshipCandidate is a relationship that structurally holds between
// the principal and the resource, before any stage has run.
type relationshipCandidate struct {
	rule RelationshipRuleID
	// usesAncestry marks candidates derived from the ancestry chain; they
	// require hub-attested ancestry.
	usesAncestry bool
	// fact, when set, resolves the rule's store fact. It returns the
	// sharing source (if any), whether the fact holds, and a detail.
	fact func(ctx context.Context) (*SharingSource, bool, string)
	// decision is the allow decision the candidate produces when accepted.
	decision Decision
}

// relationshipOutcome is the result of evaluating all candidates.
type relationshipOutcome struct {
	results  []RelationshipCandidateResult
	accepted *Decision
	// restrictedBy is the first rejection kind that names a restriction on
	// an applicable relationship (see relationshipRejectKeepsKernelReason).
	restrictedBy string
}

// isHubScopedServiceAccount reports whether the resource is a hub-scoped
// GCP service account (no parent).
func isHubScopedServiceAccount(resource Resource) bool {
	return resource.Type == "gcp_service_account" && resource.ParentType == "" && resource.ParentID == ""
}

// relationshipCandidates lists, in a stable order (ancestor, owner,
// hub-member assign, creator skill, progeny), the relationships that
// structurally hold for this principal and resource.
func (a *AuthzService) relationshipCandidates(principal PrincipalContext, resource Resource, action Action, permissionID string) []relationshipCandidate {
	var out []relationshipCandidate

	// Ancestor: the principal is in the resource's creation chain.
	if principal.ID != "" && canAccessAsAncestor(principal.ID, resource) {
		out = append(out, relationshipCandidate{
			rule:         RelationshipRuleAncestor,
			usesAncestry: true,
			decision: Decision{
				Allowed:      true,
				Reason:       "relationship grant: ancestor access",
				Scope:        ScopeTypeRelationship,
				MatchedGrant: "ancestor",
			},
		})
	}

	// Owner: a user principal that owns the resource. Assign on a hub-scoped
	// service account is governed by hub membership instead.
	if isUserPrincipal(principal.Kind) && resource.OwnerID != "" && resource.OwnerID == principal.ID &&
		!(action == ActionAssign && isHubScopedServiceAccount(resource)) {
		out = append(out, relationshipCandidate{
			rule: RelationshipRuleOwner,
			decision: Decision{
				Allowed:      true,
				Reason:       "relationship grant: resource owner",
				Scope:        ScopeTypeRelationship,
				MatchedGrant: "owner",
			},
		})
	}

	// Hub-member assign: a current hub member may assign hub-scoped
	// service accounts.
	if isUserPrincipal(principal.Kind) && principal.ID != "" && isHubScopedServiceAccount(resource) {
		userID := principal.ID
		out = append(out, relationshipCandidate{
			rule: RelationshipRuleHubMemberSAAssign,
			fact: func(ctx context.Context) (*SharingSource, bool, string) {
				if a.isCurrentHubMember(ctx, userID) {
					return nil, true, ""
				}
				return nil, false, "principal is not a current hub member"
			},
			decision: Decision{
				Allowed:      true,
				Reason:       "relationship grant: hub member hub-scoped assign",
				Scope:        "hub",
				MatchedGrant: "hub-member-assign",
			},
		})
	}

	// Creator user skill: an agent reads its origin user's user-scoped
	// skills. The origin user is the root of the ancestry chain.
	if isAgentPrincipal(principal.Kind) && resource.Type == "skill" &&
		resource.ScopeKind == store.SkillScopeUser && resource.ScopeUserID != "" {
		if agent, ok := principal.Identity.(AgentIdentity); ok && agent.OriginUserID() != "" &&
			agent.OriginUserID() == resource.ScopeUserID {
			src := &SharingSource{
				Kind:    "user_skill",
				ID:      resource.ID,
				OwnerID: agent.OriginUserID(),
				Policy:  SharingPolicyOriginDescendants,
			}
			out = append(out, relationshipCandidate{
				rule:         RelationshipRuleCreatorUserSkill,
				usesAncestry: true,
				// agentCreatorUserSkillGrant (authz_skill_scope.go) remains
				// the definition of the rule's shape.
				fact: func(context.Context) (*SharingSource, bool, string) {
					if _, ok := agentCreatorUserSkillGrant(principal, resource, action); !ok {
						return src, false, "creator user-skill shape does not hold"
					}
					return src, true, ""
				},
				decision: Decision{
					Allowed:      true,
					Reason:       "relationship grant: creator user skill",
					Scope:        ScopeTypeRelationship,
					MatchedGrant: "creator-user-skill",
				},
			})
		}
	}

	// Progeny: an agent reads a sharing source opted in by a member of its
	// ancestry chain. Only read actions match. Every kind with a progeny
	// adapter (built-in or registered) gets a candidate, so point reads and
	// ProgenyListPredicate evaluate the same kinds.
	if isAgentPrincipal(principal.Kind) && action == ActionRead {
		if relType := a.progenyRelationshipType(resource.Type); relType != "" {
			if agent, ok := principal.Identity.(AgentIdentity); ok {
				resourceID := resource.ID
				resourceType := resource.Type
				granted := allowRelationship(relType, resource.Type, resource.ID, agent.ID())
				out = append(out, relationshipCandidate{
					rule:         RelationshipRuleProgeny,
					usesAncestry: true,
					fact: func(ctx context.Context) (*SharingSource, bool, string) {
						return a.progenySourceFor(ctx, agent, resourceType, resourceID, permissionID)
					},
					decision: Decision{
						Allowed:      true,
						Reason:       "relationship grant: " + string(relType),
						Scope:        ScopeTypeRelationship,
						MatchedGrant: granted.Provenance.RoleName,
						BindingID:    granted.Provenance.BindingID,
					},
				})
			}
		}
	}

	return out
}

// evaluateRelationshipCandidates runs every structural candidate through
// the common stages. When stopAtFirst is true evaluation stops at the first
// accepted candidate.
func (a *AuthzService) evaluateRelationshipCandidates(
	ctx context.Context,
	principal PrincipalContext,
	resource Resource,
	action Action,
	permissionID string,
	restrictions []Restriction,
	stopAtFirst bool,
) relationshipOutcome {
	var out relationshipOutcome
	policyKind := permissions.RelationshipPrincipalKind(string(principal.Kind))

	for _, c := range a.relationshipCandidates(principal, resource, action, permissionID) {
		res := RelationshipCandidateResult{Rule: c.rule, Permission: permissionID}
		reject := func(kind, detail string) {
			res.RejectedBy = kind
			res.Detail = detail
			if out.restrictedBy == "" && !relationshipRejectKeepsKernelReason(kind) {
				out.restrictedBy = kind
			}
		}

		src, ok := a.runRelationshipStages(ctx, principal, policyKind, resource, permissionID, restrictions, c, reject)
		if src != nil {
			res.Source = &RelationshipSource{Kind: src.Kind}
		}
		if ok {
			res.Accepted = true
			if src != nil {
				res.Source.ID = src.ID
				res.Source.OwnerID = src.OwnerID
			}
		}
		out.results = append(out.results, res)

		if ok && out.accepted == nil {
			d := c.decision
			out.accepted = &d
			if stopAtFirst {
				break
			}
		}
	}
	return out
}

// runRelationshipStages applies the ordered stages to one candidate. It
// calls reject for the first failing stage and returns false; otherwise it
// returns true. The sharing source, when the rule has one, is returned
// once resolved.
func (a *AuthzService) runRelationshipStages(
	ctx context.Context,
	principal PrincipalContext,
	policyKind string,
	resource Resource,
	permissionID string,
	restrictions []Restriction,
	c relationshipCandidate,
	reject func(kind, detail string),
) (*SharingSource, bool) {
	// Stage 1: relationship policy.
	if !permissions.RelationshipPolicyAllows(string(c.rule), policyKind, resource.Type, permissionID) {
		reject(RelationshipRejectPolicy, "permission is not listed for this relationship")
		return nil, false
	}

	// Stage 2: hub-attested ancestry.
	if c.usesAncestry && !relationshipAncestryAttested(principal) {
		reject(RelationshipRejectUntrustedAncestry, "ancestry is not hub-attested")
		return nil, false
	}

	// Stage 3: relationship fact.
	var src *SharingSource
	if c.fact != nil {
		var holds bool
		var detail string
		src, holds, detail = c.fact(ctx)
		if !holds {
			reject(RelationshipRejectFact, detail)
			return src, false
		}
	}

	// Stage 4: sharing-source owner is active.
	if src != nil {
		if active, detail := a.relationshipSourceActive(ctx, src.OwnerID); !active {
			reject(RelationshipRejectSourceInactive, detail)
			return src, false
		}
	}

	// Stage 5: the request's restrictions.
	for _, r := range restrictions {
		if r.Check == nil || !r.Check(permissionID) {
			reject(r.Kind, r.Description)
			return src, false
		}
	}
	return src, true
}

// relationshipSourceActive reports whether a sharing-source owner is
// active. A user owner must exist with active status. An agent owner must
// exist, not be deleted, and have an active root user (the first entry of
// its ancestry chain). Any lookup failure, including a missing store,
// reports inactive.
func (a *AuthzService) relationshipSourceActive(ctx context.Context, ownerID string) (bool, string) {
	if ownerID == "" {
		return false, "sharing source has no owner"
	}
	if a.store == nil {
		return false, "store not available"
	}
	user, err := a.store.GetUser(ctx, ownerID)
	if err == nil && user != nil {
		if user.Status != store.UserStatusActive {
			return false, "sharing source owner is not active"
		}
		return true, ""
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, "sharing source owner lookup failed"
	}

	agent, err := a.store.GetAgent(ctx, ownerID)
	if err != nil || agent == nil {
		return false, "sharing source owner not found"
	}
	if !agent.DeletedAt.IsZero() {
		return false, "sharing source owner agent is deleted"
	}
	if len(agent.Ancestry) == 0 || agent.Ancestry[0] == ownerID {
		return false, "sharing source owner agent has no root user"
	}
	root, err := a.store.GetUser(ctx, agent.Ancestry[0])
	if err != nil || root == nil || root.Status != store.UserStatusActive {
		return false, "sharing source owner agent's root user is not active"
	}
	return true, ""
}

// =============================================================================
// Progeny sharing sources
// =============================================================================

// SharingPolicy states how a sharing source becomes available to the
// descendants of its owner.
type SharingPolicy string

const (
	// SharingPolicyOriginDescendants: available to agents whose origin user
	// is the owner, without an opt-in (personal skills).
	SharingPolicyOriginDescendants SharingPolicy = "origin_descendants"
	// SharingPolicyOptInRequired: available only when the owner opted the
	// source in (secrets, env vars, skill injections).
	SharingPolicyOptInRequired SharingPolicy = "opt_in_required"
)

// SharingSource is the fact a progeny adapter supplies for one source
// record. It carries identifiers only, never material. The adapter chooses
// which field is the owner (for example CreatedBy or ScopeUserID). Owner
// activity is resolved centrally by the relationship stage, not by the
// adapter.
type SharingSource struct {
	Kind    string
	ID      string
	OwnerID string
	Policy  SharingPolicy
	OptedIn bool
}

// ProgenyQuery is what a progeny adapter receives. Ancestry is always
// hub-attested: the relationship stage checks attestation before any
// adapter is called. ResourceID is empty for bucket-level (list) queries.
type ProgenyQuery struct {
	Kind       string
	ResourceID string
	Ancestry   []string
}

// ProgenyFactAdapter supplies sharing sources of one kind.
type ProgenyFactAdapter interface {
	// Kind is the resource type the adapter serves (e.g. "secret").
	Kind() string
	// ReadPermissions lists the registered read-class permissions a
	// progeny candidate of this kind may carry. RegisterProgenyAdapter
	// reads the set once, validates it and stores a copy; decisions use
	// that stored copy and do not call ReadPermissions again.
	ReadPermissions() []string
	// Sources returns the sharing sources visible to the ancestry chain.
	Sources(ctx context.Context, q ProgenyQuery) ([]SharingSource, error)
}

// relationshipReadClassActions are the registry actions a read-only
// relationship (and a progeny adapter) may carry.
var relationshipReadClassActions = map[string]bool{"read": true, "list": true, "verify": true, "secret_read": true}

// registryPermission returns the registry entry for a permission ID.
func registryPermission(id string) (permissions.Permission, bool) {
	for _, p := range permissions.Registry {
		if p.ID == id {
			return p, true
		}
	}
	return permissions.Permission{}, false
}

// errProgenyAdapter is returned for an adapter registration that cannot be
// accepted.
var errProgenyAdapter = errors.New("invalid progeny adapter")

// progenyOptInKinds are the sharing-source kinds served by the built-in
// store adapter. Their sources are shared only through an explicit opt-in:
// a source of one of these kinds with any other SharingPolicy is never
// shared, whichever adapter supplies it.
var progenyOptInKinds = map[string]bool{"secret": true, "envvar": true, "skill_injection": true}

// registeredProgenyAdapter is an adapter together with the read
// permissions validated at registration.
type registeredProgenyAdapter struct {
	adapter ProgenyFactAdapter
	perms   []string
}

// progenyAdapterRegistry holds adapters registered through
// RegisterProgenyAdapter, keyed by kind.
type progenyAdapterRegistry struct {
	mu       sync.RWMutex
	adapters map[string]registeredProgenyAdapter
	// builtinReleased lists built-in kinds whose store adapter has been
	// explicitly released so another adapter may register for the kind.
	// Only test helpers set it.
	builtinReleased map[string]bool
}

// RegisterProgenyAdapter registers an adapter for one sharing-source kind.
// Every read permission must be a registered read-class permission; a kind
// may be registered once. A kind served by the built-in store adapter
// (progenyOptInKinds) is refused. The validated permission set is copied
// and stored with the adapter; decisions read that copy.
func (a *AuthzService) RegisterProgenyAdapter(adapter ProgenyFactAdapter) error {
	if adapter == nil || adapter.Kind() == "" {
		return fmt.Errorf("%w: adapter must name a kind", errProgenyAdapter)
	}
	perms := append([]string(nil), adapter.ReadPermissions()...)
	if len(perms) == 0 {
		return fmt.Errorf("%w: %s: no read permissions", errProgenyAdapter, adapter.Kind())
	}
	for _, id := range perms {
		p, ok := registryPermission(id)
		if !ok {
			return fmt.Errorf("%w: %s: permission %q is not registered", errProgenyAdapter, adapter.Kind(), id)
		}
		if !relationshipReadClassActions[p.Action] {
			return fmt.Errorf("%w: %s: permission %q is not read-class", errProgenyAdapter, adapter.Kind(), id)
		}
	}
	a.progenyAdapters.mu.Lock()
	defer a.progenyAdapters.mu.Unlock()
	if a.progenyAdapters.adapters == nil {
		a.progenyAdapters.adapters = map[string]registeredProgenyAdapter{}
	}
	if _, exists := a.progenyAdapters.adapters[adapter.Kind()]; exists {
		return fmt.Errorf("%w: %s: already registered", errProgenyAdapter, adapter.Kind())
	}
	if progenyOptInKinds[adapter.Kind()] && !a.progenyAdapters.builtinReleased[adapter.Kind()] {
		return fmt.Errorf("%w: %s: served by the built-in store adapter", errProgenyAdapter, adapter.Kind())
	}
	a.progenyAdapters.adapters[adapter.Kind()] = registeredProgenyAdapter{adapter: adapter, perms: perms}
	return nil
}

// progenyAdapter returns the adapter for a kind and its read permissions:
// a registered adapter with the set stored at registration, or the
// built-in store adapter for secret, envvar and skill_injection with its
// fixed set. The returned slice is a copy.
func (a *AuthzService) progenyAdapter(kind string) (ProgenyFactAdapter, []string) {
	a.progenyAdapters.mu.RLock()
	reg, ok := a.progenyAdapters.adapters[kind]
	a.progenyAdapters.mu.RUnlock()
	if ok && reg.adapter != nil {
		return reg.adapter, append([]string(nil), reg.perms...)
	}
	if a.store == nil || !progenyOptInKinds[kind] {
		return nil, nil
	}
	builtin := storeProgenyAdapter{kind: kind, store: a.store}
	return builtin, builtin.ReadPermissions()
}

// hasRegisteredProgenyAdapter reports whether an adapter was registered
// for kind through RegisterProgenyAdapter.
func (a *AuthzService) hasRegisteredProgenyAdapter(kind string) bool {
	a.progenyAdapters.mu.RLock()
	defer a.progenyAdapters.mu.RUnlock()
	_, ok := a.progenyAdapters.adapters[kind]
	return ok
}

// progenyRelationshipType names the progeny relationship for a resource
// type: the built-in relationship type, or, for a kind with a registered
// adapter, "progeny_<kind>_read". It is empty when the kind has no
// progeny adapter, in which case no progeny candidate is built.
func (a *AuthzService) progenyRelationshipType(kind string) RelationshipType {
	if relType := relationshipTypeForResource(kind); relType != "" {
		return relType
	}
	if kind != "" && a.hasRegisteredProgenyAdapter(kind) {
		return RelationshipType("progeny_" + kind + "_read")
	}
	return ""
}

// adapterServesPermission reports whether permissionID is in perms, the
// adapter's read permissions as returned by progenyAdapter.
func adapterServesPermission(perms []string, permissionID string) bool {
	for _, id := range perms {
		if id == permissionID {
			return true
		}
	}
	return false
}

// storeProgenyAdapter serves opt-in sharing sources from the store's
// ListProgeny* queries. The owner is the record's CreatedBy.
type storeProgenyAdapter struct {
	kind  string
	store store.Store
}

func (s storeProgenyAdapter) Kind() string { return s.kind }

func (s storeProgenyAdapter) ReadPermissions() []string {
	if s.kind == "secret" {
		return []string{permissionProjectSecretRead}
	}
	return nil
}

func (s storeProgenyAdapter) Sources(ctx context.Context, q ProgenyQuery) ([]SharingSource, error) {
	var out []SharingSource
	add := func(id, createdBy string, optedIn bool) {
		if q.ResourceID != "" && id != q.ResourceID {
			return
		}
		out = append(out, SharingSource{Kind: s.kind, ID: id, OwnerID: createdBy, Policy: SharingPolicyOptInRequired, OptedIn: optedIn})
	}
	switch s.kind {
	case "secret":
		records, err := s.store.ListProgenySecrets(ctx, q.Ancestry)
		if err != nil {
			return nil, fmt.Errorf("ListProgenySecrets: %w", err)
		}
		for _, r := range records {
			add(r.ID, r.CreatedBy, r.AllowProgeny)
		}
	case "envvar":
		records, err := s.store.ListProgenyEnvVars(ctx, q.Ancestry)
		if err != nil {
			return nil, fmt.Errorf("ListProgenyEnvVars: %w", err)
		}
		for _, r := range records {
			add(r.ID, r.CreatedBy, r.AllowProgeny)
		}
	case "skill_injection":
		records, err := s.store.ListProgenySkillInjections(ctx, q.Ancestry)
		if err != nil {
			return nil, fmt.Errorf("ListProgenySkillInjections: %w", err)
		}
		for _, r := range records {
			add(r.ID, r.CreatedBy, r.AllowProgeny)
		}
	default:
		return nil, fmt.Errorf("unsupported progeny kind: %s", s.kind)
	}
	return out, nil
}

// ProgenyPredicate is the pure sharing check shared by point reads and
// list filtering, so the two cannot disagree.
type ProgenyPredicate struct {
	Kind             string
	AttestedAncestry []string
	// SourceActive reports whether a source owner is active.
	SourceActive func(ownerID string) bool
}

// shared reports whether the source is shared with the ancestry chain:
// same kind, owner in the chain, and the sharing policy satisfied. Kinds in
// progenyOptInKinds are shared only under SharingPolicyOptInRequired.
func (p ProgenyPredicate) shared(src SharingSource) bool {
	if len(p.AttestedAncestry) == 0 || src.Kind != p.Kind || src.OwnerID == "" {
		return false
	}
	if progenyOptInKinds[src.Kind] && src.Policy != SharingPolicyOptInRequired {
		return false
	}
	switch src.Policy {
	case SharingPolicyOptInRequired:
		if !src.OptedIn {
			return false
		}
	case SharingPolicyOriginDescendants:
		if p.AttestedAncestry[0] != src.OwnerID {
			return false
		}
	default:
		return false
	}
	return isInAncestry(p.AttestedAncestry, src.OwnerID)
}

// Matches reports whether a progeny read of src is admitted.
func (p ProgenyPredicate) Matches(src SharingSource) bool {
	return p.shared(src) && p.SourceActive != nil && p.SourceActive(src.OwnerID)
}

// ProgenyListPredicate returns the predicate for list filtering of one
// kind. A principal that is not a hub-attested agent, or a kind with no
// adapter or no progeny policy row, gets a predicate that matches nothing.
// The request's credential restrictions are applied by the per-record
// Decide call, not by the predicate.
func (a *AuthzService) ProgenyListPredicate(ctx context.Context, principal PrincipalContext, kind string) ProgenyPredicate {
	none := ProgenyPredicate{Kind: kind}
	if !isAgentPrincipal(principal.Kind) || !relationshipAncestryAttested(principal) {
		return none
	}
	agent, ok := principal.Identity.(AgentIdentity)
	if !ok || len(agent.Ancestry()) == 0 {
		return none
	}
	adapter, perms := a.progenyAdapter(kind)
	if adapter == nil {
		return none
	}
	allowed := false
	for _, id := range perms {
		if permissions.RelationshipPolicyAllows(string(RelationshipRuleProgeny), "agent", kind, id) {
			allowed = true
			break
		}
	}
	if !allowed {
		return none
	}
	return ProgenyPredicate{
		Kind:             kind,
		AttestedAncestry: append([]string(nil), agent.Ancestry()...),
		SourceActive: func(ownerID string) bool {
			active, _ := a.relationshipSourceActive(ctx, ownerID)
			return active
		},
	}
}

// EvaluateProgeny reports whether a point read of src is admitted by the
// progeny relationship. It is ProgenyListPredicate(...).Matches(src).
func (a *AuthzService) EvaluateProgeny(ctx context.Context, principal PrincipalContext, src SharingSource) bool {
	return a.ProgenyListPredicate(ctx, principal, src.Kind).Matches(src)
}

// progenySourceFor resolves the progeny fact for one resource: the
// sharing source for resourceID, if the adapter serves permissionID, the
// ancestry chain can see the source and its sharing policy is satisfied.
// Attestation has already been checked.
func (a *AuthzService) progenySourceFor(ctx context.Context, agent AgentIdentity, kind, resourceID, permissionID string) (*SharingSource, bool, string) {
	ancestry := agent.Ancestry()
	if len(ancestry) == 0 {
		return nil, false, "agent has no ancestry chain"
	}
	adapter, perms := a.progenyAdapter(kind)
	if adapter == nil {
		return nil, false, "no sharing-source adapter for " + kind
	}
	if !adapterServesPermission(perms, permissionID) {
		return nil, false, "permission is not a read permission of the sharing-source adapter"
	}
	sources, err := adapter.Sources(ctx, ProgenyQuery{Kind: kind, ResourceID: resourceID, Ancestry: ancestry})
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("progeny source lookup failed", "kind", kind, "error", err)
		}
		return nil, false, "sharing-source lookup failed"
	}
	pred := ProgenyPredicate{Kind: kind, AttestedAncestry: ancestry}
	for i := range sources {
		if sources[i].ID != resourceID {
			continue
		}
		src := sources[i]
		if !pred.shared(src) {
			return &src, false, "sharing source is not shared with this ancestry chain"
		}
		return &src, true, ""
	}
	return nil, false, "resource is not a sharing source for this ancestry chain"
}
