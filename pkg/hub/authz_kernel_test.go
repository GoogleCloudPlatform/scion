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
	"math/rand"
	"testing"
	"time"
)

// testNow is a fixed time used across all kernel tests for determinism.
var testNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// --- Helper builders ---

func makeRole(id, name, scopeType string, perms ...string) *RolePermissions {
	return NewRolePermissions(id, name, scopeType, perms)
}

func makeBinding(bindingID, roleDefID, principalType, principalID, scopeType, scopeID string) CandidateBinding {
	return CandidateBinding{
		BindingID:        bindingID,
		RoleDefinitionID: roleDefID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        scopeType,
		ScopeID:          scopeID,
	}
}

func makeTimedBinding(bindingID, roleDefID, principalType, principalID, scopeType, scopeID string, notBefore, expiresAt time.Time) CandidateBinding {
	cb := makeBinding(bindingID, roleDefID, principalType, principalID, scopeType, scopeID)
	cb.NotBefore = notBefore
	cb.ExpiresAt = expiresAt
	return cb
}

func closureOf(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

func membershipPaths(paths map[string][]string) map[string][]string {
	return paths
}

// --- Table Tests ---

func TestEvaluate_DefaultDeny_NoBindings(t *testing.T) {
	req := KernelRequest{
		Permission:       "agent.create",
		PrincipalClosure: closureOf("user1"),
		Resource:         ResourceContext{ProjectID: "proj-a"},
		Now:              testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("expected deny with no bindings")
	}
	if len(d.Provenance.DenyReasons) == 0 {
		t.Fatal("expected deny reasons")
	}
}

func TestEvaluate_DefaultDeny_EmptyPermission(t *testing.T) {
	req := KernelRequest{
		Permission:       "",
		PrincipalClosure: closureOf("user1"),
		Now:              testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("expected deny with empty permission")
	}
}

func TestEvaluate_DefaultDeny_EmptyClosure(t *testing.T) {
	req := KernelRequest{
		Permission:       "agent.create",
		PrincipalClosure: closureOf(),
		Now:              testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("expected deny with empty closure")
	}
}

func TestEvaluate_SimpleAllow(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem, "agent.create", "agent.read"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}
	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatalf("expected allow, got deny: %v", d.Provenance.DenyReasons)
	}
	if d.Provenance.Permission != "agent.create" {
		t.Fatalf("expected permission agent.create, got %s", d.Provenance.Permission)
	}
	if len(d.Provenance.GrantingBindings) == 0 {
		t.Fatal("expected granting bindings")
	}
}

func TestEvaluate_OrderIndependence(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "role-a", ScopeTypeProject, "agent.read"),
		"r2": makeRole("r2", "role-b", ScopeTypeProject, "agent.create"),
		"r3": makeRole("r3", "role-c", ScopeTypeProject, "agent.delete"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a"),
		makeBinding("b2", "r2", "user", "user1", ScopeTypeProject, "proj-a"),
		makeBinding("b3", "r3", "user", "user1", ScopeTypeProject, "proj-a"),
	}

	// Evaluate with original order.
	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d1 := Evaluate(req)

	// Shuffle bindings.
	shuffled := make([]CandidateBinding, len(bindings))
	copy(shuffled, bindings)
	shuffled[0], shuffled[2] = shuffled[2], shuffled[0]
	req.CandidateBindings = shuffled
	d2 := Evaluate(req)

	if d1.Allowed != d2.Allowed {
		t.Fatal("order of bindings changed the decision")
	}

	// Verify effective permissions are the same.
	ep1 := d1.Provenance.EffectivePermissions
	ep2 := d2.Provenance.EffectivePermissions
	if len(ep1) != len(ep2) {
		t.Fatalf("effective permissions differ: %v vs %v", ep1, ep2)
	}
	for i := range ep1 {
		if ep1[i] != ep2[i] {
			t.Fatalf("effective permissions differ at %d: %q vs %q", i, ep1[i], ep2[i])
		}
	}
}

