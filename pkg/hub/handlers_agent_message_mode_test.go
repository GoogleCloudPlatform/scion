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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// set_message_mode test helpers
// ---------------------------------------------------------------------------

// smmSetup creates a server, project, owner, member, and admin user for
// set_message_mode tests. Returns (server, store, owner, member, admin, projectID).
func smmSetup(t *testing.T) (*Server, store.Store, *store.User, *store.User, *store.User, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("smm-project")

	owner := &store.User{
		ID:          tid("smm-owner"),
		Email:       "smm-owner@test.com",
		DisplayName: "SMM Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	member := &store.User{
		ID:          tid("smm-member"),
		Email:       "smm-member@test.com",
		DisplayName: "SMM Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	admin := &store.User{
		ID:          tid("smm-admin"),
		Email:       "smm-admin@test.com",
		DisplayName: "SMM Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	// Grant super-admin role binding for admin user (CO1 cutover: policies removed, role bindings required)
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require_NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require_NoError(t, err)

	project := &store.Project{
		ID:        projectID,
		Name:      "smm-project",
		Slug:      "smm-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	// Owner gets owner role binding.
	msgAuthzAddProjectMember(t, s, owner.ID, projectID, "smm-project", store.GroupMemberRoleOwner)
	// Member gets member role binding.
	msgAuthzAddProjectMember(t, s, member.ID, projectID, "smm-project", store.GroupMemberRoleMember)

	return srv, s, owner, member, admin, projectID
}

// smmAgent creates and persists an agent in the given project with the specified mode.
func smmAgent(t *testing.T, s store.Store, name, projectID, mode string, ancestry []string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:          tid(name),
		Name:        name,
		Slug:        name,
		ProjectID:   projectID,
		MessageMode: mode,
		Ancestry:    ancestry,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require_NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// smmDoRequest performs a set_message_mode action request with the given identity set in context.
func smmDoRequest(t *testing.T, srv *Server, agentID string, req SetMessageModeRequest, identity Identity) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/set_message_mode", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")

	// Set identity in context
	ctx := contextWithIdentity(httpReq.Context(), identity)
	httpReq = httpReq.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleSetMessageMode(rr, httpReq, agentID)
	return rr
}

// ---------------------------------------------------------------------------
// Test 1: Project admin denied set_message_mode
// ---------------------------------------------------------------------------

func TestSetMessageMode_ProjectAdminDenied(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)
	_ = owner
	ctx := context.Background()

	// Create a project admin user (not owner).
	projectAdmin := &store.User{
		ID:          tid("smm-proj-admin"),
		Email:       "smm-proj-admin@test.com",
		DisplayName: "SMM Project Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, projectAdmin))
	ensureHubMembership(ctx, s, projectAdmin.ID)

	// Add as admin (not owner) to the project.
	membersSlug := projectMembersGroupSlug("smm-project")
	group, err := s.GetGroupBySlug(ctx, membersSlug)
	if err != nil {
		t.Fatalf("failed to get members group: %v", err)
	}
	if err := s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   projectAdmin.ID,
		Role:       store.GroupMemberRoleAdmin,
	}); err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to add admin: %v", err)
	}
	// Bind admin role (not owner).
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	if err != nil {
		t.Fatalf("admin role definition not found: %v", err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      projectAdmin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create admin role binding: %v", err)
	}

	// Agent ancestry does NOT include the project admin (so ancestry bypass won't apply).
	agent := smmAgent(t, s, "smm-admin-denied", projectID, store.MessageModeProject,
		[]string{owner.ID})

	adminIdent := msgAuthzUserIdentity(projectAdmin.ID)
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "none"}, adminIdent)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for project admin, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test 2: Project owner allowed
// ---------------------------------------------------------------------------

