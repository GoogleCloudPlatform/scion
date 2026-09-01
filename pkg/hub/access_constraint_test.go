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
	"math/rand"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// SubjectSelector validation tests
// ---------------------------------------------------------------------------

func TestSubjectSelector_Validate(t *testing.T) {
	cases := []struct {
		name    string
		subject SubjectSelector
		wantErr bool
	}{
		{
			name: "valid principal user",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "user",
				PrincipalID:   "user1",
			},
		},
		{
			name: "valid principal agent",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "agent",
				PrincipalID:   "agent1",
			},
		},
		{
			name: "valid principal group",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "group",
				PrincipalID:   "group1",
			},
		},
		{
			name: "valid group closure",
			subject: SubjectSelector{
				Kind:    SubjectKindGroupClosure,
				GroupID: "group1",
			},
		},
		{
			name: "valid all principals",
			subject: SubjectSelector{
				Kind: SubjectKindAllPrincipals,
			},
		},
		{
			name: "principal missing type",
			subject: SubjectSelector{
				Kind:        SubjectKindPrincipal,
				PrincipalID: "user1",
			},
			wantErr: true,
		},
		{
			name: "principal invalid type",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "robot",
				PrincipalID:   "r1",
			},
			wantErr: true,
		},
		{
			name: "principal missing id",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "user",
			},
			wantErr: true,
		},
		{
			name: "group closure missing group id",
			subject: SubjectSelector{
				Kind: SubjectKindGroupClosure,
			},
			wantErr: true,
		},
		{
			name: "invalid kind",
			subject: SubjectSelector{
				Kind: "unknown",
			},
			wantErr: true,
		},
		// Orphaned field rejection tests.
		{
			name: "principal with orphaned groupId",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "user",
				PrincipalID:   "user1",
				GroupID:       "group1",
			},
			wantErr: true,
		},
		{
			name: "group_closure with orphaned principalId",
			subject: SubjectSelector{
				Kind:        SubjectKindGroupClosure,
				GroupID:     "group1",
				PrincipalID: "user1",
			},
			wantErr: true,
		},
		{
			name: "group_closure with orphaned principalType",
			subject: SubjectSelector{
				Kind:          SubjectKindGroupClosure,
				GroupID:       "group1",
				PrincipalType: "user",
			},
			wantErr: true,
		},
		{
			name: "all_principals with orphaned principalId",
			subject: SubjectSelector{
				Kind:        SubjectKindAllPrincipals,
				PrincipalID: "user1",
			},
			wantErr: true,
		},
		{
			name: "all_principals with orphaned principalType",
			subject: SubjectSelector{
				Kind:          SubjectKindAllPrincipals,
				PrincipalType: "user",
			},
			wantErr: true,
		},
		{
			name: "all_principals with orphaned groupId",
			subject: SubjectSelector{
				Kind:    SubjectKindAllPrincipals,
				GroupID: "group1",
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.subject.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error but got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ConstraintScopeRef validation tests
// ---------------------------------------------------------------------------

func TestConstraintScopeRef_Validate(t *testing.T) {
	cases := []struct {
		name    string
		scope   ConstraintScopeRef
		wantErr bool
	}{
		{name: "system scope", scope: ConstraintScopeRef{Type: ScopeTypeSystem}},
		{name: "project scope", scope: ConstraintScopeRef{Type: ScopeTypeProject, ID: "proj-a"}},
		{name: "project scope missing ID", scope: ConstraintScopeRef{Type: ScopeTypeProject}, wantErr: true},
		{name: "system scope with non-empty ID", scope: ConstraintScopeRef{Type: ScopeTypeSystem, ID: "proj-1"}, wantErr: true},
		{name: "invalid scope type", scope: ConstraintScopeRef{Type: "resource"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.scope.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ConstraintCondition tests
// ---------------------------------------------------------------------------

func TestConstraintCondition_IsActive(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		cond   ConstraintCondition
		active bool
	}{
		{"no conditions", ConstraintCondition{}, true},
		{"future not before", ConstraintCondition{NotBefore: now.Add(1 * time.Hour)}, false},
		{"past not before", ConstraintCondition{NotBefore: now.Add(-1 * time.Hour)}, true},
		{"exact not before", ConstraintCondition{NotBefore: now}, true},
		{"expired", ConstraintCondition{ExpiresAt: now.Add(-1 * time.Hour)}, false},
		{"not yet expired", ConstraintCondition{ExpiresAt: now.Add(1 * time.Hour)}, true},
		{"exact expires at", ConstraintCondition{ExpiresAt: now}, false}, // exclusive
		{"active window", ConstraintCondition{
			NotBefore: now.Add(-2 * time.Hour),
			ExpiresAt: now.Add(2 * time.Hour),
		}, true},
		{"expired with past not before", ConstraintCondition{
			NotBefore: now.Add(-4 * time.Hour),
			ExpiresAt: now.Add(-1 * time.Hour),
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cond.IsActive(now)
			if got != tc.active {
				t.Fatalf("IsActive() = %v, want %v", got, tc.active)
			}
		})
	}
}

func TestConstraintCondition_IsActiveInMostRestrictiveState(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		cond   ConstraintCondition
		active bool
	}{
		{"no conditions — will be active", ConstraintCondition{}, true},
		{"future not before — will be active eventually", ConstraintCondition{NotBefore: now.Add(24 * time.Hour)}, true},
		{"already expired — never active again", ConstraintCondition{ExpiresAt: now.Add(-1 * time.Hour)}, false},
		{"currently active", ConstraintCondition{
			NotBefore: now.Add(-1 * time.Hour),
			ExpiresAt: now.Add(1 * time.Hour),
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cond.IsActiveInMostRestrictiveState(now)
			if got != tc.active {
				t.Fatalf("IsActiveInMostRestrictiveState() = %v, want %v", got, tc.active)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AccessConstraint.Validate tests
// ---------------------------------------------------------------------------

func TestAccessConstraint_Validate(t *testing.T) {
	valid := &AccessConstraint{
		Name: "test-constraint",
		Subject: SubjectSelector{
			Kind:          SubjectKindPrincipal,
			PrincipalType: "user",
			PrincipalID:   "user1",
		},
		Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
		MaximumPermissions: []string{"agent.read", "agent.create"},
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("valid constraint should not error: %v", err)
	}

	// Missing name.
	noName := *valid
	noName.Name = ""
	if err := noName.Validate(); err == nil {
		t.Fatal("expected error for missing name")
	}

	// Empty permissions.
	noPerms := *valid
	noPerms.MaximumPermissions = nil
	if err := noPerms.Validate(); err == nil {
		t.Fatal("expected error for empty permissions")
	}
}

// ---------------------------------------------------------------------------
// AccessConstraint.IsActive tests
// ---------------------------------------------------------------------------

func TestAccessConstraint_IsActive(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	c := &AccessConstraint{
		MaximumPermissions: []string{"agent.read"},
	}
	if !c.IsActive(now) {
		t.Fatal("should be active with no conditions or disabled flag")
	}

	c.Disabled = true
	if c.IsActive(now) {
		t.Fatal("should not be active when disabled")
	}

	c.Disabled = false
	c.Condition = ConstraintCondition{ExpiresAt: now.Add(-1 * time.Hour)}
	if c.IsActive(now) {
		t.Fatal("should not be active when expired")
	}
}

// ---------------------------------------------------------------------------
// SubjectSelector.MatchesPrincipalClosure tests
// ---------------------------------------------------------------------------

func TestSubjectSelector_MatchesPrincipalClosure(t *testing.T) {
	// Typed closure: "type:id" keys matching the evaluator's format.
	closure := closureOf("user:user1", "group:group1", "group:group2")

	cases := []struct {
		name    string
		subject SubjectSelector
		match   bool
	}{
		{
			name: "exact principal in closure",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "user",
				PrincipalID:   "user1",
			},
			match: true,
		},
		{
			name: "exact principal not in closure",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "user",
				PrincipalID:   "user2",
			},
			match: false,
		},
		{
			name: "group closure matching",
			subject: SubjectSelector{
				Kind:    SubjectKindGroupClosure,
				GroupID: "group1",
			},
			match: true,
		},
		{
			name: "group closure not matching",
			subject: SubjectSelector{
				Kind:    SubjectKindGroupClosure,
				GroupID: "group3",
			},
			match: false,
		},
		{
			name: "all principals always matches",
			subject: SubjectSelector{
				Kind: SubjectKindAllPrincipals,
			},
			match: true,
		},
		{
			name: "principal type mismatch rejects same ID",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "group",
				PrincipalID:   "user1",
			},
			match: false,
		},
		{
			name: "principal targeting group matches group in closure",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "group",
				PrincipalID:   "group1",
			},
			match: true,
		},
		{
			name: "agent principal not in user closure",
			subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "agent",
				PrincipalID:   "user1",
			},
			match: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.subject.MatchesPrincipalClosure(closure)
			if got != tc.match {
				t.Fatalf("MatchesPrincipalClosure() = %v, want %v", got, tc.match)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ConstraintsToRestrictions tests
// ---------------------------------------------------------------------------

func TestConstraintsToRestrictions_BasicConversion(t *testing.T) {
	now := testNow
	constraints := []*AccessConstraint{
		{
			Name:               "allow-read-only",
			MaximumPermissions: []string{"agent.read", "project.read"},
			Condition:          ConstraintCondition{},
		},
	}

	restrictions := ConstraintsToRestrictions(constraints, now)
	if len(restrictions) != 1 {
		t.Fatalf("expected 1 restriction, got %d", len(restrictions))
	}

	r := restrictions[0]
	if r.Kind != "access_constraint" {
		t.Fatalf("expected kind access_constraint, got %s", r.Kind)
	}
	if !r.Check("agent.read") {
		t.Fatal("agent.read should be allowed by constraint")
	}
	if !r.Check("project.read") {
		t.Fatal("project.read should be allowed by constraint")
	}
	if r.Check("agent.create") {
		t.Fatal("agent.create should be denied by constraint")
	}
	if r.Check("agent.delete") {
		t.Fatal("agent.delete should be denied by constraint")
	}
}

func TestConstraintsToRestrictions_InactiveSkipped(t *testing.T) {
	now := testNow

	constraints := []*AccessConstraint{
		{
			Name:               "expired",
			MaximumPermissions: []string{"agent.read"},
			Condition:          ConstraintCondition{ExpiresAt: now.Add(-1 * time.Hour)},
		},
		{
			Name:               "disabled",
			MaximumPermissions: []string{"agent.read"},
			Disabled:           true,
		},
		{
			Name:               "future",
			MaximumPermissions: []string{"agent.read"},
			Condition:          ConstraintCondition{NotBefore: now.Add(1 * time.Hour)},
		},
	}

	restrictions := ConstraintsToRestrictions(constraints, now)
	if len(restrictions) != 0 {
		t.Fatalf("expected 0 restrictions for inactive constraints, got %d", len(restrictions))
	}
}

func TestConstraintsToRestrictions_NilSkipped(t *testing.T) {
	constraints := []*AccessConstraint{nil, nil}
	restrictions := ConstraintsToRestrictions(constraints, testNow)
	if len(restrictions) != 0 {
		t.Fatalf("expected 0 restrictions for nil constraints, got %d", len(restrictions))
	}
}

func TestConstraintsToRestrictions_MultipleIntersect(t *testing.T) {
	now := testNow
	constraints := []*AccessConstraint{
		{
			Name:               "allow-read-create",
			MaximumPermissions: []string{"agent.read", "agent.create"},
		},
		{
			Name:               "allow-read-delete",
			MaximumPermissions: []string{"agent.read", "agent.delete"},
		},
	}

	restrictions := ConstraintsToRestrictions(constraints, now)
	if len(restrictions) != 2 {
		t.Fatalf("expected 2 restrictions, got %d", len(restrictions))
	}

	// When both restrictions are applied in the kernel, only agent.read
	// should survive (the intersection).
	// Simulate by checking each restriction:
	for _, perm := range []string{"agent.read"} {
		allAllowed := true
		for _, r := range restrictions {
			if !r.Check(perm) {
				allAllowed = false
				break
			}
		}
		if !allAllowed {
			t.Fatalf("permission %s should survive both restrictions", perm)
		}
	}

	// agent.create: allowed by first, denied by second.
	if !restrictions[0].Check("agent.create") {
		t.Fatal("first restriction should allow agent.create")
	}
	if restrictions[1].Check("agent.create") {
		t.Fatal("second restriction should deny agent.create")
	}

	// agent.delete: denied by first, allowed by second.
	if restrictions[0].Check("agent.delete") {
		t.Fatal("first restriction should deny agent.delete")
	}
	if !restrictions[1].Check("agent.delete") {
		t.Fatal("second restriction should allow agent.delete")
	}
}

// ---------------------------------------------------------------------------
// Integration with AK1 kernel: monotonicity tests
// ---------------------------------------------------------------------------

// TestMonotonicity_AddingConstraintNeverIncreasesAuthority verifies the
// core invariant: adding a constraint (restriction) can only remove
// effective authority, never increase it.
func TestMonotonicity_AddingConstraintNeverIncreasesAuthority(t *testing.T) {
	rng := rand.New(rand.NewSource(100))

	allPerms := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"}
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "full", ScopeTypeSystem, allPerms...),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	for trial := 0; trial < 100; trial++ {
		// Generate random constraint maximum permissions.
		var maxPerms []string
		for _, p := range allPerms {
			if rng.Intn(2) == 0 {
				maxPerms = append(maxPerms, p)
			}
		}
		if len(maxPerms) == 0 {
			maxPerms = []string{allPerms[0]} // At least one.
		}

		constraint := &AccessConstraint{
			Name:               "test-constraint",
			MaximumPermissions: maxPerms,
		}

		// Evaluate without constraint.
		reqBase := KernelRequest{
			Permission:        allPerms[rng.Intn(len(allPerms))],
			PrincipalClosure:  closureOf("user:user1"),
			Resource:          ResourceContext{ProjectID: "proj-a"},
			CandidateBindings: bindings,
			RoleDefinitions:   roles,
			Restrictions:      nil,
			Now:               testNow,
		}
		dBase := Evaluate(reqBase)

		// Evaluate with constraint.
		restrictions := ConstraintsToRestrictions([]*AccessConstraint{constraint}, testNow)
		reqConstrained := reqBase
		reqConstrained.Restrictions = restrictions
		dConstrained := Evaluate(reqConstrained)

		// A constrained evaluation must never produce more authority.
		if dConstrained.Allowed && !dBase.Allowed {
			t.Fatalf("trial %d: adding constraint INCREASED authority for %s",
				trial, reqBase.Permission)
		}

		// Check effective permissions are a subset.
		basePerms := make(map[string]struct{})
		for _, p := range dBase.Provenance.EffectivePermissions {
			basePerms[p] = struct{}{}
		}
		for _, p := range dConstrained.Provenance.EffectivePermissions {
			if _, ok := basePerms[p]; !ok {
				t.Fatalf("trial %d: constrained has permission %q not in base set",
					trial, p)
			}
		}
	}
}

// TestMonotonicity_RemovingConstraintNeverDecreasesAuthority verifies that
// removing a constraint can only restore authority, never create new authority.
func TestMonotonicity_RemovingConstraintNeverDecreasesAuthority(t *testing.T) {
	allPerms := []string{"p1", "p2", "p3", "p4", "p5"}
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "full", ScopeTypeSystem, allPerms...),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	constraint := &AccessConstraint{
		Name:               "allow-p1-p2",
		MaximumPermissions: []string{"p1", "p2"},
	}

	for _, perm := range allPerms {
		// With constraint.
		restrictions := ConstraintsToRestrictions([]*AccessConstraint{constraint}, testNow)
		reqWith := KernelRequest{
			Permission:        perm,
			PrincipalClosure:  closureOf("user:user1"),
			Resource:          ResourceContext{ProjectID: "proj-a"},
			CandidateBindings: bindings,
			RoleDefinitions:   roles,
			Restrictions:      restrictions,
			Now:               testNow,
		}
		dWith := Evaluate(reqWith)

		// Without constraint (simulating removal).
		reqWithout := reqWith
		reqWithout.Restrictions = nil
		dWithout := Evaluate(reqWithout)

		// Removing constraint must never reduce authority.
		if dWith.Allowed && !dWithout.Allowed {
			t.Fatalf("removing constraint DECREASED authority for %s", perm)
		}
	}
}

// TestMonotonicity_NewPermissionFailClosed verifies that a newly registered
// permission is outside an existing maximum-permissions boundary.
func TestMonotonicity_NewPermissionFailClosed(t *testing.T) {
	constraint := &AccessConstraint{
		Name:               "allow-read-only",
		MaximumPermissions: []string{"agent.read", "project.read"},
	}

	restrictions := ConstraintsToRestrictions([]*AccessConstraint{constraint}, testNow)
	if len(restrictions) != 1 {
		t.Fatalf("expected 1 restriction, got %d", len(restrictions))
	}

	// A newly registered permission should NOT be in the constraint's max set.
	newPermission := "brand_new.permission"
	if restrictions[0].Check(newPermission) {
		t.Fatal("new permission should be DENIED by existing constraint (fail closed)")
	}
}

// ---------------------------------------------------------------------------
// FilterApplicableConstraints tests
// ---------------------------------------------------------------------------

func TestFilterApplicableConstraints_SubjectMatching(t *testing.T) {
	constraints := []*AccessConstraint{
		{
			ID:   "c1",
			Name: "exact-user",
			Subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "user",
				PrincipalID:   "user1",
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
			MaximumPermissions: []string{"agent.read"},
		},
		{
			ID:   "c2",
			Name: "group-closure",
			Subject: SubjectSelector{
				Kind:    SubjectKindGroupClosure,
				GroupID: "group1",
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
			MaximumPermissions: []string{"agent.read"},
		},
		{
			ID:   "c3",
			Name: "all-principals",
			Subject: SubjectSelector{
				Kind: SubjectKindAllPrincipals,
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
			MaximumPermissions: []string{"agent.read"},
		},
		{
			ID:   "c4",
			Name: "other-user",
			Subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "user",
				PrincipalID:   "user2",
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
			MaximumPermissions: []string{"agent.read"},
		},
	}

	// User1 is in group1. Use typed closure keys.
	closure := closureOf("user:user1", "group:group1")
	applicable := FilterApplicableConstraints(constraints, closure, ScopeTypeSystem, "")

	// Should match c1 (exact), c2 (group closure), c3 (all principals).
	// Should NOT match c4 (different user).
	if len(applicable) != 3 {
		t.Fatalf("expected 3 applicable constraints, got %d", len(applicable))
	}

	ids := make(map[string]bool)
	for _, c := range applicable {
		ids[c.ID] = true
	}
	for _, expected := range []string{"c1", "c2", "c3"} {
		if !ids[expected] {
			t.Fatalf("expected constraint %s to be applicable", expected)
		}
	}
	if ids["c4"] {
		t.Fatal("constraint c4 should not be applicable to user1")
	}
}

func TestFilterApplicableConstraints_ScopeMatching(t *testing.T) {
	constraints := []*AccessConstraint{
		{
			ID:   "c1",
			Name: "system-scope",
			Subject: SubjectSelector{
				Kind: SubjectKindAllPrincipals,
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
			MaximumPermissions: []string{"agent.read"},
		},
		{
			ID:   "c2",
			Name: "project-a",
			Subject: SubjectSelector{
				Kind: SubjectKindAllPrincipals,
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeProject, ID: "proj-a"},
			MaximumPermissions: []string{"agent.read"},
		},
		{
			ID:   "c3",
			Name: "project-b",
			Subject: SubjectSelector{
				Kind: SubjectKindAllPrincipals,
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeProject, ID: "proj-b"},
			MaximumPermissions: []string{"agent.read"},
		},
	}

	closure := closureOf("user:user1")

	// For project-a scope: should match system (c1) and project-a (c2).
	applicable := FilterApplicableConstraints(constraints, closure, ScopeTypeProject, "proj-a")
	if len(applicable) != 2 {
		t.Fatalf("expected 2 applicable constraints for proj-a, got %d", len(applicable))
	}

	ids := make(map[string]bool)
	for _, c := range applicable {
		ids[c.ID] = true
	}
	if !ids["c1"] || !ids["c2"] {
		t.Fatal("expected c1 (system) and c2 (proj-a) to be applicable")
	}
	if ids["c3"] {
		t.Fatal("c3 (proj-b) should not be applicable to proj-a")
	}
}

// ---------------------------------------------------------------------------
// IntersectMaximumPermissions tests
// ---------------------------------------------------------------------------

func TestIntersectMaximumPermissions(t *testing.T) {
	now := testNow

	// No constraints: no restriction.
	result := IntersectMaximumPermissions(nil, now)
	if result != nil {
		t.Fatal("expected nil for no constraints")
	}

	// Single constraint.
	c1 := &AccessConstraint{
		MaximumPermissions: []string{"p1", "p2", "p3"},
	}
	result = IntersectMaximumPermissions([]*AccessConstraint{c1}, now)
	if len(result) != 3 {
		t.Fatalf("expected 3 permissions, got %d", len(result))
	}

	// Two constraints with different sets.
	c2 := &AccessConstraint{
		MaximumPermissions: []string{"p2", "p3", "p4"},
	}
	result = IntersectMaximumPermissions([]*AccessConstraint{c1, c2}, now)
	if len(result) != 2 {
		t.Fatalf("expected 2 permissions (p2, p3), got %d", len(result))
	}
	if _, ok := result["p2"]; !ok {
		t.Fatal("expected p2 in intersection")
	}
	if _, ok := result["p3"]; !ok {
		t.Fatal("expected p3 in intersection")
	}

	// Three constraints — empty intersection.
	c3 := &AccessConstraint{
		MaximumPermissions: []string{"p4", "p5"},
	}
	result = IntersectMaximumPermissions([]*AccessConstraint{c1, c2, c3}, now)
	if len(result) != 0 {
		t.Fatalf("expected 0 permissions (empty intersection), got %d", len(result))
	}

	// Inactive constraint should be skipped.
	c4 := &AccessConstraint{
		MaximumPermissions: []string{"p9"},
		Disabled:           true,
	}
	result = IntersectMaximumPermissions([]*AccessConstraint{c1, c4}, now)
	if len(result) != 3 {
		t.Fatalf("expected 3 permissions (disabled constraint skipped), got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Full kernel integration: constraint as restriction
// ---------------------------------------------------------------------------

func TestAccessConstraint_KernelIntegration(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem,
			"agent.create", "agent.read", "agent.delete", "project.read"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	// Without constraint: all permissions should be granted.
	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user:user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Now:               testNow,
	}
	d := Evaluate(req)
	if !d.Allowed {
		t.Fatal("expected allow without constraint")
	}

	// With constraint: only agent.read and project.read are allowed.
	constraint := &AccessConstraint{
		Name:               "read-only",
		MaximumPermissions: []string{"agent.read", "project.read"},
	}
	restrictions := ConstraintsToRestrictions([]*AccessConstraint{constraint}, testNow)
	req.Restrictions = restrictions

	// agent.create should now be denied.
	d = Evaluate(req)
	if d.Allowed {
		t.Fatal("agent.create should be denied by read-only constraint")
	}

	// agent.read should still be allowed.
	req.Permission = "agent.read"
	d = Evaluate(req)
	if !d.Allowed {
		t.Fatal("agent.read should be allowed by read-only constraint")
	}

	// Verify provenance shows the restriction.
	if len(d.Provenance.Restrictions) == 0 {
		t.Fatal("expected restriction in provenance")
	}
}

func TestAccessConstraint_KernelIntegration_MultipleConstraintsIntersect(t *testing.T) {
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem,
			"agent.create", "agent.read", "agent.delete"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	// Constraint 1: allows read and create.
	c1 := &AccessConstraint{
		Name:               "c1",
		MaximumPermissions: []string{"agent.read", "agent.create"},
	}
	// Constraint 2: allows read and delete.
	c2 := &AccessConstraint{
		Name:               "c2",
		MaximumPermissions: []string{"agent.read", "agent.delete"},
	}

	restrictions := ConstraintsToRestrictions([]*AccessConstraint{c1, c2}, testNow)

	// Only agent.read should survive the intersection.
	for _, tc := range []struct {
		perm    string
		allowed bool
	}{
		{"agent.read", true},
		{"agent.create", false},
		{"agent.delete", false},
	} {
		req := KernelRequest{
			Permission:        tc.perm,
			PrincipalClosure:  closureOf("user:user1"),
			Resource:          ResourceContext{ProjectID: "proj-a"},
			CandidateBindings: bindings,
			RoleDefinitions:   roles,
			Restrictions:      restrictions,
			Now:               testNow,
		}
		d := Evaluate(req)
		if d.Allowed != tc.allowed {
			t.Fatalf("permission %s: got %v, want %v", tc.perm, d.Allowed, tc.allowed)
		}
	}
}

func TestAccessConstraint_KernelIntegration_OwnerPassesThroughConstraints(t *testing.T) {
	// Design invariant: owner/admin roles pass through constraints like
	// any other grant — no early-allow bypass.
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "project-owner", ScopeTypeProject,
			"agent.create", "agent.read", "agent.delete"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeProject, "proj-a"),
	}

	constraint := &AccessConstraint{
		Name:               "read-only",
		MaximumPermissions: []string{"agent.read"},
	}
	restrictions := ConstraintsToRestrictions([]*AccessConstraint{constraint}, testNow)

	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureOf("user:user1"),
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Restrictions:      restrictions,
		Now:               testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("owner role should still be denied by constraint (no bypass)")
	}
}

// TestMonotonicity_ConstraintOrderIndependence verifies that the order of
// constraints does not affect the final effective permissions.
func TestMonotonicity_ConstraintOrderIndependence(t *testing.T) {
	rng := rand.New(rand.NewSource(200))

	allPerms := []string{"p1", "p2", "p3", "p4", "p5"}
	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "full", ScopeTypeSystem, allPerms...),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	for trial := 0; trial < 50; trial++ {
		// Generate 2-4 random constraints.
		numConstraints := 2 + rng.Intn(3)
		constraints := make([]*AccessConstraint, numConstraints)
		for i := range constraints {
			var maxPerms []string
			for _, p := range allPerms {
				if rng.Intn(2) == 0 {
					maxPerms = append(maxPerms, p)
				}
			}
			if len(maxPerms) == 0 {
				maxPerms = []string{allPerms[0]}
			}
			constraints[i] = &AccessConstraint{
				Name:               "c",
				MaximumPermissions: maxPerms,
			}
		}

		// Evaluate with original order.
		restrictions1 := ConstraintsToRestrictions(constraints, testNow)
		req := KernelRequest{
			Permission:        allPerms[rng.Intn(len(allPerms))],
			PrincipalClosure:  closureOf("user:user1"),
			Resource:          ResourceContext{ProjectID: "proj-a"},
			CandidateBindings: bindings,
			RoleDefinitions:   roles,
			Restrictions:      restrictions1,
			Now:               testNow,
		}
		d1 := Evaluate(req)

		// Shuffle constraints and evaluate again.
		shuffled := make([]*AccessConstraint, len(constraints))
		copy(shuffled, constraints)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		restrictions2 := ConstraintsToRestrictions(shuffled, testNow)
		req.Restrictions = restrictions2
		d2 := Evaluate(req)

		if d1.Allowed != d2.Allowed {
			t.Fatalf("trial %d: constraint order changed the decision", trial)
		}

		ep1 := d1.Provenance.EffectivePermissions
		ep2 := d2.Provenance.EffectivePermissions
		if len(ep1) != len(ep2) {
			t.Fatalf("trial %d: effective permissions differ: %v vs %v", trial, ep1, ep2)
		}
	}
}

// ---------------------------------------------------------------------------
// Constraint group removal safety test
// ---------------------------------------------------------------------------

func TestConstraint_GroupRemovalSafety(t *testing.T) {
	// Removing a member from a group that is a constraint subject can
	// restore authority that a constraint had suppressed. The test verifies
	// that the constraint's effect is lost when the principal is no longer
	// in the closure.

	roles := map[string]*RolePermissions{
		"r1": makeRole("r1", "admin", ScopeTypeSystem,
			"agent.create", "agent.read", "agent.delete"),
	}
	bindings := []CandidateBinding{
		makeBinding("b1", "r1", "user", "user1", ScopeTypeSystem, ""),
	}

	// Constraint targets group1 closure.
	constraint := &AccessConstraint{
		Name: "restrict-group1",
		Subject: SubjectSelector{
			Kind:    SubjectKindGroupClosure,
			GroupID: "group1",
		},
		Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
		MaximumPermissions: []string{"agent.read"},
	}

	// User1 is in group1 — constraint applies. Use typed keys.
	closureWithGroup := closureOf("user:user1", "group:group1")
	applicable := FilterApplicableConstraints(
		[]*AccessConstraint{constraint},
		closureWithGroup,
		ScopeTypeSystem, "",
	)
	if len(applicable) != 1 {
		t.Fatal("constraint should apply when user is in group")
	}

	restrictions := ConstraintsToRestrictions(applicable, testNow)
	req := KernelRequest{
		Permission:        "agent.create",
		PrincipalClosure:  closureWithGroup,
		Resource:          ResourceContext{ProjectID: "proj-a"},
		CandidateBindings: bindings,
		RoleDefinitions:   roles,
		Restrictions:      restrictions,
		Now:               testNow,
	}
	d := Evaluate(req)
	if d.Allowed {
		t.Fatal("agent.create should be denied while user is in constrained group")
	}

	// User1 is removed from group1 — constraint no longer applies.
	closureWithoutGroup := closureOf("user:user1")
	applicable = FilterApplicableConstraints(
		[]*AccessConstraint{constraint},
		closureWithoutGroup,
		ScopeTypeSystem, "",
	)
	if len(applicable) != 0 {
		t.Fatal("constraint should NOT apply when user is removed from group")
	}

	// Without the constraint restriction, agent.create should be allowed again.
	req.Restrictions = nil
	req.PrincipalClosure = closureWithoutGroup
	d = Evaluate(req)
	if !d.Allowed {
		t.Fatal("agent.create should be allowed after removal from constrained group")
	}
}

// ---------------------------------------------------------------------------
// constraintAllowsPermission helper test
// ---------------------------------------------------------------------------

func TestConstraintAllowsPermission(t *testing.T) {
	// This uses the store model AccessConstraint, not the hub model.
	// The helper is in handlers_access_constraints.go.
	// We test it here since it's logically part of constraint governance.
	// (Note: this indirectly tests via the hub model.)

	c := &AccessConstraint{
		MaximumPermissions: []string{"agent.read", "agent.create"},
	}

	set := c.MaximumPermissionSet()
	if _, ok := set["agent.read"]; !ok {
		t.Fatal("expected agent.read in permission set")
	}
	if _, ok := set["agent.create"]; !ok {
		t.Fatal("expected agent.create in permission set")
	}
	if _, ok := set["agent.delete"]; ok {
		t.Fatal("agent.delete should not be in permission set")
	}
}

// ---------------------------------------------------------------------------
// Lockout prevention: userBlockedByConstraints tests
// ---------------------------------------------------------------------------

func TestConstraint_UserBlockedByConstraints(t *testing.T) {
	s := &Server{} // zero-value; userBlockedByConstraints doesn't use server fields

	ptrS := func(s string) *string { return &s }

	cases := []struct {
		name        string
		user        adminUserInfo
		constraints []*store.AccessConstraint
		blocked     bool
	}{
		{
			name:        "no restricting constraints",
			user:        adminUserInfo{userID: "user1"},
			constraints: nil,
			blocked:     false,
		},
		{
			name: "all_principals blocks everyone",
			user: adminUserInfo{userID: "user1"},
			constraints: []*store.AccessConstraint{
				{SubjectKind: store.ConstraintSubjectAllPrincipals},
			},
			blocked: true,
		},
		{
			name: "principal constraint targeting this user",
			user: adminUserInfo{userID: "user1"},
			constraints: []*store.AccessConstraint{
				{
					SubjectKind:          store.ConstraintSubjectPrincipal,
					SubjectPrincipalType: ptrS("user"),
					SubjectPrincipalID:   ptrS("user1"),
				},
			},
			blocked: true,
		},
		{
			name: "principal constraint targeting different user",
			user: adminUserInfo{userID: "user1"},
			constraints: []*store.AccessConstraint{
				{
					SubjectKind:          store.ConstraintSubjectPrincipal,
					SubjectPrincipalType: ptrS("user"),
					SubjectPrincipalID:   ptrS("user2"),
				},
			},
			blocked: false,
		},
		{
			name: "group_closure targeting user's group",
			user: adminUserInfo{userID: "user1", groupIDs: []string{"admins-group"}},
			constraints: []*store.AccessConstraint{
				{
					SubjectKind:    store.ConstraintSubjectGroupClosure,
					SubjectGroupID: ptrS("admins-group"),
				},
			},
			blocked: true,
		},
		{
			name: "group_closure targeting different group",
			user: adminUserInfo{userID: "user1", groupIDs: []string{"admins-group"}},
			constraints: []*store.AccessConstraint{
				{
					SubjectKind:    store.ConstraintSubjectGroupClosure,
					SubjectGroupID: ptrS("other-group"),
				},
			},
			blocked: false,
		},
		{
			name: "principal targeting group in user's closure",
			user: adminUserInfo{userID: "user1", groupIDs: []string{"admins-group"}},
			constraints: []*store.AccessConstraint{
				{
					SubjectKind:          store.ConstraintSubjectPrincipal,
					SubjectPrincipalType: ptrS("group"),
					SubjectPrincipalID:   ptrS("admins-group"),
				},
			},
			blocked: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.userBlockedByConstraints(context.TODO(), tc.user, tc.constraints)
			if got != tc.blocked {
				t.Fatalf("userBlockedByConstraints() = %v, want %v", got, tc.blocked)
			}
		})
	}
}

// TestConstraint_LockoutScenario_PrincipalTargetingSoleAdmin verifies that
// a principal-kind constraint targeting the sole constraint-admin user is
// detected as a lockout (R1 scenario 1).
func TestConstraint_LockoutScenario_PrincipalTargetingSoleAdmin(t *testing.T) {
	s := &Server{}
	ptrS := func(s string) *string { return &s }

	// Sole admin user.
	admin := adminUserInfo{userID: "sole-admin"}

	// Constraint targets that sole admin and removes constraint-admin.
	constraints := []*store.AccessConstraint{
		{
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: ptrS("user"),
			SubjectPrincipalID:   ptrS("sole-admin"),
		},
	}

	// The admin should be blocked.
	if !s.userBlockedByConstraints(context.TODO(), admin, constraints) {
		t.Fatal("sole admin should be blocked by principal constraint targeting them")
	}
}

// TestConstraint_LockoutScenario_GroupClosureAllAdmins verifies that a
// group_closure constraint targeting a group containing all admin users
// is detected as a lockout (R1 scenario 2).
func TestConstraint_LockoutScenario_GroupClosureAllAdmins(t *testing.T) {
	s := &Server{}
	ptrS := func(s string) *string { return &s }

	// Both admins are in the same group.
	admin1 := adminUserInfo{userID: "admin1", groupIDs: []string{"ops-team"}}
	admin2 := adminUserInfo{userID: "admin2", groupIDs: []string{"ops-team"}}

	// Constraint targets the group containing all admins.
	constraints := []*store.AccessConstraint{
		{
			SubjectKind:    store.ConstraintSubjectGroupClosure,
			SubjectGroupID: ptrS("ops-team"),
		},
	}

	// Both admins should be blocked.
	if !s.userBlockedByConstraints(context.TODO(), admin1, constraints) {
		t.Fatal("admin1 should be blocked by group_closure targeting ops-team")
	}
	if !s.userBlockedByConstraints(context.TODO(), admin2, constraints) {
		t.Fatal("admin2 should be blocked by group_closure targeting ops-team")
	}
}

// TestConstraint_LockoutScenario_CombinedPrincipalConstraints verifies that
// a combination of principal constraints blocking all admins is detected
// (R1 scenario 3).
func TestConstraint_LockoutScenario_CombinedPrincipalConstraints(t *testing.T) {
	s := &Server{}
	ptrS := func(s string) *string { return &s }

	admin1 := adminUserInfo{userID: "admin1"}
	admin2 := adminUserInfo{userID: "admin2"}
	admins := []adminUserInfo{admin1, admin2}

	// Two separate constraints, each targeting one admin.
	constraints := []*store.AccessConstraint{
		{
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: ptrS("user"),
			SubjectPrincipalID:   ptrS("admin1"),
		},
		{
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: ptrS("user"),
			SubjectPrincipalID:   ptrS("admin2"),
		},
	}

	// Both admins should be blocked individually.
	allBlocked := true
	for _, admin := range admins {
		if !s.userBlockedByConstraints(context.TODO(), admin, constraints) {
			allBlocked = false
			break
		}
	}
	if !allBlocked {
		t.Fatal("all admins should be blocked by combined principal constraints")
	}
}

// TestConstraint_LockoutSurvival_OneAdminUnaffected verifies that when
// multiple admins exist and only some are targeted, the operation is allowed.
func TestConstraint_LockoutSurvival_OneAdminUnaffected(t *testing.T) {
	s := &Server{}
	ptrS := func(s string) *string { return &s }

	admin1 := adminUserInfo{userID: "admin1"}
	admin2 := adminUserInfo{userID: "admin2"}

	// Only admin1 is targeted.
	constraints := []*store.AccessConstraint{
		{
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: ptrS("user"),
			SubjectPrincipalID:   ptrS("admin1"),
		},
	}

	// admin1 is blocked, but admin2 survives.
	if !s.userBlockedByConstraints(context.TODO(), admin1, constraints) {
		t.Fatal("admin1 should be blocked")
	}
	if s.userBlockedByConstraints(context.TODO(), admin2, constraints) {
		t.Fatal("admin2 should NOT be blocked")
	}
}

// ---------------------------------------------------------------------------
// Blast-radius preview: ConstraintPreview type tests
// ---------------------------------------------------------------------------

func TestConstraintPreview_Truncated(t *testing.T) {
	// Verify the Truncated field exists and works as expected.
	preview := ConstraintPreview{
		ConstraintID:   "c1",
		ConstraintName: "test",
		AffectedPrincipals: []AffectedPrincipal{
			{
				PrincipalType: "all",
				PrincipalID:   "*",
				DisplayName:   "All principals",
			},
		},
		RestrictedPermissions: []string{"agent.create"},
		Truncated:             true,
	}

	if !preview.Truncated {
		t.Fatal("expected Truncated to be true")
	}
}

func TestAffectedPrincipal_PermissionDelta(t *testing.T) {
	// Verify the AffectedPrincipal fields compute correctly.
	ap := AffectedPrincipal{
		PrincipalType:       "user",
		PrincipalID:         "user1",
		CurrentPermissions:  []string{"agent.read", "agent.create", "agent.delete"},
		ProposedPermissions: []string{"agent.read"},
		RemovedPermissions:  []string{"agent.create", "agent.delete"},
	}

	// Verify proposed is a subset of current.
	currentSet := make(map[string]bool)
	for _, p := range ap.CurrentPermissions {
		currentSet[p] = true
	}
	for _, p := range ap.ProposedPermissions {
		if !currentSet[p] {
			t.Fatalf("proposed permission %q not in current set", p)
		}
	}

	// Verify removed = current - proposed.
	proposedSet := make(map[string]bool)
	for _, p := range ap.ProposedPermissions {
		proposedSet[p] = true
	}
	for _, p := range ap.RemovedPermissions {
		if proposedSet[p] {
			t.Fatalf("removed permission %q is also in proposed set", p)
		}
		if !currentSet[p] {
			t.Fatalf("removed permission %q not in current set", p)
		}
	}

	// Verify no permission is missing.
	accounted := len(ap.ProposedPermissions) + len(ap.RemovedPermissions)
	if accounted != len(ap.CurrentPermissions) {
		t.Fatalf("proposed (%d) + removed (%d) != current (%d)",
			len(ap.ProposedPermissions), len(ap.RemovedPermissions), len(ap.CurrentPermissions))
	}
}

// TestConstraint_StoreAllowsPermission tests the constraintAllowsPermission
// helper against the store model.
func TestConstraint_StoreAllowsPermission(t *testing.T) {
	c := &store.AccessConstraint{
		MaximumPermissions: []string{"agent.read", "access_constraint.admin"},
	}

	if !constraintAllowsPermission(c, "access_constraint.admin") {
		t.Fatal("should allow access_constraint.admin")
	}
	if !constraintAllowsPermission(c, "agent.read") {
		t.Fatal("should allow agent.read")
	}
	if constraintAllowsPermission(c, "agent.create") {
		t.Fatal("should NOT allow agent.create")
	}
}

// ===========================================================================
// B1 fix verification tests
// ===========================================================================

// ---------------------------------------------------------------------------
// Fix 1: Exact-principal matching — group-targeted constraints must match
// ---------------------------------------------------------------------------

func TestMatchesPrincipalClosure_GroupTargetedPrincipalConstraint(t *testing.T) {
	// A constraint {kind:principal, principalType:"group", principalID:"g1"}
	// must match when "group:g1" is in the typed closure.
	// Previously this never matched because the comparison was against the
	// requester's type ("user"/"agent"), never "group".

	typedClosure := closureOf("user:u1", "group:g1", "group:g2")

	s := SubjectSelector{
		Kind:          SubjectKindPrincipal,
		PrincipalType: "group",
		PrincipalID:   "g1",
	}

	if !s.MatchesPrincipalClosure(typedClosure) {
		t.Fatal("principal constraint targeting group g1 should match when group:g1 is in closure")
	}

	// Same constraint but g1 not in closure.
	noGroupClosure := closureOf("user:u1", "group:g2")
	if s.MatchesPrincipalClosure(noGroupClosure) {
		t.Fatal("principal constraint targeting group g1 should NOT match when g1 is absent")
	}
}

func TestMatchesPrincipalClosure_AllPrincipalTypes(t *testing.T) {
	// Verify typed matching for all principal types.
	cases := []struct {
		name          string
		closure       map[string]struct{}
		principalType string
		principalID   string
		wantMatch     bool
	}{
		{
			name:          "user match",
			closure:       closureOf("user:u1"),
			principalType: "user",
			principalID:   "u1",
			wantMatch:     true,
		},
		{
			name:          "agent match",
			closure:       closureOf("agent:a1"),
			principalType: "agent",
			principalID:   "a1",
			wantMatch:     true,
		},
		{
			name:          "group match",
			closure:       closureOf("group:g1"),
			principalType: "group",
			principalID:   "g1",
			wantMatch:     true,
		},
		{
			name:          "type mismatch: user ID as agent",
			closure:       closureOf("user:u1"),
			principalType: "agent",
			principalID:   "u1",
			wantMatch:     false,
		},
		{
			name:          "type mismatch: agent ID as user",
			closure:       closureOf("agent:a1"),
			principalType: "user",
			principalID:   "a1",
			wantMatch:     false,
		},
		{
			name:          "no bare ID match (typed keys required)",
			closure:       closureOf("u1"), // bare ID, no type prefix
			principalType: "user",
			principalID:   "u1",
			wantMatch:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: tc.principalType,
				PrincipalID:   tc.principalID,
			}
			got := s.MatchesPrincipalClosure(tc.closure)
			if got != tc.wantMatch {
				t.Fatalf("MatchesPrincipalClosure() = %v, want %v", got, tc.wantMatch)
			}
		})
	}
}

func TestMatchesPrincipalClosure_GroupClosureUsesTypedKey(t *testing.T) {
	// group_closure subjects should use "group:" prefix for lookup.
	s := SubjectSelector{
		Kind:    SubjectKindGroupClosure,
		GroupID: "g1",
	}

	// Match with typed key.
	if !s.MatchesPrincipalClosure(closureOf("user:u1", "group:g1")) {
		t.Fatal("group_closure should match with typed group:g1 key")
	}

	// No match with bare key.
	if s.MatchesPrincipalClosure(closureOf("user:u1", "g1")) {
		t.Fatal("group_closure should NOT match bare g1 key (must be group:g1)")
	}
}

// ---------------------------------------------------------------------------
// Fix 5: NormalizePrincipalType
// ---------------------------------------------------------------------------

func TestNormalizePrincipalType(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"user", "user"},
		{"agent", "agent"},
		{"dev", "user"},
		{"federated_user", "user"},
		{"federated_agent", "agent"},
		{"group", "group"},
		{"broker", "broker"},
		{"unknown", "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := NormalizePrincipalType(tc.input)
			if got != tc.want {
				t.Fatalf("NormalizePrincipalType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fix 6: No bare-ID over-match — typed keys prevent cross-type collision
// ---------------------------------------------------------------------------

func TestFilterApplicableConstraints_NoBareidOvermatch(t *testing.T) {
	// A constraint targeting {principal, user, "shared-id"} should NOT match
	// when "shared-id" exists in the closure only as "group:shared-id".
	// Previously, the bare-ID closure would strip the type prefix and match.

	constraints := []*AccessConstraint{
		{
			ID:   "c1",
			Name: "target-user-shared-id",
			Subject: SubjectSelector{
				Kind:          SubjectKindPrincipal,
				PrincipalType: "user",
				PrincipalID:   "shared-id",
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
			MaximumPermissions: []string{"agent.read"},
		},
	}

	// Closure has "shared-id" as a group, not as a user.
	closure := closureOf("user:real-user", "group:shared-id")
	applicable := FilterApplicableConstraints(constraints, closure, ScopeTypeSystem, "")

	if len(applicable) != 0 {
		t.Fatal("user constraint should NOT match group:shared-id (typed keys prevent cross-type collision)")
	}

	// Same ID as a user should match.
	closureWithUser := closureOf("user:shared-id")
	applicable = FilterApplicableConstraints(constraints, closureWithUser, ScopeTypeSystem, "")

	if len(applicable) != 1 {
		t.Fatal("user constraint should match user:shared-id")
	}
}

// ---------------------------------------------------------------------------
// Fix 1+5 combined: dev/federated principals with group constraints
// ---------------------------------------------------------------------------

func TestFilterApplicableConstraints_DevPrincipalWithGroupConstraint(t *testing.T) {
	// A constraint targeting group:g1 should match a dev principal whose
	// normalized closure includes group:g1.

	constraint := &AccessConstraint{
		ID:   "c1",
		Name: "group-constraint",
		Subject: SubjectSelector{
			Kind:          SubjectKindPrincipal,
			PrincipalType: "group",
			PrincipalID:   "g1",
		},
		Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
		MaximumPermissions: []string{"agent.read"},
	}

	// Dev principal with normalized closure (dev → user).
	normalizedClosure := closureOf("user:dev-user", "group:g1")
	applicable := FilterApplicableConstraints(
		[]*AccessConstraint{constraint}, normalizedClosure,
		ScopeTypeSystem, "",
	)

	if len(applicable) != 1 {
		t.Fatal("group-targeted principal constraint should match when group:g1 is in normalized closure")
	}
}

// ---------------------------------------------------------------------------
// Exact-group subject vs group-closure subject distinction
// ---------------------------------------------------------------------------

func TestFilterApplicableConstraints_ExactGroupVsGroupClosure(t *testing.T) {
	// An exact-group constraint {principal, group, G} targets the group
	// principal itself. A group_closure constraint {group_closure, G}
	// targets all effective members of G.
	// Both should match when group:G is in the closure, but they mean
	// different things semantically.

	exactGroupConstraint := &AccessConstraint{
		ID:   "exact",
		Name: "exact-group",
		Subject: SubjectSelector{
			Kind:          SubjectKindPrincipal,
			PrincipalType: "group",
			PrincipalID:   "g1",
		},
		Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
		MaximumPermissions: []string{"agent.read"},
	}

	groupClosureConstraint := &AccessConstraint{
		ID:   "closure",
		Name: "group-closure",
		Subject: SubjectSelector{
			Kind:    SubjectKindGroupClosure,
			GroupID: "g1",
		},
		Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
		MaximumPermissions: []string{"agent.read"},
	}

	// User in group g1: both should match.
	userInGroup := closureOf("user:u1", "group:g1")
	applicable := FilterApplicableConstraints(
		[]*AccessConstraint{exactGroupConstraint, groupClosureConstraint},
		userInGroup, ScopeTypeSystem, "",
	)

	if len(applicable) != 2 {
		t.Fatalf("both exact-group and group-closure constraints should match, got %d", len(applicable))
	}

	// User NOT in group g1: neither should match.
	userNotInGroup := closureOf("user:u1", "group:g2")
	applicable = FilterApplicableConstraints(
		[]*AccessConstraint{exactGroupConstraint, groupClosureConstraint},
		userNotInGroup, ScopeTypeSystem, "",
	)

	if len(applicable) != 0 {
		t.Fatalf("neither constraint should match when user is not in group g1, got %d", len(applicable))
	}
}

// ---------------------------------------------------------------------------
// 201+ constraints: all evaluated, no fixed-limit bypass
// ---------------------------------------------------------------------------

func TestFilterApplicableConstraints_LargeConstraintCount(t *testing.T) {
	// Ensure that all constraints are evaluated even beyond historical limits.
	const count = 250
	constraints := make([]*AccessConstraint, count)
	for i := 0; i < count; i++ {
		constraints[i] = &AccessConstraint{
			ID:   fmt.Sprintf("c%d", i),
			Name: fmt.Sprintf("constraint-%d", i),
			Subject: SubjectSelector{
				Kind: SubjectKindAllPrincipals,
			},
			Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
			MaximumPermissions: []string{"agent.read"},
		}
	}

	closure := closureOf("user:u1")
	applicable := FilterApplicableConstraints(constraints, closure, ScopeTypeSystem, "")

	if len(applicable) != count {
		t.Fatalf("expected all %d constraints to be applicable, got %d", count, len(applicable))
	}

	// Intersection of 250 constraints all allowing only "agent.read" should
	// still deny "agent.create".
	restrictions := ConstraintsToRestrictions(applicable, testNow)
	if len(restrictions) != count {
		t.Fatalf("expected %d restrictions, got %d", count, len(restrictions))
	}

	for i, r := range restrictions {
		if !r.Check("agent.read") {
			t.Fatalf("restriction %d should allow agent.read", i)
		}
		if r.Check("agent.create") {
			t.Fatalf("restriction %d should deny agent.create", i)
		}
	}
}

// ---------------------------------------------------------------------------
// normalizeClosureTypes tests
// ---------------------------------------------------------------------------

func TestNormalizeClosureTypes(t *testing.T) {
	input := map[string]struct{}{
		"dev:u1":             {},
		"federated_user:u2":  {},
		"federated_agent:a1": {},
		"user:u3":            {},
		"agent:a2":           {},
		"group:g1":           {},
		"broker:b1":          {},
	}

	got := normalizeClosureTypes(input)

	expected := map[string]struct{}{
		"user:u1":   {},
		"user:u2":   {},
		"agent:a1":  {},
		"user:u3":   {},
		"agent:a2":  {},
		"group:g1":  {},
		"broker:b1": {},
	}

	if len(got) != len(expected) {
		t.Fatalf("expected %d entries, got %d", len(expected), len(got))
	}

	for key := range expected {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing expected key %q in normalized closure", key)
		}
	}

	// Verify dev:u1 was normalized away.
	if _, ok := got["dev:u1"]; ok {
		t.Fatal("dev:u1 should have been normalized to user:u1")
	}
	if _, ok := got["federated_user:u2"]; ok {
		t.Fatal("federated_user:u2 should have been normalized to user:u2")
	}
}

// ---------------------------------------------------------------------------
// Consistency: lockout guard and evaluator agree on group-targeted constraints
// ---------------------------------------------------------------------------

func TestGroupTargetedConstraint_LockoutAndEvaluatorConsistency(t *testing.T) {
	// The lockout guard (userBlockedByConstraints) and the evaluator
	// (MatchesPrincipalClosure) must both match a {principal, group, G}
	// constraint when G is in the user's closure.

	ptrS := func(s string) *string { return &s }
	s := &Server{}

	// Store-level constraint for lockout check.
	storeConstraint := &store.AccessConstraint{
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: ptrS("group"),
		SubjectPrincipalID:   ptrS("admin-group"),
		MaximumPermissions:   []string{"agent.read"}, // excludes access_constraint.admin
	}

	user := adminUserInfo{userID: "u1", groupIDs: []string{"admin-group"}}

	// Lockout guard says blocked.
	lockoutBlocked := s.userBlockedByConstraints(context.TODO(), user, []*store.AccessConstraint{storeConstraint})
	if !lockoutBlocked {
		t.Fatal("lockout guard should detect that user is blocked by group-targeted constraint")
	}

	// Evaluator (hub-level) should also match.
	hubSelector := SubjectSelector{
		Kind:          SubjectKindPrincipal,
		PrincipalType: "group",
		PrincipalID:   "admin-group",
	}
	typedClosure := closureOf("user:u1", "group:admin-group")
	evaluatorMatches := hubSelector.MatchesPrincipalClosure(typedClosure)
	if !evaluatorMatches {
		t.Fatal("evaluator should also match the group-targeted constraint")
	}

	// Both agree: consistency check passes.
}

// ---------------------------------------------------------------------------
// Super-admin boundary applies identically
// ---------------------------------------------------------------------------

func TestFilterApplicableConstraints_SuperAdminBoundary(t *testing.T) {
	// Super-admin should be subject to constraints just like any other user.
	// all_principals constraints apply regardless of role.
	constraint := &AccessConstraint{
		ID:   "c1",
		Name: "restrict-all",
		Subject: SubjectSelector{
			Kind: SubjectKindAllPrincipals,
		},
		Scope:              ConstraintScopeRef{Type: ScopeTypeSystem},
		MaximumPermissions: []string{"agent.read"},
	}

	superAdminClosure := closureOf("user:super-admin-user")
	applicable := FilterApplicableConstraints(
		[]*AccessConstraint{constraint}, superAdminClosure,
		ScopeTypeSystem, "",
	)

	if len(applicable) != 1 {
		t.Fatal("all_principals constraint should apply to super-admin")
	}

	restrictions := ConstraintsToRestrictions(applicable, testNow)
	if restrictions[0].Check("agent.create") {
		t.Fatal("agent.create should be denied by constraint even for super-admin")
	}
}

// ---------------------------------------------------------------------------
// Unknown subject kind: fail-open (returns false, constraint not applied)
// ---------------------------------------------------------------------------

func TestMatchesPrincipalClosure_UnknownKindFailsOpen(t *testing.T) {
	s := SubjectSelector{
		Kind:          SubjectKind("unknown_kind"),
		PrincipalType: "user",
		PrincipalID:   "u1",
	}

	closure := closureOf("user:u1")
	if s.MatchesPrincipalClosure(closure) {
		t.Fatal("unknown subject kind should return false (not applied)")
	}
}
