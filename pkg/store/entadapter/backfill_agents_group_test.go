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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBackfillProjectAgentsGroupMarkers_MarksLegitimateGroup verifies that the
// backfill adds the system annotation to a pre-existing project agents group
// that has a valid project ID and matching slug.
func TestBackfillProjectAgentsGroupMarkers_MarksLegitimateGroup(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	ownerID := uuid.New().String()
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID:      ownerID,
		Email:   "backfill-owner@example.com",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}))
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Backfill Project",
		Slug:    "backfill-project",
		OwnerID: ownerID,
		Created: time.Now(),
		Updated: time.Now(),
	}))

	groupID := uuid.New().String()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Backfill Project Agents",
		Slug:      "project:backfill-project:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
		OwnerID:   ownerID,
	}))

	// Run the backfill.
	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))

	// Verify the annotation was set.
	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, "true", group.Annotations[systemProjectAgentsGroupAnnotation],
		"backfill should mark legitimate agents group with system annotation")
}

// TestBackfillProjectAgentsGroupMarkers_UsesOwnMarkerSection verifies that the
// agents backfill uses its own idempotency marker, independent of the members
// backfill.
func TestBackfillProjectAgentsGroupMarkers_UsesOwnMarkerSection(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Simulate that the members backfill has already run by setting its marker.
	_, err := cs.UpsertHubSetting(ctx, projectMembersGroupMarkerBackfillSection,
		[]byte(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err)

	// Create a project and an agents group.
	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Marker Test",
		Slug:    "marker-test",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	groupID := uuid.New().String()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Marker Test Agents",
		Slug:      "project:marker-test:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
	}))

	// Run the agents backfill — it should succeed even though the members
	// marker is already present.
	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))

	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, "true", group.Annotations[systemProjectAgentsGroupAnnotation],
		"agents backfill must run independently of members marker")
}

// TestBackfillProjectAgentsGroupMarkers_Idempotent verifies that running the
// backfill twice is safe and does not error.
func TestBackfillProjectAgentsGroupMarkers_Idempotent(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Idempotent Test",
		Slug:    "idempotent-test",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID:        uuid.New().String(),
		Name:      "Idempotent Test Agents",
		Slug:      "project:idempotent-test:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
	}))

	// Run twice; second call should be a no-op.
	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))
	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))
}

// TestBackfillProjectAgentsGroupMarkers_OwnerMismatchWarning verifies that the
// backfill still marks a group when owner differs from project owner (preserving
// status quo), and that the function completes without error. The warning log
// is verified implicitly by the function not failing.
func TestBackfillProjectAgentsGroupMarkers_OwnerMismatchWarning(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectOwnerID := uuid.New().String()
	groupOwnerID := uuid.New().String()
	projectID := uuid.New().String()

	// Seed both users so FK constraints are satisfied.
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID:      projectOwnerID,
		Email:   "project-owner@example.com",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}))
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID:      groupOwnerID,
		Email:   "group-owner@example.com",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}))

	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Mismatch Owner",
		Slug:    "mismatch-owner",
		OwnerID: projectOwnerID,
		Created: time.Now(),
		Updated: time.Now(),
	}))

	groupID := uuid.New().String()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Mismatch Owner Agents",
		Slug:      "project:mismatch-owner:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
		OwnerID:   groupOwnerID, // Different from project owner
	}))

	// Backfill should complete without error and still mark the group.
	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))

	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, "true", group.Annotations[systemProjectAgentsGroupAnnotation],
		"backfill should still mark group even with owner mismatch")
	assert.Equal(t, groupOwnerID, group.OwnerID,
		"backfill must not change the group owner")
}

// D8-fix: Backfill sets scion.io/adoption-review-required annotation on groups
// with owner mismatch.
func TestBackfillProjectAgentsGroupMarkers_SetsAdoptionReviewAnnotation(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectOwnerID := uuid.New().String()
	groupOwnerID := uuid.New().String()
	projectID := uuid.New().String()

	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: projectOwnerID, Email: "proj-owner-review@example.com",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}))
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: groupOwnerID, Email: "grp-owner-review@example.com",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}))

	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Review Project", Slug: "review-project",
		OwnerID: projectOwnerID, Created: time.Now(), Updated: time.Now(),
	}))

	groupID := uuid.New().String()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Review Project Agents",
		Slug:      "project:review-project:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
		OwnerID:   groupOwnerID, // Different from project owner → mismatch
	}))

	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))

	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, "true", group.Annotations[systemProjectAgentsGroupAnnotation],
		"system annotation should be set")
	assert.Equal(t, "true", group.Annotations[adoptionReviewRequiredAnnotation],
		"adoption-review-required should be set when owners mismatch")
}

// D8-fix: Backfill does NOT set scion.io/adoption-review-required on groups
// with matching owners.
func TestBackfillProjectAgentsGroupMarkers_NoAdoptionReviewOnMatchingOwner(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	ownerID := uuid.New().String()
	projectID := uuid.New().String()

	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: ownerID, Email: "same-owner@example.com",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}))

	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Same Owner Project", Slug: "same-owner",
		OwnerID: ownerID, Created: time.Now(), Updated: time.Now(),
	}))

	groupID := uuid.New().String()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Same Owner Agents",
		Slug:      "project:same-owner:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
		OwnerID:   ownerID, // Same as project owner → no mismatch
	}))

	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))

	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, "true", group.Annotations[systemProjectAgentsGroupAnnotation],
		"system annotation should be set")
	assert.Empty(t, group.Annotations[adoptionReviewRequiredAnnotation],
		"adoption-review-required should NOT be set when owners match")
}

// D8-fix: The adoption-review-required annotation persists and is queryable
// after backfill completes — reading the group back returns the annotation.
func TestBackfillProjectAgentsGroupMarkers_AdoptionReviewPersists(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectOwnerID := uuid.New().String()
	groupOwnerID := uuid.New().String()
	projectID := uuid.New().String()

	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: projectOwnerID, Email: "persist-proj@example.com",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}))
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: groupOwnerID, Email: "persist-grp@example.com",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}))

	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Persist Project", Slug: "persist-project",
		OwnerID: projectOwnerID, Created: time.Now(), Updated: time.Now(),
	}))

	groupID := uuid.New().String()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Persist Agents",
		Slug:      "project:persist-project:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
		OwnerID:   groupOwnerID, // Mismatch
	}))

	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))

	// Read the group back and verify both annotations persist.
	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, "true", group.Annotations[adoptionReviewRequiredAnnotation],
		"adoption-review-required annotation must persist after backfill and be queryable")
	assert.Equal(t, "true", group.Annotations[systemProjectAgentsGroupAnnotation],
		"system annotation must persist after backfill")
}

// TestBackfillProjectAgentsGroupMarkers_SkipsMismatchedSlug verifies that a
// group whose slug does not match the project's expected agents slug pattern
// is skipped.
func TestBackfillProjectAgentsGroupMarkers_SkipsMismatchedSlug(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create project with slug "real-project"
	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Real Project",
		Slug:    "real-project",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	// Create a group with slug pattern that matches the prefix/suffix filter
	// but whose project slug doesn't match (e.g., project was renamed).
	groupID := uuid.New().String()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Renamed Agents",
		Slug:      "project:old-slug:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID, // points at "real-project" but slug says "old-slug"
	}))

	require.NoError(t, cs.BackfillProjectAgentsGroupMarkers(ctx))

	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Empty(t, group.Annotations[systemProjectAgentsGroupAnnotation],
		"backfill should skip groups with mismatched slugs")
}