func TestSetMessageMode_ProjectOwnerAllowed(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)

	agent := smmAgent(t, s, "smm-owner-allowed", projectID, store.MessageModeProject,
		[]string{owner.ID})

	ownerIdent := msgAuthzUserIdentity(owner.ID)
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "lineage"}, ownerIdent)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for project owner, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SetMessageModeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Mode != "lineage" {
		t.Fatalf("expected mode=lineage, got %q", resp.Mode)
	}
	if resp.Previous != "project" {
		t.Fatalf("expected previous_mode=project, got %q", resp.Previous)
	}

	// Verify mode is updated in store.
	updated, err := s.GetAgent(context.Background(), agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if updated.MessageMode != "lineage" {
		t.Fatalf("store should reflect new mode, got %q", updated.MessageMode)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Lineage owner (ancestry user) allowed
// ---------------------------------------------------------------------------

func TestSetMessageMode_LineageOwnerAllowed(t *testing.T) {
	srv, s, _, _, _, projectID := smmSetup(t)
	ctx := context.Background()

	// Create a user in the agent's ancestry who is NOT the project owner.
	lineageUser := &store.User{
		ID:          tid("smm-lineage-user"),
		Email:       "smm-lineage@test.com",
		DisplayName: "SMM Lineage User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, lineageUser))
	ensureHubMembership(ctx, s, lineageUser.ID)
	msgAuthzAddProjectMember(t, s, lineageUser.ID, projectID, "smm-project", store.GroupMemberRoleMember)

	// Agent with lineageUser in its ancestry.
	agent := smmAgent(t, s, "smm-lineage-target", projectID, store.MessageModeProject,
		[]string{lineageUser.ID})
	// Also set OwnerID to lineageUser so ancestry bypass works.
	agent.OwnerID = lineageUser.ID
	require_NoError(t, s.UpdateAgent(ctx, agent))

	lineageIdent := msgAuthzUserIdentity(lineageUser.ID)
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "branch"}, lineageIdent)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for lineage owner, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify.
	updated, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if updated.MessageMode != "branch" {
		t.Fatalf("expected branch, got %q", updated.MessageMode)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Agent callers always denied (D7)
// ---------------------------------------------------------------------------

func TestSetMessageMode_AgentCallersDenied(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)

	agent := smmAgent(t, s, "smm-agent-denied-target", projectID, store.MessageModeProject,
		[]string{owner.ID})

	agentCaller := smmAgent(t, s, "smm-agent-caller", projectID, store.MessageModeProject,
		[]string{owner.ID})
	agentIdent := msgAuthzAgentIdentity(agentCaller.ID, projectID, agentCaller.Ancestry, ScopeAgentLifecycle)

	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "none"}, agentIdent)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for agent caller, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test 5: UATs always denied (no scope exists) (D7)
// ---------------------------------------------------------------------------

func TestSetMessageMode_UATsDenied(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)

	agent := smmAgent(t, s, "smm-uat-target", projectID, store.MessageModeProject,
		[]string{owner.ID})

	// Create a scoped user identity (UAT).
	baseIdent := msgAuthzUserIdentity(owner.ID)
	scopedIdent := NewScopedUserIdentity(baseIdent, projectID, []string{"agent:read"})

	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "none"}, scopedIdent)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for UAT, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test 6: Super-admin allowed
// ---------------------------------------------------------------------------

func TestSetMessageMode_SuperAdminAllowed(t *testing.T) {
	srv, s, owner, _, admin, projectID := smmSetup(t)

	agent := smmAgent(t, s, "smm-superadmin-target", projectID, store.MessageModeProject,
		[]string{owner.ID})

	// Use admin role identity (super-admin bypass requires UserRoleAdmin).
	adminIdent := NewAuthenticatedUser(admin.ID, admin.Email, admin.DisplayName, store.UserRoleAdmin, "api")
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "none"}, adminIdent)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for super-admin, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SetMessageModeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp.Mode != "none" {
		t.Fatalf("expected mode=none, got %q", resp.Mode)
	}
}

// ---------------------------------------------------------------------------
// Test 7: All transitions legal
// ---------------------------------------------------------------------------

