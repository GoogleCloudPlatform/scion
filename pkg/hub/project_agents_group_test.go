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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateProjectGroup_NewGroupGetsAnnotation verifies that newly created
// project agents groups carry the system annotation.
func TestCreateProjectGroup_NewGroupGetsAnnotation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "Annotation Test",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	agentsSlug := "project:" + project.Slug + ":agents"
	group, err := s.GetGroupBySlug(ctx, agentsSlug)
	require.NoError(t, err, "agents group should have been created")
	assert.Equal(t, project.ID, group.ProjectID)
	assert.Equal(t, store.GroupTypeProjectAgents, group.GroupType)
	assert.Equal(t, "true", group.Annotations[store.AnnotationProjectAgentsGroup],
		"new agents group must carry the system annotation")
}

// TestCreateProjectGroup_AdoptionRefusedWithoutAnnotation verifies that
// createProjectGroup refuses to adopt a colliding group that lacks the
// system annotation.
func TestCreateProjectGroup_AdoptionRefusedWithoutAnnotation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a group with the agents slug pattern but without the system annotation.
	squatterGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Squatter Group",
		Slug:      "project:squatter-project:agents",
		GroupType: store.GroupTypeExplicit, // wrong type, no annotation
	}
	require.NoError(t, s.CreateGroup(ctx, squatterGroup))

	// Create a project whose slug collides with the squatter group.
	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Squatter Project",
		Slug:      "squatter-project",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Call createProjectGroup — it should refuse to adopt.
	srv.createProjectGroup(ctx, project)

	// The squatter group should still have its original ProjectID (empty).
	after, err := s.GetGroupBySlug(ctx, "project:squatter-project:agents")
	require.NoError(t, err)
	assert.Empty(t, after.ProjectID, "squatter group should not have been adopted")
	assert.Equal(t, squatterGroup.OwnerID, after.OwnerID, "squatter group owner should be unchanged")
}

// TestCreateProjectGroup_AdoptionSucceedsWithAnnotation verifies that
// createProjectGroup accepts a colliding group that carries both the correct
// annotation and group type (i.e. a legitimate system-managed agents group).
func TestCreateProjectGroup_AdoptionSucceedsWithAnnotation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := api.NewUUID()

	// Pre-create a legitimate agents group with annotation.
	legitimateGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Legit Agents",
		Slug:      "project:legit-project:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
		CreatedBy: DevUserID,
		Annotations: map[string]string{
			store.AnnotationProjectAgentsGroup: "true",
		},
	}
	require.NoError(t, s.CreateGroup(ctx, legitimateGroup))

	// Create a project that matches.
	project := &store.Project{
		ID:        projectID,
		Name:      "Legit Project",
		Slug:      "legit-project",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// createProjectGroup should see the existing group passes the check
	// and not log an error (the group already has the correct ProjectID).
	srv.createProjectGroup(ctx, project)

	after, err := s.GetGroupBySlug(ctx, "project:legit-project:agents")
	require.NoError(t, err)
	assert.Equal(t, projectID, after.ProjectID, "legitimate group should retain project ID")
}

// TestCreateProjectGroup_WrongGroupTypeRefused verifies that even with the
// annotation present, a group with the wrong GroupType is not adopted.
func TestCreateProjectGroup_WrongGroupTypeRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := api.NewUUID()

	// Create a group with the annotation but wrong GroupType.
	wrongTypeGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Wrong Type Agents",
		Slug:      "project:wrongtype-project:agents",
		GroupType: store.GroupTypeExplicit, // wrong type
		ProjectID: projectID,
		CreatedBy: DevUserID,
		Annotations: map[string]string{
			store.AnnotationProjectAgentsGroup: "true",
		},
	}
	require.NoError(t, s.CreateGroup(ctx, wrongTypeGroup))

	project := &store.Project{
		ID:        projectID,
		Name:      "WrongType Project",
		Slug:      "wrongtype-project",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// createProjectGroup should refuse because GroupType is wrong.
	srv.createProjectGroup(ctx, project)

	after, err := s.GetGroupBySlug(ctx, "project:wrongtype-project:agents")
	require.NoError(t, err)
	// The group should not be modified — it already had the projectID,
	// but the function should have refused and returned before any update.
	assert.Equal(t, store.GroupTypeExplicit, after.GroupType,
		"group type should not be changed by adoption")
}

