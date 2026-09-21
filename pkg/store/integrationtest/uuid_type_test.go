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

//go:build integration

// Regression test for https://github.com/ptone/scion/issues/1634
//
// GetEffectiveGroupsForAgent seeds a recursive CTE with uuid.UUID args.
// On Postgres, an untyped `SELECT $1 AS id` infers text when the parameter
// is bound as text (google/uuid.UUID's driver.Valuer returns a string,
// which can happen even after OpenPostgres registers uuid with pgx).
// The join `gc.parent_group_id = e.id` then fails with SQLSTATE 42883
// ("operator does not exist: uuid = text"). The seed now uses `$1::uuid`.
package integrationtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestUUID_GetEffectiveGroupsForAgent_Postgres verifies that the recursive CTE
// in GetEffectiveGroupsForAgent correctly encodes uuid.UUID parameters on a
// live Postgres backend. This is the exact code path that produced SQLSTATE
// 42883 before the pgx UUID type registration fix (issue #1634).
func TestUUID_GetEffectiveGroupsForAgent_Postgres(t *testing.T) {
	requirePG(t)
	cs := newStore(t)
	ctx := context.Background()

	// Create a project
	proj := makeProject("uuid-regression-1634")
	require.NoError(t, cs.CreateProject(ctx, proj))

	// Create an agent in that project
	agent := makeAgent(proj.ID, "uuid-agent")
	require.NoError(t, cs.CreateAgent(ctx, agent))

	// Create a project_agents group linked to the project
	projectGroup := &store.Group{
		ID:        uuid.NewString(),
		Name:      "Project Agents",
		Slug:      "project:uuid-regression-1634:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: proj.ID,
	}
	require.NoError(t, cs.CreateGroup(ctx, projectGroup))

	// Create a parent group and nest the project group under it
	parentGroup := &store.Group{
		ID:   uuid.NewString(),
		Name: "Parent Group",
		Slug: "uuid-parent-1634",
	}
	require.NoError(t, cs.CreateGroup(ctx, parentGroup))

	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    parentGroup.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   projectGroup.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// This is the call that fails with SQLSTATE 42883 when the CTE seed
	// parameter is bound as text. Casting the seed to uuid on Postgres
	// makes the CTE column type uuid regardless of how pgx encodes the arg.
	effective, err := cs.GetEffectiveGroupsForAgent(ctx, agent.ID)
	require.NoError(t, err, "GetEffectiveGroupsForAgent must not fail with uuid/text mismatch on Postgres")

	found := make(map[string]bool, len(effective))
	for _, gid := range effective {
		found[gid] = true
	}
	assert.True(t, found[projectGroup.ID], "expected implicit project_agents group")
	assert.True(t, found[parentGroup.ID], "expected transitive parent group")
}