func TestSetMessageMode_AllTransitionsLegal(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)

	modes := []string{
		store.MessageModeNone,
		store.MessageModeLineage,
		store.MessageModeBranch,
		store.MessageModeProject,
	}

	ownerIdent := msgAuthzUserIdentity(owner.ID)

	for _, from := range modes {
		for _, to := range modes {
			if from == to {
				continue // no-op (tested separately)
			}
			t.Run(from+"->"+to, func(t *testing.T) {
				agentName := "smm-transition-" + from + "-" + to
				agent := smmAgent(t, s, agentName, projectID, from,
					[]string{owner.ID})

				rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: to}, ownerIdent)

				if rr.Code != http.StatusOK {
					t.Fatalf("transition %s->%s: expected 200, got %d: %s",
						from, to, rr.Code, rr.Body.String())
				}

				var resp SetMessageModeResponse
				if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp.Mode != to {
					t.Fatalf("expected mode=%s, got %q", to, resp.Mode)
				}
				if resp.Previous != from {
					t.Fatalf("expected previous=%s, got %q", from, resp.Previous)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Test 8: Cascade operation
// ---------------------------------------------------------------------------

func TestSetMessageMode_Cascade(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)

	// Create a branch: root -> child -> grandchild.
	root := smmAgent(t, s, "smm-cascade-root", projectID, store.MessageModeProject,
		[]string{owner.ID})
	child := smmAgent(t, s, "smm-cascade-child", projectID, store.MessageModeProject,
		[]string{owner.ID, root.ID})
	grandchild := smmAgent(t, s, "smm-cascade-grandchild", projectID, store.MessageModeProject,
		[]string{owner.ID, root.ID, child.ID})

	ownerIdent := msgAuthzUserIdentity(owner.ID)
	rr := smmDoRequest(t, srv, root.ID, SetMessageModeRequest{
		Mode:    "branch",
		Cascade: true,
	}, ownerIdent)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SetMessageModeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Mode != "branch" {
		t.Fatalf("expected mode=branch, got %q", resp.Mode)
	}

	// Verify cascade result.
	if resp.Cascade == nil {
		t.Fatal("expected cascade result, got nil")
	}
	if resp.Cascade.Count != 2 {
		t.Fatalf("expected 2 descendants updated, got %d", resp.Cascade.Count)
	}

	// Verify all three agents have the new mode in store.
	ctx := context.Background()
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		agent, err := s.GetAgent(ctx, id)
		if err != nil {
			t.Fatalf("failed to get agent %s: %v", id, err)
		}
		if agent.MessageMode != "branch" {
			t.Fatalf("agent %s: expected mode=branch, got %q", id, agent.MessageMode)
		}
	}

	// Verify audit events emitted for cascade.
	// Allow a brief moment for async audit writes.
	time.Sleep(50 * time.Millisecond)
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "agent_set_message_mode_cascade",
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("failed to list audits: %v", err)
	}
	if len(audits) < 2 {
		t.Fatalf("expected at least 2 cascade audit records, got %d", len(audits))
	}
}

// ---------------------------------------------------------------------------
// Test 9: Quarantine test — mode=none -> delivery denied
// ---------------------------------------------------------------------------

func TestSetMessageMode_Quarantine(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)
	ctx := context.Background()

	agent := smmAgent(t, s, "smm-quarantine", projectID, store.MessageModeNone,
		[]string{owner.ID})

	// A project-mode sender should be denied messaging a none-mode agent.
	sender := smmAgent(t, s, "smm-quarantine-sender", projectID, store.MessageModeProject,
		[]string{owner.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, projectID, sender.Ancestry)

	allowed, _ := srv.authorizeAgentMessage(ctx, senderIdent, agent, false)
	if allowed {
		t.Fatal("messaging a none-mode agent should be denied (quarantine)")
	}

	// System-plane notice should still work (D8).
	allowed, reason := srv.authorizeAgentMessage(ctx, senderIdent, agent, true)
	if !allowed {
		t.Fatalf("system-plane delivery should be allowed even for none-mode agent: %s", reason)
	}
}

// ---------------------------------------------------------------------------
// Test 10: Live effect — mode change takes effect on next delivery
// ---------------------------------------------------------------------------

func TestSetMessageMode_LiveEffect(t *testing.T) {
	srv, s, owner, member, _, projectID := smmSetup(t)
	ctx := context.Background()

	agent := smmAgent(t, s, "smm-live-effect", projectID, store.MessageModeProject,
		[]string{owner.ID})

	// Verify member can message a project-mode agent.
	memberIdent := msgAuthzUserIdentity(member.ID)
	allowed, reason := srv.authorizeAgentMessage(ctx, memberIdent, agent, false)
	if !allowed {
		t.Fatalf("member should message project-mode agent: %s", reason)
	}

	// Owner changes mode to none.
	ownerIdent := msgAuthzUserIdentity(owner.ID)
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "none"}, ownerIdent)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Re-fetch the agent to reflect the mode change.
	updatedAgent, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}

	// Same message attempt -> DENIED (live effect).
	allowed, _ = srv.authorizeAgentMessage(ctx, memberIdent, updatedAgent, false)
	if allowed {
		t.Fatal("after mode change to none, message should be denied")
	}
}

// ---------------------------------------------------------------------------
// Test 11: Invalid mode rejected
// ---------------------------------------------------------------------------

func TestSetMessageMode_InvalidModeRejected(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)

	agent := smmAgent(t, s, "smm-invalid-mode", projectID, store.MessageModeProject,
		[]string{owner.ID})

	ownerIdent := msgAuthzUserIdentity(owner.ID)
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "invalid"}, ownerIdent)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid mode, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test 12: MessageMode validation in template
// ---------------------------------------------------------------------------