// TestIsSystemProjectAgentsGroup verifies the predicate checks both
// ProjectID and annotation.
func TestIsSystemProjectAgentsGroup(t *testing.T) {
	projectID := "proj-123"

	tests := []struct {
		name   string
		group  *store.Group
		expect bool
	}{
		{
			name:   "nil group",
			group:  nil,
			expect: false,
		},
		{
			name: "matching ProjectID and annotation",
			group: &store.Group{
				ProjectID:   projectID,
				Annotations: map[string]string{store.AnnotationProjectAgentsGroup: "true"},
			},
			expect: true,
		},
		{
			name: "wrong ProjectID",
			group: &store.Group{
				ProjectID:   "other-project",
				Annotations: map[string]string{store.AnnotationProjectAgentsGroup: "true"},
			},
			expect: false,
		},
		{
			name: "missing annotation",
			group: &store.Group{
				ProjectID: projectID,
			},
			expect: false,
		},
		{
			name: "nil annotations map",
			group: &store.Group{
				ProjectID:   projectID,
				Annotations: nil,
			},
			expect: false,
		},
		{
			name: "annotation value not true",
			group: &store.Group{
				ProjectID:   projectID,
				Annotations: map[string]string{store.AnnotationProjectAgentsGroup: "false"},
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, isSystemProjectAgentsGroup(tt.group, projectID))
		})
	}
}

