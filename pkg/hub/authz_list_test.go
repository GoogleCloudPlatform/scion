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

// --- toCandidateBindings tests ---

func TestToCandidateBindings_Basic(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	later := now.Add(24 * time.Hour)

	bindings := []*storeRoleBindingShim{
		{
			id: "b1", roleDefID: "r1",
			principalType: "user", principalID: "u1",
			scopeType: "system",
		},
		{
			id: "b2", roleDefID: "r2",
			principalType: "group", principalID: "g1",
			scopeType: "project", scopeID: "p1",
			notBefore: &now, expiresAt: &later,
		},
	}

	candidates := toCandidateBindingsFromShims(bindings)

	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	// First binding: no time constraints.
	c0 := candidates[0]
	if c0.BindingID != "b1" {
		t.Errorf("c0.BindingID = %q, want %q", c0.BindingID, "b1")
	}
	if c0.ScopeType != "system" {
		t.Errorf("c0.ScopeType = %q, want %q", c0.ScopeType, "system")
	}
	if !c0.NotBefore.IsZero() {
		t.Errorf("c0.NotBefore should be zero, got %v", c0.NotBefore)
	}

	// Second binding: has time constraints.
	c1 := candidates[1]
	if c1.BindingID != "b2" {
		t.Errorf("c1.BindingID = %q, want %q", c1.BindingID, "b2")
	}
	if c1.ScopeType != "project" || c1.ScopeID != "p1" {
		t.Errorf("c1 scope = %s/%s, want project/p1", c1.ScopeType, c1.ScopeID)
	}
	if !c1.NotBefore.Equal(now) {
		t.Errorf("c1.NotBefore = %v, want %v", c1.NotBefore, now)
	}
	if !c1.ExpiresAt.Equal(later) {
		t.Errorf("c1.ExpiresAt = %v, want %v", c1.ExpiresAt, later)
	}
}

// --- collectRoleDefinitionIDs tests ---

func TestCollectRoleDefinitionIDs_Deduplicated(t *testing.T) {
	bindings := []*storeRoleBindingShim{
		{roleDefID: "r1"},
		{roleDefID: "r2"},
		{roleDefID: "r1"}, // duplicate
		{roleDefID: "r3"},
		{roleDefID: "r2"}, // duplicate
	}

	ids := collectRoleDefinitionIDsFromShims(bindings)
	if len(ids) != 3 {
		t.Fatalf("expected 3 unique IDs, got %d: %v", len(ids), ids)
	}

	// Check all three are present.
	seen := make(map[string]bool)
	for _, id := range ids {
		seen[id] = true
	}
	for _, want := range []string{"r1", "r2", "r3"} {
		if !seen[want] {
			t.Errorf("missing role definition ID %q", want)
		}
	}
}

func TestCollectRoleDefinitionIDs_Empty(t *testing.T) {
	ids := collectRoleDefinitionIDsFromShims(nil)
	if len(ids) != 0 {
		t.Fatalf("expected 0 IDs from nil input, got %d", len(ids))
	}
}

// --- Scope-aware list authorization integration tests ---

// TestScopeAwareList_SystemAdmin verifies that a user with a system-scoped role
// containing the list permission gets ScopeSetAll.
func TestScopeAwareList_SystemAdmin(t *testing.T) {
	closure := map[string]struct{}{"user:admin1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "super-admin",
			PrincipalType:    "user",
			PrincipalID:      "admin1",
			ScopeType:        ScopeTypeSystem,
		},
	}
	roles := map[string]*RolePermissions{
		"super-admin": NewRolePermissions("super-admin", "Super Admin", ScopeTypeSystem,
			[]string{"project.list", "agent.list", "project.read", "agent.read"}),
	}

	projectScopes := ResolveAuthorizedScopes(closure, "project.list", bindings, roles, testNow)
	if !projectScopes.IsAll() {
		t.Fatalf("system admin should get All for project.list, got %v", projectScopes)
	}

	agentScopes := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	if !agentScopes.IsAll() {
		t.Fatalf("system admin should get All for agent.list, got %v", agentScopes)
	}
}

// TestScopeAwareList_OrdinaryMember verifies that a user with only project-scoped
// bindings sees only their bound projects.
func TestScopeAwareList_OrdinaryMember(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "project-member",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-alpha",
		},
	}
	roles := map[string]*RolePermissions{
		"project-member": NewRolePermissions("project-member", "Project Member", ScopeTypeProject,
			[]string{"project.list", "project.read", "agent.list", "agent.read"}),
	}

	scopes := ResolveAuthorizedScopes(closure, "project.list", bindings, roles, testNow)
	if scopes.IsAll() {
		t.Fatal("ordinary member should NOT get All")
	}
	if scopes.IsNone() {
		t.Fatal("ordinary member with project binding should NOT get None")
	}
	want := ScopeSetExplicit("proj-alpha")
	if !scopes.Equal(want) {
		t.Fatalf("got %v, want %v", scopes, want)
	}
}

