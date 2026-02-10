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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/store"
)

// ============================================================================
// Group Endpoint Tests
// ============================================================================

func TestGroupList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create some test groups
	for i := 0; i < 3; i++ {
		group := &store.Group{
			ID:      "group_" + string(rune('a'+i)),
			Name:    "Test Group " + string(rune('A'+i)),
			Slug:    "test-group-" + string(rune('a'+i)),
			Created: time.Now(),
			Updated: time.Now(),
		}
		if err := s.CreateGroup(ctx, group); err != nil {
			t.Fatalf("failed to create group: %v", err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groups", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListGroupsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Groups) != 3 {
		t.Errorf("expected 3 groups, got %d", len(resp.Groups))
	}

	if resp.TotalCount != 3 {
		t.Errorf("expected total 3, got %d", resp.TotalCount)
	}
}

func TestGroupCreate(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateGroupRequest{
		Name:        "Platform Team",
		Slug:        "platform-team",
		Description: "The platform engineering team",
		Labels:      map[string]string{"department": "engineering"},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var group store.Group
	if err := json.NewDecoder(rec.Body).Decode(&group); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if group.Name != "Platform Team" {
		t.Errorf("expected name 'Platform Team', got %q", group.Name)
	}
	if group.Slug != "platform-team" {
		t.Errorf("expected slug 'platform-team', got %q", group.Slug)
	}
	if group.ID == "" {
		t.Error("expected ID to be set")
	}
}

func TestGroupCreateValidation(t *testing.T) {
	srv, _ := testServer(t)

	// Missing name
	body := CreateGroupRequest{}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", body)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing name, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupGet(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:          "group_xyz123",
		Name:        "Test Group",
		Slug:        "test-group",
		Description: "A test group",
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	// Get by ID
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groups/"+group.ID, nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.Group
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != group.ID {
		t.Errorf("expected ID %q, got %q", group.ID, resp.ID)
	}

	// Get by slug
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/groups/"+group.Slug, nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupUpdate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      "group_upd123",
		Name:    "Original Name",
		Slug:    "original-name",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	body := UpdateGroupRequest{
		Name:        "Updated Name",
		Description: "New description",
	}

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/groups/"+group.ID, body)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.Group
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Name != "Updated Name" {
		t.Errorf("expected name 'Updated Name', got %q", resp.Name)
	}
	if resp.Description != "New description" {
		t.Errorf("expected description 'New description', got %q", resp.Description)
	}
}

func TestGroupDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      "group_del123",
		Name:    "Delete Me",
		Slug:    "delete-me",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groups/"+group.ID, nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify deleted
	_, err := s.GetGroup(ctx, group.ID)
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGroupMembersAdd(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      "group_mem123",
		Name:    "Test Group",
		Slug:    "test-group",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	body := AddGroupMemberRequest{
		MemberType: "user",
		MemberID:   "user_abc123",
		Role:       "member",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+group.ID+"/members", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.GroupMember
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.MemberID != "user_abc123" {
		t.Errorf("expected memberId 'user_abc123', got %q", resp.MemberID)
	}
}

func TestGroupMembersList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      "group_lst123",
		Name:    "Test Group",
		Slug:    "test-group-list",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	// Add members
	for i := 0; i < 3; i++ {
		member := &store.GroupMember{
			GroupID:    group.ID,
			MemberType: "user",
			MemberID:   "user_" + string(rune('a'+i)),
			Role:       "member",
			AddedAt:    time.Now(),
		}
		if err := s.AddGroupMember(ctx, member); err != nil {
			t.Fatalf("failed to add member: %v", err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groups/"+group.ID+"/members", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListGroupMembersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Members) != 3 {
		t.Errorf("expected 3 members, got %d", len(resp.Members))
	}
}

func TestGroupMemberRemove(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      "group_rem123",
		Name:    "Test Group",
		Slug:    "test-group-remove",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	member := &store.GroupMember{
		GroupID:    group.ID,
		MemberType: "user",
		MemberID:   "user_remove",
		Role:       "member",
		AddedAt:    time.Now(),
	}
	if err := s.AddGroupMember(ctx, member); err != nil {
		t.Fatalf("failed to add member: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groups/"+group.ID+"/members/user/user_remove", nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify removed
	_, err := s.GetGroupMembership(ctx, group.ID, "user", "user_remove")
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGroupCycleDetection(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two groups
	groupA := &store.Group{
		ID:      "group_a",
		Name:    "Group A",
		Slug:    "group-a",
		Created: time.Now(),
		Updated: time.Now(),
	}
	groupB := &store.Group{
		ID:      "group_b",
		Name:    "Group B",
		Slug:    "group-b",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateGroup(ctx, groupA); err != nil {
		t.Fatalf("failed to create group A: %v", err)
	}
	if err := s.CreateGroup(ctx, groupB); err != nil {
		t.Fatalf("failed to create group B: %v", err)
	}

	// Add B as a member of A
	body := AddGroupMemberRequest{
		MemberType: "group",
		MemberID:   groupB.ID,
		Role:       "member",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+groupA.ID+"/members", body)
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Try to add A as a member of B (should fail - would create cycle)
	body = AddGroupMemberRequest{
		MemberType: "group",
		MemberID:   groupA.ID,
		Role:       "member",
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+groupB.ID+"/members", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for cycle, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Policy Endpoint Tests
// ============================================================================

func TestPolicyList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create some test policies
	for i := 0; i < 3; i++ {
		policy := &store.Policy{
			ID:           "policy_" + string(rune('a'+i)),
			Name:         "Test Policy " + string(rune('A'+i)),
			ScopeType:    "hub",
			ResourceType: "*",
			Actions:      []string{"read"},
			Effect:       "allow",
			Created:      time.Now(),
			Updated:      time.Now(),
		}
		if err := s.CreatePolicy(ctx, policy); err != nil {
			t.Fatalf("failed to create policy: %v", err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/policies", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListPoliciesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Policies) != 3 {
		t.Errorf("expected 3 policies, got %d", len(resp.Policies))
	}
}

func TestPolicyCreate(t *testing.T) {
	srv, _ := testServer(t)

	body := CreatePolicyRequest{
		Name:         "Admin Access",
		Description:  "Full admin access",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"*"},
		Effect:       "allow",
		Priority:     100,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var policy store.Policy
	if err := json.NewDecoder(rec.Body).Decode(&policy); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if policy.Name != "Admin Access" {
		t.Errorf("expected name 'Admin Access', got %q", policy.Name)
	}
	if policy.Effect != "allow" {
		t.Errorf("expected effect 'allow', got %q", policy.Effect)
	}
	if policy.Priority != 100 {
		t.Errorf("expected priority 100, got %d", policy.Priority)
	}
}

func TestPolicyCreateValidation(t *testing.T) {
	srv, _ := testServer(t)

	testCases := []struct {
		name string
		body CreatePolicyRequest
	}{
		{
			name: "missing name",
			body: CreatePolicyRequest{ScopeType: "hub", Actions: []string{"read"}, Effect: "allow"},
		},
		{
			name: "missing scopeType",
			body: CreatePolicyRequest{Name: "Test", Actions: []string{"read"}, Effect: "allow"},
		},
		{
			name: "missing actions",
			body: CreatePolicyRequest{Name: "Test", ScopeType: "hub", Effect: "allow"},
		},
		{
			name: "missing effect",
			body: CreatePolicyRequest{Name: "Test", ScopeType: "hub", Actions: []string{"read"}},
		},
		{
			name: "invalid scopeType",
			body: CreatePolicyRequest{Name: "Test", ScopeType: "invalid", Actions: []string{"read"}, Effect: "allow"},
		},
		{
			name: "invalid effect",
			body: CreatePolicyRequest{Name: "Test", ScopeType: "hub", Actions: []string{"read"}, Effect: "invalid"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected status 400 for %s, got %d: %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPolicyGet(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	policy := &store.Policy{
		ID:           "policy_get123",
		Name:         "Test Policy",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read", "write"},
		Effect:       "allow",
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreatePolicy(ctx, policy); err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/policies/"+policy.ID, nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.Policy
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != policy.ID {
		t.Errorf("expected ID %q, got %q", policy.ID, resp.ID)
	}
}

func TestPolicyUpdate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	policy := &store.Policy{
		ID:           "policy_upd123",
		Name:         "Original Policy",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read"},
		Effect:       "allow",
		Priority:     0,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreatePolicy(ctx, policy); err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	newPriority := 50
	body := UpdatePolicyRequest{
		Name:        "Updated Policy",
		Description: "New description",
		Actions:     []string{"read", "write"},
		Priority:    &newPriority,
	}

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/policies/"+policy.ID, body)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.Policy
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Name != "Updated Policy" {
		t.Errorf("expected name 'Updated Policy', got %q", resp.Name)
	}
	if resp.Priority != 50 {
		t.Errorf("expected priority 50, got %d", resp.Priority)
	}
	if len(resp.Actions) != 2 {
		t.Errorf("expected 2 actions, got %d", len(resp.Actions))
	}
}

func TestPolicyDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	policy := &store.Policy{
		ID:           "policy_del123",
		Name:         "Delete Me",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read"},
		Effect:       "allow",
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreatePolicy(ctx, policy); err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/policies/"+policy.ID, nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify deleted
	_, err := s.GetPolicy(ctx, policy.ID)
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestPolicyBindingsAdd(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	policy := &store.Policy{
		ID:           "policy_bind123",
		Name:         "Test Policy",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read"},
		Effect:       "allow",
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreatePolicy(ctx, policy); err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	body := AddPolicyBindingRequest{
		PrincipalType: "user",
		PrincipalID:   "user_abc123",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies/"+policy.ID+"/bindings", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.PolicyBinding
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.PrincipalID != "user_abc123" {
		t.Errorf("expected principalId 'user_abc123', got %q", resp.PrincipalID)
	}
}

func TestPolicyBindingsList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	policy := &store.Policy{
		ID:           "policy_blst123",
		Name:         "Test Policy",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read"},
		Effect:       "allow",
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreatePolicy(ctx, policy); err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	// Add bindings
	for i := 0; i < 3; i++ {
		binding := &store.PolicyBinding{
			PolicyID:      policy.ID,
			PrincipalType: "user",
			PrincipalID:   "user_" + string(rune('a'+i)),
		}
		if err := s.AddPolicyBinding(ctx, binding); err != nil {
			t.Fatalf("failed to add binding: %v", err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/policies/"+policy.ID+"/bindings", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListPolicyBindingsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Bindings) != 3 {
		t.Errorf("expected 3 bindings, got %d", len(resp.Bindings))
	}
}

func TestPolicyBindingRemove(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	policy := &store.Policy{
		ID:           "policy_brem123",
		Name:         "Test Policy",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read"},
		Effect:       "allow",
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreatePolicy(ctx, policy); err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	binding := &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "user",
		PrincipalID:   "user_remove",
	}
	if err := s.AddPolicyBinding(ctx, binding); err != nil {
		t.Fatalf("failed to add binding: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/policies/"+policy.ID+"/bindings/user/user_remove", nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify removed
	bindings, err := s.GetPolicyBindings(ctx, policy.ID)
	if err != nil {
		t.Fatalf("failed to get bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Errorf("expected 0 bindings, got %d", len(bindings))
	}
}

// ============================================================================
// Store Integration Tests (for Group and Policy)
// ============================================================================

func TestGetEffectiveGroups(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	_ = srv // We just need the store

	// Create a group hierarchy: A contains B, B contains C
	// User is a member of C, should also be effective member of B and A
	groupA := &store.Group{
		ID:      "group_eff_a",
		Name:    "Group A",
		Slug:    "group-eff-a",
		Created: time.Now(),
		Updated: time.Now(),
	}
	groupB := &store.Group{
		ID:      "group_eff_b",
		Name:    "Group B",
		Slug:    "group-eff-b",
		Created: time.Now(),
		Updated: time.Now(),
	}
	groupC := &store.Group{
		ID:      "group_eff_c",
		Name:    "Group C",
		Slug:    "group-eff-c",
		Created: time.Now(),
		Updated: time.Now(),
	}

	for _, g := range []*store.Group{groupA, groupB, groupC} {
		if err := s.CreateGroup(ctx, g); err != nil {
			t.Fatalf("failed to create group %s: %v", g.ID, err)
		}
	}

	// B is member of A
	if err := s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupA.ID,
		MemberType: "group",
		MemberID:   groupB.ID,
		Role:       "member",
		AddedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("failed to add B to A: %v", err)
	}

	// C is member of B
	if err := s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupB.ID,
		MemberType: "group",
		MemberID:   groupC.ID,
		Role:       "member",
		AddedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("failed to add C to B: %v", err)
	}

	// User is member of C
	if err := s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupC.ID,
		MemberType: "user",
		MemberID:   "test_user",
		Role:       "member",
		AddedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("failed to add user to C: %v", err)
	}

	// Get effective groups for user
	effectiveGroups, err := s.GetEffectiveGroups(ctx, "test_user")
	if err != nil {
		t.Fatalf("failed to get effective groups: %v", err)
	}

	// User should be in C, B, and A
	if len(effectiveGroups) != 3 {
		t.Errorf("expected 3 effective groups, got %d: %v", len(effectiveGroups), effectiveGroups)
	}

	// Check that all expected groups are present
	found := make(map[string]bool)
	for _, gid := range effectiveGroups {
		found[gid] = true
	}
	for _, expected := range []string{groupA.ID, groupB.ID, groupC.ID} {
		if !found[expected] {
			t.Errorf("expected group %s in effective groups", expected)
		}
	}
}

func TestGetPoliciesForPrincipal(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	_ = srv // We just need the store

	// Create a policy
	policy := &store.Policy{
		ID:           "policy_forprinc",
		Name:         "Test Policy",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read"},
		Effect:       "allow",
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreatePolicy(ctx, policy); err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	// Bind to user
	if err := s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "user",
		PrincipalID:   "test_user",
	}); err != nil {
		t.Fatalf("failed to add binding: %v", err)
	}

	// Get policies for user
	policies, err := s.GetPoliciesForPrincipal(ctx, "user", "test_user")
	if err != nil {
		t.Fatalf("failed to get policies: %v", err)
	}

	if len(policies) != 1 {
		t.Errorf("expected 1 policy, got %d", len(policies))
	}
	if policies[0].ID != policy.ID {
		t.Errorf("expected policy ID %q, got %q", policy.ID, policies[0].ID)
	}
}