func TestEvaluate_AdditiveGrants(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "role-a", ScopeTypeProject, "agent.read"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a"),
	}

	req := KernelRequest{
		Permission:        "agent.read",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d1 := Evaluate(req)
	if !d1.Allowed {
		t.Fatal("expected allow")
	}
	basePerms := len(d1.Provenance.EffectivePermissions)

	// Add another binding with a different role.
	roles["r2"] = makeRole("r2", "role-b", ScopeTypeProject, "agent.create")
	req.CandidateBindings = append(req.CandidateBindings,
		makeBinding("b2", "r2", "user", "user1", ScopeTypeProject, "proj-a"))
	d2 := Evaluate(req)
	if !d2.Allowed {
		t.Fatal("expected allow with additional binding")
	}
	if len(d2.Provenance.EffectivePermissions) < basePerms {
		t.Fatal("adding a binding reduced authority")
	}
}

func TestEvaluate_SubtractiveRestrictions(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem, "agent.create", "agent.read", "agent.delete"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d1 := Evaluate(req)
	if !d1.Allowed {
		t.Fatal("expected allow without restrictions")
	}
	basePerms := len(d1.Provenance.EffectivePermissions)

	// Add a restriction that removes agent.create.
	req.Restrictions = []Restriction{
		{
			Kind:        "credential_scope",
			Description: "UAT does not include agent.create",
			Check: func(permissionID string) bool {
				return permissionID != "agent.create"
			},
		},
	}
	d2 := Evaluate(req)
	if d2.Allowed {
		t.Fatal("expected deny after restriction removes agent.create")
	}
	if len(d2.Provenance.EffectivePermissions) > basePerms {
		t.Fatal("adding a restriction increased authority")
	}
}

func TestEvaluate_ExpirationBoundaries(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "temp-role", ScopeTypeSystem, "agent.create"),
	}

	pastExpiry := testNow.Add(-1 * time.Hour)
	futureNotBefore := testNow.Add(1 * time.Hour)
	pastNotBefore := testNow.Add(-2 * time.Hour)
	futureExpiry := testNow.Add(24 * time.Hour)

	cases := []struct {
		name      string
		notBefore time.Time
		expiresAt time.Time
		wantAllow bool
	}{
		{"expired", time.Time{}, pastExpiry, false},
		{"not_yet_active", futureNotBefore, time.Time{}, false},
		{"active_window", pastNotBefore, futureExpiry, true},
		{"no_conditions", time.Time{}, time.Time{}, true},
		{"exact_notBefore", testNow, time.Time{}, true},
		{"exact_expiresAt", time.Time{}, testNow, false}, // expiresAt is exclusive
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bindings := []CandidateBinding{
				makeTimedBinding("b1", "r1", "user", "user1", ScopeTypeSystem, "", tc.notBefore, tc.expiresAt),
			}
			req := KernelRequest{
				Permission:        "agent.create",
				PrincipalClosure:  closureOf("user1"),
				Resource:          ResourceContext{ProjectID: "proj-a"},
				CandidateBindings: bindings,
				RoleDefinitions:   roles,
				Now:               testNow,
			}
			d := Evaluate(req)
			if d.Allowed != tc.wantAllow {
				t.Fatalf("got Allowed=%v, want %v (reasons: %v)", d.Allowed, tc.wantAllow, d.Provenance.DenyReasons)
			}
		})
	}
}

func TestEvaluate_ScopeIsolation(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "project-member", ScopeTypeProject, "agent.create"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a"),
	}

	// Request for proj-a: should allow.
	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatal("expected allow for proj-a")
	}

	// Request for proj-b: should deny.
	req.Resource = ResourceContext{ProjectID: "proj-b"}
	d = Evaluate(req)
	if d.Allowed {
		t.Fatal("project-A grant should not authorize project-B resource")
	}
}

func TestEvaluate_SystemScopeContainment(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "super-admin", ScopeTypeSystem, "agent.create"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	// System grant should authorize any project resource.
	projects := []string{"proj-a", "proj-b", "proj-c"}
	for _, proj := range projects {
		req := KernelRequest{
			Permission:        "agent.create",
			PrincipalClosure:  closureOf("user1"),
			Resource:          ResourceContext{ProjectID: proj},
			CandidateBindings: bindings,
			RoleDefinitions:   roles,
			Now:               testNow,
		}
		d := Evaluate(req)
		if !d.Allowed {
			t.Fatalf("system grant should authorize project %s", proj)
		}
	}
}