// TestGetProject_SecurityGatePrecedesGroupWrites verifies that getProject
// enforces CheckAccess before backfilling project groups, so denied callers
// cause no store writes while authorized user and agent callers still backfill
// and read the project as before (ptone/scion#1393).
func TestGetProject_SecurityGatePrecedesGroupWrites(t *testing.T) {
	setupPreBackfillProject := func(t *testing.T, slug string) (
		srv *Server,
		s store.Store,
		alice *store.User,
		bob *store.User,
		project *store.Project,
		agentsSlug string,
		membersSlug string,
	) {
		t.Helper()
		srv, s = testServer(t)
		ctx := context.Background()

		alice = &store.User{
			ID:          api.NewUUID(),
			Email:       "alice-" + slug + "@test.com",
			DisplayName: "Alice",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, alice))
		ensureHubMembership(ctx, s, alice.ID)

		bob = &store.User{
			ID:          api.NewUUID(),
			Email:       "bob-" + slug + "@test.com",
			DisplayName: "Bob",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, bob))
		ensureHubMembership(ctx, s, bob.ID)

		project = &store.Project{
			ID:        api.NewUUID(),
			Name:      "Pre-Backfill " + slug,
			Slug:      slug,
			OwnerID:   alice.ID,
			CreatedBy: alice.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, project))
		require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, alice.ID))

		agentsSlug = "project:" + project.Slug + ":agents"
		membersSlug = projectMembersGroupSlug(project.Slug)

		// Precondition: neither system group exists before getProject runs.
		_, err := s.GetGroupBySlug(ctx, agentsSlug)
		require.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetGroupByProjectID(ctx, project.ID)
		require.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetGroupBySlug(ctx, membersSlug)
		require.ErrorIs(t, err, store.ErrNotFound)

		return srv, s, alice, bob, project, agentsSlug, membersSlug
	}

	assertNoProjectGroupsCreated := func(t *testing.T, s store.Store, projectID, agentsSlug, membersSlug string) {
		t.Helper()
		ctx := context.Background()
		_, err := s.GetGroupBySlug(ctx, agentsSlug)
		assert.ErrorIs(t, err, store.ErrNotFound, "denied getProject must not create project_agents group by slug")
		_, err = s.GetGroupByProjectID(ctx, projectID)
		assert.ErrorIs(t, err, store.ErrNotFound, "denied getProject must not create project_agents group for project")
		_, err = s.GetGroupBySlug(ctx, membersSlug)
		assert.ErrorIs(t, err, store.ErrNotFound, "denied getProject must not create project members group")
	}

	assertBothProjectGroupsCreated := func(t *testing.T, s store.Store, projectID, agentsSlug, membersSlug string) {
		t.Helper()
		ctx := context.Background()
		agentsGroup, err := s.GetGroupBySlug(ctx, agentsSlug)
		require.NoError(t, err, "authorized getProject should backfill project_agents group")
		assert.Equal(t, projectID, agentsGroup.ProjectID)
		assert.Equal(t, store.GroupTypeProjectAgents, agentsGroup.GroupType)
		assert.Equal(t, "true", agentsGroup.Annotations[store.AnnotationProjectAgentsGroup])

		byProj, err := s.GetGroupByProjectID(ctx, projectID)
		require.NoError(t, err)
		assert.Equal(t, agentsGroup.ID, byProj.ID)

		membersGroup, err := s.GetGroupBySlug(ctx, membersSlug)
		require.NoError(t, err, "authorized getProject should backfill project members group")
		assert.Equal(t, projectID, membersGroup.ProjectID)
		assert.Equal(t, "true", membersGroup.Annotations[store.AnnotationProjectMembersGroup])
	}

	t.Run("unauthorized user causes no group writes", func(t *testing.T) {
		srv, s, _, bob, project, agentsSlug, membersSlug := setupPreBackfillProject(t, "secgate-unauth-user")

		rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/projects/"+project.ID, nil)
		require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())

		assertNoProjectGroupsCreated(t, s, project.ID, agentsSlug, membersSlug)
	})

	t.Run("unauthorized agent from another project causes no group writes", func(t *testing.T) {
		srv, s, _, bob, project, agentsSlug, membersSlug := setupPreBackfillProject(t, "secgate-unauth-agent")
		ctx := context.Background()

		otherProject := &store.Project{
			ID:        api.NewUUID(),
			Name:      "Bob Project",
			Slug:      "secgate-bob-other",
			OwnerID:   bob.ID,
			CreatedBy: bob.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, otherProject))
		require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, otherProject.ID, bob.ID))

		foreignAgentID := api.NewUUID()
		createDCAgent(t, s, foreignAgentID, otherProject.ID, bob.ID, AgentRoleBaseline)
		seedRecordedDelegationEdge(t, s, "user", bob.ID, "agent", foreignAgentID, store.RoleScopeProject, otherProject.ID, string(AgentRoleBaseline))

		tok := srv.generateRecordedAgentToken(t, foreignAgentID, otherProject.ID, []AgentTokenScope{ScopeProjectRead}, nil)

		rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID, nil, tok)
		require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())

		assertNoProjectGroupsCreated(t, s, project.ID, agentsSlug, membersSlug)
	})

	t.Run("missing identity causes no group writes", func(t *testing.T) {
		srv, s, _, _, project, agentsSlug, membersSlug := setupPreBackfillProject(t, "secgate-no-identity")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+project.ID, nil)
		rec := httptest.NewRecorder()
		srv.getProject(rec, req, project.ID)
		require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())

		assertNoProjectGroupsCreated(t, s, project.ID, agentsSlug, membersSlug)
	})

	t.Run("authorized user backfills both groups and returns project", func(t *testing.T) {
		srv, s, alice, _, project, agentsSlug, membersSlug := setupPreBackfillProject(t, "secgate-auth-user")

		rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/projects/"+project.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var resp ProjectWithCapabilities
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, project.ID, resp.ID)

		assertBothProjectGroupsCreated(t, s, project.ID, agentsSlug, membersSlug)
	})

	t.Run("authorized project agent backfills both groups and returns project", func(t *testing.T) {
		srv, s, alice, _, project, agentsSlug, membersSlug := setupPreBackfillProject(t, "secgate-auth-agent")

		agentID := api.NewUUID()
		createDCAgent(t, s, agentID, project.ID, alice.ID, AgentRoleBaseline)
		seedRecordedDelegationEdge(t, s, "user", alice.ID, "agent", agentID, store.RoleScopeProject, project.ID, string(AgentRoleBaseline))

		tok := srv.generateRecordedAgentToken(t, agentID, project.ID, []AgentTokenScope{ScopeProjectRead}, nil)

		rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID, nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var resp ProjectWithCapabilities
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, project.ID, resp.ID)

		assertBothProjectGroupsCreated(t, s, project.ID, agentsSlug, membersSlug)
	})
}
