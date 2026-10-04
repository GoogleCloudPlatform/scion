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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Capabilities represents the set of actions a user can perform on a resource.
type Capabilities struct {
	Actions []string `json:"actions"`
}

// ResourceActions maps resource types to the actions applicable to individual resources.
var ResourceActions = actionMapFromRegistry(permissions.ResourceActions())

// ScopeActions maps resource types to scope-level actions (e.g., create, list).
var ScopeActions = actionMapFromRegistry(permissions.ScopeActions())

func actionMapFromRegistry(in map[string][]string) map[string][]Action {
	out := make(map[string][]Action, len(in))
	for resource, actions := range in {
		out[resource] = make([]Action, len(actions))
		for i, action := range actions {
			out[resource][i] = Action(action)
		}
	}
	return out
}

// agentResource constructs a Resource from a store.Agent for capability computation.
func agentResource(a *store.Agent) Resource {
	return Resource{
		Type:       "agent",
		ID:         a.ID,
		OwnerID:    a.OwnerID,
		ParentType: "project",
		ParentID:   a.ProjectID,
		Labels:     a.Labels,
		Ancestry:   a.Ancestry,
	}
}

// projectResource constructs a Resource from a store.Project for capability computation.
func projectResource(g *store.Project) Resource {
	return Resource{
		Type:    "project",
		ID:      g.ID,
		OwnerID: g.OwnerID,
		Labels:  g.Labels,
	}
}

// templateResource constructs a Resource from a store.Template for capability computation.
func templateResource(t *store.Template) Resource {
	r := Resource{
		Type:      "template",
		ID:        t.ID,
		OwnerID:   t.OwnerID,
		ScopeKind: t.Scope,
	}
	// Project-scoped templates are children of their project (mirrors
	// harnessConfigResource and policyResource). Without this the resource is
	// parentless, and since #595 made project-scoped policy matching an
	// allow-list, a parentless resource matches no project-scoped policy at
	// all — so project-scoped template policies would match nothing.
	//
	// ScopeID is the authoritative field. Deliberately no fallback to the
	// deprecated t.ProjectID (store/models.go): a deprecated field must not
	// become load-bearing in the authz engine. Legacy ProjectID-only rows are
	// handled by backfill, not here.
	//
	// Global- and user-scoped templates stay parentless, which is correct:
	// they do not belong to a project.
	if t.Scope == store.TemplateScopeProject && t.ScopeID != "" {
		r.ParentType = "project"
		r.ParentID = t.ScopeID
	}
	return r
}

// harnessConfigResource constructs a Resource from a store.HarnessConfig for capability computation.
func harnessConfigResource(hc *store.HarnessConfig) Resource {
	if hc == nil {
		return Resource{}
	}
	r := Resource{
		Type:      "harness_config",
		ID:        hc.ID,
		OwnerID:   hc.OwnerID,
		ScopeKind: hc.Scope,
	}
	// Project-scoped harness configs are children of the project, so project
	// owner/admin bypass applies (mirrors gcpServiceAccountResource).
	if hc.Scope == store.HarnessConfigScopeProject && hc.ScopeID != "" {
		r.ParentType = "project"
		r.ParentID = hc.ScopeID
	}
	return r
}

// groupResource constructs a Resource from a store.Group for capability computation.
func groupResource(g *store.Group) Resource {
	r := Resource{
		Type:    "group",
		ID:      g.ID,
		OwnerID: g.OwnerID,
		Labels:  g.Labels,
	}
	// Project-scoped groups (e.g. "project:<slug>:members") are children of the
	// project. Setting the parent lets project owner/admin bypass apply.
	if g.ProjectID != "" {
		r.ParentType = "project"
		r.ParentID = g.ProjectID
	}
	return r
}

// userResource constructs a Resource from a store.User for capability computation.
func userResource(u *store.User) Resource {
	return Resource{
		Type: "user",
		ID:   u.ID,
	}
}

// brokerResource constructs a Resource from a store.RuntimeBroker for capability computation.
func brokerResource(b *store.RuntimeBroker) Resource {
	return Resource{
		Type:    "broker",
		ID:      b.ID,
		OwnerID: b.CreatedBy,
	}
}

// gcpServiceAccountResource constructs a Resource from a store.GCPServiceAccount for capability computation.
func gcpServiceAccountResource(sa *store.GCPServiceAccount) Resource {
	if sa == nil {
		return Resource{}
	}
	r := Resource{
		Type:    "gcp_service_account",
		ID:      sa.ID,
		OwnerID: sa.CreatedBy,
	}
	// Only project-scoped service accounts get a project ParentType/ParentID
	// (mirrors harnessConfigResource). That link — not any SA-specific rule in
	// the kernel — is what makes the ComputeCapabilities owner/admin
	// short-circuit below apply to project-scoped accounts alone. For hub- and
	// user-scoped accounts ScopeID is a hub or user ID, not a project ID:
	// giving them a project parent would hand the short-circuit to the owner
	// of whatever project happened to share that ID.
	if sa.Scope == store.ScopeProject && sa.ScopeID != "" {
		r.ParentType = "project"
		r.ParentID = sa.ScopeID
	}
	return r
}

