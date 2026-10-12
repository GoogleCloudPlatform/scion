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

// membershipBindings returns the bindings in candidates whose role confers
// project membership (store.RoleDefinition.ConfersProjectMembership). A
// binding of a role that does not, such as project-visitor, is dropped,
// and so is one whose role definition is missing (fail closed). The role
// definitions are resolved with one read, and only when candidates is
// non-empty. A read error is returned; callers deny.
//
// Every rule that means "member" filters its bindings through this (or
// through store.RoleDefinition.ConfersProjectMembership on a role
// definition it already holds); permission decisions do not, since a
// visitor's binding grants exactly its role's permissions.
func membershipBindings(ctx context.Context, st store.Store, candidates []*store.RoleBinding) ([]*store.RoleBinding, error) {
	return filterMembershipBindings(ctx, st, candidates, false)
}

// withoutNonMembershipBindings returns bindings without those whose role
// definition exists and does not confer membership (project-visitor). It
// is for listings: unlike membershipBindings, a binding whose role
// definition is missing is kept, as the listing showed it before.
func withoutNonMembershipBindings(ctx context.Context, st store.Store, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	return filterMembershipBindings(ctx, st, bindings, true)
}

// filterMembershipBindings keeps the bindings whose role confers
// membership; keepUnresolved also keeps those whose role definition is
// missing.
func filterMembershipBindings(ctx context.Context, st store.Store, candidates []*store.RoleBinding, keepUnresolved bool) ([]*store.RoleBinding, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(candidates))
	ids := make([]string, 0, len(candidates))
	for _, b := range candidates {
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
	out := make([]*store.RoleBinding, 0, len(candidates))
	for _, b := range candidates {
		if b == nil {
			continue
		}
		rd, resolved := defs[b.RoleDefinitionID]
		if rd.ConfersProjectMembership() || (keepUnresolved && (!resolved || rd == nil)) {
			out = append(out, b)
		}
	}
	return out, nil
}
