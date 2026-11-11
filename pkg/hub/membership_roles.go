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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// roleDefsResolver returns the role definitions for roleDefIDs, keyed by
// definition ID. A definition absent from the result is missing.
type roleDefsResolver func(roleDefIDs []string) (map[string]*RolePermissions, error)

// roleDefsLoader returns a roleDefsResolver that loads the requested
// definitions from the store (loadRoleDefinitions).
func (a *AuthzService) roleDefsLoader(ctx context.Context) roleDefsResolver {
	return func(roleDefIDs []string) (map[string]*RolePermissions, error) {
		return a.loadRoleDefinitions(ctx, roleDefIDs)
	}
}

// membershipBindings returns the bindings in candidates whose role confers
// project membership (RolePermissions.ConfersMembership). A binding of a
// role that does not, such as project-visitor, is dropped, and so is one
// whose role definition is missing (fail closed). resolve is called once,
// and only when candidates is non-empty. A resolve error is returned;
// callers deny.
//
// Every rule that means "member" filters its bindings through this (or
// through store.RoleDefinition.ConfersProjectMembership on a role
// definition it already holds); permission decisions do not, since a
// visitor's binding grants exactly its role's permissions.
func membershipBindings(candidates []*store.RoleBinding, resolve roleDefsResolver) ([]*store.RoleBinding, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	defs, err := resolve(collectRoleDefinitionIDs(candidates))
	if err != nil {
		return nil, fmt.Errorf("resolve role definitions for membership: %w", err)
	}
	out := make([]*store.RoleBinding, 0, len(candidates))
	for _, b := range candidates {
		if rp := defs[b.RoleDefinitionID]; rp != nil && rp.ConfersMembership {
			out = append(out, b)
		}
	}
	return out, nil
}

// withoutNonMembershipBindings returns bindings without those whose role
// definition exists and does not confer membership (project-visitor). It
// is for listings: unlike membershipBindings, a binding whose role
// definition is missing is kept, as the listing showed it before.
func withoutNonMembershipBindings(ctx context.Context, st store.Store, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(bindings))
	ids := make([]string, 0, len(bindings))
	for _, b := range bindings {
		if b == nil || seen[b.RoleDefinitionID] {
			continue
		}
		seen[b.RoleDefinitionID] = true
		ids = append(ids, b.RoleDefinitionID)
	}
	defs, err := st.GetRoleDefinitionsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve role definitions for membership: %w", err)
	}
	out := make([]*store.RoleBinding, 0, len(bindings))
	for _, b := range bindings {
		if b == nil {
			continue
		}
		if rd, resolved := defs[b.RoleDefinitionID]; !resolved || rd == nil || rd.ConfersProjectMembership() {
			out = append(out, b)
		}
	}
	return out, nil
}
