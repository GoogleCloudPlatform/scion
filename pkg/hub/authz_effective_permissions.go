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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// EffectiveScope selects the scope at which effective permissions are resolved.
// An empty ProjectID resolves the hub (system) scope.
type EffectiveScope struct {
	ProjectID string
}

// EffectivePermissionSet is the resolved effective permission set and grant
// provenance for one principal at one scope (§3.1 JSON contract).
type EffectivePermissionSet struct {
	Principal   EffectivePermissionPrincipal `json:"principal"`
	Scope       EffectivePermissionScope     `json:"scope"`
	StatusGate  *EffectiveStatusGate         `json:"statusGate"`
	EvaluatedAt string                       `json:"evaluatedAt"`
	Permissions []EffectivePermissionEntry   `json:"permissions"`
	Removed     []EffectivePermissionRemoved `json:"removed"`
}

// EffectivePermissionPrincipal identifies the target principal and its status.
type EffectivePermissionPrincipal struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status"`
}

// EffectivePermissionScope identifies the evaluated scope.
type EffectivePermissionScope struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// EffectiveStatusGate explains why an inactive principal holds zero permissions.
type EffectiveStatusGate struct {
	Reason string `json:"reason"`
}

// EffectivePermissionEntry is one effective permission and all contributing grants.
type EffectivePermissionEntry struct {
	ID                string                     `json:"id"`
	Resource          string                     `json:"resource"`
	Action            string                     `json:"action"`
	Description       string                     `json:"description"`
	AppliesInProjects bool                       `json:"appliesInProjects"`
	Grants            []EffectivePermissionGrant `json:"grants"`
}

// EffectivePermissionGrant is one role binding that grants a permission.
type EffectivePermissionGrant struct {
	BindingID string                        `json:"bindingId"`
	Role      EffectivePermissionRoleRef    `json:"role"`
	Scope     EffectivePermissionGrantScope `json:"scope"`
	Via       *EffectivePermissionGroupVia  `json:"via"`
	ExpiresAt *string                       `json:"expiresAt"`
}

// EffectivePermissionRoleRef identifies the role definition on a granting binding.
type EffectivePermissionRoleRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	System bool   `json:"system"`
}

// EffectivePermissionGrantScope identifies the scope of a granting binding.
type EffectivePermissionGrantScope struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// EffectivePermissionGroupVia identifies the bound group for a group-derived grant.
type EffectivePermissionGroupVia struct {
	GroupID   string   `json:"groupId"`
	GroupName string   `json:"groupName"`
	GroupSlug string   `json:"groupSlug"`
	Path      []string `json:"path,omitempty"`
}

// EffectivePermissionRemoved is a permission granted by role bindings that was
// removed by one or more active access constraints.
type EffectivePermissionRemoved struct {
	ID                string                           `json:"id"`
	Resource          string                           `json:"resource"`
	Action            string                           `json:"action"`
	Description       string                           `json:"description"`
	AppliesInProjects bool                             `json:"appliesInProjects"`
	Grants            []EffectivePermissionGrant       `json:"grants"`
	RemovedBy         []EffectivePermissionBoundaryRef `json:"removedBy"`
}

// EffectivePermissionBoundaryRef identifies an access constraint that removed a permission.
type EffectivePermissionBoundaryRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ScopeType string `json:"scopeType"`
	ScopeID   string `json:"scopeId,omitempty"`
	Redacted  bool   `json:"redacted"`
}

func (a *AuthzService) now() time.Time {
	if c := a.kindCache(); c != nil {
		return c.clock()
	}
	return time.Now()
}