// ComputeCapabilities evaluates which actions the identity can perform on a single resource.
func (a *AuthzService) ComputeCapabilities(ctx context.Context, identity Identity, resource Resource) *Capabilities {
	// Reuse the caller's principals, access constraints and delegation edges
	// across every decision below (no-op if a memo or mask is already set).
	// Nothing below writes authorization state, so the memo stays valid.
	ctx = withAuthzInputMemo(ctx)
	actions, ok := ResourceActions[resource.Type]
	if !ok {
		return &Capabilities{Actions: []string{}}
	}

	// Super-admins get all actions via CheckAccess/Decide step-1 bypass.
	// Hub-admins get correct capabilities from their role bindings.
	if IsScopedUserIdentity(identity) {
		return a.computeCapabilitiesWithContext(ctx, identity, resource, actions)
	}

	// Every action is answered by the common decision, so the capability
	// list is exactly what the caller can do.
	var allowed []string
	for _, action := range actions {
		decision := a.CheckAccess(ctx, identity, resource, action)
		if decision.Allowed {
			allowed = append(allowed, string(action))
		}
	}
	if allowed == nil {
		allowed = []string{}
	}
	return &Capabilities{Actions: allowed}
}

// ComputeScopeCapabilities evaluates scope-level actions (e.g., create, list) for a resource type.
// Each action is decided without an explicit permission, so an action whose
// (resource type, action) pair does not resolve to exactly one registered
// permission is reported as not allowed. Every "hub" scope action is such a
// pair; hub-level checks pass an explicit Permission to Decide instead.
func (a *AuthzService) ComputeScopeCapabilities(ctx context.Context, identity Identity, scopeType, scopeID, resourceType string) *Capabilities {
	// Reuse the caller's principals, access constraints and delegation edges
	// across every decision below (no-op if a memo or mask is already set).
	// Nothing below writes authorization state, so the memo stays valid.
	ctx = withAuthzInputMemo(ctx)
	actions, ok := ScopeActions[resourceType]
	if !ok {
		return &Capabilities{Actions: []string{}}
	}

	// Super-admins get all actions via CheckAccess/Decide step-1 bypass.
	// Hub-admins get correct capabilities from their role bindings.

	resource := Resource{
		Type:       resourceType,
		ParentType: scopeType,
		ParentID:   scopeID,
	}
	if IsScopedUserIdentity(identity) {
		return a.computeCapabilitiesWithContext(ctx, identity, resource, actions)
	}

	var allowed []string
	for _, action := range actions {
		decision := a.CheckAccess(ctx, identity, resource, action)
		if decision.Allowed {
			allowed = append(allowed, string(action))
		}
	}
	if allowed == nil {
		allowed = []string{}
	}
	return &Capabilities{Actions: allowed}
}

// ComputeCapabilitiesBatch evaluates capabilities for a list of resources.
// It installs the request-local authorization input memo, so the caller's
// principals, access constraints and delegation edges are loaded once for
// the whole batch rather than once per decision; every decision still runs
// the full evaluation path.
//
// PINNED to ComputeCapabilitiesForActions below: the two evaluation loops
// (the IsScopedUserIdentity branch and the CheckAccess branch) must stay in
// lockstep, field for field, with ComputeCapabilitiesForActions's loops over
// an explicit action list. They are intentionally a duplicated body rather
// than one delegating to the other. Both install the request-local input
// memo at entry, so either can be called directly or nested inside an outer
// install site with the same result.
// TestListProjectAgentsSorted_CapsDeepEqualLegacy asserts the two stay
// byte-identical on real requests; if you change one loop, change the other
// and re-run that test.
func (a *AuthzService) ComputeCapabilitiesBatch(ctx context.Context, identity Identity, resources []Resource, resourceType string) []*Capabilities {
	// Reuse the caller's principals, access constraints and delegation edges
	// across every decision below (no-op if a memo or mask is already set).
	// Nothing below writes authorization state, so the memo stays valid.
	ctx = withAuthzInputMemo(ctx)
	actions, ok := ResourceActions[resourceType]
	if !ok {
		caps := make([]*Capabilities, len(resources))
		for i := range caps {
			caps[i] = &Capabilities{Actions: []string{}}
		}
		return caps
	}

	// Super-admins get all actions via CheckAccess/Decide step-1 bypass.
	// Hub-admins get correct capabilities from their role bindings.
	if IsScopedUserIdentity(identity) {
		caps := make([]*Capabilities, len(resources))
		for i, resource := range resources {
			caps[i] = a.computeCapabilitiesWithContext(ctx, identity, resource, actions)
		}
		return caps
	}

	// Every principal, project owners and admins included, gets each
	// capability from one Decide call per resource and action, so a batch
	// costs len(resources) × len(actions) decisions.
	caps := make([]*Capabilities, len(resources))
	for i, resource := range resources {
		var allowed []string
		for _, action := range actions {
			decision := a.CheckAccess(ctx, identity, resource, action)
			if decision.Allowed {
				allowed = append(allowed, string(action))
			}
		}
		if allowed == nil {
			allowed = []string{}
		}
		caps[i] = &Capabilities{Actions: allowed}
	}
	return caps
}