// TestScopeAwareList_MixedDirectAndGroupScopes verifies that a user with a
// direct binding on project A and a group binding on project B sees both.
func TestScopeAwareList_MixedDirectAndGroupScopes(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}, "group:group-team": {}}
	bindings := []CandidateBinding{
		// Direct user binding on project A.
		{
			BindingID:        "b1",
			RoleDefinitionID: "project-member",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-a",
		},
		// Group binding on project B (user is member of group-team).
		{
			BindingID:        "b2",
			RoleDefinitionID: "project-member",
			PrincipalType:    "group",
			PrincipalID:      "group-team",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-b",
		},
	}
	roles := map[string]*RolePermissions{
		"project-member": NewRolePermissions("project-member", "Project Member", ScopeTypeProject,
			[]string{"project.list", "project.read", "agent.list", "agent.read"}),
	}

	scopes := ResolveAuthorizedScopes(closure, "project.list", bindings, roles, testNow)
	want := ScopeSetExplicit("proj-a", "proj-b")
	if !scopes.Equal(want) {
		t.Fatalf("mixed direct/group scopes should union; got %v, want %v", scopes, want)
	}
}

// TestScopeAwareList_NoBindings verifies that a user with no bindings gets None.
func TestScopeAwareList_NoBindings(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}}
	scopes := ResolveAuthorizedScopes(closure, "project.list", nil, nil, testNow)
	if !scopes.IsNone() {
		t.Fatalf("no bindings should produce None, got %v", scopes)
	}
}

// TestScopeAwareList_ExpiredBindingExcluded verifies that expired bindings
// do not contribute to the scope set.
func TestScopeAwareList_ExpiredBindingExcluded(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	closure := map[string]struct{}{"user:user1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b-expired",
			RoleDefinitionID: "project-member",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-expired",
			ExpiresAt:        now.Add(-1 * time.Hour),
		},
		{
			BindingID:        "b-active",
			RoleDefinitionID: "project-member",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-active",
		},
	}
	roles := map[string]*RolePermissions{
		"project-member": NewRolePermissions("project-member", "Project Member", ScopeTypeProject,
			[]string{"project.list"}),
	}

	scopes := ResolveAuthorizedScopes(closure, "project.list", bindings, roles, now)
	if scopes.Contains("proj-expired") {
		t.Fatal("expired binding should NOT be included")
	}
	if !scopes.Contains("proj-active") {
		t.Fatal("active binding SHOULD be included")
	}
}

// TestScopeAwareList_RoleWithoutListPermission verifies that a user whose role
// does NOT include the list permission gets None even with project-scoped bindings.
func TestScopeAwareList_RoleWithoutListPermission(t *testing.T) {
	// A user whose role does NOT include project.list should get None even
	// though they have project-scoped bindings.
	closure := map[string]struct{}{"user:user1": {}}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "secret-reader",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-a",
		},
	}
	roles := map[string]*RolePermissions{
		"secret-reader": NewRolePermissions("secret-reader", "Secret Reader", ScopeTypeProject,
			[]string{"secret.read"}),
	}

	scopes := ResolveAuthorizedScopes(closure, "project.list", bindings, roles, testNow)
	if !scopes.IsNone() {
		t.Fatalf("role without project.list should produce None, got %v", scopes)
	}
}

// TestScopeAwareList_MultipleGroupsUnion verifies that bindings from multiple
// different groups correctly union.
func TestScopeAwareList_MultipleGroupsUnion(t *testing.T) {
	closure := map[string]struct{}{
		"user:user1":    {},
		"group:group-a": {},
		"group:group-b": {},
		"group:group-c": {},
	}
	bindings := []CandidateBinding{
		{
			BindingID:        "b1",
			RoleDefinitionID: "project-member",
			PrincipalType:    "group",
			PrincipalID:      "group-a",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-1",
		},
		{
			BindingID:        "b2",
			RoleDefinitionID: "project-member",
			PrincipalType:    "group",
			PrincipalID:      "group-b",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-2",
		},
		{
			BindingID:        "b3",
			RoleDefinitionID: "project-member",
			PrincipalType:    "group",
			PrincipalID:      "group-c",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-3",
		},
	}
	roles := map[string]*RolePermissions{
		"project-member": NewRolePermissions("project-member", "Project Member", ScopeTypeProject,
			[]string{"agent.list"}),
	}

	scopes := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	want := ScopeSetExplicit("proj-1", "proj-2", "proj-3")
	if !scopes.Equal(want) {
		t.Fatalf("multiple group bindings should union; got %v, want %v", scopes, want)
	}
}

