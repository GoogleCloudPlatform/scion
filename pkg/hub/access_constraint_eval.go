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
	"fmt"
	"time"
)

// ConstraintsToRestrictions converts stored AccessConstraint objects to AK1
// Restriction objects.
//
// Each active constraint becomes a Restriction whose Check function returns
// true only for permissions in the constraint's MaximumPermissions set.
//
// Multiple constraints intersect: a permission must be allowed by ALL
// applicable constraints to survive. An empty intersection is a deny, not a
// conflict.
//
// Constraints that are not active (disabled, outside time window) are skipped.
func ConstraintsToRestrictions(constraints []*AccessConstraint, now time.Time) []Restriction {
	var restrictions []Restriction

	for _, c := range constraints {
		if c == nil {
			continue
		}
		if !c.IsActive(now) {
			continue
		}

		// Build the permission set for efficient lookup.
		maxPerms := c.MaximumPermissionSet()

		// Capture for closure.
		constraintName := c.Name
		constraintID := c.ID

		restrictions = append(restrictions, Restriction{
			Kind:        "access_constraint",
			Description: fmt.Sprintf("access constraint %q (%s)", constraintName, constraintID),
			Check: func(permissionID string) bool {
				_, ok := maxPerms[permissionID]
				return ok
			},
		})
	}

	return restrictions
}

// FilterApplicableConstraints filters a list of constraints to those that
// apply to the given principal closure and scope. This is used when the
// store returns all potentially matching constraints and the caller needs
// to do additional filtering (e.g., subject matching).
//
// typedClosure is the set of typed principal keys ("type:id" format, e.g.
// "user:u1", "group:g1", "agent:a1") including transitive group memberships.
// scopeType and scopeID identify the target scope.
func FilterApplicableConstraints(
	constraints []*AccessConstraint,
	typedClosure map[string]struct{},
	scopeType string,
	scopeID string,
) []*AccessConstraint {
	var applicable []*AccessConstraint

	for _, c := range constraints {
		if c == nil {
			continue
		}

		// Check scope applicability.
		if !constraintScopeApplies(c, scopeType, scopeID) {
			continue
		}

		// Check subject applicability using the typed closure.
		if !c.Subject.MatchesPrincipalClosure(typedClosure) {
			continue
		}

		applicable = append(applicable, c)
	}

	return applicable
}

// constraintScopeApplies checks whether a constraint's scope covers the
// target scope.
//   - A system-scoped constraint applies to everything (system and project).
//   - A project-scoped constraint applies only when its scope ID matches the
//     target project ID.
func constraintScopeApplies(c *AccessConstraint, targetScopeType, targetScopeID string) bool {
	switch c.Scope.Type {
	case ScopeTypeSystem:
		// System constraints apply to all scopes.
		return true
	case ScopeTypeProject:
		// Project constraints apply only to matching projects.
		if targetScopeType != ScopeTypeProject {
			return false
		}
		return c.Scope.ID == targetScopeID
	default:
		// Unknown scope type: fail closed — apply the constraint so that
		// unrecognised scopes cannot silently bypass restrictions.
		return true
	}
}

// IntersectMaximumPermissions computes the intersection of multiple constraints'
// maximum permission sets. A permission must appear in ALL constraints to survive.
//
// If no constraints are provided, returns nil (no restriction).
// An empty intersection means all permissions are denied.
func IntersectMaximumPermissions(constraints []*AccessConstraint, now time.Time) map[string]struct{} {
	if len(constraints) == 0 {
		return nil
	}

	// Start with the first active constraint's permission set.
	var result map[string]struct{}
	initialized := false

	for _, c := range constraints {
		if c == nil || !c.IsActive(now) {
			continue
		}

		perms := c.MaximumPermissionSet()
		if !initialized {
			result = perms
			initialized = true
			continue
		}

		// Intersect: keep only permissions that are in both sets.
		for p := range result {
			if _, ok := perms[p]; !ok {
				delete(result, p)
			}
		}
	}

	return result
}
