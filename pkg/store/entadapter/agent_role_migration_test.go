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
}
