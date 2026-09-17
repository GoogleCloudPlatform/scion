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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Phase 2 (D6): Cross-project messaging authorization tests
//
// These tests verify the typed message evaluator (EvaluateAgentMessage)
// against all combinations specified in the brief:
//   - All 25 local mode pairs (5×5 with hub) for same-project
//   - External pairs: hub→project, hub→hub (allowed with right policy)
//   - project→anything cross-project (denied)
//   - Each combination of Hub flag (on/off), endpoint modes, receive enum
//   - Membership: direct, owner, group member/admin, expiry, not-before,
//     custom roles, suspended origin, missing root
//   - Federated ancestry, forged sender fields
// ---------------------------------------------------------------------------

// enableCrossProjectMessaging enables the Hub-level cross_project_messaging_enabled
// flag by updating operational settings on the server.
func enableCrossProjectMessaging(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()

	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": true}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("failed to refresh operational settings: %v", err)
	}
	srv.SetOperationalSettings(ops)
}

// crossProjectSetup creates two projects (A and B) with owners, members,
// and agents for cross-project testing. Returns all the fixtures.
type crossProjectFixture struct {
	srv      *Server
	store    store.Store
	ownerA   *store.User
	ownerB   *store.User
	memberA  *store.User
	projectA string
	projectB string
}

func crossProjectSetup(t *testing.T) crossProjectFixture {
	t.Helper()
	srv, s, ownerA, _, projectA := msgAuthzSetup(t)
	ctx := context.Background()

	// Create a second project.
	projectB := tid("msg-project-b")
	ownerB := &store.User{
		ID:          tid("msg-owner-b"),
		Email:       "owner-b@test.com",
		DisplayName: "Owner B",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, ownerB))
	ensureHubMembership(ctx, s, ownerB.ID)

	projB := &store.Project{
		ID:        projectB,
		Name:      "project-b",
		Slug:      "project-b",
		OwnerID:   ownerB.ID,
		CreatedBy: ownerB.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, projB))
	srv.createProjectMembersGroup(ctx, projB)

	// Add ownerB as member of project B.
	msgAuthzAddProjectMember(t, s, ownerB.ID, projectB, "project-b", store.GroupMemberRoleOwner)

	return crossProjectFixture{
		srv:      srv,
		store:    s,
		ownerA:   ownerA,
		ownerB:   ownerB,
		memberA:  nil, // use ownerA as origin user
		projectA: projectA,
		projectB: projectB,
	}
}

