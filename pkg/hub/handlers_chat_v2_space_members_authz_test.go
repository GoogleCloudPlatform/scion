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
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// spaceMembersAuthzStore fails role binding loads once the members handler
// has listed the project's human members. The project read check runs
// before that point and still succeeds, so the failure lands on the agent
// list scope resolution.
type spaceMembersAuthzStore struct {
	store.Store
	armed bool
}

func (s *spaceMembersAuthzStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	members, err := s.Store.ListProjectMembers(ctx, projectID)
	s.armed = true
	return members, err
}

func (s *spaceMembersAuthzStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.armed {
		return nil, errors.New("injected role binding failure")
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// createSpaceMembersReader creates a user bound to projectID with a project
// role that grants project.read but not agent.list.
func createSpaceMembersReader(t *testing.T, s store.Store, projectID string) *store.User {
	t.Helper()
	ctx := context.Background()
	reader := &store.User{
		ID:          tid("members-reader"),
		Email:       "members-reader@test.com",
		DisplayName: "Members Reader",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, reader))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "members-project-reader",
		Description: "project read without agent list",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require_NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      reader.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require_NoError(t, err)
	return reader
}

func assertSpaceMembersAgentsHidden(t *testing.T, resp chatMembersResponse) {
	t.Helper()
	if resp.Agents == nil {
		t.Fatal("agents = null, want an empty array")
	}
	if len(resp.Agents) != 0 {
		t.Fatalf("agents = %d, want 0 without agent list access", len(resp.Agents))
	}
	if len(resp.Humans) == 0 {
		t.Fatal("humans section is empty; project read must still list humans")
	}
}

// A caller with project read but without agent.list sees the humans section
// and no agent rows, and no per-agent attach decision is made.
func TestSpaceMembers_ReaderWithoutAgentListGetsNoAgents(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	createSpaceMembersAgents(t, s, projectID, owner.ID, "authz-hidden", 3)
	reader := createSpaceMembersReader(t, s, projectID)
	audits := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(audits)
	srv.authzService.DecisionAuditSampleRate = 1.0

	// Control: a project member holds agent.list and sees the agents, and
	// one attach decision is recorded per agent.
	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	if resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes()); len(resp.Agents) != 3 {
		t.Fatalf("fixture: member sees %d agents, want 3", len(resp.Agents))
	}
	if n := countSpaceMembersAttachChecks(audits); n != 3 {
		t.Fatalf("fixture: member attach checks = %d, want 3", n)
	}
	audits.records = nil

	counting := &spaceMembersStore{Store: s}
	srv.store = counting

	rec = doRequestAsUser(t, srv, reader, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	assertSpaceMembersAgentsHidden(t, decodeSpaceMembers(t, rec.Code, rec.Body.Bytes()))
	if counting.listAgentsCalls != 0 {
		t.Fatalf("ListAgents calls = %d, want 0 when agents are hidden", counting.listAgentsCalls)
	}
	if n := countSpaceMembersAttachChecks(audits); n != 0 {
		t.Fatalf("attach checks = %d, want 0 when agents are hidden", n)
	}
}

// countSpaceMembersAttachChecks counts the agent attach decisions recorded
// by the decision audit emitter.
func countSpaceMembersAttachChecks(audits *recordingDecisionAuditEmitter) int {
	n := 0
	for _, r := range audits.records {
		if r.ResourceType == "agent" && r.Permission == string(ActionAttach) {
			n++
		}
	}
	return n
}

// A caller with agent.list on another project only (an Explicit scope that
// does not contain this project) and project read here sees the humans
// section and no agent rows, matching GET /api/v1/agents for this project.
func TestSpaceMembers_AgentListOnOtherProjectGetsNoAgents(t *testing.T) {
	ctx := context.Background()
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	createSpaceMembersAgents(t, s, projectID, owner.ID, "authz-explicit-a", 3)
	reader := createSpaceMembersReader(t, s, projectID)
	other := createSpaceMembersProject(t, s, "members-authz-explicit-b")
	createSpaceMembersAgents(t, s, other.ID, owner.ID, "authz-explicit-b", 2)
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "members-other-agent-lister",
		Description: "agent list on another project",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"agent.list", "project.read"},
	})
	require_NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      reader.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          other.ID,
		CreatedBy:        "test",
	})
	require_NoError(t, err)

	// Fixture guard: the scope must be Explicit, contain the other project
	// and not contain this one, which is the case this test covers.
	scope, err := srv.authzService.ResolveListScopes(ctx,
		NewAuthenticatedUser(reader.ID, reader.Email, reader.DisplayName, "member", "api"), "agent.list")
	require_NoError(t, err)
	if scope.Scopes.IsNone() || scope.Scopes.IsAll() ||
		scope.Scopes.Contains(projectID) || !scope.Scopes.Contains(other.ID) {
		t.Fatalf("fixture: scope projects=%v, want explicit with %s and without %s",
			scope.Scopes.ProjectIDs(), other.ID, projectID)
	}

	// Reference: GET /api/v1/agents for this project returns no rows.
	rec := doRequestAsUser(t, srv, reader, http.MethodGet, "/api/v1/agents?projectId="+projectID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reference: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var ref struct {
		Agents []json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ref); err != nil {
		t.Fatalf("reference: decode: %v: %s", err, rec.Body.String())
	}
	if len(ref.Agents) != 0 {
		t.Fatalf("reference: agents = %d, want 0", len(ref.Agents))
	}

	rec = doRequestAsUser(t, srv, reader, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes())
	if len(resp.Agents) != len(ref.Agents) {
		t.Fatalf("members agents = %d, want %d to match GET /api/v1/agents", len(resp.Agents), len(ref.Agents))
	}
	assertSpaceMembersAgentsHidden(t, resp)
}