// TestScopeAwareList_SystemBindingShortCircuits verifies that a system-scoped
// binding produces All immediately, even if project bindings are also present.
func TestScopeAwareList_SystemBindingShortCircuits(t *testing.T) {
	closure := map[string]struct{}{"user:user1": {}, "group:hub-admins": {}}
	bindings := []CandidateBinding{
		// Project binding.
		{
			BindingID:        "b1",
			RoleDefinitionID: "project-member",
			PrincipalType:    "user",
			PrincipalID:      "user1",
			ScopeType:        ScopeTypeProject,
			ScopeID:          "proj-a",
		},
		// System binding via group.
		{
			BindingID:        "b2",
			RoleDefinitionID: "hub-admin",
			PrincipalType:    "group",
			PrincipalID:      "hub-admins",
			ScopeType:        ScopeTypeSystem,
		},
	}
	roles := map[string]*RolePermissions{
		"project-member": NewRolePermissions("project-member", "Project Member", ScopeTypeProject,
			[]string{"agent.list"}),
		"hub-admin": NewRolePermissions("hub-admin", "Hub Admin", ScopeTypeSystem,
			[]string{"agent.list", "project.list"}),
	}

	scopes := ResolveAuthorizedScopes(closure, "agent.list", bindings, roles, testNow)
	if !scopes.IsAll() {
		t.Fatalf("system binding should produce All regardless of project bindings, got %v", scopes)
	}
}

// --- Test helpers for the adapter that use store shims ---
// These avoid importing the store package directly; they test the conversion
// logic at the hub-package level.

type storeRoleBindingShim struct {
	id            string
	roleDefID     string
	principalType string
	principalID   string
	scopeType     string
	scopeID       string
	notBefore     *time.Time
	expiresAt     *time.Time
}

func toCandidateBindingsFromShims(shims []*storeRoleBindingShim) []CandidateBinding {
	candidates := make([]CandidateBinding, len(shims))
	for i, s := range shims {
		candidates[i] = CandidateBinding{
			BindingID:        s.id,
			RoleDefinitionID: s.roleDefID,
			PrincipalType:    s.principalType,
			PrincipalID:      s.principalID,
			ScopeType:        s.scopeType,
			ScopeID:          s.scopeID,
		}
		if s.notBefore != nil {
			candidates[i].NotBefore = *s.notBefore
		}
		if s.expiresAt != nil {
			candidates[i].ExpiresAt = *s.expiresAt
		}
	}
	return candidates
}

func collectRoleDefinitionIDsFromShims(shims []*storeRoleBindingShim) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, s := range shims {
		if _, ok := seen[s.roleDefID]; !ok {
			seen[s.roleDefID] = struct{}{}
			ids = append(ids, s.roleDefID)
		}
	}
	return ids
}

// --- applyCredentialCaveats tests (R2 — LS1 review) ---

// TestApplyCredentialCaveats_ScopedUser verifies that a ScopedUserIdentity
// with a project scope narrows ScopeSetAll to only that project.
func TestApplyCredentialCaveats_ScopedUser(t *testing.T) {
	user := NewAuthenticatedUser("u1", "user@example.com", "User", "member", "cli")
	scoped := NewScopedUserIdentity(user, "proj-1", []string{"agent:read"})

	result := applyCredentialCaveats(scoped, ScopeSetAll())
	want := ScopeSetExplicit("proj-1")
	if !result.Equal(want) {
		t.Fatalf("ScopedUser with project scope: got %v, want %v", result, want)
	}
}

// TestApplyCredentialCaveats_ScopedUserExplicitIntersection verifies that a
// ScopedUserIdentity intersects an explicit scope set, keeping only the
// common project.
func TestApplyCredentialCaveats_ScopedUserExplicitIntersection(t *testing.T) {
	user := NewAuthenticatedUser("u1", "user@example.com", "User", "member", "cli")
	scoped := NewScopedUserIdentity(user, "proj-1", []string{"agent:read"})

	result := applyCredentialCaveats(scoped, ScopeSetExplicit("proj-1", "proj-2"))
	want := ScopeSetExplicit("proj-1")
	if !result.Equal(want) {
		t.Fatalf("ScopedUser intersecting Explicit(proj-1,proj-2): got %v, want %v", result, want)
	}
}

// TestApplyCredentialCaveats_ScopedUserDisjoint verifies that a
// ScopedUserIdentity produces None when its project is not in the scope set.
func TestApplyCredentialCaveats_ScopedUserDisjoint(t *testing.T) {
	user := NewAuthenticatedUser("u1", "user@example.com", "User", "member", "cli")
	scoped := NewScopedUserIdentity(user, "proj-1", []string{"agent:read"})

	result := applyCredentialCaveats(scoped, ScopeSetExplicit("proj-other"))
	if !result.IsNone() {
		t.Fatalf("ScopedUser with disjoint project: got %v, want None", result)
	}
}