// ComputeCapabilitiesForActions evaluates identity's capabilities over
// resources for exactly the given actions, in that order, rather than the
// full ResourceActions[resourceType] set ComputeCapabilitiesBatch uses. It is
// the thin read-pass variant used by the sorted project endpoint for its
// per-candidate ActionRead-only pass and for the full-row re-decision and
// remaining-actions merge.
//
// It runs the identical evaluation path ComputeCapabilitiesBatch does —
// DecideFromContext for a scoped UAT, CheckAccess otherwise — so a caller
// that passes ResourceActions[resourceType] here gets byte-identical results
// to ComputeCapabilitiesBatch (the decision-count test suite's
// non-waivable gate asserts this deep-equality for the merged per-item
// result). Each (resource, action) pair costs exactly one decision and one
// audit record, same as today.
func (a *AuthzService) ComputeCapabilitiesForActions(ctx context.Context, identity Identity, resources []Resource, actions []Action) []*Capabilities {
	// Reuse the caller's principals, access constraints and delegation edges
	// across every decision below (no-op if a memo or mask is already set).
	// Nothing below writes authorization state, so the memo stays valid.
	ctx = withAuthzInputMemo(ctx)
	if IsScopedUserIdentity(identity) {
		caps := make([]*Capabilities, len(resources))
		for i, resource := range resources {
			caps[i] = a.computeCapabilitiesWithContext(ctx, identity, resource, actions)
		}
		return caps
	}

	caps := make([]*Capabilities, len(resources))
	for i, resource := range resources {
		var allowed []string
		for _, action := range actions {
			decision := a.CheckAccess(ctx, identity, resource, action)
			if decision.Allowed {
				allowed = append(allowed, string(action))
			}
		}
		if allowed == nil {
			allowed = []string{}
		}
		caps[i] = &Capabilities{Actions: allowed}
	}
	return caps
}

// mergeCapabilities combines a read-only capability result (the read pass,
// evaluated on the member snapshot) with a capability result for the
// remaining actions (evaluated on the full row), preserving the action order
// ResourceActions[resourceType] defines — the same order
// ComputeCapabilitiesBatch produces, which is what the non-waivable
// deep-equality gate requires and what the decision-count accounting
// depends on: an item whose read decision came from the read pass and
// whose remaining actions came from the remaining-actions pass must look
// identical to one where every action was decided by a single
// ComputeCapabilitiesBatch call.
func mergeCapabilities(order []Action, readCap, restCap *Capabilities) *Capabilities {
	allowed := make([]string, 0, len(order))
	for _, action := range order {
		if capabilityAllows(readCap, action) || capabilityAllows(restCap, action) {
			allowed = append(allowed, string(action))
		}
	}
	return &Capabilities{Actions: allowed}
}

// computeCapabilitiesWithContext evaluates every action through the canonical
// request path so credential caveats (notably UAT project and scope limits)
// cannot be bypassed by capability projections.
func (a *AuthzService) computeCapabilitiesWithContext(ctx context.Context, identity Identity, resource Resource, actions []Action) *Capabilities {
	if GetIdentityFromContext(ctx) != identity {
		ctx = contextWithIdentity(ctx, identity)
	}
	allowed := make([]string, 0, len(actions))
	for _, action := range actions {
		if a.DecideFromContext(ctx, resource, action).Allowed {
			allowed = append(allowed, string(action))
		}
	}
	return &Capabilities{Actions: allowed}
}

// capabilityAllows returns true when the capability set includes the action.
func capabilityAllows(cap *Capabilities, action Action) bool {
	if cap == nil {
		return false
	}
	needle := string(action)
	for _, allowed := range cap.Actions {
		if allowed == needle {
			return true
		}
	}
	return false
}
