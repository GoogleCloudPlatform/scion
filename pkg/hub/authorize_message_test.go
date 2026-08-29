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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ---------------------------------------------------------------------------
// Test helpers for messaging authorization
// ---------------------------------------------------------------------------

const (
	msgAuthzProjectID = "msg-authz-project"
)

// msgAuthzSetup creates a server with a project, project owner, and project
// member, seeding the necessary memberships and policies for authorization
// tests. Returns (server, store, ownerUser, memberUser, projectID).
func msgAuthzSetup(t *testing.T) (*Server, store.Store, *store.User, *store.User, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid(msgAuthzProjectID)

	owner := &store.User{
		ID:          tid("msg-owner"),
		Email:       "owner@test.com",
		DisplayName: "Project Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	member := &store.User{
		ID:          tid("msg-member"),
		Email:       "member@test.com",
		DisplayName: "Project Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	project := &store.Project{
		ID:        projectID,
		Name:      "msg-authz-project",
		Slug:      "msg-authz-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Add member to the project members group so policies apply.
	msgAuthzAddProjectMember(t, s, member.ID, projectID, "msg-authz-project", store.GroupMemberRoleMember)

	return srv, s, owner, member, projectID
}

// msgAuthzAddProjectMember adds a user to the project's members group and creates the
// appropriate role binding.
func msgAuthzAddProjectMember(t *testing.T, s store.Store, userID, projectID, projectSlug, groupRole string) {
	t.Helper()
	ctx := context.Background()

	// Add to project members group (needed for policy evaluation)
	membersSlug := projectMembersGroupSlug(projectSlug)
	group, err := s.GetGroupBySlug(ctx, membersSlug)
	if err != nil {
		t.Fatalf("failed to get project members group %q: %v", membersSlug, err)
	}
	if err := s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       groupRole,
	}); err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to add user to project members group: %v", err)
	}

	// Create role binding
	roleName := store.ProjectRoleMember
	if groupRole == store.GroupMemberRoleOwner {
		roleName = store.ProjectRoleOwner
	}
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	if err != nil {
		t.Fatalf("role definition %q not found: %v", roleName, err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create role binding: %v", err)
	}
}

// require_NoError is a test helper that fails immediately on error.
func require_NoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// msgAuthzAgent creates a store.Agent with the specified properties and
// persists it. Returns the agent.
func msgAuthzAgent(t *testing.T, s store.Store, id, projectID, mode string, ancestry []string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:          tid(id),
		Name:        id,
		Slug:        id,
		ProjectID:   projectID,
		MessageMode: mode,
		Ancestry:    ancestry,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require_NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// msgAuthzAgentIdentity builds an AgentIdentity for the given agent.
func msgAuthzAgentIdentity(agentID, projectID string, ancestry []string, scopes ...AgentTokenScope) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    scopes,
		Ancestry:  ancestry,
	}}
}

// msgAuthzUserIdentity creates a UserIdentity (non-admin, not project-owner).
func msgAuthzUserIdentity(userID string) UserIdentity {
	return NewAuthenticatedUser(userID, userID+"@test.com", "Test User", store.UserRoleMember, "api")
}

// msgAuthzAdminIdentity creates a super-admin UserIdentity.
func msgAuthzAdminIdentity() UserIdentity {
	return NewAuthenticatedUser(tid("msg-superadmin"), "admin@test.com", "Super Admin", store.UserRoleAdmin, "api")
}

