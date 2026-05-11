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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Capabilities represents the set of actions a user can perform on a resource.
type Capabilities struct {
	Actions []string `json:"actions"`
}

// ResourceActions maps resource types to the actions applicable to individual resources.
var ResourceActions = map[string][]Action{
	"agent":               {ActionRead, ActionUpdate, ActionDelete, ActionStart, ActionStop, ActionMessage, ActionAttach},
	"project":               {ActionRead, ActionUpdate, ActionDelete, ActionManage, ActionRegister},
	"template":            {ActionRead, ActionUpdate, ActionDelete},
	"group":               {ActionRead, ActionUpdate, ActionDelete, ActionAddMember, ActionRemoveMember},
	"user":                {ActionRead, ActionUpdate},
	"policy":              {ActionRead, ActionUpdate, ActionDelete},
	"broker":              {ActionRead, ActionUpdate, ActionDelete, ActionDispatch},
	"gcp_service_account": {ActionRead, ActionDelete, ActionVerify},
}

// ScopeActions maps resource types to scope-level actions (e.g., create, list).
var ScopeActions = map[string][]Action{
	"agent":               {ActionCreate, ActionList, ActionStopAll},
	"project":               {ActionCreate, ActionList},
	"template":            {ActionCreate, ActionList},
	"group":               {ActionCreate, ActionList},
	"policy":              {ActionCreate, ActionList},
	"broker":              {ActionCreate, ActionList},
	"gcp_service_account": {ActionCreate, ActionList, ActionMint},
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
	return Resource{
		Type:    "template",
		ID:      t.ID,
		OwnerID: t.OwnerID,
	}
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

// policyResource constructs a Resource from a store.Policy for capability computation.
func policyResource(p *store.Policy) Resource {
	r := Resource{
		Type:   "policy",
		ID:     p.ID,
		Labels: p.Labels,
	}
	// Project-scoped policies are children of the project for authz purposes.
	if p.ScopeType == "project" && p.ScopeID != "" {
		r.ParentType = "project"
		r.ParentID = p.ScopeID
	}
	return r
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
	return Resource{
		Type:       "gcp_service_account",
		ID:         sa.ID,
		OwnerID:    sa.CreatedBy,
		ParentType: "project",
		ParentID:   sa.ScopeID,
	}
}

// ComputeCapabilities evaluates which actions the identity can perform on a single resource.
func (a *AuthzService) ComputeCapabilities(ctx context.Context, identity Identity, resource Resource) *Capabilities {
	actions, ok := ResourceActions[resource.Type]
	if !ok {
		return &Capabilities{Actions: []string{}}
	}

	// Admin short-circuit: return all actions
	if user, ok := identity.(UserIdentity); ok && user.Role() == "admin" {
		return allActions(actions)
	}

	// Project owner/admin short-circuit: full access on project and project-scoped
	// resources. Mirrors the bypass in checkAccessForUser so capability lists
	// match what the user can actually do.
	if user, ok := identity.(UserIdentity); ok {
		if projectID := projectIDForResource(resource); projectID != "" {
			if a.isProjectOwnerOrAdmin(ctx, user.ID(), projectID) {
				return allActions(actions)
			}
		}
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

// ComputeScopeCapabilities evaluates scope-level actions (e.g., create, list) for a resource type.
func (a *AuthzService) ComputeScopeCapabilities(ctx context.Context, identity Identity, scopeType, scopeID, resourceType string) *Capabilities {
	actions, ok := ScopeActions[resourceType]
	if !ok {
		return &Capabilities{Actions: []string{}}
	}

	// Admin short-circuit
	if user, ok := identity.(UserIdentity); ok && user.Role() == "admin" {
		return allActions(actions)
	}

	resource := Resource{
		Type:       resourceType,
		ParentType: scopeType,
		ParentID:   scopeID,
	}

	// Project owner/admin short-circuit at scope level (e.g. agent:create
	// inside a project the user owns).
	if user, ok := identity.(UserIdentity); ok && scopeType == "project" && scopeID != "" {
		if a.isProjectOwnerOrAdmin(ctx, user.ID(), scopeID) {
			return allActions(actions)
		}
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

// ComputeCapabilitiesBatch evaluates capabilities for a list of resources, optimized
// for batch operation by expanding groups and fetching policies once.
func (a *AuthzService) ComputeCapabilitiesBatch(ctx context.Context, identity Identity, resources []Resource, resourceType string) []*Capabilities {
	actions, ok := ResourceActions[resourceType]
	if !ok {
		caps := make([]*Capabilities, len(resources))
		for i := range caps {
			caps[i] = &Capabilities{Actions: []string{}}
		}
		return caps
	}

	// Admin short-circuit: return all actions for all resources
	if user, ok := identity.(UserIdentity); ok && user.Role() == "admin" {
		allCap := allActions(actions)
		caps := make([]*Capabilities, len(resources))
		for i := range caps {
			caps[i] = allCap
		}
		return caps
	}

	// Pre-fetch principals and policies once for the identity
	principals, policies := a.precomputeForIdentity(ctx, identity)

	// Per-batch project ownership cache. Most batches list resources from a
	// single project, so this collapses to one lookup per project.
	projectOwnerCache := map[string]bool{}
	isProjectOwner := func(projectID string) bool {
		if projectID == "" {
			return false
		}
		user, ok := identity.(UserIdentity)
		if !ok {
			return false
		}
		if cached, ok := projectOwnerCache[projectID]; ok {
			return cached
		}
		v := a.isProjectOwnerOrAdmin(ctx, user.ID(), projectID)
		projectOwnerCache[projectID] = v
		return v
	}

	caps := make([]*Capabilities, len(resources))
	for i, resource := range resources {
		// Owner short-circuit
		if resource.OwnerID != "" && resource.OwnerID == identity.ID() {
			caps[i] = allActions(actions)
			continue
		}
		// Ancestry short-circuit: ancestors get full access
		if canAccessAsAncestor(identity.ID(), resource) {
			caps[i] = allActions(actions)
			continue
		}
		// Project owner/admin short-circuit
		if isProjectOwner(projectIDForResource(resource)) {
			caps[i] = allActions(actions)
			continue
		}

		var allowed []string
		for _, action := range actions {
			decision := a.checkAccessPrecomputed(identity, principals, policies, resource, action)
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

// precomputeForIdentity fetches group memberships and policies once for an identity.
func (a *AuthzService) precomputeForIdentity(ctx context.Context, identity Identity) ([]store.PrincipalRef, []store.Policy) {
	var principals []store.PrincipalRef

	switch identity.Type() {
	case "user", "dev":
		principals = append(principals, store.PrincipalRef{Type: "user", ID: identity.ID()})
		groupIDs, err := a.store.GetEffectiveGroups(ctx, identity.ID())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			a.logger.Warn("failed to get effective groups for user", "userID", identity.ID(), "error", err.Error())
		}
		for _, gid := range groupIDs {
			principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
		}
	case "agent":
		principals = append(principals, store.PrincipalRef{Type: "agent", ID: identity.ID()})
		groupIDs, err := a.store.GetEffectiveGroupsForAgent(ctx, identity.ID())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			a.logger.Warn("failed to get effective groups for agent", "agent_id", identity.ID(), "error", err.Error())
		}
		for _, gid := range groupIDs {
			principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
		}
	}

	policies, err := a.store.GetPoliciesForPrincipals(ctx, principals)
	if err != nil {
		a.logger.Warn("failed to get policies for principals", "error", err)
	}

	return principals, policies
}

// checkAccessPrecomputed evaluates access using pre-fetched principals and policies.
func (a *AuthzService) checkAccessPrecomputed(identity Identity, _ []store.PrincipalRef, policies []store.Policy, resource Resource, action Action) Decision {
	// Owner bypass (already handled in batch caller, but kept for single-resource calls)
	if user, ok := identity.(UserIdentity); ok {
		if resource.OwnerID != "" && resource.OwnerID == user.ID() {
			return Decision{Allowed: true, Reason: "resource owner"}
		}
	}

	// Ancestry bypass (already handled in batch caller, but kept for single-resource calls)
	if canAccessAsAncestor(identity.ID(), resource) {
		return Decision{Allowed: true, Reason: "ancestor access"}
	}

	return a.evaluatePolicies(policies, resource, action)
}

// allActions returns a Capabilities with all provided actions.
func allActions(actions []Action) *Capabilities {
	strs := make([]string, len(actions))
	for i, a := range actions {
		strs[i] = string(a)
	}
	return &Capabilities{Actions: strs}
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
