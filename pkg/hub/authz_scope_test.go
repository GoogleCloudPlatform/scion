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
)

func TestScopeSetAll(t *testing.T) {
	s := ScopeSetAll()
	if !s.IsAll() {
		t.Fatal("expected IsAll")
	}
	if s.IsNone() {
		t.Fatal("All should not be None")
	}
	if !s.Contains("any-project") {
		t.Fatal("All should contain any project")
	}
}

func TestScopeSetNone(t *testing.T) {
	s := ScopeSetNone()
	if !s.IsNone() {
		t.Fatal("expected IsNone")
	}
	if s.IsAll() {
		t.Fatal("None should not be All")
	}
	if s.Contains("any-project") {
		t.Fatal("None should not contain any project")
	}
}

func TestScopeSetExplicit(t *testing.T) {
	s := ScopeSetExplicit("p1", "p2", "p3")
	if s.IsAll() {
		t.Fatal("Explicit should not be All")
	}
	if s.IsNone() {
		t.Fatal("Explicit with items should not be None")
	}
	if !s.Contains("p1") {
		t.Fatal("should contain p1")
	}
	if !s.Contains("p2") {
		t.Fatal("should contain p2")
	}
	if s.Contains("p4") {
		t.Fatal("should not contain p4")
	}
}

func TestScopeSetExplicitEmpty(t *testing.T) {
	s := ScopeSetExplicit()
	if !s.IsNone() {
		t.Fatal("empty explicit should be None")
	}
}

func TestScopeSetProjectIDs(t *testing.T) {
	s := ScopeSetExplicit("p3", "p1", "p2")
	ids := s.ProjectIDs()
	want := []string{"p1", "p2", "p3"}
	if len(ids) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i, id := range ids {
		if id != want[i] {
			t.Fatalf("index %d: got %q, want %q", i, id, want[i])
		}
	}

	// All and None return nil.
	if ScopeSetAll().ProjectIDs() != nil {
		t.Fatal("All.ProjectIDs should be nil")
	}
	if ScopeSetNone().ProjectIDs() != nil {
		t.Fatal("None.ProjectIDs should be nil")
	}
}

// --- Union tests ---

func TestUnion_AllWithX(t *testing.T) {
	cases := []struct {
		name string
		x    ScopeSet
	}{
		{"All", ScopeSetAll()},
		{"None", ScopeSetNone()},
		{"Explicit", ScopeSetExplicit("p1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ScopeSetAll().Union(tc.x)
			if !r.IsAll() {
				t.Fatal("All.Union(X) should be All")
			}
			r = tc.x.Union(ScopeSetAll())
			if !r.IsAll() {
				t.Fatal("X.Union(All) should be All")
			}
		})
	}
}

func TestUnion_NoneWithX(t *testing.T) {
	x := ScopeSetExplicit("p1", "p2")
	r := ScopeSetNone().Union(x)
	if !r.Equal(x) {
		t.Fatalf("None.Union(X) should be X, got %v", r)
	}
	r = x.Union(ScopeSetNone())
	if !r.Equal(x) {
		t.Fatalf("X.Union(None) should be X, got %v", r)
	}
}

func TestUnion_ExplicitCombines(t *testing.T) {
	a := ScopeSetExplicit("p1", "p2")
	b := ScopeSetExplicit("p2", "p3")
	r := a.Union(b)
	want := ScopeSetExplicit("p1", "p2", "p3")
	if !r.Equal(want) {
		t.Fatalf("got %v, want %v", r, want)
	}
}

// --- Intersection tests ---

func TestIntersection_AllWithX(t *testing.T) {
	x := ScopeSetExplicit("p1", "p2")
	r := ScopeSetAll().Intersection(x)
	if !r.Equal(x) {
		t.Fatalf("All.Intersection(X) should be X, got %v", r)
	}
	r = x.Intersection(ScopeSetAll())
	if !r.Equal(x) {
		t.Fatalf("X.Intersection(All) should be X, got %v", r)
	}
}

