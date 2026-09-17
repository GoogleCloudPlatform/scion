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
)

// ---------------------------------------------------------------------------
// Hub-mode grant guard tests (D2)
// ---------------------------------------------------------------------------

// grantGuardSetup creates a server with a project, owner, and agents for
// testing AuthorizeMessageModeGrant.
func grantGuardSetup(t *testing.T) (*Server, store.Store, string, *store.User) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("gg-project")

	owner := &store.User{
		ID:          tid("gg-owner"),
		Email:       "gg-owner@test.com",
		DisplayName: "GG Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require_NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        projectID,
		Name:      "gg-project",
		Slug:      "gg-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require_NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	return srv, s, projectID, owner
}

func createAgentWithModeAndRole(t *testing.T, s store.Store, projectID, agentID, slug, mode, role string, ancestry []string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:          agentID,
		Name:        slug,
		Slug:        slug,
		ProjectID:   projectID,
		MessageMode: mode,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: role,
		},
		Ancestry: ancestry,
		Created:  time.Now(),
		Updated:  time.Now(),
	}
	require_NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

func TestAuthorizeMessageModeGrant_NonHubMode(t *testing.T) {
	srv, _, projectID, _ := grantGuardSetup(t)
	ctx := context.Background()

	// Any mode that is not hub should be allowed without further checks.
	for _, mode := range []string{"none", "lineage", "branch", "project"} {
		decision := srv.AuthorizeMessageModeGrant(ctx, nil, projectID, mode)
		if !decision.Allowed {
			t.Errorf("non-hub mode %q should be allowed, got denied: %s", mode, decision.Reason)
		}
	}
}

func TestAuthorizeMessageModeGrant_HumanCallerAllowed(t *testing.T) {
	srv, _, projectID, owner := grantGuardSetup(t)
	ctx := context.Background()

	userIdent := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "cli")
	decision := srv.AuthorizeMessageModeGrant(ctx, userIdent, projectID, store.MessageModeHub)
	if !decision.Allowed {
		t.Errorf("human caller should be allowed to grant hub, got denied: %s", decision.Reason)
	}
}

func TestAuthorizeMessageModeGrant_FullHubAgentAllowed(t *testing.T) {
	srv, s, projectID, owner := grantGuardSetup(t)
	ctx := context.Background()

	// Create a full/hub agent.
	callerID := tid("gg-caller-full-hub")
	createAgentWithModeAndRole(t, s, projectID, callerID, "caller-full-hub",
		store.MessageModeHub, string(AgentRoleFull), []string{owner.ID})

	agentIdent := newTestAgentIdentity(callerID, projectID, ScopesForRole(AgentRoleFull))
	decision := srv.AuthorizeMessageModeGrant(ctx, agentIdent, projectID, store.MessageModeHub)
	if !decision.Allowed {
		t.Errorf("full/hub agent should be allowed to grant hub, got denied: %s", decision.Reason)
	}
}

func TestAuthorizeMessageModeGrant_FullProjectAgentDenied(t *testing.T) {
	srv, s, projectID, owner := grantGuardSetup(t)
	ctx := context.Background()

	// Create a full/project agent — should NOT be able to grant hub.
	callerID := tid("gg-caller-full-proj")
	createAgentWithModeAndRole(t, s, projectID, callerID, "caller-full-proj",
		store.MessageModeProject, string(AgentRoleFull), []string{owner.ID})

	agentIdent := newTestAgentIdentity(callerID, projectID, ScopesForRole(AgentRoleFull))
	decision := srv.AuthorizeMessageModeGrant(ctx, agentIdent, projectID, store.MessageModeHub)
	if decision.Allowed {
		t.Error("full/project agent should NOT be able to grant hub mode")
	}
}

func TestAuthorizeMessageModeGrant_HubBaselineAgentDenied(t *testing.T) {
	srv, s, projectID, owner := grantGuardSetup(t)
	ctx := context.Background()

	// Create a baseline/hub agent — should NOT be able to grant hub (not full role).
	callerID := tid("gg-caller-base-hub")
	createAgentWithModeAndRole(t, s, projectID, callerID, "caller-base-hub",
		store.MessageModeHub, string(AgentRoleBaseline), []string{owner.ID})

	agentIdent := newTestAgentIdentity(callerID, projectID, ScopesForRole(AgentRoleBaseline))
	decision := srv.AuthorizeMessageModeGrant(ctx, agentIdent, projectID, store.MessageModeHub)
	if decision.Allowed {
		t.Error("baseline/hub agent should NOT be able to grant hub mode")
	}
}

