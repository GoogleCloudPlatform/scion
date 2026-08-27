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
	assert.Equal(t, "true", group.Annotations[systemProjectAgentsGroupAnnotation],
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
			systemProjectAgentsGroupAnnotation: "true",
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
			systemProjectAgentsGroupAnnotation: "true",
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
				Annotations: map[string]string{systemProjectAgentsGroupAnnotation: "true"},
			},
			expect: true,
		},
		{
			name: "wrong ProjectID",
			group: &store.Group{
				ProjectID:   "other-project",
				Annotations: map[string]string{systemProjectAgentsGroupAnnotation: "true"},
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
				Annotations: map[string]string{systemProjectAgentsGroupAnnotation: "false"},
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
