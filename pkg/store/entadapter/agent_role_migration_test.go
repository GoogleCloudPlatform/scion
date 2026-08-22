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
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateBackfillsEmptyAgentRolesToFull(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "role-migration-project",
		Slug:    "role-migration-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	noConfig := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "no-config",
		Name:      "no-config",
		ProjectID: project.ID,
		Phase:     "created",
	}
	require.NoError(t, cs.CreateAgent(ctx, noConfig))

	emptyRole := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "empty-role",
		Name:      "empty-role",
		ProjectID: project.ID,
		Phase:     "created",
		AppliedConfig: &store.AgentAppliedConfig{
			Task: "keep me",
		},
	}
	require.NoError(t, cs.CreateAgent(ctx, emptyRole))

	explicitRole := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "explicit-role",
		Name:      "explicit-role",
		ProjectID: project.ID,
		Phase:     "created",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "readonly",
		},
	}
	require.NoError(t, cs.CreateAgent(ctx, explicitRole))

	require.NoError(t, cs.Migrate(ctx))

	gotNoConfig, err := cs.GetAgent(ctx, noConfig.ID)
	require.NoError(t, err)
	require.NotNil(t, gotNoConfig.AppliedConfig)
	assert.Equal(t, "full", gotNoConfig.AppliedConfig.AgentRole)

	gotEmptyRole, err := cs.GetAgent(ctx, emptyRole.ID)
	require.NoError(t, err)
	require.NotNil(t, gotEmptyRole.AppliedConfig)
	assert.Equal(t, "full", gotEmptyRole.AppliedConfig.AgentRole)
	assert.Equal(t, "keep me", gotEmptyRole.AppliedConfig.Task)

	gotExplicitRole, err := cs.GetAgent(ctx, explicitRole.ID)
	require.NoError(t, err)
	require.NotNil(t, gotExplicitRole.AppliedConfig)
	assert.Equal(t, "readonly", gotExplicitRole.AppliedConfig.AgentRole)

	futureEmptyRole := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "future-empty-role",
		Name:      "future-empty-role",
		ProjectID: project.ID,
		Phase:     "created",
		AppliedConfig: &store.AgentAppliedConfig{
			Task: "created after one-shot backfill",
		},
	}
	require.NoError(t, cs.CreateAgent(ctx, futureEmptyRole))

	require.NoError(t, cs.Migrate(ctx))

	gotFutureEmptyRole, err := cs.GetAgent(ctx, futureEmptyRole.ID)
	require.NoError(t, err)
	require.NotNil(t, gotFutureEmptyRole.AppliedConfig)
	assert.Empty(t, gotFutureEmptyRole.AppliedConfig.AgentRole)
	assert.Equal(t, "created after one-shot backfill", gotFutureEmptyRole.AppliedConfig.Task)
}

func TestMigrateBackfillsProjectMembersGroupSystemMarkers(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "testproj",
		Slug:    "testproj",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	legacyMembersGroup := &store.Group{
		ID:        uuid.NewString(),
		Name:      "Test Project Members",
		Slug:      "project:testproj:members",
		GroupType: store.GroupTypeExplicit,
		ProjectID: project.ID,
	}
	require.NoError(t, cs.CreateGroup(ctx, legacyMembersGroup))

	suspiciousGroup := &store.Group{
		ID:        uuid.NewString(),
		Name:      "Suspicious Members",
		Slug:      "project:suspicious:members",
		GroupType: store.GroupTypeExplicit,
	}
	require.NoError(t, cs.CreateGroup(ctx, suspiciousGroup))

	mismatchedGroup := &store.Group{
		ID:        uuid.NewString(),
		Name:      "Mismatched Members",
		Slug:      "project:not-testproj:members",
		GroupType: store.GroupTypeExplicit,
		ProjectID: project.ID,
	}
	require.NoError(t, cs.CreateGroup(ctx, mismatchedGroup))

	require.NoError(t, cs.Migrate(ctx))

	gotLegacy, err := cs.GetGroup(ctx, legacyMembersGroup.ID)
	require.NoError(t, err)
	assert.Equal(t, "true", gotLegacy.Annotations["scion.io/system-project-members-group"])

	gotSuspicious, err := cs.GetGroup(ctx, suspiciousGroup.ID)
	require.NoError(t, err)
	assert.NotContains(t, gotSuspicious.Annotations, "scion.io/system-project-members-group")

	gotMismatched, err := cs.GetGroup(ctx, mismatchedGroup.ID)
	require.NoError(t, err)
	assert.NotContains(t, gotMismatched.Annotations, "scion.io/system-project-members-group")

	futureLegacyMembersGroup := &store.Group{
		ID:        uuid.NewString(),
		Name:      "Future Legacy Members",
		Slug:      "project:future:members",
		GroupType: store.GroupTypeExplicit,
		ProjectID: project.ID,
	}
	require.NoError(t, cs.CreateGroup(ctx, futureLegacyMembersGroup))

	require.NoError(t, cs.Migrate(ctx))

	gotFuture, err := cs.GetGroup(ctx, futureLegacyMembersGroup.ID)
	require.NoError(t, err)
	assert.NotContains(t, gotFuture.Annotations, "scion.io/system-project-members-group")
}