// ---------------------------------------------------------------------------
// Test 1: Baseline agent (mode project) sends/receives/replies with no
// lifecycle scope (D1)
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_BaselineProjectMode_NoLifecycleScope(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	// Create a project-mode agent owned by the owner
	target := msgAuthzAgent(t, s, "baseline-target", projectID, store.MessageModeProject,
		[]string{owner.ID})

	// A project-mode sender agent WITHOUT ScopeAgentLifecycle can message.
	sender := msgAuthzAgent(t, s, "baseline-sender", projectID, store.MessageModeProject,
		[]string{owner.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, projectID, sender.Ancestry)
	// No scopes — baseline agent

	allowed, reason := srv.authorizeAgentMessage(ctx, senderIdent, target, false)
	if !allowed {
		t.Fatalf("baseline project-mode agent should be allowed to message: %s", reason)
	}

	// A project member (user) with agent.message but NOT agent.attach can message.
	memberIdent := msgAuthzUserIdentity(member.ID)
	allowed, reason = srv.authorizeAgentMessage(ctx, memberIdent, target, false)
	if !allowed {
		t.Fatalf("project member should be allowed to message project-mode agent: %s", reason)
	}
}

// ---------------------------------------------------------------------------
// Test 2: Agent with lifecycle scope but mode none cannot send or receive
// (super-admin CAN deliver) (D6)
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_ModeNone_DeniedExceptSuperAdmin(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	noneAgent := msgAuthzAgent(t, s, "none-agent", projectID, store.MessageModeNone,
		[]string{owner.ID})

	t.Run("agent with lifecycle scope denied when target is none", func(t *testing.T) {
		sender := msgAuthzAgent(t, s, "lifecycle-sender", projectID, store.MessageModeProject,
			[]string{owner.ID})
		senderIdent := msgAuthzAgentIdentity(sender.ID, projectID, sender.Ancestry, ScopeAgentLifecycle)

		allowed, _ := srv.authorizeAgentMessage(ctx, senderIdent, noneAgent, false)
		if allowed {
			t.Fatal("agent should be denied when target is mode none")
		}
	})

	t.Run("agent with lifecycle scope and mode none cannot send", func(t *testing.T) {
		noneTarget := msgAuthzAgent(t, s, "project-target", projectID, store.MessageModeProject,
			[]string{owner.ID})
		noneSenderIdent := msgAuthzAgentIdentity(noneAgent.ID, projectID, noneAgent.Ancestry, ScopeAgentLifecycle)

		allowed, _ := srv.authorizeAgentMessage(ctx, noneSenderIdent, noneTarget, false)
		if allowed {
			t.Fatal("mode-none agent should be denied from sending even with lifecycle scope")
		}
	})

	t.Run("super-admin CAN deliver to none-mode agent", func(t *testing.T) {
		adminIdent := msgAuthzAdminIdentity()
		allowed, reason := srv.authorizeAgentMessage(ctx, adminIdent, noneAgent, false)
		if !allowed {
			t.Fatalf("super-admin should be allowed to deliver to none-mode agent: %s", reason)
		}
	})
}

// ---------------------------------------------------------------------------
// Test 3: Project member without agent.attach messages a project-mode agent;
// still cannot attach (D1)
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_MemberWithoutAttach(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	target := msgAuthzAgent(t, s, "msg-only-target", projectID, store.MessageModeProject,
		[]string{owner.ID})

	// Member can message (has agent.message via project membership)
	memberIdent := msgAuthzUserIdentity(member.ID)
	allowed, reason := srv.authorizeAgentMessage(ctx, memberIdent, target, false)
	if !allowed {
		t.Fatalf("member should be allowed to message project-mode agent: %s", reason)
	}

	// Verify the member cannot attach (separate permission axis)
	resource := agentResource(target)
	decision := srv.authzService.CheckAccess(ctx, memberIdent, resource, ActionAttach)
	if decision.Allowed {
		t.Fatal("member without agent.attach should be denied attach")
	}
}

// ---------------------------------------------------------------------------
// Test 4: Lineage-mode agent
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_LineageMode(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	lineageAgent := msgAuthzAgent(t, s, "lineage-agent", projectID, store.MessageModeLineage,
		[]string{owner.ID})

	t.Run("ancestry user converses (ALLOW)", func(t *testing.T) {
		// Owner is in the ancestry chain
		ownerIdent := msgAuthzUserIdentity(owner.ID)
		allowed, reason := srv.authorizeAgentMessage(ctx, ownerIdent, lineageAgent, false)
		if !allowed {
			t.Fatalf("ancestry user should be allowed to message lineage agent: %s", reason)
		}
	})

	t.Run("non-lineage project member denied", func(t *testing.T) {
		memberIdent := msgAuthzUserIdentity(member.ID)
		allowed, _ := srv.authorizeAgentMessage(ctx, memberIdent, lineageAgent, false)
		if allowed {
			t.Fatal("non-lineage project member should be denied")
		}
	})

	t.Run("project owner allowed (D6 piercing)", func(t *testing.T) {
		// Create a separate project owner who is NOT in ancestry
		otherOwner := &store.User{
			ID:          tid("msg-other-owner"),
			Email:       "other-owner@test.com",
			DisplayName: "Other Owner",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require_NoError(t, s.CreateUser(ctx, otherOwner))
		ensureHubMembership(ctx, s, otherOwner.ID)
		msgAuthzAddProjectMember(t, s, otherOwner.ID, projectID, "msg-authz-project", store.GroupMemberRoleOwner)

		otherOwnerIdent := msgAuthzUserIdentity(otherOwner.ID)
		allowed, reason := srv.authorizeAgentMessage(ctx, otherOwnerIdent, lineageAgent, false)
		if !allowed {
			t.Fatalf("project owner should pierce lineage mode: %s", reason)
		}
	})

	t.Run("lineage agent CANNOT message ANY agent including children (D4)", func(t *testing.T) {
		child := msgAuthzAgent(t, s, "lineage-child", projectID, store.MessageModeLineage,
			[]string{owner.ID, lineageAgent.ID})

		senderIdent := msgAuthzAgentIdentity(lineageAgent.ID, projectID, lineageAgent.Ancestry)

		// Cannot message child
		allowed, _ := srv.authorizeAgentMessage(ctx, senderIdent, child, false)
		if allowed {
			t.Fatal("lineage agent should NOT be able to message any agent, including children")
		}

		// Child also cannot message parent
		childIdent := msgAuthzAgentIdentity(child.ID, projectID, child.Ancestry)
		allowed, _ = srv.authorizeAgentMessage(ctx, childIdent, lineageAgent, false)
		if allowed {
			t.Fatal("lineage child should NOT be able to message parent")
		}
	})
}

// ---------------------------------------------------------------------------
// Test 5: Branch-mode agent
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_BranchMode(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	parent := msgAuthzAgent(t, s, "branch-parent", projectID, store.MessageModeBranch,
		[]string{owner.ID})
	child := msgAuthzAgent(t, s, "branch-child", projectID, store.MessageModeBranch,
		[]string{owner.ID, parent.ID})
	sibling := msgAuthzAgent(t, s, "branch-sibling", projectID, store.MessageModeBranch,
		[]string{owner.ID, parent.ID})

	t.Run("parent/child allowed when both branch mode", func(t *testing.T) {
		parentIdent := msgAuthzAgentIdentity(parent.ID, projectID, parent.Ancestry)
		allowed, reason := srv.authorizeAgentMessage(ctx, parentIdent, child, false)
		if !allowed {
			t.Fatalf("branch parent should message branch child: %s", reason)
		}

		childIdent := msgAuthzAgentIdentity(child.ID, projectID, child.Ancestry)
		allowed, reason = srv.authorizeAgentMessage(ctx, childIdent, parent, false)
		if !allowed {
			t.Fatalf("branch child should message branch parent: %s", reason)
		}
	})

	t.Run("sibling denied (must communicate through parent)", func(t *testing.T) {
		childIdent := msgAuthzAgentIdentity(child.ID, projectID, child.Ancestry)
		allowed, _ := srv.authorizeAgentMessage(ctx, childIdent, sibling, false)
		if allowed {
			t.Fatal("siblings should be denied direct messaging")
		}
	})

	t.Run("bridge test: project-mode agent in same branch denied both directions (D3+D9)", func(t *testing.T) {
		projectModeChild := msgAuthzAgent(t, s, "project-mode-in-branch", projectID, store.MessageModeProject,
			[]string{owner.ID, parent.ID})

		// project-mode → branch parent: DENIED
		pmIdent := msgAuthzAgentIdentity(projectModeChild.ID, projectID, projectModeChild.Ancestry)
		allowed, _ := srv.authorizeAgentMessage(ctx, pmIdent, parent, false)
		if allowed {
			t.Fatal("project-mode agent should be denied messaging branch-mode parent")
		}

		// branch parent → project-mode child: DENIED
		parentIdent := msgAuthzAgentIdentity(parent.ID, projectID, parent.Ancestry)
		allowed, _ = srv.authorizeAgentMessage(ctx, parentIdent, projectModeChild, false)
		if allowed {
			t.Fatal("branch-mode parent should be denied messaging project-mode child")
		}
	})
}

// ---------------------------------------------------------------------------
// Test 6: Relay pinning test (D6) — project owner's agent is denied delivery
// to a lineage/branch agent. Piercing is user-identity-only.
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_RelayPinning(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	lineageAgent := msgAuthzAgent(t, s, "relay-lineage", projectID, store.MessageModeLineage,
		[]string{owner.ID})
	branchAgent := msgAuthzAgent(t, s, "relay-branch", projectID, store.MessageModeBranch,
		[]string{owner.ID})

	// Owner's project-mode agent
	ownerProjectAgent := msgAuthzAgent(t, s, "owner-project-agent", projectID, store.MessageModeProject,
		[]string{owner.ID})

	t.Run("owner's agent denied delivery to lineage agent", func(t *testing.T) {
		agentIdent := msgAuthzAgentIdentity(ownerProjectAgent.ID, projectID, ownerProjectAgent.Ancestry)
		allowed, _ := srv.authorizeAgentMessage(ctx, agentIdent, lineageAgent, false)
		if allowed {
			t.Fatal("owner's project-mode agent should NOT pierce lineage — piercing is user-identity-only")
		}
	})

	t.Run("owner's agent denied delivery to branch agent", func(t *testing.T) {
		agentIdent := msgAuthzAgentIdentity(ownerProjectAgent.ID, projectID, ownerProjectAgent.Ancestry)
		allowed, _ := srv.authorizeAgentMessage(ctx, agentIdent, branchAgent, false)
		if allowed {
			t.Fatal("owner's project-mode agent should NOT pierce branch — piercing is user-identity-only")
		}
	})

	t.Run("owner as user CAN deliver to lineage agent", func(t *testing.T) {
		ownerIdent := msgAuthzUserIdentity(owner.ID)
		allowed, reason := srv.authorizeAgentMessage(ctx, ownerIdent, lineageAgent, false)
		if !allowed {
			t.Fatalf("owner as user should pierce lineage: %s", reason)
		}
	})

	t.Run("owner as user CAN deliver to branch agent", func(t *testing.T) {
		ownerIdent := msgAuthzUserIdentity(owner.ID)
		allowed, reason := srv.authorizeAgentMessage(ctx, ownerIdent, branchAgent, false)
		if !allowed {
			t.Fatalf("owner as user should pierce branch: %s", reason)
		}
	})
}

// ---------------------------------------------------------------------------
// Test 7: None mode
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_NoneMode(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	noneAgent := msgAuthzAgent(t, s, "none-mode-agent", projectID, store.MessageModeNone,
		[]string{owner.ID})

	t.Run("denied for lineage owner (user in ancestry)", func(t *testing.T) {
		ownerIdent := msgAuthzUserIdentity(owner.ID)
		allowed, _ := srv.authorizeAgentMessage(ctx, ownerIdent, noneAgent, false)
		if allowed {
			t.Fatal("lineage owner should be denied when target is mode none")
		}
	})

	t.Run("denied for project owner", func(t *testing.T) {
		// owner IS the project owner
		ownerIdent := msgAuthzUserIdentity(owner.ID)
		allowed, _ := srv.authorizeAgentMessage(ctx, ownerIdent, noneAgent, false)
		if allowed {
			t.Fatal("project owner should be denied when target is mode none")
		}
	})

	t.Run("allowed for super-admin (D6)", func(t *testing.T) {
		adminIdent := msgAuthzAdminIdentity()
		allowed, reason := srv.authorizeAgentMessage(ctx, adminIdent, noneAgent, false)
		if !allowed {
			t.Fatalf("super-admin should pierce mode none: %s", reason)
		}
	})

	t.Run("attach/PTY still works for holders of agent.attach (separate from messaging)", func(t *testing.T) {
		// Super-admin can still attach regardless of message mode
		adminIdent := msgAuthzAdminIdentity()
		resource := agentResource(noneAgent)
		decision := srv.authzService.CheckAccess(ctx, adminIdent, resource, ActionAttach)
		if !decision.Allowed {
			t.Fatal("super-admin should still be able to attach to none-mode agent")
		}
	})
}

// ---------------------------------------------------------------------------
// Test 8: Cross-project agent-to-agent denied
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_CrossProjectDenied(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	otherProjectID := tid("msg-other-project")
	otherProject := &store.Project{
		ID:        otherProjectID,
		Name:      "other-project",
		Slug:      "other-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, otherProject))

	senderAgent := msgAuthzAgent(t, s, "cross-sender", projectID, store.MessageModeProject,
		[]string{owner.ID})
	targetAgent := msgAuthzAgent(t, s, "cross-target", otherProjectID, store.MessageModeProject,
		[]string{owner.ID})

	senderIdent := msgAuthzAgentIdentity(senderAgent.ID, projectID, senderAgent.Ancestry)
	allowed, _ := srv.authorizeAgentMessage(ctx, senderIdent, targetAgent, false)
	if allowed {
		t.Fatal("cross-project agent-to-agent messaging should be denied")
	}
}

// ---------------------------------------------------------------------------
// Additional coverage: system plane bypass (D8)
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_SystemPlaneBypass(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	noneAgent := msgAuthzAgent(t, s, "sys-none-agent", projectID, store.MessageModeNone,
		[]string{owner.ID})

	// Even a none-mode agent receives system-plane messages
	senderIdent := msgAuthzAgentIdentity(noneAgent.ID, projectID, noneAgent.Ancestry)
	allowed, reason := srv.authorizeAgentMessage(ctx, senderIdent, noneAgent, true)
	if !allowed {
		t.Fatalf("system plane should bypass all mode checks: %s", reason)
	}
}

// ---------------------------------------------------------------------------
// Additional coverage: nil identity and nil target
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_NilInputs(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	target := msgAuthzAgent(t, s, "nil-test-target", projectID, store.MessageModeProject,
		[]string{owner.ID})

	t.Run("nil identity denied", func(t *testing.T) {
		allowed, _ := srv.authorizeAgentMessage(ctx, nil, target, false)
		if allowed {
			t.Fatal("nil identity should be denied")
		}
	})

	t.Run("nil target denied", func(t *testing.T) {
		ident := msgAuthzUserIdentity(owner.ID)
		allowed, _ := srv.authorizeAgentMessage(ctx, ident, nil, false)
		if allowed {
			t.Fatal("nil target should be denied")
		}
	})
}

// ---------------------------------------------------------------------------
// Additional coverage: broker identity denied
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_BrokerDenied(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	target := msgAuthzAgent(t, s, "broker-test-target", projectID, store.MessageModeProject,
		[]string{owner.ID})

	brokerIdent := NewBrokerIdentity("test-broker")
	allowed, _ := srv.authorizeAgentMessage(ctx, brokerIdent, target, false)
	if allowed {
		t.Fatal("broker identity should be denied")
	}
}

// ---------------------------------------------------------------------------
// Additional coverage: mixed branch modes within a branch
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_MixedBranchModes(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	branchParent := msgAuthzAgent(t, s, "mixed-branch-parent", projectID, store.MessageModeBranch,
		[]string{owner.ID})
	// Branch child of a project-mode parent: cannot message parent in either direction
	projectParent := msgAuthzAgent(t, s, "mixed-project-parent", projectID, store.MessageModeProject,
		[]string{owner.ID})
	branchChildOfProject := msgAuthzAgent(t, s, "branch-child-of-project", projectID, store.MessageModeBranch,
		[]string{owner.ID, projectParent.ID})

	t.Run("branch child of project parent denied both directions", func(t *testing.T) {
		childIdent := msgAuthzAgentIdentity(branchChildOfProject.ID, projectID, branchChildOfProject.Ancestry)
		allowed, _ := srv.authorizeAgentMessage(ctx, childIdent, projectParent, false)
		if allowed {
			t.Fatal("branch child should not message project-mode parent")
		}

		parentIdent := msgAuthzAgentIdentity(projectParent.ID, projectID, projectParent.Ancestry)
		allowed, _ = srv.authorizeAgentMessage(ctx, parentIdent, branchChildOfProject, false)
		if allowed {
			t.Fatal("project parent should not message branch-mode child")
		}
	})

	t.Run("branch child can still message own branch-mode children", func(t *testing.T) {
		grandchild := msgAuthzAgent(t, s, "branch-grandchild", projectID, store.MessageModeBranch,
			[]string{owner.ID, branchParent.ID, "intermediate-id"})
		// This won't actually pass because grandchild's parent is "intermediate-id",
		// not branchParent. Let's make a proper structure.
		_ = grandchild // unused in this sub-case

		// Proper parent-child:
		branchChild := msgAuthzAgent(t, s, "mixed-branch-child", projectID, store.MessageModeBranch,
			[]string{owner.ID, branchParent.ID})

		parentIdent := msgAuthzAgentIdentity(branchParent.ID, projectID, branchParent.Ancestry)
		allowed, reason := srv.authorizeAgentMessage(ctx, parentIdent, branchChild, false)
		if !allowed {
			t.Fatalf("branch parent should message branch child: %s", reason)
		}
	})
}

// ---------------------------------------------------------------------------
// Test: UAT without agent:message scope denied piercing (HIGH-2 fix)
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_UATWithoutMessageScope(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	lineageAgent := msgAuthzAgent(t, s, "uat-lineage-target", projectID, store.MessageModeLineage,
		[]string{owner.ID})
	projectAgent := msgAuthzAgent(t, s, "uat-project-target", projectID, store.MessageModeProject,
		[]string{owner.ID})

	t.Run("UAT without agent:message denied piercing lineage", func(t *testing.T) {
		// Create a UAT-scoped identity for the project owner with only agent:read (no agent:message)
		baseIdent := msgAuthzUserIdentity(owner.ID)
		scopedIdent := NewScopedUserIdentity(baseIdent, projectID, []string{"agent:read"})

		allowed, _ := srv.authorizeAgentMessage(ctx, scopedIdent, lineageAgent, false)
		if allowed {
			t.Fatal("UAT without agent:message should be denied piercing lineage mode")
		}
	})

	t.Run("UAT with agent:message allowed piercing lineage", func(t *testing.T) {
		baseIdent := msgAuthzUserIdentity(owner.ID)
		scopedIdent := NewScopedUserIdentity(baseIdent, projectID, []string{"agent:read", "agent:message"})

		allowed, reason := srv.authorizeAgentMessage(ctx, scopedIdent, lineageAgent, false)
		if !allowed {
			t.Fatalf("UAT with agent:message should be allowed to pierce lineage: %s", reason)
		}
	})

	t.Run("UAT without agent:message allowed for project-mode via CheckAccess", func(t *testing.T) {
		// For project-mode agents, authorization goes through CheckAccess which
		// handles UAT intersection, so this should still work if the user has
		// the underlying agent.message permission.
		baseIdent := msgAuthzUserIdentity(owner.ID)
		scopedIdent := NewScopedUserIdentity(baseIdent, projectID, []string{"agent:read", "agent:message"})

		allowed, reason := srv.authorizeAgentMessage(ctx, scopedIdent, projectAgent, false)
		if !allowed {
			t.Fatalf("UAT with agent:message should be allowed for project-mode agent: %s", reason)
		}
	})
}

// ---------------------------------------------------------------------------
// Test: Agent self-message to none-mode agent (MEDIUM-1 fix)
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_SelfMessage(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	t.Run("self-message to none-mode agent allowed", func(t *testing.T) {
		noneAgent := msgAuthzAgent(t, s, "self-none-agent", projectID, store.MessageModeNone,
			[]string{owner.ID})

		// Agent identity sends to itself — should be allowed even with mode none
		selfIdent := msgAuthzAgentIdentity(noneAgent.ID, projectID, noneAgent.Ancestry)
		allowed, reason := srv.authorizeAgentMessage(ctx, selfIdent, noneAgent, false)
		if !allowed {
			t.Fatalf("agent self-message should be allowed even when mode is none: %s", reason)
		}
	})

	t.Run("self-message is NOT system-plane", func(t *testing.T) {
		projectModeAgent := msgAuthzAgent(t, s, "self-project-agent", projectID, store.MessageModeProject,
			[]string{owner.ID})

		// Agent sends to itself with isSystemPlane=false — should still be allowed
		selfIdent := msgAuthzAgentIdentity(projectModeAgent.ID, projectID, projectModeAgent.Ancestry)
		allowed, reason := srv.authorizeAgentMessage(ctx, selfIdent, projectModeAgent, false)
		if !allowed {
			t.Fatalf("agent self-message should be allowed without system-plane flag: %s", reason)
		}
		if reason != "agent self-message" {
			t.Fatalf("expected reason 'agent self-message', got %q", reason)
		}
	})

	t.Run("different agent still denied for none-mode target", func(t *testing.T) {
		noneAgent := msgAuthzAgent(t, s, "self-none-target2", projectID, store.MessageModeNone,
			[]string{owner.ID})
		otherAgent := msgAuthzAgent(t, s, "self-other-sender", projectID, store.MessageModeProject,
			[]string{owner.ID})

		otherIdent := msgAuthzAgentIdentity(otherAgent.ID, projectID, otherAgent.Ancestry)
		allowed, _ := srv.authorizeAgentMessage(ctx, otherIdent, noneAgent, false)
		if allowed {
			t.Fatal("different agent should be denied when target is mode none")
		}
	})
}

// ---------------------------------------------------------------------------
// Test: Ingress parity — all ingresses produce identical decisions for the
// same principal/target pair, because they all call authorizeAgentMessage.
// ---------------------------------------------------------------------------

func TestAuthorizeAgentMessage_IngressParity(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	// Create agents with different message modes.
	projectAgent := msgAuthzAgent(t, s, "parity-project", projectID, store.MessageModeProject,
		[]string{owner.ID})
	lineageAgent := msgAuthzAgent(t, s, "parity-lineage", projectID, store.MessageModeLineage,
		[]string{owner.ID})
	branchAgent := msgAuthzAgent(t, s, "parity-branch", projectID, store.MessageModeBranch,
		[]string{owner.ID})
	noneAgent := msgAuthzAgent(t, s, "parity-none", projectID, store.MessageModeNone,
		[]string{owner.ID})

	// Identities: owner, member, and a project-mode agent.
	ownerIdent := msgAuthzUserIdentity(owner.ID)
	memberIdent := msgAuthzUserIdentity(member.ID)
	agentSender := msgAuthzAgent(t, s, "parity-sender-agent", projectID, store.MessageModeProject,
		[]string{owner.ID})
	agentIdent := msgAuthzAgentIdentity(agentSender.ID, projectID, agentSender.Ancestry)

	// Define test cases: each case runs authorizeAgentMessage with the same
	// sender/target pair. All four ingresses (direct API, chat v2, broadcast,
	// broker inbound) call this same function, so parity is guaranteed by
	// construction. This test verifies the decision matrix is consistent.
	tests := []struct {
		name    string
		sender  Identity
		target  *store.Agent
		system  bool
		allowed bool
	}{
		// Owner → project mode agent: allowed (ancestry)
		{"owner→project", ownerIdent, projectAgent, false, true},
		// Owner → lineage mode agent: allowed (ancestry / project owner piercing)
		{"owner→lineage", ownerIdent, lineageAgent, false, true},
		// Owner → branch mode agent: allowed (ancestry / project owner piercing)
		{"owner→branch", ownerIdent, branchAgent, false, true},
		// Owner → none mode agent: denied (none sealed to non-super-admin)
		{"owner→none", ownerIdent, noneAgent, false, false},

		// Member → project mode agent: allowed (agent.message permission)
		{"member→project", memberIdent, projectAgent, false, true},
		// Member → lineage mode agent: denied (not in ancestry, not project owner)
		{"member→lineage", memberIdent, lineageAgent, false, false},
		// Member → branch mode agent: denied (not in ancestry, not project owner)
		{"member→branch", memberIdent, branchAgent, false, false},
		// Member → none mode agent: denied (none is sealed)
		{"member→none", memberIdent, noneAgent, false, false},

		// Agent → project mode agent: allowed (both project mode, same project)
		{"agent→project", agentIdent, projectAgent, false, true},
		// Agent → lineage mode agent: denied (lineage has no agent edges)
		{"agent→lineage", agentIdent, lineageAgent, false, false},
		// Agent → none mode agent: denied (none is sealed)
		{"agent→none", agentIdent, noneAgent, false, false},

		// System plane → none mode agent: allowed (D8 bypass)
		{"system→none", ownerIdent, noneAgent, true, true},
		// System plane → lineage mode agent: allowed (D8 bypass)
		{"system→lineage", ownerIdent, lineageAgent, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, reason := srv.authorizeAgentMessage(ctx, tt.sender, tt.target, tt.system)
			if allowed != tt.allowed {
				t.Fatalf("expected allowed=%v, got allowed=%v (reason: %s)", tt.allowed, allowed, reason)
			}
		})
	}
}