// A caller whose agent.list scope is All but whose access to this project
// is cut by a project-scoped access constraint sees no agent rows, matching
// the ExcludedProjectIDs filter GET /api/v1/agents applies.
func TestSpaceMembers_ConstraintExcludedProjectGetsNoAgents(t *testing.T) {
	srv, s, owner, _, projectID := msgAuthzSetup(t)
	createSpaceMembersAgents(t, s, projectID, owner.ID, "authz-excluded", 3)
	admin := &store.User{ID: tid("msg-superadmin"), Email: "admin@test.com", DisplayName: "admin@test.com", Role: "admin"}

	// Control: before the constraint the admin sees the agents.
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	if resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes()); len(resp.Agents) != 3 {
		t.Fatalf("fixture: admin sees %d agents, want 3", len(resp.Agents))
	}

	_, err := s.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		Name:               "members-exclude-agent-list",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeProject,
		ScopeID:            projectID,
		MaximumPermissions: []string{"project.read"},
		Purpose:            "members endpoint agent list exclusion test",
		CreatedBy:          "test",
	})
	require_NoError(t, err)

	// Fixture guard: the constraint must surface as an All-scope exclusion,
	// which is the case this test covers.
	scope, err := srv.authzService.ResolveListScopes(context.Background(),
		NewAuthenticatedUser(admin.ID, admin.Email, admin.DisplayName, admin.Role, "api"), "agent.list")
	require_NoError(t, err)
	if !scope.Scopes.IsAll() || len(scope.ExcludedProjectIDs) != 1 || scope.ExcludedProjectIDs[0] != projectID {
		t.Fatalf("fixture: scope all=%v excluded=%v, want all with %s excluded",
			scope.Scopes.IsAll(), scope.ExcludedProjectIDs, projectID)
	}

	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	assertSpaceMembersAgentsHidden(t, decodeSpaceMembers(t, rec.Code, rec.Body.Bytes()))
}

// A failure resolving the agent list scope is a 500 from the handler's own
// error path, not an empty or partial member list.
func TestSpaceMembers_AgentListScopeErrorReturns500(t *testing.T) {
	srv, s := testServer(t)
	proj := createSpaceMembersProject(t, s, "members-authz-scope-err")
	createSpaceMembersAgents(t, s, proj.ID, DevUserID, "authz-scope-err", 3)
	wrapped := &spaceMembersAuthzStore{Store: s}
	srv.store = wrapped
	srv.authzService.store = wrapped
	logs := captureSpaceMembersLogs(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error body: %v: %s", err, rec.Body.String())
	}
	// ErrorResponse ignores unknown keys, so check the raw body separately:
	// an agents payload on the error path would be a leak.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw body: %v: %s", err, rec.Body.String())
	}
	if _, ok := raw["agents"]; ok {
		t.Fatalf("error body has an agents key: %s", rec.Body.String())
	}
	const want = "failed to resolve agent list scope"
	if errResp.Error.Code != "INTERNAL" || errResp.Error.Message != want {
		t.Fatalf("error = {%q, %q}, want {%q, %q}", errResp.Error.Code, errResp.Error.Message, "INTERNAL", want)
	}
	if !strings.Contains(logs.String(), "chat members: failed to resolve agent list scope") {
		t.Fatalf("scope failure was not logged:\n%s", logs.String())
	}
}
