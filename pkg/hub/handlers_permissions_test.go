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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// permSeedUser ensures a user row exists so that group-membership / policy-binding
// foreign keys resolve. The Ent store enforces user/agent FK edges that the
// former raw-SQL store did not, so fixtures must create referenced principals.
func permSeedUser(t *testing.T, ctx context.Context, s store.Store, id string) {
	t.Helper()
	err := s.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@example.com", DisplayName: "Seed User",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// permSeedAgent ensures an agent (and its required project) exists so that
// membership / binding foreign keys resolve.
func permSeedAgent(t *testing.T, ctx context.Context, s store.Store, id string) {
	t.Helper()
	projectID := tid("perm-agent-project")
	_ = s.CreateProject(ctx, &store.Project{ID: projectID, Name: "Perm Agent Project", Slug: "perm-agent-project"})
	err := s.CreateAgent(ctx, &store.Agent{
		ID: id, Name: "Seed Agent", Slug: "seed-agent-" + id[:8],
		ProjectID: projectID, Phase: "stopped",
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("seed agent %s: %v", id, err)
	}
}

// permSeedMember seeds the user or agent referenced by a group membership.
func permSeedMember(t *testing.T, ctx context.Context, s store.Store, m *store.GroupMember) {
	t.Helper()
	if m.MemberID == "" {
		return
	}
	if m.MemberType == store.GroupMemberTypeAgent {
		permSeedAgent(t, ctx, s, m.MemberID)
	} else {
		permSeedUser(t, ctx, s, m.MemberID)
	}
}

// permSeedPrincipal seeds the user or agent referenced by a policy binding.
// Group principals are created by the test itself, so they are skipped.
func permSeedPrincipal(t *testing.T, ctx context.Context, s store.Store, principalType, principalID string) {
	t.Helper()
	if principalID == "" || principalType == "group" {
		return
	}
	if principalType == "agent" {
		permSeedAgent(t, ctx, s, principalID)
	} else {
		permSeedUser(t, ctx, s, principalID)
	}
}

// ============================================================================
// Group Endpoint Tests
// ============================================================================

func TestGroupList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create some test groups
	for i := 0; i < 3; i++ {
		group := &store.Group{
			ID:      tid("group_" + string(rune('a'+i))),
			Name:    "Test Group " + string(rune('A'+i)),
			Slug:    tid("test-group-" + string(rune('a'+i))),
			Created: time.Now(),
			Updated: time.Now(),
		}
		if group.OwnerID != "" {
			permSeedUser(t, ctx, s, group.OwnerID)
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

	// 3 test groups + 1 seeded hub-members group = 4
	if len(resp.Groups) != 4 {
		t.Errorf("expected 4 groups (3 test + 1 seeded), got %d", len(resp.Groups))
	}

	if resp.TotalCount != 4 {
		t.Errorf("expected total 4, got %d", resp.TotalCount)
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
		ID:          tid("group_xyz123"),
		Name:        "Test Group",
		Slug:        "test-group",
		Description: "A test group",
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
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
		ID:      tid("group_upd123"),
		Name:    "Original Name",
		Slug:    "original-name",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
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
		ID:      tid("group_del123"),
		Name:    "Delete Me",
		Slug:    "delete-me",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
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
		ID:      tid("group_mem123"),
		Name:    "Test Group",
		Slug:    "test-group",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	// Create the user to be added as a member
	user := &store.User{
		ID:          tid("user_abc123"),
		Email:       "user@example.com",
		DisplayName: "Test User",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	body := AddGroupMemberRequest{
		MemberType: "user",
		MemberID:   tid("user_abc123"),
		Role:       "member",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+group.ID+"/members", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp GroupMemberInfo
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.MemberID != tid("user_abc123") {
		t.Errorf("expected memberId 'user_abc123', got %q", resp.MemberID)
	}
	if resp.DisplayName != "Test User" {
		t.Errorf("expected displayName 'Test User', got %q", resp.DisplayName)
	}
}

func TestGroupMembersAddByEmail(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      tid("group_email123"),
		Name:    "Test Group Email",
		Slug:    "test-group-email",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	// Create the user
	user := &store.User{
		ID:          tid("user_email_test"),
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Add by email address instead of ID
	body := AddGroupMemberRequest{
		MemberType: "user",
		MemberID:   "alice@example.com",
		Role:       "member",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+group.ID+"/members", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp GroupMemberInfo
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should resolve email to user ID
	if resp.MemberID != tid("user_email_test") {
		t.Errorf("expected memberId 'user_email_test', got %q", resp.MemberID)
	}
	if resp.DisplayName != "Alice" {
		t.Errorf("expected displayName 'Alice', got %q", resp.DisplayName)
	}
}

func TestGroupMembersAddByEmail_NotFound(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      tid("group_email_nf"),
		Name:    "Test Group",
		Slug:    "test-group-email-nf",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	body := AddGroupMemberRequest{
		MemberType: "user",
		MemberID:   "nobody@example.com",
		Role:       "member",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+group.ID+"/members", body)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupMembersAddGroupBySlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	parentGroup := &store.Group{
		ID:      tid("parent_grp"),
		Name:    "Parent Group",
		Slug:    "parent-group",
		Created: time.Now(),
		Updated: time.Now(),
	}
	childGroup := &store.Group{
		ID:      tid("child_grp"),
		Name:    "Child Group",
		Slug:    "child-group",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if parentGroup.OwnerID != "" {
		permSeedUser(t, ctx, s, parentGroup.OwnerID)
	}
	if err := s.CreateGroup(ctx, parentGroup); err != nil {
		t.Fatalf("failed to create parent group: %v", err)
	}
	if childGroup.OwnerID != "" {
		permSeedUser(t, ctx, s, childGroup.OwnerID)
	}
	if err := s.CreateGroup(ctx, childGroup); err != nil {
		t.Fatalf("failed to create child group: %v", err)
	}

	// Add child group by slug
	body := AddGroupMemberRequest{
		MemberType: "group",
		MemberID:   "child-group",
		Role:       "member",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+parentGroup.ID+"/members", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp GroupMemberInfo
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should resolve slug to group ID
	if resp.MemberID != tid("child_grp") {
		t.Errorf("expected memberId 'child_grp', got %q", resp.MemberID)
	}
	if resp.DisplayName != "Child Group" {
		t.Errorf("expected displayName 'Child Group', got %q", resp.DisplayName)
	}
}

func TestGroupMembersList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      tid("group_lst123"),
		Name:    "Test Group",
		Slug:    "test-group-list",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	// Add members
	for i := 0; i < 3; i++ {
		member := &store.GroupMember{
			GroupID:    group.ID,
			MemberType: "user",
			MemberID:   tid("user_" + string(rune('a'+i))),
			Role:       "member",
			AddedAt:    time.Now(),
		}
		permSeedMember(t, ctx, s, member)
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
		ID:      tid("group_rem123"),
		Name:    "Test Group",
		Slug:    "test-group-remove",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	member := &store.GroupMember{
		GroupID:    group.ID,
		MemberType: "user",
		MemberID:   tid("user_remove"),
		Role:       "member",
		AddedAt:    time.Now(),
	}
	permSeedMember(t, ctx, s, member)
	if err := s.AddGroupMember(ctx, member); err != nil {
		t.Fatalf("failed to add member: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groups/"+group.ID+"/members/user/"+tid("user_remove"), nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify removed
	_, err := s.GetGroupMembership(ctx, group.ID, "user", tid("user_remove"))
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGroupCycleDetection(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two groups
	groupA := &store.Group{
		ID:      tid("group_a"),
		Name:    "Group A",
		Slug:    "group-a",
		Created: time.Now(),
		Updated: time.Now(),
	}
	groupB := &store.Group{
		ID:      tid("group_b"),
		Name:    "Group B",
		Slug:    "group-b",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if groupA.OwnerID != "" {
		permSeedUser(t, ctx, s, groupA.OwnerID)
	}
	if err := s.CreateGroup(ctx, groupA); err != nil {
		t.Fatalf("failed to create group A: %v", err)
	}
	if groupB.OwnerID != "" {
		permSeedUser(t, ctx, s, groupB.OwnerID)
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

func TestGroupMembersAddAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project for the agent
	project := &store.Project{
		ID:   tid("project_agent_test"),
		Name: "Test Project",
		Slug: "test-project-agent",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create the agent
	agent := &store.Agent{
		ID:        tid("agent_abc123"),
		Name:      "Test Agent",
		Slug:      "test-agent-abc123",
		ProjectID: project.ID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	group := &store.Group{
		ID:      tid("group_agent123"),
		Name:    "Test Group",
		Slug:    "test-group-agent",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	body := AddGroupMemberRequest{
		MemberType: "agent",
		MemberID:   tid("agent_abc123"),
		Role:       "member",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+group.ID+"/members", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp GroupMemberInfo
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.MemberType != "agent" {
		t.Errorf("expected memberType 'agent', got %q", resp.MemberType)
	}
	if resp.MemberID != tid("agent_abc123") {
		t.Errorf("expected memberId 'agent_abc123', got %q", resp.MemberID)
	}
	if resp.DisplayName != "Test Agent" {
		t.Errorf("expected displayName 'Test Agent', got %q", resp.DisplayName)
	}
}

func TestGroupMemberRemoveAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:      tid("group_rmagent"),
		Name:    "Test Group",
		Slug:    "test-group-rm-agent",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	member := &store.GroupMember{
		GroupID:    group.ID,
		MemberType: "agent",
		MemberID:   tid("agent_remove"),
		Role:       "member",
		AddedAt:    time.Now(),
	}
	permSeedMember(t, ctx, s, member)
	if err := s.AddGroupMember(ctx, member); err != nil {
		t.Fatalf("failed to add member: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groups/"+group.ID+"/members/agent/"+tid("agent_remove"), nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify removed
	_, err := s.GetGroupMembership(ctx, group.ID, "agent", tid("agent_remove"))
	if err != store.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGroupCreateWithGroupType(t *testing.T) {
	srv, _ := testServer(t)

	// Default type (explicit) should succeed
	body := CreateGroupRequest{
		Name: "Explicit Group",
		Slug: "explicit-group",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", body)
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var group store.Group
	if err := json.NewDecoder(rec.Body).Decode(&group); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if group.GroupType != "explicit" {
		t.Errorf("expected groupType 'explicit', got %q", group.GroupType)
	}
}

func TestGroupCreateProjectAgentsRejected(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateGroupRequest{
		Name:      "Project Group",
		Slug:      "project-group",
		GroupType: "project_agents",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for project_agents creation, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupCreateInvalidGroupType(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateGroupRequest{
		Name:      "Bad Type",
		Slug:      "bad-type",
		GroupType: "invalid",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid groupType, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupListWithGroupTypeFilter(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create groups with different (or default) types
	g1 := &store.Group{
		ID:        tid("group_explicit_1"),
		Name:      "Explicit 1",
		Slug:      "explicit-1",
		GroupType: "explicit",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	g2 := &store.Group{
		ID:        tid("group_explicit_2"),
		Name:      "Explicit 2",
		Slug:      "explicit-2",
		GroupType: "explicit",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	for _, g := range []*store.Group{g1, g2} {
		if g.OwnerID != "" {
			permSeedUser(t, ctx, s, g.OwnerID)
		}
		if err := s.CreateGroup(ctx, g); err != nil {
			t.Fatalf("failed to create group: %v", err)
		}
	}

	// Filter by groupType=explicit
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groups?groupType=explicit", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListGroupsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	// 2 test explicit groups + 1 seeded hub-members explicit group = 3
	if len(resp.Groups) != 3 {
		t.Errorf("expected 3 groups (2 test + 1 seeded), got %d", len(resp.Groups))
	}
}

func TestGroupDeleteProjectAgentsRejected(t *testing.T) {
	// This test requires the Ent-backed store to persist GroupType.
	// The legacy SQLite store has no group_type column, so GroupType
	// always defaults to "explicit" on read. This test validates the
	// handler logic which is exercised via the entadapter tests.
	t.Skip("requires Ent-backed store (GroupType not persisted in legacy SQLite)")
}

// ============================================================================
// Group Authorization Tests
// ============================================================================

// doGroupRequestAsUser creates a user token and performs an HTTP request as that user.
// This is a local copy to avoid depending on testify in this file.
func doGroupRequestAsUser(t *testing.T, srv *Server, user *store.User, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestGroupUpdateAuthz_OwnerAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user_owner_upd"),
		Email:       "owner@example.com",
		DisplayName: "Owner",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, owner); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	group := &store.Group{
		ID:      tid("group_authz_upd"),
		Name:    "Owned Group",
		Slug:    "owned-group-upd",
		OwnerID: owner.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	body := UpdateGroupRequest{Name: "Renamed"}
	rec := doGroupRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/groups/"+group.ID, body)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for owner update, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupUpdateAuthz_NonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	other := &store.User{
		ID:          tid("user_other_upd"),
		Email:       "other@example.com",
		DisplayName: "Other",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, other); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	group := &store.Group{
		ID:      tid("group_authz_upd2"),
		Name:    "Someone Else Group",
		Slug:    "someone-else-upd",
		OwnerID: tid("user_someone_else"),
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	body := UpdateGroupRequest{Name: "Hacked"}
	rec := doGroupRequestAsUser(t, srv, other, http.MethodPatch, "/api/v1/groups/"+group.ID, body)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner update, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupDeleteAuthz_NonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	other := &store.User{
		ID:          tid("user_other_del"),
		Email:       "other-del@example.com",
		DisplayName: "Other",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, other); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	group := &store.Group{
		ID:      tid("group_authz_del"),
		Name:    "Protected Group",
		Slug:    "protected-group",
		OwnerID: tid("user_someone_else"),
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	rec := doGroupRequestAsUser(t, srv, other, http.MethodDelete, "/api/v1/groups/"+group.ID, nil)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner delete, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupAddMemberAuthz_OwnerAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user_owner_add"),
		Email:       "owner-add@example.com",
		DisplayName: "Owner",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	memberUser := &store.User{
		ID:          tid("user_to_add"),
		Email:       "toadd@example.com",
		DisplayName: "To Add",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	for _, u := range []*store.User{owner, memberUser} {
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
	}

	group := &store.Group{
		ID:      tid("group_authz_add"),
		Name:    "Owned Group",
		Slug:    "owned-group-add",
		OwnerID: owner.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	body := AddGroupMemberRequest{
		MemberType: "user",
		MemberID:   memberUser.ID,
		Role:       "member",
	}
	rec := doGroupRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/groups/"+group.ID+"/members", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected 201 for owner add member, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupAddMemberAuthz_NonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	other := &store.User{
		ID:          tid("user_other_add"),
		Email:       "other-add@example.com",
		DisplayName: "Other",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, other); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	group := &store.Group{
		ID:      tid("group_authz_add2"),
		Name:    "Protected Group",
		Slug:    "protected-group-add",
		OwnerID: tid("user_someone_else"),
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	body := AddGroupMemberRequest{
		MemberType: "user",
		MemberID:   other.ID,
		Role:       "member",
	}
	rec := doGroupRequestAsUser(t, srv, other, http.MethodPost, "/api/v1/groups/"+group.ID+"/members", body)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner add member, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupRemoveMemberAuthz_NonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	other := &store.User{
		ID:          tid("user_other_rm"),
		Email:       "other-rm@example.com",
		DisplayName: "Other",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, other); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	group := &store.Group{
		ID:      tid("group_authz_rm"),
		Name:    "Protected Group",
		Slug:    "protected-group-rm",
		OwnerID: tid("user_someone_else"),
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	// Add a member directly via store
	member := &store.GroupMember{
		GroupID:    group.ID,
		MemberType: "user",
		MemberID:   tid("user_existing"),
		Role:       "member",
		AddedAt:    time.Now(),
	}
	permSeedMember(t, ctx, s, member)
	if err := s.AddGroupMember(ctx, member); err != nil {
		t.Fatalf("failed to add member: %v", err)
	}

	rec := doGroupRequestAsUser(t, srv, other, http.MethodDelete, "/api/v1/groups/"+group.ID+"/members/user/"+tid("user_existing"), nil)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner remove member, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGroupRemoveMemberAuthz_OwnerAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user_owner_rm"),
		Email:       "owner-rm@example.com",
		DisplayName: "Owner",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, owner); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	group := &store.Group{
		ID:      tid("group_authz_rm2"),
		Name:    "Owned Group",
		Slug:    "owned-group-rm",
		OwnerID: owner.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if group.OwnerID != "" {
		permSeedUser(t, ctx, s, group.OwnerID)
	}
	if err := s.CreateGroup(ctx, group); err != nil {
		t.Fatalf("failed to create group: %v", err)
	}

	member := &store.GroupMember{
		GroupID:    group.ID,
		MemberType: "user",
		MemberID:   tid("user_to_remove"),
		Role:       "member",
		AddedAt:    time.Now(),
	}
	permSeedMember(t, ctx, s, member)
	if err := s.AddGroupMember(ctx, member); err != nil {
		t.Fatalf("failed to add member: %v", err)
	}

	rec := doGroupRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/groups/"+group.ID+"/members/user/"+tid("user_to_remove"), nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for owner remove member, got %d: %s", rec.Code, rec.Body.String())
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
			ID:           tid("policy_" + string(rune('a'+i))),
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

	// 3 test policies + 5 seeded policies (hub-member-read-{user,group,template,harness-config}, hub-member-create-projects) = 8
	if len(resp.Policies) != 8 {
		t.Errorf("expected 8 policies (3 test + 5 seeded), got %d", len(resp.Policies))
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
		ID:           tid("policy_get123"),
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
		ID:           tid("policy_upd123"),
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
		ID:           tid("policy_del123"),
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
		ID:           tid("policy_bind123"),
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
		PrincipalID:   tid("user_abc123"),
	}
	permSeedUser(t, ctx, s, tid("user_abc123"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies/"+policy.ID+"/bindings", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.PolicyBinding
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.PrincipalID != tid("user_abc123") {
		t.Errorf("expected principalId 'user_abc123', got %q", resp.PrincipalID)
	}
}

func TestPolicyBindingsList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	policy := &store.Policy{
		ID:           tid("policy_blst123"),
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
			PrincipalID:   tid("user_" + string(rune('a'+i))),
		}
		permSeedPrincipal(t, ctx, s, binding.PrincipalType, binding.PrincipalID)
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
		ID:           tid("policy_brem123"),
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
		PrincipalID:   tid("user_remove"),
	}
	permSeedPrincipal(t, ctx, s, binding.PrincipalType, binding.PrincipalID)
	if err := s.AddPolicyBinding(ctx, binding); err != nil {
		t.Fatalf("failed to add binding: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/policies/"+policy.ID+"/bindings/user/"+tid("user_remove"), nil)

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
		ID:      tid("group_eff_a"),
		Name:    "Group A",
		Slug:    "group-eff-a",
		Created: time.Now(),
		Updated: time.Now(),
	}
	groupB := &store.Group{
		ID:      tid("group_eff_b"),
		Name:    "Group B",
		Slug:    "group-eff-b",
		Created: time.Now(),
		Updated: time.Now(),
	}
	groupC := &store.Group{
		ID:      tid("group_eff_c"),
		Name:    "Group C",
		Slug:    "group-eff-c",
		Created: time.Now(),
		Updated: time.Now(),
	}

	for _, g := range []*store.Group{groupA, groupB, groupC} {
		if g.OwnerID != "" {
			permSeedUser(t, ctx, s, g.OwnerID)
		}
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
	permSeedMember(t, ctx, s, &store.GroupMember{
		GroupID:    groupC.ID,
		MemberType: "user",
		MemberID:   tid("test_user"),
		Role:       "member",
		AddedAt:    time.Now(),
	})
	if err := s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupC.ID,
		MemberType: "user",
		MemberID:   tid("test_user"),
		Role:       "member",
		AddedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("failed to add user to C: %v", err)
	}

	// Get effective groups for user
	effectiveGroups, err := s.GetEffectiveGroups(ctx, tid("test_user"))
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
		ID:           tid("policy_forprinc"),
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
	permSeedPrincipal(t, ctx, s, "user", tid("test_user"))
	if err := s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "user",
		PrincipalID:   tid("test_user"),
	}); err != nil {
		t.Fatalf("failed to add binding: %v", err)
	}

	// Get policies for user
	policies, err := s.GetPoliciesForPrincipal(ctx, "user", tid("test_user"))
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
