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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// resolveUserOwnerProjectIDsOrError returns project IDs where the user has an
// active, direct project-owner RoleBinding. Unlike resolveUserOwnerProjectIDs,
// this version propagates dependency errors instead of silently returning nil
// (Finding 9: owner lookup errors must fail closed, not silently change
// classification).
func (s *Server) resolveUserOwnerProjectIDsOrError(ctx context.Context, userID string) ([]string, error) {
	bindings, err := s.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return nil, fmt.Errorf("owner resolution: list bindings: %w", err)
	}
	if len(bindings) == 0 {
		return nil, nil
	}

	ownerRoleDef, err := s.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		return nil, fmt.Errorf("owner resolution: get role definition: %w", err)
	}
	if ownerRoleDef == nil {
		return nil, fmt.Errorf("owner resolution: project-owner role definition not found")
	}

	now := time.Now()
	projectIDSet := make(map[string]struct{})
	for _, rb := range bindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID == "" {
			continue
		}
		if rb.RoleDefinitionID != ownerRoleDef.ID {
			continue
		}
		if rb.NotBefore != nil && now.Before(*rb.NotBefore) {
			continue
		}
		if rb.ExpiresAt != nil && now.After(*rb.ExpiresAt) {
			continue
		}
		projectIDSet[rb.ScopeID] = struct{}{}
	}

	if len(projectIDSet) == 0 {
		return nil, nil
	}

	projectIDs := make([]string, 0, len(projectIDSet))
	for id := range projectIDSet {
		projectIDs = append(projectIDs, id)
	}
	sort.Strings(projectIDs) // Finding 8: canonical ordering
	return projectIDs, nil
}

// SharedFilterResult holds the filter state for a Shared scope query.
// For All scope, OwnerExcludeIDs must be appended to ExcludedProjectIDs.
// For Explicit scope, ProjectIDs restricts to the shared set.
type SharedFilterResult struct {
	// IsAllScope is true when the authorized scope is All (system-wide).
	// The caller must exclude OwnerExcludeIDs rather than restricting to a set.
	IsAllScope bool
	// OwnerExcludeIDs holds Mine project IDs to exclude from an All-scope query.
	// Only meaningful when IsAllScope is true.
	OwnerExcludeIDs []string
	// ProjectIDs holds the explicit shared project set (All minus Mine).
	// Only meaningful when IsAllScope is false.
	ProjectIDs []string
}

// resolveSharedProjectFilter derives the Shared filter from the fully reduced
// ListScopeResult (Finding 7). For All scope, it returns the owner IDs to
// exclude (Shared = All minus Mine). For Explicit scope, it returns the
// concrete shared project ID set.
func (s *Server) resolveSharedProjectFilter(ctx context.Context, userID string, scopeResult ListScopeResult) (*SharedFilterResult, error) {
	ownerIDs, err := s.resolveUserOwnerProjectIDsOrError(ctx, userID)
	if err != nil {
		return nil, err
	}

	if scopeResult.Scopes.IsAll() {
		// System-wide scope (Finding 7): Shared = All minus Mine.
		// We cannot enumerate all projects, so we return the owner IDs to
		// exclude from the full-scope query.
		return &SharedFilterResult{
			IsAllScope:      true,
			OwnerExcludeIDs: ownerIDs,
		}, nil
	}

	// Explicit scope: Shared = authorized IDs minus owner IDs.
	authorizedIDs := scopeResult.Scopes.ProjectIDs()
	sharedIDs := subtractProjectIDs(authorizedIDs, ownerIDs)
	sort.Strings(sharedIDs) // Finding 8: canonical ordering
	return &SharedFilterResult{
		IsAllScope: false,
		ProjectIDs: sharedIDs,
	}, nil
}

// canonicalizeStringSlice sorts and deduplicates a string slice in place,
// returning the canonical result. Finding 8: identical authorization state
// must yield identical cursor binding regardless of input order.
func canonicalizeStringSlice(s []string) []string {
	if len(s) <= 1 {
		return s
	}
	sort.Strings(s)
	j := 0
	for i := 1; i < len(s); i++ {
		if s[i] != s[j] {
			j++
			s[j] = s[i]
		}
	}
	return s[:j+1]
}