func TestEvaluate_MultipleRolesUnion(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "reader", ScopeTypeProject, "agent.read"),
		"r2": makeRole("r2", "creator", ScopeTypeProject, "agent.create"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a"),
		makeBinding("b2", "r2", "user", "user1", ScopeTypeProject, "proj-a"),
	}

	// Both agent.read and agent.create should be allowed.
	for _, perm := range []string{"agent.read", "agent.create"} {
		req := KernelRequest{
			Permission:        perm,
			PrincipalClosure:  closureOf("user1"),
			Resource:          ResourceContext{ProjectID: "proj-a"},
			CandidateBindings: bindings,
			RoleDefinitions:   roles,
			Now:               testNow,
		}
		d := Evaluate(req)
		if !d.Allowed {
			t.Fatalf("expected allow for %s from union of roles", perm)
		}
	}

	// agent.delete should still be denied.
	req := KernelRequest{
		Permission:        "agent.delete",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("agent.delete should not be in the union of reader + creator")
	}
}

func TestEvaluate_GroupMembership(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "team-role", ScopeTypeProject, "agent.create"),
	}
	// Binding is to a group, not the user directly.
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "group", "group1", ScopeTypeProject, "proj-a"),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1", "group1"),
		MembershipPaths:   membershipPaths(map[string][]string{"group1": {"user1", "group1"}}),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatalf("expected allow via group membership, got deny: %v", d.Provenance.DenyReasons)
	}

	// Verify provenance shows the membership path.
	if len(d.Provenance.GrantingBindings) == 0 {
		t.Fatal("expected granting bindings")
	}
	gb := d.Provenance.GrantingBindings[0]
	if len(gb.MembershipPath) < 2 {
		t.Fatalf("expected membership path showing group derivation, got %v", gb.MembershipPath)
	}
}

func TestEvaluate_NestedGroups(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "team-role", ScopeTypeProject, "agent.create"),
	}
	// Binding is to a parent group.
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "group", "parent-group", ScopeTypeProject, "proj-a"),
	}

	// User is in child-group, which is in parent-group.
	req := KernelRequest{
		Permission:       "agent.create",
		PrincipalClosure: closureOf("user1", "child-group", "parent-group"),
		MembershipPaths: membershipPaths(map[string][]string{
			"child-group":  {"user1", "child-group"},
			"parent-group": {"user1", "child-group", "parent-group"},
		}),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatalf("expected allow via nested group, got deny: %v", d.Provenance.DenyReasons)
	}

	// Verify provenance shows nested path.
	if len(d.Provenance.GrantingBindings) == 0 {
		t.Fatal("expected granting bindings")
	}
	gb := d.Provenance.GrantingBindings[0]
	if len(gb.MembershipPath) < 3 {
		t.Fatalf("expected nested membership path, got %v", gb.MembershipPath)
	}
}

func TestEvaluate_FalseActivationOnlyAffectsOwnGrant(t *testing.T) {
	// Design invariant: a false activation condition suppresses ONLY that
	// grant. It does not deny permissions from another grant.
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "temp-role", ScopeTypeSystem, "agent.create"),
		"r2": makeRole("r2", "perm-role", ScopeTypeSystem, "agent.create"),
	}
	bindings := []CandidateBinding{
		// This binding is expired.
		makeTimedBinding("b1", "r1", "user", "user1", ScopeTypeSystem, "",
			time.Time{}, testNow.Add(-1*time.Hour)),
		// This binding is active.
		makeBinding("b2", "r2", "user", "user1", ScopeTypeSystem, ""),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatal("expired binding should not deny permissions from the active binding")
	}
}

func TestEvaluate_OwnerAdminPassesThoughRestrictions(t *testing.T) {
	// Design invariant: owner/admin roles pass through restrictions like
	// any other grant — no early-allow bypass.
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "project-owner", ScopeTypeProject, "agent.create", "agent.delete"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a"),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Restrictions: []Restriction{
			{
				Kind:        "credential_scope",
				Description: "UAT does not include agent.create",
				Check: func(permissionID string) bool {
					return permissionID != "agent.create"
				},
			},
		},
		Now: testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("owner role should still be denied by credential restriction")
	}
}