func TestIntersection_NoneWithX(t *testing.T) {
	cases := []struct {
		name string
		x    ScopeSet
	}{
		{"All", ScopeSetAll()},
		{"None", ScopeSetNone()},
		{"Explicit", ScopeSetExplicit("p1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ScopeSetNone().Intersection(tc.x)
			if !r.IsNone() {
				t.Fatal("None.Intersection(X) should be None")
			}
			r = tc.x.Intersection(ScopeSetNone())
			if !r.IsNone() {
				t.Fatal("X.Intersection(None) should be None")
			}
		})
	}
}

func TestIntersection_ExplicitFindsCommon(t *testing.T) {
	a := ScopeSetExplicit("p1", "p2", "p3")
	b := ScopeSetExplicit("p2", "p3", "p4")
	r := a.Intersection(b)
	want := ScopeSetExplicit("p2", "p3")
	if !r.Equal(want) {
		t.Fatalf("got %v, want %v", r, want)
	}
}

func TestIntersection_ExplicitDisjoint(t *testing.T) {
	a := ScopeSetExplicit("p1", "p2")
	b := ScopeSetExplicit("p3", "p4")
	r := a.Intersection(b)
	if !r.IsNone() {
		t.Fatal("disjoint intersection should be None")
	}
}

// --- Contains tests for each form ---

func TestContains_All(t *testing.T) {
	if !ScopeSetAll().Contains("anything") {
		t.Fatal("All should contain anything")
	}
}

func TestContains_None(t *testing.T) {
	if ScopeSetNone().Contains("anything") {
		t.Fatal("None should not contain anything")
	}
}

func TestContains_Explicit(t *testing.T) {
	s := ScopeSetExplicit("p1")
	if !s.Contains("p1") {
		t.Fatal("should contain p1")
	}
	if s.Contains("p2") {
		t.Fatal("should not contain p2")
	}
}

// --- Equal tests ---

func TestEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b ScopeSet
		want bool
	}{
		{"All==All", ScopeSetAll(), ScopeSetAll(), true},
		{"None==None", ScopeSetNone(), ScopeSetNone(), true},
		{"All!=None", ScopeSetAll(), ScopeSetNone(), false},
		{"Explicit==Same", ScopeSetExplicit("p1", "p2"), ScopeSetExplicit("p2", "p1"), true},
		{"Explicit!=Different", ScopeSetExplicit("p1"), ScopeSetExplicit("p2"), false},
		{"EmptyExplicit==None", ScopeSetExplicit(), ScopeSetNone(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.a.Equal(tc.b)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// --- String tests ---

func TestString(t *testing.T) {
	if s := ScopeSetAll().String(); s != "ScopeSet(All)" {
		t.Fatalf("got %q", s)
	}
	if s := ScopeSetNone().String(); s != "ScopeSet(None)" {
		t.Fatalf("got %q", s)
	}
	s := ScopeSetExplicit("p2", "p1").String()
	if s != "ScopeSet(p1, p2)" {
		t.Fatalf("got %q", s)
	}
}

// --- ResolveAuthorizedScopes tests ---

func TestResolveAuthorizedScopes_SystemBinding(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeSystem,
		},
	}
	roles := map[string]*RolePermissions{
		"r1": NewRolePermissions("r1", "admin", ScopeTypeSystem, []string{"agent.list"}),
	}

	result := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	if !result.IsAll() {
		t.Fatalf("system binding should produce All, got %v", result)
	}
}

func TestResolveAuthorizedScopes_ProjectBindings(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-a",
		},
		{
			BindingID:        "b2",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-b",
		},
	}
	roles := map[string]*RolePermissions{
		"r1": NewRolePermissions("r1", "member", ScopeTypeProject, []string{"agent.list", "agent.read"}),
	}

	result := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	want := ScopeSetExplicit("proj-a", "proj-b")
	if !result.Equal(want) {
		t.Fatalf("got %v, want %v", result, want)
	}
}

func TestResolveAuthorizedScopes_MixedBindings(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}, "group:group1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-a",
		},
		{
			BindingID:        "b2",
			RoleDefinitionID: "r2",
			PrincipalType:    "group",
			PrincipalID:      "group1",
			ScopeType:        ScopeTypeSystem,
		},
	}
	roles := map[string]*RolePermissions{
		"r1": NewRolePermissions("r1", "member", ScopeTypeProject, []string{"agent.list"}),
		"r2": NewRolePermissions("r2", "viewer", ScopeTypeSystem, []string{"agent.list"}),
	}

	result := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	if !result.IsAll() {
		t.Fatalf("system binding via group should produce All, got %v", result)
	}
}

