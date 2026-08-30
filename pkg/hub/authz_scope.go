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
	"sort"
	"strings"
	"time"
)

// scopeKind distinguishes the three forms of a ScopeSet.
type scopeKind int

const (
	scopeKindNone     scopeKind = iota // No authority.
	scopeKindAll                       // System-wide authority.
	scopeKindExplicit                  // Authority over an explicit set of project IDs.
)

// ScopeSet represents the set of projects over which a principal holds a
// particular permission. It has three forms:
//   - All: system-wide authority (every project, including future ones).
//   - None: no authority.
//   - Explicit: authority over a named set of project IDs.
//
// ScopeSet is a value type. The zero value is None.
type ScopeSet struct {
	kind       scopeKind
	projectIDs map[string]struct{}
}

// ScopeSetAll returns a ScopeSet representing system-wide authority.
func ScopeSetAll() ScopeSet {
	return ScopeSet{kind: scopeKindAll}
}

// ScopeSetNone returns a ScopeSet representing no authority.
func ScopeSetNone() ScopeSet {
	return ScopeSet{kind: scopeKindNone}
}

// ScopeSetExplicit returns a ScopeSet for the given project IDs.
// An empty list produces None, not an empty Explicit set.
func ScopeSetExplicit(projectIDs ...string) ScopeSet {
	if len(projectIDs) == 0 {
		return ScopeSetNone()
	}
	m := make(map[string]struct{}, len(projectIDs))
	for _, id := range projectIDs {
		m[id] = struct{}{}
	}
	return ScopeSet{kind: scopeKindExplicit, projectIDs: m}
}

// IsAll returns true if this ScopeSet represents system-wide authority.
func (s ScopeSet) IsAll() bool { return s.kind == scopeKindAll }

// IsNone returns true if this ScopeSet represents no authority.
func (s ScopeSet) IsNone() bool {
	if s.kind == scopeKindNone {
		return true
	}
	// An explicit set with no project IDs is equivalent to None.
	if s.kind == scopeKindExplicit && len(s.projectIDs) == 0 {
		return true
	}
	return false
}

// Contains returns true if the ScopeSet authorizes the given project ID.
func (s ScopeSet) Contains(projectID string) bool {
	switch s.kind {
	case scopeKindAll:
		return true
	case scopeKindExplicit:
		_, ok := s.projectIDs[projectID]
		return ok
	default:
		return false
	}
}

// ProjectIDs returns the explicit project IDs in sorted order.
// Returns nil for All or None.
func (s ScopeSet) ProjectIDs() []string {
	if s.kind != scopeKindExplicit || len(s.projectIDs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.projectIDs))
	for id := range s.projectIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Union returns a ScopeSet that contains projects authorized by either s or other.
//   - All.Union(X) = All
//   - X.Union(All) = All
//   - None.Union(X) = X
//   - X.Union(None) = X
//   - Explicit.Union(Explicit) = combined set
func (s ScopeSet) Union(other ScopeSet) ScopeSet {
	// All absorbs everything.
	if s.kind == scopeKindAll || other.kind == scopeKindAll {
		return ScopeSetAll()
	}
	// None is the identity.
	if s.IsNone() {
		return other
	}
	if other.IsNone() {
		return s
	}
	// Both are explicit.
	m := make(map[string]struct{}, len(s.projectIDs)+len(other.projectIDs))
	for id := range s.projectIDs {
		m[id] = struct{}{}
	}
	for id := range other.projectIDs {
		m[id] = struct{}{}
	}
	return ScopeSet{kind: scopeKindExplicit, projectIDs: m}
}