func TestEvaluate_UnknownRoleDefinition(t *testing.T) {
	// Fail closed when role definition is missing.
	bindings := []CandidateBinding{
		makeBinding("b1", "unknown-role", "user", "user1", ScopeTypeSystem, ""),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   map[string]*RolePermissions{}, // empty
		Now:               testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("expected deny when role definition is unknown")
	}
}

func TestEvaluate_UnknownScopeType(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "role", "custom", "agent.create"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", "custom", ""),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("unknown scope type should fail closed")
	}
}

func TestEvaluate_EmptyResourceProjectID(t *testing.T) {
	// A project-scoped binding should not match when the resource has no project.
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "member", ScopeTypeProject, "hub.settings.read"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a"),
	}

	req := KernelRequest{
		Permission:        "hub.settings.read",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{}, // no project
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("project binding should not match hub-level resource with no project")
	}
}

func TestEvaluate_MultipleRestrictions(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem, "agent.create", "agent.read", "agent.delete"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	// Two restrictions that both allow agent.create.
	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Restrictions: []Restriction{
			{
				Kind:        "credential_scope",
				Description: "allows create",
				Check: func(permissionID string) bool {
					return permissionID == "agent.create" || permissionID == "agent.read"
				},
			},
			{
				Kind:        "delegation_ceiling",
				Description: "allows create and delete",
				Check: func(permissionID string) bool {
					return permissionID == "agent.create" || permissionID == "agent.delete"
				},
			},
		},
		Now: testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatal("permission allowed by both restrictions should be granted")
	}

	// Now check agent.read — allowed by first, denied by second.
	req.Permission = "agent.read"
	d = Evaluate(req)
	if d.Allowed {
		t.Fatal("permission denied by one restriction should be denied overall")
	}
}

func TestEvaluate_SuspensionRestriction(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem, "agent.create"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Restrictions: []Restriction{
			{
				Kind:        "suspension",
				Description: "principal is suspended",
				Check:       func(string) bool { return false }, // deny everything
			},
		},
		Now: testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("suspended principal should be denied")
	}
}

func TestEvaluate_Provenance_FullTrace(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "member", ScopeTypeProject, "agent.read"),
		"r2": makeRole("r2", "creator", ScopeTypeProject, "agent.create"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a"),
		makeBinding("b2", "r2", "group", "group1", ScopeTypeProject, "proj-a"),
		// This binding is for a different project.
		makeBinding("b3", "r2", "user", "user1", ScopeTypeProject, "proj-b"),
	}

	req := KernelRequest{
		Permission:       "agent.read",
		PrincipalClosure: closureOf("user1", "group1"),
		MembershipPaths: membershipPaths(map[string][]string{
			"user1":  {"user1"},
			"group1": {"user1", "group1"},
		}),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatalf("expected allow, got deny: %v", d.Provenance.DenyReasons)
	}

	p := d.Provenance
	if p.Permission != "agent.read" {
		t.Fatalf("expected permission agent.read, got %s", p.Permission)
	}
	if !p.Granted {
		t.Fatal("expected Granted=true")
	}
	if len(p.GrantingBindings) < 1 {
		t.Fatal("expected at least one granting binding")
	}
	if len(p.RejectedCandidates) < 1 {
		t.Fatal("expected at least one rejected candidate (b3, wrong project)")
	}
	if len(p.EffectivePermissions) == 0 {
		t.Fatal("expected effective permissions")
	}
}

func TestEvaluate_Provenance_RejectedReasons(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "member", ScopeTypeProject, "agent.read"),
	}

	// Expired binding.
	bindings := []CandidateBinding{
		makeTimedBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a",
			time.Time{}, testNow.Add(-1*time.Hour)),
	}

	req := KernelRequest{
		Permission:        "agent.read",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("expected deny for expired binding")
	}
	if len(d.Provenance.RejectedCandidates) == 0 {
		t.Fatal("expected rejected candidate with reason")
	}
	rc := d.Provenance.RejectedCandidates[0]
	if len(rc.RejectReasons) == 0 {
		t.Fatal("expected reject reasons")
	}
	found := false
	for _, reason := range rc.RejectReasons {
		if reason == "binding expired (expiresAt)" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected expiration reason in %v", rc.RejectReasons)
	}
}

