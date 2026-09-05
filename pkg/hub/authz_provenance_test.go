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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provenanceTestNow is a fixed time for deterministic provenance tests.
var provenanceTestNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// =============================================================================
// Kernel-level provenance tests (no store dependency)
// =============================================================================

// TestProvenance_NestedMultiPathGroups verifies that when a principal is
// reachable via multiple group chains, all contributing paths are reported
// in the provenance.
func TestProvenance_NestedMultiPathGroups(t *testing.T) {
	// Setup: user1 is in group-a and group-b.
	// group-a and group-b both have a binding for agent.read.
	// Both paths should appear in the granting bindings.
	roleA := makeRole("role-a", "viewer-a", ScopeTypeSystem, "agent.read")
	roleB := makeRole("role-b", "viewer-b", ScopeTypeSystem, "agent.read")

	req := KernelRequest{
		Permission: "agent.read",
		PrincipalClosure: closureOf(
			"user:user1",
			"group:group-a",
			"group:group-b",
		),
		MembershipPaths: map[string][]string{
			"user:user1":    {"user:user1"},
			"group:group-a": {"user:user1", "group:group-a"},
			"group:group-b": {"user:user1", "group:group-b"},
		},
		Resource: ResourceContext{ProjectID: "proj-1"},
		CandidateBindings: []CandidateBinding{
			makeBinding("bind-a", "role-a", "group", "group-a", ScopeTypeSystem, ""),
			makeBinding("bind-b", "role-b", "group", "group-b", ScopeTypeSystem, ""),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-a": roleA,
			"role-b": roleB,
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	require.True(t, result.Allowed, "permission should be granted via both paths")

	// Both bindings should appear as granting.
	assert.Len(t, result.Provenance.GrantingBindings, 2,
		"both group paths should contribute")

	// Verify each path has a multi-element membership path.
	for _, gb := range result.Provenance.GrantingBindings {
		assert.Greater(t, len(gb.MembershipPath), 1,
			"membership path should be multi-element for group binding %s", gb.BindingID)
	}
}

// TestProvenance_OverlappingBoundaries verifies that when two access
// constraint boundaries apply to the same scope, both appear in the
// provenance restrictions.
func TestProvenance_OverlappingBoundaries(t *testing.T) {
	role := makeRole("role-1", "editor", ScopeTypeSystem, "agent.read", "agent.update", "agent.delete")

	req := KernelRequest{
		Permission:       "agent.delete",
		PrincipalClosure: closureOf("user:user1"),
		Resource:         ResourceContext{ProjectID: "proj-1"},
		CandidateBindings: []CandidateBinding{
			makeBinding("bind-1", "role-1", "user", "user1", ScopeTypeSystem, ""),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-1": role,
		},
		Restrictions: []Restriction{
			{
				Kind:              "access_constraint",
				Description:       `access constraint "boundary-1" (ac-1)`,
				BoundaryName:      "boundary-1",
				BoundaryID:        "ac-1",
				BoundaryScopeType: ScopeTypeSystem,
				Check: func(permID string) bool {
					// boundary-1 allows agent.read and agent.update only.
					return permID == "agent.read" || permID == "agent.update"
				},
			},
			{
				Kind:              "access_constraint",
				Description:       `access constraint "boundary-2" (ac-2)`,
				BoundaryName:      "boundary-2",
				BoundaryID:        "ac-2",
				BoundaryScopeType: ScopeTypeProject,
				BoundaryScopeID:   "proj-1",
				Check: func(permID string) bool {
					// boundary-2 allows agent.read only.
					return permID == "agent.read"
				},
			},
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	assert.False(t, result.Allowed, "agent.delete should be denied by boundaries")

	// Both restrictions should appear in provenance.
	assert.Len(t, result.Provenance.Restrictions, 2,
		"both boundaries should be in provenance")

	// First restriction should be applied (it removed agent.delete).
	assert.True(t, result.Provenance.Restrictions[0].Applied,
		"boundary-1 should have applied")
	assert.Equal(t, "boundary-1", result.Provenance.Restrictions[0].BoundaryName)
	assert.Equal(t, "ac-1", result.Provenance.Restrictions[0].BoundaryID)

	// Second restriction: agent.delete was already removed by boundary-1,
	// so boundary-2 did not change the outcome.
	assert.Equal(t, "boundary-2", result.Provenance.Restrictions[1].BoundaryName)
	assert.Equal(t, "ac-2", result.Provenance.Restrictions[1].BoundaryID)
}

// TestProvenance_NoncontributingRoles verifies that roles that exist but
// did not grant the requested permission appear as inactive (rejected
// candidates) in the explain output.
func TestProvenance_NoncontributingRoles(t *testing.T) {
	viewerRole := makeRole("role-viewer", "viewer", ScopeTypeProject, "agent.read")
	editorRole := makeRole("role-editor", "editor", ScopeTypeProject, "agent.update")

	req := KernelRequest{
		Permission:       "agent.update",
		PrincipalClosure: closureOf("user:user1"),
		Resource:         ResourceContext{ProjectID: "proj-1"},
		CandidateBindings: []CandidateBinding{
			// This binding's role does not contain agent.update.
			makeBinding("bind-viewer", "role-viewer", "user", "user1", ScopeTypeProject, "proj-1"),
			// This binding's role does contain agent.update.
			makeBinding("bind-editor", "role-editor", "user", "user1", ScopeTypeProject, "proj-1"),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-viewer": viewerRole,
			"role-editor": editorRole,
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	require.True(t, result.Allowed, "agent.update should be granted by editor role")

	// Viewer role contributed (it contributed agent.read to the union)
	// but it does NOT contain the requested permission.
	foundViewer := false
	foundEditor := false
	for _, gb := range result.Provenance.GrantingBindings {
		if gb.BindingID == "bind-viewer" {
			foundViewer = true
			assert.False(t, gb.ContainsRequested,
				"viewer binding should NOT contain the requested permission")
		}
		if gb.BindingID == "bind-editor" {
			foundEditor = true
			assert.True(t, gb.ContainsRequested,
				"editor binding should contain the requested permission")
		}
	}
	assert.True(t, foundViewer, "viewer binding should be in granting bindings")
	assert.True(t, foundEditor, "editor binding should be in granting bindings")
}

// TestProvenance_InactiveGrants verifies that expired or not-yet-active
// bindings appear in provenance with the correct rejection reasons.
func TestProvenance_InactiveGrants(t *testing.T) {
	role := makeRole("role-1", "editor", ScopeTypeSystem, "agent.read")

	expired := provenanceTestNow.Add(-1 * time.Hour)
	future := provenanceTestNow.Add(1 * time.Hour)

	req := KernelRequest{
		Permission:       "agent.read",
		PrincipalClosure: closureOf("user:user1"),
		Resource:         ResourceContext{},
		CandidateBindings: []CandidateBinding{
			// Expired binding.
			makeTimedBinding("bind-expired", "role-1", "user", "user1",
				ScopeTypeSystem, "", time.Time{}, expired),
			// Not yet active binding.
			makeTimedBinding("bind-future", "role-1", "user", "user1",
				ScopeTypeSystem, "", future, time.Time{}),
			// Active binding.
			makeBinding("bind-active", "role-1", "user", "user1", ScopeTypeSystem, ""),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-1": role,
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	require.True(t, result.Allowed, "should be granted by active binding")

	// Two bindings should be rejected.
	assert.Len(t, result.Provenance.RejectedCandidates, 2,
		"expired and future bindings should be rejected")

	for _, rc := range result.Provenance.RejectedCandidates {
		if rc.BindingID == "bind-expired" {
			assert.Contains(t, rc.RejectReasons[0], "expired",
				"expired binding should have expired reason")
		}
		if rc.BindingID == "bind-future" {
			assert.Contains(t, rc.RejectReasons[0], "not yet active",
				"future binding should have not-yet-active reason")
		}
	}
}

// TestProvenance_NeverGrantedPermission verifies that when a permission
// was never available (no binding grants it and no boundary removed it),
// the explain shows no grant and no boundary.
func TestProvenance_NeverGrantedPermission(t *testing.T) {
	role := makeRole("role-1", "viewer", ScopeTypeSystem, "agent.read")

	req := KernelRequest{
		Permission:       "agent.delete",
		PrincipalClosure: closureOf("user:user1"),
		Resource:         ResourceContext{},
		CandidateBindings: []CandidateBinding{
			makeBinding("bind-1", "role-1", "user", "user1", ScopeTypeSystem, ""),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-1": role,
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	assert.False(t, result.Allowed, "agent.delete should be denied")

	// The granting binding contributed agent.read but not agent.delete.
	assert.Len(t, result.Provenance.GrantingBindings, 1,
		"viewer binding should contribute agent.read to the union")
	assert.False(t, result.Provenance.GrantingBindings[0].ContainsRequested,
		"viewer binding should not contain the requested permission")

	// Deny reasons should indicate the permission was not in any role.
	assert.NotEmpty(t, result.Provenance.DenyReasons)
	assert.Contains(t, result.Provenance.DenyReasons[0], "do not include permission",
		"deny reason should explain the permission was not granted")
}

// TestProvenance_ExactVsClosure verifies that the provenance correctly
// distinguishes "direct" vs "group_membership" vs "group_closure" paths.
func TestProvenance_ExactVsClosure(t *testing.T) {
	role := makeRole("role-1", "viewer", ScopeTypeSystem, "agent.read")

	req := KernelRequest{
		Permission: "agent.read",
		PrincipalClosure: closureOf(
			"user:user1",
			"group:direct-group",
			"group:closure-group",
		),
		MembershipPaths: map[string][]string{
			"user:user1":          {"user:user1"},
			"group:direct-group":  {"user:user1", "group:direct-group"},
			"group:closure-group": {"user:user1", "group:direct-group", "group:closure-group"},
		},
		Resource: ResourceContext{},
		CandidateBindings: []CandidateBinding{
			// Binding via closure group (3-element path).
			makeBinding("bind-closure", "role-1", "group", "closure-group", ScopeTypeSystem, ""),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-1": role,
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	require.True(t, result.Allowed)

	// The granting binding should have the closure path.
	require.Len(t, result.Provenance.GrantingBindings, 1)
	gb := result.Provenance.GrantingBindings[0]
	assert.Len(t, gb.MembershipPath, 3,
		"closure path should have 3 elements: user, direct-group, closure-group")
	assert.Equal(t, "user:user1", gb.MembershipPath[0])
	assert.Equal(t, "group:direct-group", gb.MembershipPath[1])
	assert.Equal(t, "group:closure-group", gb.MembershipPath[2])
}

// TestProvenance_TypedMembershipPathKeys verifies that the kernel uses
// typed composite keys (type:id) for membership path lookup, preventing
// cross-type collisions.
func TestProvenance_TypedMembershipPathKeys(t *testing.T) {
	role := makeRole("role-1", "viewer", ScopeTypeSystem, "agent.read")

	// Create a scenario where a user and agent have the same bare ID.
	// The typed key should prevent collision.
	req := KernelRequest{
		Permission: "agent.read",
		PrincipalClosure: closureOf(
			"user:shared-id",
			"group:group-1",
		),
		MembershipPaths: map[string][]string{
			"user:shared-id": {"user:shared-id"},
			"group:group-1":  {"user:shared-id", "group:group-1"},
		},
		Resource: ResourceContext{},
		CandidateBindings: []CandidateBinding{
			makeBinding("bind-1", "role-1", "group", "group-1", ScopeTypeSystem, ""),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-1": role,
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	require.True(t, result.Allowed)

	// The granting binding's path should use the typed key from the
	// membershipPaths map, not a single-element fallback.
	require.Len(t, result.Provenance.GrantingBindings, 1)
	gb := result.Provenance.GrantingBindings[0]
	assert.Equal(t, []string{"user:shared-id", "group:group-1"}, gb.MembershipPath,
		"membership path should use typed key lookup")
}

// TestProvenance_ExplicitPermissionIDAudit verifies that the provenance
// includes the exact permission ID that was checked.
func TestProvenance_ExplicitPermissionIDAudit(t *testing.T) {
	role := makeRole("role-1", "viewer", ScopeTypeSystem, "hub.settings.read")

	req := KernelRequest{
		Permission:       "hub.settings.read",
		PrincipalClosure: closureOf("user:user1"),
		Resource:         ResourceContext{},
		CandidateBindings: []CandidateBinding{
			makeBinding("bind-1", "role-1", "user", "user1", ScopeTypeSystem, ""),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-1": role,
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	require.True(t, result.Allowed)
	assert.Equal(t, "hub.settings.read", result.Provenance.Permission,
		"provenance must include the exact permission ID that was checked")
}

// TestProvenance_FailureProvenance verifies that when no bindings exist,
// the provenance includes the deny reason instead of an empty trace.
func TestProvenance_FailureProvenance(t *testing.T) {
	req := KernelRequest{
		Permission:       "agent.create",
		PrincipalClosure: closureOf("user:user1"),
		Resource:         ResourceContext{ProjectID: "proj-1"},
		Now:              provenanceTestNow,
	}

	result := Evaluate(req)
	assert.False(t, result.Allowed)
	assert.NotEmpty(t, result.Provenance.DenyReasons,
		"deny decision must have non-empty DenyReasons, not an empty trace")
	assert.Contains(t, result.Provenance.DenyReasons[0], "no candidate bindings",
		"deny reason should explain why")
}

// TestProvenance_RestrictionBoundaryMetadata verifies that restriction
// results carry the full boundary metadata (name, ID, scope).
func TestProvenance_RestrictionBoundaryMetadata(t *testing.T) {
	role := makeRole("role-1", "editor", ScopeTypeSystem, "agent.read", "agent.delete")

	req := KernelRequest{
		Permission:       "agent.delete",
		PrincipalClosure: closureOf("user:user1"),
		Resource:         ResourceContext{},
		CandidateBindings: []CandidateBinding{
			makeBinding("bind-1", "role-1", "user", "user1", ScopeTypeSystem, ""),
		},
		RoleDefinitions: map[string]*RolePermissions{
			"role-1": role,
		},
		Restrictions: []Restriction{
			{
				Kind:              "access_constraint",
				Description:       `access constraint "prod-safety" (ac-prod)`,
				BoundaryName:      "prod-safety",
				BoundaryID:        "ac-prod",
				BoundaryScopeType: ScopeTypeProject,
				BoundaryScopeID:   "proj-prod",
				Check: func(permID string) bool {
					return permID == "agent.read" // Only allows read.
				},
			},
		},
		Now: provenanceTestNow,
	}

	result := Evaluate(req)
	assert.False(t, result.Allowed)

	require.Len(t, result.Provenance.Restrictions, 1)
	rr := result.Provenance.Restrictions[0]
	assert.True(t, rr.Applied, "restriction should have been applied")
	assert.Equal(t, "prod-safety", rr.BoundaryName)
	assert.Equal(t, "ac-prod", rr.BoundaryID)
	assert.Equal(t, ScopeTypeProject, rr.BoundaryScopeType)
	assert.Equal(t, "proj-prod", rr.BoundaryScopeID)
}

// =============================================================================
// DecisionProvenance conversion tests
// =============================================================================

func TestBuildDecisionProvenance_GrantsAndInactive(t *testing.T) {
	kp := KernelProvenance{
		Permission: "agent.read",
		Granted:    true,
		GrantingBindings: []GrantProvenance{
			{
				BindingID:         "bind-1",
				RoleID:            "role-1",
				RoleName:          "editor",
				ScopeType:         ScopeTypeSystem,
				PrincipalType:     "user",
				PrincipalID:       "user1",
				ContainsRequested: true,
				MembershipPath:    []string{"user:user1"},
				Contributed:       true,
			},
		},
		RejectedCandidates: []GrantProvenance{
			{
				BindingID:     "bind-expired",
				RoleID:        "role-1",
				RoleName:      "editor",
				ScopeType:     ScopeTypeSystem,
				PrincipalType: "user",
				PrincipalID:   "user1",
				RejectReasons: []string{"binding expired (expiresAt)"},
			},
		},
		Restrictions: []RestrictionResult{
			{
				Kind:              "access_constraint",
				Description:       `boundary "test" (ac-1)`,
				Applied:           false,
				BoundaryName:      "test",
				BoundaryID:        "ac-1",
				BoundaryScopeType: ScopeTypeSystem,
			},
		},
		EffectivePermissions: []string{"agent.read", "agent.update"},
	}

	dp := buildDecisionProvenance(kp)

	require.NotNil(t, dp)
	assert.Equal(t, "agent.read", dp.Permission)
	assert.Len(t, dp.Grants, 1)
	assert.Len(t, dp.InactiveGrants, 1)
	assert.Len(t, dp.Restrictions, 1)
	assert.Equal(t, []string{"agent.read", "agent.update"}, dp.EffectivePermissions)

	// Check that inactive grant has the rejection reason.
	assert.Equal(t, "binding expired (expiresAt)", dp.InactiveGrants[0].InactiveReason)

	// Check restriction metadata.
	assert.Equal(t, "test", dp.Restrictions[0].BoundaryName)
	assert.Equal(t, "ac-1", dp.Restrictions[0].BoundaryID)
	assert.Equal(t, "system", dp.Restrictions[0].BoundaryScope)
}

func TestBuildDecisionProvenance_StatusRestrictions(t *testing.T) {
	kp := KernelProvenance{
		Permission: "agent.read",
		Restrictions: []RestrictionResult{
			{
				Kind:        "credential_scope",
				Description: "UAT scope restriction",
				Applied:     true,
				Detail:      "permission removed by credential_scope",
			},
			{
				Kind:              "access_constraint",
				Description:       `boundary "b1" (ac-b1)`,
				Applied:           false,
				BoundaryName:      "b1",
				BoundaryID:        "ac-b1",
				BoundaryScopeType: ScopeTypeProject,
				BoundaryScopeID:   "proj-1",
			},
		},
		DenyReasons: []string{"restriction removed permission"},
	}

	dp := buildDecisionProvenance(kp)

	// credential_scope goes to StatusRestrictions.
	assert.Len(t, dp.StatusRestrictions, 1)
	assert.Equal(t, "credential_scope", dp.StatusRestrictions[0].Kind)
	assert.True(t, dp.StatusRestrictions[0].Applied)

	// access_constraint goes to Restrictions.
	assert.Len(t, dp.Restrictions, 1)
	assert.Equal(t, "access_constraint", dp.Restrictions[0].Kind)
	assert.Equal(t, "project:proj-1", dp.Restrictions[0].BoundaryScope)
}

func TestBuildDecisionProvenance_MembershipPaths(t *testing.T) {
	kp := KernelProvenance{
		Permission: "agent.read",
		Granted:    true,
		GrantingBindings: []GrantProvenance{
			{
				BindingID:      "bind-direct",
				PrincipalType:  "user",
				PrincipalID:    "user1",
				MembershipPath: []string{"user:user1"},
				Contributed:    true,
			},
			{
				BindingID:      "bind-group",
				PrincipalType:  "group",
				PrincipalID:    "eng",
				MembershipPath: []string{"user:user1", "group:eng"},
				Contributed:    true,
			},
			{
				BindingID:      "bind-closure",
				PrincipalType:  "group",
				PrincipalID:    "all-eng",
				MembershipPath: []string{"user:user1", "group:eng", "group:all-eng"},
				Contributed:    true,
			},
		},
	}

	dp := buildDecisionProvenance(kp)

	require.Len(t, dp.MembershipPaths, 3)

	// Direct path.
	assert.Equal(t, "user:user1", dp.MembershipPaths[0].TargetID)
	assert.Equal(t, "direct", dp.MembershipPaths[0].Kind)

	// Group membership (2-element path).
	assert.Equal(t, "group:eng", dp.MembershipPaths[1].TargetID)
	assert.Equal(t, "group_membership", dp.MembershipPaths[1].Kind)

	// Group closure (3+ element path).
	assert.Equal(t, "group:all-eng", dp.MembershipPaths[2].TargetID)
	assert.Equal(t, "group_closure", dp.MembershipPaths[2].Kind)
}

// =============================================================================
// Redaction tests
// =============================================================================

func TestRedactCrossPrincipalProvenance(t *testing.T) {
	dp := &DecisionProvenance{
		Permission: "agent.read",
		Grants: []GrantDetail{
			{
				BindingID:      "bind-1",
				RoleID:         "role-1",
				RoleName:       "editor",
				PrincipalType:  "user",
				PrincipalID:    "secret-user-id",
				MembershipPath: []string{"user:secret-user-id", "group:secret-group"},
				Permissions:    []string{"agent.read"},
			},
		},
		InactiveGrants: []GrantDetail{
			{
				BindingID:      "bind-expired",
				RoleID:         "role-1",
				PrincipalType:  "group",
				PrincipalID:    "secret-group",
				InactiveReason: "expired",
			},
		},
		Restrictions: []RestrictionProvenance{
			{
				Kind:         "access_constraint",
				BoundaryName: "sensitive-boundary-name",
				BoundaryID:   "ac-1",
			},
		},
		MembershipPaths: []MembershipPathDetail{
			{
				TargetID: "group:secret-group",
				Path:     []string{"user:secret-user-id", "group:secret-group"},
				Kind:     "group_membership",
			},
		},
	}

	redacted := redactCrossPrincipalProvenance(dp)

	// Permission and structure should be preserved.
	assert.Equal(t, "agent.read", redacted.Permission)
	assert.Len(t, redacted.Grants, 1)
	assert.Len(t, redacted.InactiveGrants, 1)
	assert.Len(t, redacted.Restrictions, 1)
	assert.Len(t, redacted.MembershipPaths, 1)

	// Principal IDs should be redacted in grants.
	assert.Equal(t, "[redacted]", redacted.Grants[0].PrincipalID,
		"grant principal ID should be redacted")
	assert.Equal(t, "[redacted]", redacted.InactiveGrants[0].PrincipalID,
		"inactive grant principal ID should be redacted")

	// Membership paths in grants should be nil (redacted).
	assert.Nil(t, redacted.Grants[0].MembershipPath,
		"grant membership path should be redacted")

	// Boundary names should be redacted.
	assert.Equal(t, "[redacted]", redacted.Restrictions[0].BoundaryName,
		"boundary name should be redacted")

	// Boundary ID should be preserved (stable identifier).
	assert.Equal(t, "ac-1", redacted.Restrictions[0].BoundaryID,
		"boundary ID should be preserved as stable identifier")

	// Binding and role IDs should be preserved (stable identifiers).
	assert.Equal(t, "bind-1", redacted.Grants[0].BindingID,
		"binding ID should be preserved")
	assert.Equal(t, "role-1", redacted.Grants[0].RoleID,
		"role ID should be preserved")

	// Role names should be preserved (system-defined, not sensitive).
	assert.Equal(t, "editor", redacted.Grants[0].RoleName,
		"role name should be preserved")

	// Permissions should be preserved.
	assert.Equal(t, []string{"agent.read"}, redacted.Grants[0].Permissions,
		"permissions should be preserved")

	// Membership path elements should be redacted.
	assert.Len(t, redacted.MembershipPaths[0].Path, 2)
	assert.Equal(t, "user:[redacted]", redacted.MembershipPaths[0].Path[0])
	assert.Equal(t, "group:[redacted]", redacted.MembershipPaths[0].Path[1])

	// Kind should be preserved.
	assert.Equal(t, "group_membership", redacted.MembershipPaths[0].Kind)
}

func TestRedactPathElement(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"user:alice", "user:[redacted]"},
		{"group:engineers", "group:[redacted]"},
		{"agent:my-agent", "agent:[redacted]"},
		{"dev:dev-user", "dev:[redacted]"},
		{"federated_user:ext-user", "federated_user:[redacted]"},
		{"federated_agent:ext-agent", "federated_agent:[redacted]"},
		{"unknown-format", "[redacted]"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, redactPathElement(tt.input))
		})
	}
}

// =============================================================================
// BFS path finding tests
// =============================================================================

func TestBfsGroupPath_DirectGroup(t *testing.T) {
	directGroups := map[string]bool{"g1": true}
	result := bfsGroupPath(directGroups, "g1", nil)
	assert.Equal(t, []string{"g1"}, result)
}

func TestBfsGroupPath_OneHop(t *testing.T) {
	directGroups := map[string]bool{"g1": true}
	childToParents := map[string][]string{
		"g1": {"g2"},
	}
	result := bfsGroupPath(directGroups, "g2", childToParents)
	assert.Equal(t, []string{"g1", "g2"}, result)
}

func TestBfsGroupPath_NoPath(t *testing.T) {
	directGroups := map[string]bool{"g1": true}
	childToParents := map[string][]string{
		"g3": {"g4"},
	}
	result := bfsGroupPath(directGroups, "g4", childToParents)
	assert.Nil(t, result, "should return nil when no path exists")
}

func TestFormatBoundaryScope(t *testing.T) {
	tests := []struct {
		scopeType string
		scopeID   string
		expected  string
	}{
		{"system", "", "system"},
		{"project", "proj-1", "project:proj-1"},
		{"", "", ""},
		{"", "something", ""},
	}
	for _, tt := range tests {
		result := formatBoundaryScope(tt.scopeType, tt.scopeID)
		assert.Equal(t, tt.expected, result)
	}
}

// TestProvenance_EmptySlicesNotNil verifies that all slice fields in
// DecisionProvenance are non-nil (empty slices, not nil) for clean JSON
// serialization.
func TestProvenance_EmptySlicesNotNil(t *testing.T) {
	kp := KernelProvenance{
		Permission: "agent.read",
	}

	dp := buildDecisionProvenance(kp)
	require.NotNil(t, dp)

	assert.NotNil(t, dp.Grants, "Grants should be non-nil empty slice")
	assert.NotNil(t, dp.InactiveGrants, "InactiveGrants should be non-nil empty slice")
	assert.NotNil(t, dp.Restrictions, "Restrictions should be non-nil empty slice")
	assert.NotNil(t, dp.MembershipPaths, "MembershipPaths should be non-nil empty slice")
}