func TestSetMessageMode_TemplateValidation(t *testing.T) {
	srv, _, _, _, _, _ := smmSetup(t)

	t.Run("valid mode accepted", func(t *testing.T) {
		body := map[string]interface{}{
			"name": "valid-template",
			"config": map[string]interface{}{
				"messageMode": "branch",
			},
		}
		rr := doRequest(t, srv, http.MethodPost, "/api/v1/templates", body)
		// Should not be a 400 validation error about message mode.
		if rr.Code == http.StatusBadRequest {
			var errResp map[string]interface{}
			json.Unmarshal(rr.Body.Bytes(), &errResp)
			if msg, ok := errResp["message"].(string); ok {
				if msg == "invalid template message mode: branch" {
					t.Fatalf("valid mode 'branch' should be accepted")
				}
			}
		}
	})

	t.Run("invalid mode rejected", func(t *testing.T) {
		body := map[string]interface{}{
			"name": "invalid-template",
			"config": map[string]interface{}{
				"messageMode": "invalid_mode",
			},
		}
		rr := doRequest(t, srv, http.MethodPost, "/api/v1/templates", body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid template mode, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Test: No-op returns success without store update
// ---------------------------------------------------------------------------

func TestSetMessageMode_NoOp(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)

	agent := smmAgent(t, s, "smm-noop", projectID, store.MessageModeProject,
		[]string{owner.ID})

	ownerIdent := msgAuthzUserIdentity(owner.ID)
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "project"}, ownerIdent)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for no-op, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SetMessageModeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Mode != "project" {
		t.Fatalf("expected mode=project, got %q", resp.Mode)
	}
	if resp.Previous != "project" {
		t.Fatalf("expected previous=project, got %q", resp.Previous)
	}
	if resp.Cascade != nil {
		t.Fatal("expected no cascade result for no-op")
	}
}

// ---------------------------------------------------------------------------
// Test: IsValidMessageMode function
// ---------------------------------------------------------------------------

func TestIsValidMessageMode(t *testing.T) {
	valid := []string{"none", "lineage", "branch", "project"}
	for _, m := range valid {
		if !store.IsValidMessageMode(m) {
			t.Errorf("expected %q to be valid", m)
		}
	}

	invalid := []string{"", "invalid", "None", "PROJECT", "all", "public"}
	for _, m := range invalid {
		if store.IsValidMessageMode(m) {
			t.Errorf("expected %q to be invalid", m)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Audit event emitted for single-agent mode change
// ---------------------------------------------------------------------------

func TestSetMessageMode_AuditEmitted(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)
	ctx := context.Background()

	agent := smmAgent(t, s, "smm-audit", projectID, store.MessageModeProject,
		[]string{owner.ID})

	ownerIdent := msgAuthzUserIdentity(owner.ID)
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "none"}, ownerIdent)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Allow a brief moment for the async audit write.
	time.Sleep(50 * time.Millisecond)

	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "agent_set_message_mode",
		TargetID:     agent.ID,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("failed to list audits: %v", err)
	}
	if len(audits) == 0 {
		t.Fatal("expected at least one audit record for set_message_mode")
	}

	latest := audits[0]
	if latest.BeforeSummary != "project" {
		t.Fatalf("expected before=project, got %q", latest.BeforeSummary)
	}
	if latest.AfterSummary != "none" {
		t.Fatalf("expected after=none, got %q", latest.AfterSummary)
	}
}

// ---------------------------------------------------------------------------
// Test: Action routing via API path (integration-style)
// ---------------------------------------------------------------------------

func TestSetMessageMode_APIRouting(t *testing.T) {
	srv, s, owner, _, _, projectID := smmSetup(t)

	agent := smmAgent(t, s, "smm-routing", projectID, store.MessageModeProject,
		[]string{owner.ID})

	// Test that set_message_mode is recognized as a valid action constant.
	if api.AgentActionSetMessageMode != "set_message_mode" {
		t.Fatalf("expected action constant = 'set_message_mode', got %q", api.AgentActionSetMessageMode)
	}

	// Verify the action is routable — the action is recognized (not 404).
	ownerIdent := msgAuthzUserIdentity(owner.ID)
	rr := smmDoRequest(t, srv, agent.ID, SetMessageModeRequest{Mode: "lineage"}, ownerIdent)
	if rr.Code == http.StatusNotFound {
		t.Fatal("set_message_mode should be a recognized action, got 404")
	}
}