// Intersection returns a ScopeSet that contains only projects authorized by
// both s and other.
//   - All.Intersection(X) = X
//   - X.Intersection(All) = X
//   - None.Intersection(X) = None
//   - X.Intersection(None) = None
//   - Explicit.Intersection(Explicit) = common set
func (s ScopeSet) Intersection(other ScopeSet) ScopeSet {
	// None absorbs everything.
	if s.IsNone() || other.IsNone() {
		return ScopeSetNone()
	}
	// All is the identity.
	if s.kind == scopeKindAll {
		return other
	}
	if other.kind == scopeKindAll {
		return s
	}
	// Both are explicit: find common elements.
	// Iterate the smaller set for efficiency.
	small, big := s.projectIDs, other.projectIDs
	if len(small) > len(big) {
		small, big = big, small
	}
	m := make(map[string]struct{})
	for id := range small {
		if _, ok := big[id]; ok {
			m[id] = struct{}{}
		}
	}
	if len(m) == 0 {
		return ScopeSetNone()
	}
	return ScopeSet{kind: scopeKindExplicit, projectIDs: m}
}

// String returns a human-readable representation.
func (s ScopeSet) String() string {
	switch s.kind {
	case scopeKindAll:
		return "ScopeSet(All)"
	case scopeKindExplicit:
		ids := s.ProjectIDs()
		if len(ids) == 0 {
			return "ScopeSet(None)"
		}
		return "ScopeSet(" + strings.Join(ids, ", ") + ")"
	default:
		return "ScopeSet(None)"
	}
}

// Equal returns true if s and other represent the same set of projects.
func (s ScopeSet) Equal(other ScopeSet) bool {
	// Normalize empty explicit sets to None for comparison.
	sKind, oKind := s.kind, other.kind
	if sKind == scopeKindExplicit && len(s.projectIDs) == 0 {
		sKind = scopeKindNone
	}
	if oKind == scopeKindExplicit && len(other.projectIDs) == 0 {
		oKind = scopeKindNone
	}
	if sKind != oKind {
		return false
	}
	if sKind == scopeKindExplicit {
		if len(s.projectIDs) != len(other.projectIDs) {
			return false
		}
		for id := range s.projectIDs {
			if _, ok := other.projectIDs[id]; !ok {
				return false
			}
		}
	}
	return true
}

// ResolveAuthorizedScopes computes the ScopeSet for a given permission by
// examining role bindings and their role definitions.
//
// The inputs are already resolved — this function does not query a store.
//
// For each binding in candidateBindings:
//   - The binding must be active (checked via evaluateActivation with now).
//   - The binding's role definition must contain the requested permission.
//   - A system-scoped binding contributes All.
//   - A project-scoped binding contributes its project ID.
//
// The results are unioned, producing the broadest authorized scope.
//
// principalClosure is the set of principal IDs (direct + transitive groups)
// the caller has already resolved. Only bindings whose principal ID appears in
// this closure are considered.
//
// This function resolves scopes from active grants only. Restrictions (credential caveats,
// delegation ceilings, constraints) must be applied by the caller after scope resolution.
func ResolveAuthorizedScopes(
	principalClosure map[string]struct{},
	permissionID string,
	candidateBindings []CandidateBinding,
	roleDefinitions map[string]*RolePermissions,
	now time.Time,
) ScopeSet {
	result := ScopeSetNone()

	for i := range candidateBindings {
		cb := &candidateBindings[i]

		// Only consider bindings for principals in the closure.
		if _, ok := principalClosure[cb.PrincipalID]; !ok {
			continue
		}

		// Check activation conditions (notBefore / expiresAt).
		activation := evaluateActivation(cb, now)
		if !activation.Active {
			continue
		}

		// Look up the role.
		role, ok := roleDefinitions[cb.RoleDefinitionID]
		if !ok {
			continue
		}

		// Check if the role grants the requested permission.
		if !role.HasPermission(permissionID) {
			continue
		}

		// System scope means All.
		if cb.ScopeType == ScopeTypeSystem {
			return ScopeSetAll()
		}

		// Project scope contributes the project ID.
		if cb.ScopeType == ScopeTypeProject && cb.ScopeID != "" {
			result = result.Union(ScopeSetExplicit(cb.ScopeID))
		}
	}

	return result
}