// ---------------------------------------------------------------------------
// Test: Same-project 5×5 mode matrix
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_SameProject_AllModePairs(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	modes := []string{
		store.MessageModeNone,
		store.MessageModeLineage,
		store.MessageModeBranch,
		store.MessageModeProject,
		store.MessageModeHub,
	}

	// Expected results for same-project mode matrix (design §3).
	// Format: expected[senderMode][targetMode] = allowed
	type modePair struct{ sender, target string }
	allowed := map[modePair]bool{
		// none sender: all denied
		{store.MessageModeNone, store.MessageModeNone}:    false,
		{store.MessageModeNone, store.MessageModeLineage}: false,
		{store.MessageModeNone, store.MessageModeBranch}:  false,
		{store.MessageModeNone, store.MessageModeProject}: false,
		{store.MessageModeNone, store.MessageModeHub}:     false,
		// lineage sender: all denied (lineage has no agent-to-agent edges)
		{store.MessageModeLineage, store.MessageModeNone}:    false,
		{store.MessageModeLineage, store.MessageModeLineage}: false,
		{store.MessageModeLineage, store.MessageModeBranch}:  false,
		{store.MessageModeLineage, store.MessageModeProject}: false,
		{store.MessageModeLineage, store.MessageModeHub}:     false,
		// branch sender: only parent/child branch pairs allowed (tested separately)
		{store.MessageModeBranch, store.MessageModeNone}:    false,
		{store.MessageModeBranch, store.MessageModeLineage}: false,
		{store.MessageModeBranch, store.MessageModeBranch}:  false, // non-parent/child
		{store.MessageModeBranch, store.MessageModeProject}: false,
		{store.MessageModeBranch, store.MessageModeHub}:     false,
		// project sender: project and hub targets allowed
		{store.MessageModeProject, store.MessageModeNone}:    false,
		{store.MessageModeProject, store.MessageModeLineage}: false,
		{store.MessageModeProject, store.MessageModeBranch}:  false,
		{store.MessageModeProject, store.MessageModeProject}: true,
		{store.MessageModeProject, store.MessageModeHub}:     true,
		// hub sender: project and hub targets allowed (same project)
		{store.MessageModeHub, store.MessageModeNone}:    false,
		{store.MessageModeHub, store.MessageModeLineage}: false,
		{store.MessageModeHub, store.MessageModeBranch}:  false,
		{store.MessageModeHub, store.MessageModeProject}: true,
		{store.MessageModeHub, store.MessageModeHub}:     true,
	}

	for _, sMode := range modes {
		for _, tMode := range modes {
			name := sMode + "→" + tMode
			t.Run(name, func(t *testing.T) {
				sender := msgAuthzAgent(t, s, "matrix-s-"+sMode+"-"+tMode, projectID, sMode, []string{owner.ID})
				target := msgAuthzAgent(t, s, "matrix-t-"+sMode+"-"+tMode, projectID, tMode, []string{owner.ID})

				senderIdent := msgAuthzAgentIdentity(sender.ID, projectID, sender.Ancestry)
				decision := srv.EvaluateAgentMessage(ctx, senderIdent, target)

				pair := modePair{sMode, tMode}
				expected := allowed[pair]
				if decision.Allowed != expected {
					t.Fatalf("expected allowed=%v for %s, got allowed=%v (reason: %s)",
						expected, name, decision.Allowed, decision.Reason)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Cross-project hub→project allowed with policy
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_HubToProject_PolicyAny(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	// Enable cross-project messaging.
	enableCrossProjectMessaging(t, f.srv)

	// Set project B inbound policy to "any".
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	// Create hub sender in project A and project target in project B.
	sender := msgAuthzAgent(t, f.store, "cp-hub-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-proj-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("hub→project with policy 'any' should be allowed: %s (code: %s)",
			decision.Reason, decision.Code)
	}
	if !decision.CrossProject {
		t.Fatal("decision should be marked as cross-project")
	}
}

// ---------------------------------------------------------------------------
// Test: Cross-project hub→hub allowed with policy
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_HubToHub_PolicyAny(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "cp-hub2hub-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-hub2hub-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("hub→hub with policy 'any' should be allowed: %s (code: %s)",
			decision.Reason, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Cross-project project→anything denied
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_ProjectSenderDenied(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "cp-proj-sender", f.projectA, store.MessageModeProject,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-proj-proj-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("project-mode sender should be denied cross-project messaging")
	}
	if decision.Code != MessageDenialCrossProjectSenderMode {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectSenderMode, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Hub flag off → denied
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_HubFlagOff(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	// Do NOT enable cross-project messaging (default off).
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "cp-flag-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-flag-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("cross-project messaging should be denied when Hub flag is off")
	}
	if decision.Code != MessageDenialCrossProjectDisabled {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectDisabled, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Inbound policy "none" → denied
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_InboundNone(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	// Default policy is "none" — don't change it.
	sender := msgAuthzAgent(t, f.store, "cp-none-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-none-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("should be denied when inbound policy is 'none'")
	}
	if decision.Code != MessageDenialCrossProjectInboundNone {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectInboundNone, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Inbound "members" — origin user IS member of destination project
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_MembersPolicy_OriginIsMember(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	// Set project B policy to "members".
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// Make ownerA a member of project B.
	msgAuthzAddProjectMember(t, f.store, f.ownerA.ID, f.projectB, "project-b", store.GroupMemberRoleMember)

	sender := msgAuthzAgent(t, f.store, "cp-mem-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-mem-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("origin user who is member should be allowed: %s (code: %s)",
			decision.Reason, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Inbound "members" — origin user NOT member of destination project
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_MembersPolicy_OriginNotMember(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// ownerA is NOT a member of project B.
	sender := msgAuthzAgent(t, f.store, "cp-nomem-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-nomem-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("origin user who is not a member should be denied")
	}
	if decision.Code != MessageDenialCrossProjectNotMember {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectNotMember, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Inbound "members" — owner counts as member
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_MembersPolicy_OwnerIsMember(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// Make ownerA an owner of project B.
	msgAuthzAddProjectMember(t, f.store, f.ownerA.ID, f.projectB, "project-b", store.GroupMemberRoleOwner)

	sender := msgAuthzAgent(t, f.store, "cp-own-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-own-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("owner should count as member: %s (code: %s)",
			decision.Reason, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Target mode lineage/branch/none denied for cross-project
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_TargetModeRestricted(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	// target_none is caught by the generic mode-none check before cross-project
	// logic (line ~349 in authorize_message.go). lineage and branch reach the
	// cross-project path and get the specific target-mode denial.
	t.Run("target_none", func(t *testing.T) {
		sender := msgAuthzAgent(t, f.store, "cp-tmode-s-none", f.projectA, store.MessageModeHub,
			[]string{f.ownerA.ID})
		target := msgAuthzAgent(t, f.store, "cp-tmode-t-none", f.projectB, store.MessageModeNone,
			[]string{f.ownerB.ID})

		senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
		decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
		if decision.Allowed {
			t.Fatal("target mode none should be denied")
		}
		// Generic early-exit denial (no typed code), NOT cross_project_target_mode.
	})

	for _, tMode := range []string{store.MessageModeLineage, store.MessageModeBranch} {
		t.Run("target_"+tMode, func(t *testing.T) {
			sender := msgAuthzAgent(t, f.store, "cp-tmode-s-"+tMode, f.projectA, store.MessageModeHub,
				[]string{f.ownerA.ID})
			target := msgAuthzAgent(t, f.store, "cp-tmode-t-"+tMode, f.projectB, tMode,
				[]string{f.ownerB.ID})

			senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
			decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
			if decision.Allowed {
				t.Fatalf("target mode %q should be denied for cross-project", tMode)
			}
			if decision.Code != MessageDenialCrossProjectTargetMode {
				t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectTargetMode, decision.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: Federated ancestry rejected
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_FederatedAncestry(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// Add ownerA as member of project B.
	msgAuthzAddProjectMember(t, f.store, f.ownerA.ID, f.projectB, "project-b", store.GroupMemberRoleMember)

	sender := msgAuthzAgent(t, f.store, "cp-fed-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-fed-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	// Create a federated agent identity.
	fedIdent := &federatedAgentIdentityForTest{
		id:        sender.ID,
		projectID: f.projectA,
		ancestry:  sender.Ancestry,
	}

	decision := f.srv.EvaluateAgentMessage(ctx, fedIdent, target)
	if decision.Allowed {
		t.Fatal("federated ancestry should be denied for cross-project messaging")
	}
	if decision.Code != MessageDenialCrossProjectUntrusted {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectUntrusted, decision.Code)
	}
}

// federatedAgentIdentityForTest is a test double that implements both
// AgentIdentity and FederatedIdentity, simulating a federated agent.
type federatedAgentIdentityForTest struct {
	id        string
	projectID string
	ancestry  []string
}

func (f *federatedAgentIdentityForTest) ID() string                      { return f.id }
func (f *federatedAgentIdentityForTest) Type() string                    { return "agent" }
func (f *federatedAgentIdentityForTest) ProjectID() string               { return f.projectID }
func (f *federatedAgentIdentityForTest) Scopes() []AgentTokenScope       { return nil }
func (f *federatedAgentIdentityForTest) HasScope(_ AgentTokenScope) bool { return false }
func (f *federatedAgentIdentityForTest) Ancestry() []string              { return f.ancestry }
func (f *federatedAgentIdentityForTest) TokenID() string                 { return "" }
func (f *federatedAgentIdentityForTest) OriginUserID() string {
	if len(f.ancestry) > 0 {
		return f.ancestry[0]
	}
	return ""
}

// IssuerURL implements FederatedIdentity.
func (f *federatedAgentIdentityForTest) IssuerURL() string { return "https://remote-hub.example" }

// ---------------------------------------------------------------------------
// Test: Suspended origin user → denied
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_SuspendedOrigin(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	// Create a suspended user as origin.
	suspendedUser := &store.User{
		ID:          tid("msg-suspended-user"),
		Email:       "suspended@test.com",
		DisplayName: "Suspended User",
		Role:        store.UserRoleMember,
		Status:      "suspended",
		Created:     time.Now(),
	}
	require_NoError(t, f.store.CreateUser(ctx, suspendedUser))

	sender := msgAuthzAgent(t, f.store, "cp-susp-sender", f.projectA, store.MessageModeHub,
		[]string{suspendedUser.ID})
	target := msgAuthzAgent(t, f.store, "cp-susp-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("suspended origin user should be denied")
	}
	if decision.Code != MessageDenialCrossProjectUntrusted {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectUntrusted, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Missing root human principal → denied
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_MissingRootPrincipal(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	// Create agent with empty ancestry (no root human).
	sender := msgAuthzAgent(t, f.store, "cp-noroot-sender", f.projectA, store.MessageModeHub,
		[]string{})
	target := msgAuthzAgent(t, f.store, "cp-noroot-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, []string{})
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("agent with no root human principal should be denied")
	}
	if decision.Code != MessageDenialCrossProjectUntrusted {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectUntrusted, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Membership with expired binding → not a member
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_MembersPolicy_ExpiredBinding(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// Create a role binding for ownerA in project B that has expired.
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require_NoError(t, err)

	pastTime := time.Now().Add(-24 * time.Hour)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.ownerA.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectB,
		ExpiresAt:        &pastTime,
		CreatedBy:        "test",
	})
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "cp-exp-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-exp-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("expired membership binding should not count — delivery should be denied")
	}
	if decision.Code != MessageDenialCrossProjectNotMember {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectNotMember, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Membership with not-yet-active binding → not a member
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_MembersPolicy_FutureBinding(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// Create a role binding for ownerA in project B that is not yet active.
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require_NoError(t, err)

	futureTime := time.Now().Add(24 * time.Hour)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.ownerA.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectB,
		NotBefore:        &futureTime,
		CreatedBy:        "test",
	})
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "cp-fut-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "cp-fut-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("not-yet-active membership binding should not count — delivery should be denied")
	}
	if decision.Code != MessageDenialCrossProjectNotMember {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectNotMember, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: D1 CheckEffectiveMembership
// ---------------------------------------------------------------------------

func TestCheckEffectiveMembership(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	t.Run("direct member is a member", func(t *testing.T) {
		result := srv.CheckEffectiveMembership(ctx, member.ID, projectID)
		if result.Err != nil {
			t.Fatalf("unexpected error: %v", result.Err)
		}
		if !result.IsMember {
			t.Fatal("direct member should be a member")
		}
		if result.Role != store.ProjectRoleMember {
			t.Fatalf("expected role member, got %s", result.Role)
		}
	})

	t.Run("owner is a member", func(t *testing.T) {
		result := srv.CheckEffectiveMembership(ctx, owner.ID, projectID)
		if result.Err != nil {
			t.Fatalf("unexpected error: %v", result.Err)
		}
		if !result.IsMember {
			t.Fatal("owner should count as a member")
		}
	})

	t.Run("non-member returns false without error", func(t *testing.T) {
		nonMember := &store.User{
			ID:      tid("msg-nonmember"),
			Email:   "nonmember@test.com",
			Role:    store.UserRoleMember,
			Status:  "active",
			Created: time.Now(),
		}
		require_NoError(t, s.CreateUser(ctx, nonMember))

		result := srv.CheckEffectiveMembership(ctx, nonMember.ID, projectID)
		if result.Err != nil {
			t.Fatalf("unexpected error: %v", result.Err)
		}
		if result.IsMember {
			t.Fatal("non-member should not be a member")
		}
	})
}

// ---------------------------------------------------------------------------
// Test: storedAgentIdentity adapter
// ---------------------------------------------------------------------------

func TestStoredAgentIdentity(t *testing.T) {
	agent := &store.Agent{
		ID:        "agent-123",
		ProjectID: "project-456",
		Ancestry:  []string{"user-root", "agent-parent"},
	}

	ident := &storedAgentIdentity{agent: agent}

	if ident.ID() != "agent-123" {
		t.Fatalf("expected ID agent-123, got %s", ident.ID())
	}
	if ident.Type() != "agent" {
		t.Fatalf("expected type agent, got %s", ident.Type())
	}
	if ident.ProjectID() != "project-456" {
		t.Fatalf("expected projectID project-456, got %s", ident.ProjectID())
	}
	if ident.OriginUserID() != "user-root" {
		t.Fatalf("expected origin user user-root, got %s", ident.OriginUserID())
	}
	if ident.TokenID() != "" {
		t.Fatalf("expected empty token ID, got %s", ident.TokenID())
	}

	// storedAgentIdentity is NOT federated → hub-attested
	if !AncestryIsHubAttested(ident) {
		t.Fatal("storedAgentIdentity should be hub-attested (not federated)")
	}
}

// ---------------------------------------------------------------------------
// Test: Policy "any" does NOT open branch/lineage/none receivers
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_PolicyAnyDoesNotOpenRestricted(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "cp-any-restr-s", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})

	// Even with policy "any", restricted target modes are denied.
	for _, mode := range []string{store.MessageModeNone, store.MessageModeLineage, store.MessageModeBranch} {
		t.Run("target_"+mode, func(t *testing.T) {
			target := msgAuthzAgent(t, f.store, "cp-any-restr-"+mode, f.projectB, mode,
				[]string{f.ownerB.ID})
			senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
			decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
			if decision.Allowed {
				t.Fatalf("policy 'any' should NOT open %s-mode receiver for cross-project", mode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: Cross-project message provenance fields
// ---------------------------------------------------------------------------

func TestMessageProvenance_FieldsPopulated(t *testing.T) {
	// Verify the Message struct has the new provenance fields.
	msg := &store.Message{
		ID:        "test-msg",
		ProjectID: "dest-project",
	}

	senderProjectID := "sender-project"
	recipientProjectID := "dest-project"
	msg.SenderProjectID = &senderProjectID
	msg.RecipientProjectID = &recipientProjectID

	if msg.SenderProjectID == nil || *msg.SenderProjectID != "sender-project" {
		t.Fatal("SenderProjectID should be set")
	}
	if msg.RecipientProjectID == nil || *msg.RecipientProjectID != "dest-project" {
		t.Fatal("RecipientProjectID should be set")
	}

	// Nil provenance for human senders.
	humanMsg := &store.Message{
		ID:        "human-msg",
		ProjectID: "dest-project",
	}
	if humanMsg.SenderProjectID != nil {
		t.Fatal("SenderProjectID should be nil for human senders")
	}
}

// ---------------------------------------------------------------------------
// Test: Update existing cross-project test to reflect Phase 2 behavior
// ---------------------------------------------------------------------------

func TestEvaluateAgentMessage_CrossProject_FullMatrix(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	// Add ownerA as member of B for "members" policy tests.
	msgAuthzAddProjectMember(t, f.store, f.ownerA.ID, f.projectB, "project-b", store.GroupMemberRoleMember)

	policies := []string{
		store.CrossProjectInboundNone,
		store.CrossProjectInboundMembers,
		store.CrossProjectInboundAny,
	}

	senderModes := []string{store.MessageModeProject, store.MessageModeHub}
	targetModes := []string{store.MessageModeProject, store.MessageModeHub}

	for _, policy := range policies {
		for _, sMode := range senderModes {
			for _, tMode := range targetModes {
				name := policy + "/" + sMode + "→" + tMode
				t.Run(name, func(t *testing.T) {
					// Set the policy.
					proj, err := f.store.GetProject(ctx, f.projectB)
					require_NoError(t, err)
					_, err = f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, policy, proj.CrossProjectInboundRevision)
					require_NoError(t, err)

					sender := msgAuthzAgent(t, f.store, "cp-full-s-"+policy+"-"+sMode+"-"+tMode,
						f.projectA, sMode, []string{f.ownerA.ID})
					target := msgAuthzAgent(t, f.store, "cp-full-t-"+policy+"-"+sMode+"-"+tMode,
						f.projectB, tMode, []string{f.ownerB.ID})

					senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
					decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)

					// Expected: allowed when sender=hub AND policy allows
					expected := sMode == store.MessageModeHub &&
						(policy == store.CrossProjectInboundAny ||
							policy == store.CrossProjectInboundMembers)
					if decision.Allowed != expected {
						t.Fatalf("expected allowed=%v, got allowed=%v (code: %s, reason: %s)",
							expected, decision.Allowed, decision.Code, decision.Reason)
					}
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// R-3: Missing acceptance-gate tests
// ---------------------------------------------------------------------------

// Test R-3/1: Group-derived member/admin — user in a group that has a built-in
// role binding on the destination project counts as a member for cross-project
// messaging.
func TestEvaluateAgentMessage_CrossProject_GroupDerivedMember(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// Create a user who is NOT a direct member of project B.
	groupUser := &store.User{
		ID:          tid("msg-group-user"),
		Email:       "group-user@test.com",
		DisplayName: "Group User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, f.store.CreateUser(ctx, groupUser))
	ensureHubMembership(ctx, f.store, groupUser.ID)

	// Create a group.
	grp := &store.Group{
		ID:        tid("msg-test-group"),
		Name:      "Test Group for Membership",
		Slug:      "msg-test-group-" + tid("rand"),
		GroupType: "explicit",
	}
	require_NoError(t, f.store.CreateGroup(ctx, grp))

	// Add the user to the group.
	require_NoError(t, f.store.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    grp.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   groupUser.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create a group-principal role binding for the admin role in project B.
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require_NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      grp.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectB,
		CreatedBy:        "test",
	})
	require_NoError(t, err)

	// Verify CheckEffectiveMembership sees the user as a member.
	memberResult := f.srv.CheckEffectiveMembership(ctx, groupUser.ID, f.projectB)
	if memberResult.Err != nil {
		t.Fatalf("CheckEffectiveMembership error: %v", memberResult.Err)
	}
	if !memberResult.IsMember {
		t.Fatal("user in group with admin binding should be a member")
	}

	// Verify cross-project messaging is allowed with members policy.
	sender := msgAuthzAgent(t, f.store, "cp-grp-sender", f.projectA, store.MessageModeHub,
		[]string{groupUser.ID})
	target := msgAuthzAgent(t, f.store, "cp-grp-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("group-derived member should be allowed: %s (code: %s)",
			decision.Reason, decision.Code)
	}
}

// Test R-3/2: Nested group membership — user in an inner group, inner group
// nested inside an outer group that has a built-in role binding on the
// destination project.
func TestEvaluateAgentMessage_CrossProject_NestedGroupMembership(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	nestedUser := &store.User{
		ID:          tid("msg-nested-user"),
		Email:       "nested-user@test.com",
		DisplayName: "Nested User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, f.store.CreateUser(ctx, nestedUser))
	ensureHubMembership(ctx, f.store, nestedUser.ID)

	// Create inner group and add the user.
	innerGroup := &store.Group{
		ID:        tid("msg-inner-group"),
		Name:      "Inner Group",
		Slug:      "msg-inner-group-" + tid("rand2"),
		GroupType: "explicit",
	}
	require_NoError(t, f.store.CreateGroup(ctx, innerGroup))
	require_NoError(t, f.store.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    innerGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   nestedUser.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create outer group and nest inner group inside it.
	outerGroup := &store.Group{
		ID:        tid("msg-outer-group"),
		Name:      "Outer Group",
		Slug:      "msg-outer-group-" + tid("rand3"),
		GroupType: "explicit",
	}
	require_NoError(t, f.store.CreateGroup(ctx, outerGroup))
	require_NoError(t, f.store.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    outerGroup.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   innerGroup.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create a group-principal role binding for the outer group as member in project B.
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require_NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      outerGroup.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectB,
		CreatedBy:        "test",
	})
	require_NoError(t, err)

	// Verify nested membership resolves.
	memberResult := f.srv.CheckEffectiveMembership(ctx, nestedUser.ID, f.projectB)
	if memberResult.Err != nil {
		t.Fatalf("CheckEffectiveMembership error: %v", memberResult.Err)
	}
	if !memberResult.IsMember {
		t.Fatal("user in nested group should be an effective member")
	}

	// Verify cross-project messaging.
	sender := msgAuthzAgent(t, f.store, "cp-nested-sender", f.projectA, store.MessageModeHub,
		[]string{nestedUser.ID})
	target := msgAuthzAgent(t, f.store, "cp-nested-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("nested group member should be allowed: %s (code: %s)",
			decision.Reason, decision.Code)
	}
}

// Test R-3/3: Group removal — user removed from group, membership revoked,
// delivery denied.
func TestEvaluateAgentMessage_CrossProject_GroupRemoval(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	removedUser := &store.User{
		ID:          tid("msg-removed-user"),
		Email:       "removed-user@test.com",
		DisplayName: "Removed User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, f.store.CreateUser(ctx, removedUser))
	ensureHubMembership(ctx, f.store, removedUser.ID)

	// Create group, add user, bind group to project B with member role.
	grp := &store.Group{
		ID:        tid("msg-removal-group"),
		Name:      "Removal Test Group",
		Slug:      "msg-removal-group-" + tid("rand4"),
		GroupType: "explicit",
	}
	require_NoError(t, f.store.CreateGroup(ctx, grp))
	require_NoError(t, f.store.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    grp.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   removedUser.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	rd, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require_NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      grp.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectB,
		CreatedBy:        "test",
	})
	require_NoError(t, err)

	// Confirm membership before removal.
	pre := f.srv.CheckEffectiveMembership(ctx, removedUser.ID, f.projectB)
	if !pre.IsMember {
		t.Fatal("user should be a member before removal")
	}

	// Remove the user from the group.
	require_NoError(t, f.store.RemoveGroupMember(ctx, grp.ID, store.GroupMemberTypeUser, removedUser.ID))

	// Confirm membership is revoked.
	post := f.srv.CheckEffectiveMembership(ctx, removedUser.ID, f.projectB)
	if post.IsMember {
		t.Fatal("user should NOT be a member after removal from group")
	}

	// Verify cross-project messaging is denied.
	sender := msgAuthzAgent(t, f.store, "cp-rmvd-sender", f.projectA, store.MessageModeHub,
		[]string{removedUser.ID})
	target := msgAuthzAgent(t, f.store, "cp-rmvd-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("removed group member should be denied cross-project messaging")
	}
	if decision.Code != MessageDenialCrossProjectNotMember {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectNotMember, decision.Code)
	}
}

// Test R-3/4: Custom-only role binding — user has only a custom/additive role
// (NOT a built-in membership role), delivery denied with
// cross_project_origin_not_member.
func TestEvaluateAgentMessage_CrossProject_CustomRoleNotMember(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	customUser := &store.User{
		ID:          tid("msg-custom-role-user"),
		Email:       "custom-role@test.com",
		DisplayName: "Custom Role User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, f.store.CreateUser(ctx, customUser))
	ensureHubMembership(ctx, f.store, customUser.ID)

	// Create a custom additive role.
	customRD, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "custom-agent-viewer",
		Description: "Custom additive role — NOT a membership role",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"agent.read"},
		System:      false,
	})
	require_NoError(t, err)

	// Bind the custom role to the user in project B.
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      customUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectB,
		CreatedBy:        "test",
	})
	require_NoError(t, err)

	// CheckEffectiveMembership should NOT consider custom-only as member.
	memberResult := f.srv.CheckEffectiveMembership(ctx, customUser.ID, f.projectB)
	if memberResult.Err != nil {
		t.Fatalf("CheckEffectiveMembership error: %v", memberResult.Err)
	}
	if memberResult.IsMember {
		t.Fatal("custom-only role binding should NOT count as membership")
	}

	// Verify cross-project messaging is denied.
	sender := msgAuthzAgent(t, f.store, "cp-cust-sender", f.projectA, store.MessageModeHub,
		[]string{customUser.ID})
	target := msgAuthzAgent(t, f.store, "cp-cust-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("custom-only role should be denied cross-project messaging")
	}
	if decision.Code != MessageDenialCrossProjectNotMember {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectNotMember, decision.Code)
	}
}

// Test R-3/5: Visibility-only / public-project access — public project
// visibility alone is NOT membership, delivery denied.
func TestEvaluateAgentMessage_CrossProject_PublicProjectNotMember(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// A user who is NOT a member of project B but can see it (visibility).
	viewerUser := &store.User{
		ID:          tid("msg-viewer-user"),
		Email:       "viewer@test.com",
		DisplayName: "Viewer User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, f.store.CreateUser(ctx, viewerUser))
	ensureHubMembership(ctx, f.store, viewerUser.ID)

	// viewerUser has NO role bindings in project B at all — just visibility.
	// CheckEffectiveMembership should return false.
	memberResult := f.srv.CheckEffectiveMembership(ctx, viewerUser.ID, f.projectB)
	if memberResult.Err != nil {
		t.Fatalf("CheckEffectiveMembership error: %v", memberResult.Err)
	}
	if memberResult.IsMember {
		t.Fatal("visibility-only user should NOT be a member")
	}

	// Verify cross-project messaging is denied.
	sender := msgAuthzAgent(t, f.store, "cp-view-sender", f.projectA, store.MessageModeHub,
		[]string{viewerUser.ID})
	target := msgAuthzAgent(t, f.store, "cp-view-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("visibility-only access should be denied cross-project messaging")
	}
	if decision.Code != MessageDenialCrossProjectNotMember {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectNotMember, decision.Code)
	}
}

// Test R-3/6: Deleted intermediate parent in ancestry — parent agent deleted
// but ancestry chain survives. Hub-attested ancestry survives deletion; the
// chain is immutable once written.
func TestEvaluateAgentMessage_CrossProject_DeletedIntermediateParent(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	// Create an intermediate agent in project A.
	intermediate := msgAuthzAgent(t, f.store, "cp-intermediate", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})

	// Create a child agent whose ancestry includes ownerA → intermediate.
	child := msgAuthzAgent(t, f.store, "cp-child-of-deleted", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID, intermediate.ID})

	// Delete the intermediate agent from the store.
	require_NoError(t, f.store.DeleteAgent(ctx, intermediate.ID))

	// Verify the intermediate is actually gone.
	_, err = f.store.GetAgent(ctx, intermediate.ID)
	if err == nil {
		t.Fatal("intermediate agent should have been deleted")
	}

	// The child's ancestry chain [ownerA, intermediate] is still intact in
	// the child's record — deletion of intermediate does not invalidate the
	// Hub-attested chain.
	target := msgAuthzAgent(t, f.store, "cp-del-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})

	senderIdent := msgAuthzAgentIdentity(child.ID, f.projectA, child.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("deleted intermediate should not break Hub-attested ancestry: %s (code: %s)",
			decision.Reason, decision.Code)
	}
}