// TestApplyCredentialCaveats_Agent verifies that an AgentIdentity with a
// project scope narrows ScopeSetExplicit to only the agent's project.
func TestApplyCredentialCaveats_Agent(t *testing.T) {
	agent := &agentIdentityWrapper{
		AgentTokenClaims: &AgentTokenClaims{
			ProjectID: "proj-a",
		},
	}
	agent.Subject = "agent-1"

	result := applyCredentialCaveats(agent, ScopeSetExplicit("proj-a", "proj-b"))
	want := ScopeSetExplicit("proj-a")
	if !result.Equal(want) {
		t.Fatalf("Agent with project scope: got %v, want %v", result, want)
	}
}

// TestApplyCredentialCaveats_AgentAll verifies that an AgentIdentity with a
// project scope narrows ScopeSetAll to just that project.
func TestApplyCredentialCaveats_AgentAll(t *testing.T) {
	agent := &agentIdentityWrapper{
		AgentTokenClaims: &AgentTokenClaims{
			ProjectID: "proj-a",
		},
	}
	agent.Subject = "agent-1"

	result := applyCredentialCaveats(agent, ScopeSetAll())
	want := ScopeSetExplicit("proj-a")
	if !result.Equal(want) {
		t.Fatalf("Agent narrowing All: got %v, want %v", result, want)
	}
}

// TestApplyCredentialCaveats_AgentDisjoint verifies that an AgentIdentity
// whose project is not in the scope set produces None.
func TestApplyCredentialCaveats_AgentDisjoint(t *testing.T) {
	agent := &agentIdentityWrapper{
		AgentTokenClaims: &AgentTokenClaims{
			ProjectID: "proj-a",
		},
	}
	agent.Subject = "agent-1"

	result := applyCredentialCaveats(agent, ScopeSetExplicit("proj-b", "proj-c"))
	if !result.IsNone() {
		t.Fatalf("Agent with disjoint project: got %v, want None", result)
	}
}

// TestApplyCredentialCaveats_UnscopedUser verifies that an unscoped user
// identity returns the scope set unchanged.
func TestApplyCredentialCaveats_UnscopedUser(t *testing.T) {
	user := NewAuthenticatedUser("u1", "user@example.com", "User", "admin", "cli")

	all := ScopeSetAll()
	result := applyCredentialCaveats(user, all)
	if !result.Equal(all) {
		t.Fatalf("Unscoped user with All: got %v, want All", result)
	}

	explicit := ScopeSetExplicit("proj-1", "proj-2")
	result = applyCredentialCaveats(user, explicit)
	if !result.Equal(explicit) {
		t.Fatalf("Unscoped user with Explicit: got %v, want %v", result, explicit)
	}

	none := ScopeSetNone()
	result = applyCredentialCaveats(user, none)
	if !result.Equal(none) {
		t.Fatalf("Unscoped user with None: got %v, want None", result)
	}
}

// TestApplyCredentialCaveats_ScopedUserNoProject verifies that a
// ScopedUserIdentity without a project scope returns the scope set unchanged.
func TestApplyCredentialCaveats_ScopedUserNoProject(t *testing.T) {
	user := NewAuthenticatedUser("u1", "user@example.com", "User", "member", "cli")
	scoped := NewScopedUserIdentity(user, "", []string{"agent:read"})

	all := ScopeSetAll()
	result := applyCredentialCaveats(scoped, all)
	if !result.Equal(all) {
		t.Fatalf("ScopedUser with empty project scope: got %v, want All", result)
	}
}

// TestApplyCredentialCaveats_AgentNoProject verifies that an AgentIdentity
// without a project scope returns the scope set unchanged.
func TestApplyCredentialCaveats_AgentNoProject(t *testing.T) {
	agent := &agentIdentityWrapper{
		AgentTokenClaims: &AgentTokenClaims{
			ProjectID: "",
		},
	}
	agent.Subject = "agent-1"

	explicit := ScopeSetExplicit("proj-a", "proj-b")
	result := applyCredentialCaveats(agent, explicit)
	if !result.Equal(explicit) {
		t.Fatalf("Agent with empty project: got %v, want %v", result, explicit)
	}
}

// TestApplyCredentialCaveats_NilIdentity verifies that a nil identity
// returns the scope set unchanged (no panic).
func TestApplyCredentialCaveats_NilIdentity(t *testing.T) {
	all := ScopeSetAll()
	result := applyCredentialCaveats(nil, all)
	if !result.Equal(all) {
		t.Fatalf("nil identity: got %v, want All", result)
	}
}