// --- Property Tests ---

// TestProperty_AdditiveGrants verifies: for any set of bindings B and any
// additional binding b, Evaluate(B+b).permissions is a superset of
// Evaluate(B).permissions.
func TestProperty_AdditiveGrants(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "role-a", ScopeTypeSystem, "p1", "p2"),
		"r2": makeRole("r2", "role-b", ScopeTypeSystem, "p2", "p3"),
		"r3": makeRole("r3", "role-c", ScopeTypeSystem, "p3", "p4"),
		"r4": makeRole("r4", "role-d", ScopeTypeProject, "p5"),
	}

	allBindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
		makeBinding("b2", "r2", "user", "user1", ScopeTypeSystem, ""),
		makeBinding("b3", "r3", "user", "user1", ScopeTypeSystem, ""),
		makeBinding("b4", "r4", "user", "user1", ScopeTypeProject, "proj-a"),
	}

	for trial := 0; trial < 50; trial++ {
		// Pick a random subset as the base.
		baseSize := rng.Intn(len(allBindings))
		perm := rng.Perm(len(allBindings))
		base := make([]CandidateBinding, baseSize)
		for i := 0; i < baseSize; i++ {
			base[i] = allBindings[perm[i]]
		}

		// Pick one binding to add.
		extra := allBindings[rng.Intn(len(allBindings))]
		extended := append(append([]CandidateBinding{}, base...), extra)

		reqBase := KernelRequest{
			Permission:        "p2",
			PrincipalClosure:  closureOf("user1"),
			Resource:          ResourceContext{ProjectID: "proj-a"},
			CandidateBindings: base,
			RoleDefinitions:   roles,
			Now:               testNow,
		}
		reqExtended := reqBase
		reqExtended.CandidateBindings = extended

		dBase := Evaluate(reqBase)
		dExtended := Evaluate(reqExtended)

		// Every permission in base must be in extended.
		basePerms := make(map[string]struct{})
		for _, p := range dBase.Provenance.EffectivePermissions {
			basePerms[p] = struct{}{}
		}
		for p := range basePerms {
			found := false
			for _, ep := range dExtended.Provenance.EffectivePermissions {
				if ep == p {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("trial %d: adding binding removed permission %q from effective set. base=%v extended=%v",
					trial, p, dBase.Provenance.EffectivePermissions, dExtended.Provenance.EffectivePermissions)
			}
		}
	}
}

// TestProperty_SubtractiveRestrictions verifies: for any set of restrictions R
// and any additional restriction r, Evaluate(R+r).permissions is a subset of
// Evaluate(R).permissions.
func TestProperty_SubtractiveRestrictions(t *testing.T) {
	rng := rand.New(rand.NewSource(43))

	allPerms := []string{"p1", "p2", "p3", "p4", "p5"}
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "full", ScopeTypeSystem, allPerms...),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	// Generate restrictions that allow random subsets.
	makeRestriction := func(seed int) Restriction {
		allowed := make(map[string]struct{})
		for _, p := range allPerms {
			if rng.Intn(2) == 0 {
				allowed[p] = struct{}{}
			}
		}
		return Restriction{
			Kind:        "test-restriction",
			Description: "random subset",
			Check: func(permissionID string) bool {
				_, ok := allowed[permissionID]
				return ok
			},
		}
	}

	for trial := 0; trial < 50; trial++ {
		// Generate a random set of restrictions.
		numBase := rng.Intn(4)
		base := make([]Restriction, numBase)
		for i := range base {
			base[i] = makeRestriction(trial*10 + i)
		}

		// Add one more.
		extra := makeRestriction(trial * 100)
		extended := append(append([]Restriction{}, base...), extra)

		for _, perm := range allPerms {
			reqBase := KernelRequest{
				Permission:        perm,
				PrincipalClosure:  closureOf("user1"),
				Resource:          ResourceContext{ProjectID: "proj-a"},
				CandidateBindings: bindings,
				RoleDefinitions:   roles,
				Restrictions:      base,
				Now:               testNow,
			}
			reqExtended := reqBase
			reqExtended.Restrictions = extended

			dBase := Evaluate(reqBase)
			dExtended := Evaluate(reqExtended)

			// If extended allows, base must also allow.
			if dExtended.Allowed && !dBase.Allowed {
				t.Fatalf("trial %d perm %s: adding restriction increased authority", trial, perm)
			}
		}
	}
}