// ResolveEffectivePermissions resolves the effective permissions and grant
// provenance for principal in scope using the AK1 kernel without emitting
// decision-audit records.
func (a *AuthzService) ResolveEffectivePermissions(
	ctx context.Context,
	principal PrincipalContext,
	scope EffectiveScope,
) (*EffectivePermissionSet, error) {
	if err := requireLocalUserPrincipal(principal); err != nil {
		return nil, err
	}
	if a == nil || a.store == nil {
		return nil, errors.New("authorization store unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	evalTime := a.now()
	evaluatedAt := evalTime.UTC().Format(time.RFC3339)

	isProjectScope := scope.ProjectID != ""
	scopeInfo := EffectivePermissionScope{Type: ScopeTypeSystem}
	if isProjectScope {
		proj, err := a.store.GetProject(ctx, scope.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("load project scope: %w", err)
		}
		scopeInfo = EffectivePermissionScope{
			Type: ScopeTypeProject,
			ID:   proj.ID,
			Name: proj.Name,
			Slug: proj.Slug,
		}
	}

	// Step 1: Account status gate (shared with decide via activeUserPredicate).
	user, err := a.userForStatusGate(ctx, principal.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidInput) {
		return nil, fmt.Errorf("resolve user status: %w", err)
	}
	if user != nil && user.Kind == store.UserKindFederated {
		return nil, ErrUnsupportedPrincipalKind
	}
	if gateErr := activeUserPredicate(principal, user, err); gateErr != nil {
		reason := "missing"
		if err == nil && user != nil && user.Status != "" {
			reason = user.Status
		}
		return &EffectivePermissionSet{
			Principal: EffectivePermissionPrincipal{
				Type:   store.RoleBindingPrincipalUser,
				ID:     principal.ID,
				Status: reason,
			},
			Scope:       scopeInfo,
			StatusGate:  &EffectiveStatusGate{Reason: reason},
			EvaluatedAt: evaluatedAt,
			Permissions: []EffectivePermissionEntry{},
			Removed:     []EffectivePermissionRemoved{},
		}, nil
	}

	// Steps 2 & 3: Closure CTE + 1 batch clamped bindings + 1 batch role defs,
	// filtered by scope containment (system only, or system + project P).
	refs, activeBindings, roleDefs, err := a.loadScopedActiveBindings(ctx, principal, evalTime, func(b *store.RoleBinding) bool {
		if b.ScopeType == ScopeTypeSystem {
			return true
		}
		return isProjectScope && b.ScopeType == ScopeTypeProject && b.ScopeID == scope.ProjectID
	})
	if err != nil {
		return nil, err
	}

	bindingByID := make(map[string]*store.RoleBinding, len(activeBindings))
	for _, b := range activeBindings {
		bindingByID[b.ID] = b
	}
	candidates := toCandidateBindings(activeBindings)

	closure := make(map[string]struct{}, len(refs))
	membershipPaths := make(map[string][]string, len(refs))
	for _, ref := range refs {
		key := ref.Type + ":" + ref.ID
		closure[key] = struct{}{}
		membershipPaths[key] = []string{ref.ID}
	}

	// Step 4: Access constraints at the evaluated scope (fail closed on error).
	resourceCtx := ResourceContext{ProjectID: scope.ProjectID}
	restrictions, err := a.accessConstraintRestrictions(ctx, closure, resourceCtx)
	if err != nil {
		return nil, fmt.Errorf("load access constraints: %w", err)
	}

	// Step 5: Test-admin policy cage status.
	caged, err := a.principalIsCagedTestAdmin(ctx, principal.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve test-admin kind: %w", err)
	}

	// Step 6: Pure AK1 kernel pass per non-reserved permission in registry order.
	result := &EffectivePermissionSet{
		Principal: EffectivePermissionPrincipal{
			Type:   store.RoleBindingPrincipalUser,
			ID:     principal.ID,
			Status: store.UserStatusActive,
		},
		Scope:       scopeInfo,
		StatusGate:  nil,
		EvaluatedAt: evaluatedAt,
		Permissions: []EffectivePermissionEntry{},
		Removed:     []EffectivePermissionRemoved{},
	}

	boundGroupIDs := make(map[string]struct{})

	for _, perm := range permissions.Registry {
		if perm.IsReserved() {
			continue
		}
		// Deliver permissions require an internal delivery credential and are
		// never satisfied by role grants (Decide Steps 0 & 8b).
		if isDeliverRequest(perm.ID, Action(perm.Action)) {
			continue
		}
		applies, reviewed := permissions.AppliesToExistingProjectTarget(perm.ID)
		appliesInProjects := reviewed && applies
		if isProjectScope && !appliesInProjects {
			continue
		}

		permCandidates := candidates
		if isProjectScope {
			permCandidates = applyHubWideScopeFilters(candidates, roleDefs, ContemplatedProjectClass(perm.ID))
		}

		k := Evaluate(KernelRequest{
			Permission:        perm.ID,
			PrincipalClosure:  closure,
			MembershipPaths:   membershipPaths,
			Resource:          resourceCtx,
			CandidateBindings: permCandidates,
			RoleDefinitions:   roleDefs,
			Restrictions:      restrictions,
			Now:               evalTime,
		})

		var grants []EffectivePermissionGrant
		for _, gb := range k.Provenance.GrantingBindings {
			if !gb.ContainsRequested {
				continue
			}
			role := roleDefs[gb.RoleID]
			grant := EffectivePermissionGrant{
				BindingID: gb.BindingID,
				Role: EffectivePermissionRoleRef{
					ID:     gb.RoleID,
					Name:   gb.RoleName,
					System: role != nil && role.System,
				},
				Scope: EffectivePermissionGrantScope{
					Type: gb.ScopeType,
					ID:   gb.ScopeID,
				},
			}
			if gb.PrincipalType == store.RoleBindingPrincipalGroup {
				grant.Via = &EffectivePermissionGroupVia{
					GroupID: gb.PrincipalID,
				}
				boundGroupIDs[gb.PrincipalID] = struct{}{}
			}
			if raw := bindingByID[gb.BindingID]; raw != nil && raw.ExpiresAt != nil {
				expStr := raw.ExpiresAt.UTC().Format(time.RFC3339)
				grant.ExpiresAt = &expStr
			}
			grants = append(grants, grant)
		}

		if len(grants) == 0 {
			continue
		}
		if caged && testAdminPermissionDenied(perm.ID) {
			continue
		}

		if k.Allowed {
			result.Permissions = append(result.Permissions, EffectivePermissionEntry{
				ID:                perm.ID,
				Resource:          perm.Resource,
				Action:            perm.Action,
				Description:       perm.Description,
				AppliesInProjects: appliesInProjects,
				Grants:            grants,
			})
		} else {
			removedBy := collectRemovingBoundaries(perm.ID, restrictions)
			result.Removed = append(result.Removed, EffectivePermissionRemoved{
				ID:                perm.ID,
				Resource:          perm.Resource,
				Action:            perm.Action,
				Description:       perm.Description,
				AppliesInProjects: appliesInProjects,
				Grants:            grants,
				RemovedBy:         removedBy,
			})
		}
	}

	// Step 7: Batch-enrich bound group names and slugs, then sort grants.
	if len(boundGroupIDs) > 0 {
		ids := make([]string, 0, len(boundGroupIDs))
		for id := range boundGroupIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		groups, err := a.store.GetGroupsByIDs(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("load bound groups: %w", err)
		}
		groupMap := make(map[string]store.Group, len(groups))
		for _, g := range groups {
			groupMap[g.ID] = g
		}
		enrichGrantsWithGroups := func(grants []EffectivePermissionGrant) {
			for i := range grants {
				if grants[i].Via == nil {
					continue
				}
				if g, ok := groupMap[grants[i].Via.GroupID]; ok {
					grants[i].Via.GroupName = g.Name
					grants[i].Via.GroupSlug = g.Slug
				}
			}
		}
		for i := range result.Permissions {
			enrichGrantsWithGroups(result.Permissions[i].Grants)
		}
		for i := range result.Removed {
			enrichGrantsWithGroups(result.Removed[i].Grants)
		}
	}

	for i := range result.Permissions {
		sortEffectivePermissionGrants(result.Permissions[i].Grants)
	}
	for i := range result.Removed {
		sortEffectivePermissionGrants(result.Removed[i].Grants)
	}

	return result, nil
}

func collectRemovingBoundaries(permID string, restrictions []Restriction) []EffectivePermissionBoundaryRef {
	seen := make(map[string]struct{})
	out := make([]EffectivePermissionBoundaryRef, 0)
	for _, r := range restrictions {
		if r.Check != nil && r.Check(permID) {
			continue
		}
		key := r.BoundaryID + "|" + r.BoundaryName + "|" + r.BoundaryScopeType + "|" + r.BoundaryScopeID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, EffectivePermissionBoundaryRef{
			ID:        r.BoundaryID,
			Name:      r.BoundaryName,
			ScopeType: r.BoundaryScopeType,
			ScopeID:   r.BoundaryScopeID,
			Redacted:  false,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func sortEffectivePermissionGrants(grants []EffectivePermissionGrant) {
	scopeRank := func(scopeType string) int {
		if scopeType == ScopeTypeProject {
			return 0
		}
		return 1
	}
	sort.Slice(grants, func(i, j int) bool {
		ri, rj := scopeRank(grants[i].Scope.Type), scopeRank(grants[j].Scope.Type)
		if ri != rj {
			return ri < rj
		}
		if grants[i].Role.Name != grants[j].Role.Name {
			return grants[i].Role.Name < grants[j].Role.Name
		}
		iDirect, jDirect := grants[i].Via == nil, grants[j].Via == nil
		if iDirect != jDirect {
			return iDirect
		}
		if !iDirect && grants[i].Via.GroupName != grants[j].Via.GroupName {
			return grants[i].Via.GroupName < grants[j].Via.GroupName
		}
		return grants[i].BindingID < grants[j].BindingID
	})
}