func TestAuthorizeMessageModeGrant_CrossProjectAgentDenied(t *testing.T) {
	srv, s, projectID, owner := grantGuardSetup(t)
	ctx := context.Background()

	otherProjectID := tid("gg-other-project")

	// Create a full/hub agent in the other project.
	callerID := tid("gg-caller-cross")
	createAgentWithModeAndRole(t, s, projectID, callerID, "caller-cross",
		store.MessageModeHub, string(AgentRoleFull), []string{owner.ID})

	// Agent identity says it's in the other project.
	agentIdent := newTestAgentIdentity(callerID, otherProjectID, ScopesForRole(AgentRoleFull))
	decision := srv.AuthorizeMessageModeGrant(ctx, agentIdent, projectID, store.MessageModeHub)
	if decision.Allowed {
		t.Error("agent in different project should NOT be able to grant hub mode")
	}
}

func TestAuthorizeMessageModeGrant_MissingCallerRecordDenied(t *testing.T) {
	srv, _, projectID, _ := grantGuardSetup(t)
	ctx := context.Background()

	// Agent identity with an ID that doesn't exist in the store.
	agentIdent := newTestAgentIdentity(tid("gg-nonexistent"), projectID, ScopesForRole(AgentRoleFull))
	decision := srv.AuthorizeMessageModeGrant(ctx, agentIdent, projectID, store.MessageModeHub)
	if decision.Allowed {
		t.Error("agent with missing record should be denied (fail closed)")
	}
}

func TestAuthorizeMessageModeGrant_NoIdentityDenied(t *testing.T) {
	srv, _, projectID, _ := grantGuardSetup(t)
	ctx := context.Background()

	decision := srv.AuthorizeMessageModeGrant(ctx, nil, projectID, store.MessageModeHub)
	if decision.Allowed {
		t.Error("nil identity should be denied")
	}
}

func TestIsNewHubGrant(t *testing.T) {
	tests := []struct {
		current  string
		resolved string
		want     bool
	}{
		// Hub → hub is NOT a new grant (no-op restart).
		{store.MessageModeHub, store.MessageModeHub, false},
		// Project → hub IS a new grant.
		{store.MessageModeProject, store.MessageModeHub, true},
		// Branch → hub IS a new grant.
		{store.MessageModeBranch, store.MessageModeHub, true},
		// None → hub IS a new grant.
		{store.MessageModeNone, store.MessageModeHub, true},
		// Any → project is NOT a new hub grant.
		{store.MessageModeHub, store.MessageModeProject, false},
		{store.MessageModeProject, store.MessageModeProject, false},
	}
	for _, tt := range tests {
		got := isNewHubGrant(tt.current, tt.resolved)
		if got != tt.want {
			t.Errorf("isNewHubGrant(%q, %q) = %v, want %v", tt.current, tt.resolved, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// grantGuardAgentIdentity is a minimal AgentIdentity implementation for
// grant guard unit tests, with scope and ancestry support.
// ---------------------------------------------------------------------------

type grantGuardAgentIdentity struct {
	id        string
	projectID string
	scopes    []AgentTokenScope
	ancestry  []string
}

func newTestAgentIdentity(id, projectID string, scopes []AgentTokenScope) *grantGuardAgentIdentity {
	return &grantGuardAgentIdentity{id: id, projectID: projectID, scopes: scopes}
}

func (a *grantGuardAgentIdentity) ID() string         { return a.id }
func (a *grantGuardAgentIdentity) Type() string       { return "agent" }
func (a *grantGuardAgentIdentity) ProjectID() string  { return a.projectID }
func (a *grantGuardAgentIdentity) Ancestry() []string { return a.ancestry }
func (a *grantGuardAgentIdentity) OriginUserID() string {
	if len(a.ancestry) > 0 {
		return a.ancestry[0]
	}
	return ""
}
func (a *grantGuardAgentIdentity) TokenID() string { return "test-token" }
func (a *grantGuardAgentIdentity) Scopes() []AgentTokenScope {
	return a.scopes
}
func (a *grantGuardAgentIdentity) HasScope(scope AgentTokenScope) bool {
	for _, s := range a.scopes {
		if s == scope {
			return true
		}
	}
	return false
}