// TestProperty_OrderIndependence_Randomized verifies that shuffling bindings
// does not change the effective permission set.
func TestProperty_OrderIndependence_Randomized(t *testing.T) {
	rng := rand.New(rand.NewSource(44))

	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "a", ScopeTypeSystem, "p1", "p2"),
		"r2": makeRole("r2", "b", ScopeTypeProject, "p2", "p3"),
		"r3": makeRole("r3", "c", ScopeTypeProject, "p4"),
		"r4": makeRole("r4", "d", ScopeTypeSystem, "p5"),
	}

	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
		makeBinding("b2", "r2", "user", "user1", ScopeTypeProject, "proj-a"),
		makeBinding("b3", "r3", "group", "group1", ScopeTypeProject, "proj-a"),
		makeBinding("b4", "r4", "user", "user1", ScopeTypeSystem, ""),
	}

	for trial := 0; trial < 20; trial++ {
		shuffled := make([]CandidateBinding, len(bindings))
		copy(shuffled, bindings)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})

		reqOrig := KernelRequest{
			Permission:        "p2",
			PrincipalClosure:  closureOf("user1", "group1"),
			Resource:          ResourceContext{ProjectID: "proj-a"},
			CandidateBindings: bindings,
			RoleDefinitions:   roles,
			Now:               testNow,
		}
		reqShuffled := reqOrig
		reqShuffled.CandidateBindings = shuffled

		dOrig := Evaluate(reqOrig)
		dShuffled := Evaluate(reqShuffled)

		if dOrig.Allowed != dShuffled.Allowed {
			t.Fatalf("trial %d: shuffle changed decision", trial)
		}

		ep1 := dOrig.Provenance.EffectivePermissions
		ep2 := dShuffled.Provenance.EffectivePermissions
		if len(ep1) != len(ep2) {
			t.Fatalf("trial %d: effective permissions differ after shuffle: %v vs %v", trial, ep1, ep2)
		}
		for i := range ep1 {
			if ep1[i] != ep2[i] {
				t.Fatalf("trial %d: permission %d differs: %q vs %q", trial, i, ep1[i], ep2[i])
			}
		}
	}
}

func TestEvaluate_NilRestrictionCheckAllowsEverything(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem, "agent.create"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Restrictions: []Restriction{
			{
				Kind:        "no-op",
				Description: "restriction with nil check",
				Check:       nil,
			},
		},
		Now: testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatal("nil check restriction should allow everything")
	}
}

func TestEvaluate_CredentialCaveatIsRestriction(t *testing.T) {
	// Verify that credential caveats and delegation ceilings use the
	// restriction mechanism and can only reduce authority.
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem,
			"agent.create", "agent.read", "agent.delete", "project.read"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	// UAT scoped to only agent.read.
	uatScopes := map[string]struct{}{"agent.read": {}}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Restrictions: []Restriction{
			{
				Kind:        "credential_scope",
				Description: "UAT scoped to agent.read only",
				Check: func(permissionID string) bool {
					_, ok := uatScopes[permissionID]
					return ok
				},
			},
		},
		Now: testNow,
	}

	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("agent.create should be denied by UAT scope restriction")
	}

	// agent.read should still be allowed.
	req.Permission = "agent.read"
	d = Evaluate(req)
	if !d.Allowed {
		t.Fatal("agent.read should be allowed by UAT scope restriction")
	}
}

func TestEvaluate_RestrictionProvenanceRecorded(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem, "agent.create"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Restrictions: []Restriction{
			{
				Kind:        "suspension",
				Description: "principal is suspended",
				Check:       func(string) bool { return false },
			},
		},
		Now: testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("expected deny")
	}

	if len(d.Provenance.Restrictions) == 0 {
		t.Fatal("expected restriction in provenance")
	}
	rr := d.Provenance.Restrictions[0]
	if rr.Kind != "suspension" {
		t.Fatalf("expected suspension restriction, got %s", rr.Kind)
	}
	if !rr.Applied {
		t.Fatal("expected restriction to be applied")
	}
}