func TestResolveAuthorizedScopes_NoMatchingPermission(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-a",
		},
	}
	roles := map[string]*RolePermissions{
		"r1": NewRolePermissions("r1", "member", ScopeTypeProject, []string{"agent.read"}),
	}

	result := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	if !result.IsNone() {
		t.Fatalf("no matching permission should produce None, got %v", result)
	}
}

func TestResolveAuthorizedScopes_PrincipalNotInClosure(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user2", // not in closure
			ScopeType:        ScopeTypeSystem,
		},
	}
	roles := map[string]*RolePermissions{
		"r1": NewRolePermissions("r1", "admin", ScopeTypeSystem, []string{"agent.list"}),
	}

	result := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	if !result.IsNone() {
		t.Fatalf("binding for non-closure principal should not contribute, got %v", result)
	}
}

func TestResolveAuthorizedScopes_NoBindings(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}}
	result := ResolveAuthorizedScopes(closure, "agent.list", nil, nil, testNow)
	if !result.IsNone() {
		t.Fatalf("no bindings should produce None, got %v", result)
	}
}

func TestResolveAuthorizedScopes_ScopeTypeMismatch_Rejected(t *testing.T) {
	// A binding with ScopeType "system" pointing at a project-scoped role
	// should not contribute to the scope set. Vice versa as well.
	closure := map[string]struct{}{"user:user1": {}}
	roles := map[string]*RolePermissions{
		"r-project": NewRolePermissions("r-project", "project-role", ScopeTypeProject, []string{"agent.list"}),
		"r-system":  NewRolePermissions("r-system", "system-role", ScopeTypeSystem, []string{"agent.list"}),
	}

	bindings := []CandidateBinding{
		// System binding pointing at a project-scoped role — mismatch.
		{
			BindingID:        "b1",
			RoleDefinitionID: "r-project",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeSystem,
		},
		// Project binding pointing at a system-scoped role — mismatch.
		{
			BindingID:        "b2",
			RoleDefinitionID: "r-system",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-a",
		},
	}

	result := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	if !result.IsNone() {
		t.Fatalf("scope-type mismatch bindings should not contribute, got %v", result)
	}
}

func TestResolveAuthorizedScopes_ExpiredAndNotYetActive(t *testing.T) {
	// R2 regression: expired and not-yet-active bindings must not contribute
	// to the scope set.
	closure := map[string]struct{}{"user:user1": {}}
	roles := map[string]*RolePermissions{
		"r1": NewRolePermissions("r1", "member", ScopeTypeProject, []string{"agent.list"}),
	}

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	bindings := []CandidateBinding{
		// Expired binding — should NOT contribute.
		{
			BindingID:        "b-expired",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-expired",
			ExpiresAt:        now.Add(-1 * time.Hour),
		},
		// Not-yet-active binding — should NOT contribute.
		{
			BindingID:        "b-future",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-future",
			NotBefore:        now.Add(1 * time.Hour),
		},
		// Active binding — SHOULD contribute.
		{
			BindingID:        "b-active",
			RoleDefinitionID: "r1",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-active",
			NotBefore:        now.Add(-2 * time.Hour),
			ExpiresAt:        now.Add(24 * time.Hour),
		},
	}

	result := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, now)

	// Only proj-active should be in the scope set.
	want := ScopeSetExplicit("proj-active")
	if !result.Equal(want) {
		t.Fatalf("expired and not-yet-active bindings should not contribute; got %v, want %v", result, want)
	}

	// Verify explicitly that expired and future projects are excluded.
	if result.Contains("proj-expired") {
		t.Fatal("expired binding should not contribute to scope set")
	}
	if result.Contains("proj-future") {
		t.Fatal("not-yet-active binding should not contribute to scope set")
	}
	if !result.Contains("proj-active") {
		t.Fatal("active binding should contribute to scope set")
	}
}
