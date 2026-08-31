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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the hub-admin role definition permission backfill
// (backfillHubAdminRolePermissions in seed.go).
//
// Existing deployments have a hub-admin role definition whose Permissions list
// was fixed at creation time and does not include permissions added later
// (e.g. scheduled_event.*). The backfill additively appends the missing
// permissions without removing any that an operator may have customised.

// scheduledEventPermissionIDs returns the 5 scheduled_event.* permission IDs
// that the backfill is expected to add.
func scheduledEventPermissionIDs() []string {
	return []string{
		"scheduled_event.read",
		"scheduled_event.list",
		"scheduled_event.create",
		"scheduled_event.delete",
		"scheduled_event.update",
	}
}

// TestBackfillHubAdminRolePermissions verifies that backfillHubAdminRolePermissions
// is a no-op after CO1 cutover: stripped permissions remain absent and
// non-scheduled_event permissions are untouched.
func TestBackfillHubAdminRolePermissions(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Seed role definitions to create the hub-admin role.
	seedRoleDefinitions(ctx, s)

	// Fetch the hub-admin role and strip the scheduled_event.* permissions to
	// simulate a pre-existing deployment.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	sePerms := make(map[string]bool)
	for _, id := range scheduledEventPermissionIDs() {
		sePerms[id] = true
	}

	var stripped []string
	for _, p := range rd.Permissions {
		if !sePerms[p] {
			stripped = append(stripped, p)
		}
	}
	require.Less(t, len(stripped), len(rd.Permissions),
		"fixture precondition: hub-admin should contain scheduled_event permissions to strip")

	// Persist the stripped permissions.
	err = s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, stripped)
	require.NoError(t, err)

	// Verify precondition: scheduled_event permissions are absent.
	rd, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	for _, id := range scheduledEventPermissionIDs() {
		assert.NotContains(t, rd.Permissions, id,
			"fixture precondition: %s should have been removed", id)
	}

	// CO1: backfillHubAdminRolePermissions is now a no-op after cutover.
	// Call it to verify it does not restore the stripped permissions.
	backfillHubAdminRolePermissions(ctx, s)

	// Verify all 5 scheduled_event.* permissions are still absent (no-op after CO1 cutover).
	rd, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	for _, id := range scheduledEventPermissionIDs() {
		assert.NotContains(t, rd.Permissions, id,
			"backfill is a no-op after CO1 cutover, %s should remain absent", id)
	}

	// Verify the stripped (non-scheduled_event) permissions are still present.
	// The no-op backfill must not disturb existing permissions.
	for _, p := range stripped {
		assert.Contains(t, rd.Permissions, p,
			"no-op backfill must not remove existing permission %s", p)
	}
}

// TestBackfillHubAdminRolePermissions_Idempotent verifies the backfill is safe
// to run multiple times without duplicating permissions.
func TestBackfillHubAdminRolePermissions_Idempotent(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	seedRoleDefinitions(ctx, s)

	// Run the backfill three times.
	backfillHubAdminRolePermissions(ctx, s)
	backfillHubAdminRolePermissions(ctx, s)
	backfillHubAdminRolePermissions(ctx, s)

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	// Count occurrences of each scheduled_event permission — each should appear
	// exactly once.
	counts := make(map[string]int)
	for _, p := range rd.Permissions {
		counts[p]++
	}
	for _, id := range scheduledEventPermissionIDs() {
		assert.Equal(t, 1, counts[id],
			"repeated backfills must not duplicate permission %s", id)
	}
}

// TestBackfillHubAdminRolePermissions_PreservesCustomPermissions verifies that
// operator-added permissions are not removed by the backfill.
func TestBackfillHubAdminRolePermissions_PreservesCustomPermissions(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	seedRoleDefinitions(ctx, s)

	// Add a custom permission that is NOT in hubAdminPermissionIDs().
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	customPerm := "custom.operator.permission"
	permsWithCustom := append(rd.Permissions, customPerm)
	err = s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, permsWithCustom)
	require.NoError(t, err)

	// Run the backfill.
	backfillHubAdminRolePermissions(ctx, s)

	rd, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Contains(t, rd.Permissions, customPerm,
		"backfill must not remove operator-customised permissions")
}

// TestBackfillHubAdminRolePermissions_NoOpWhenComplete verifies the backfill
// is a no-op when all desired permissions are already present.
func TestBackfillHubAdminRolePermissions_NoOpWhenComplete(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	seedRoleDefinitions(ctx, s)

	// Capture the permissions before the backfill.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	before := make([]string, len(rd.Permissions))
	copy(before, rd.Permissions)

	// Run the backfill — should be a no-op on a fresh seed.
	backfillHubAdminRolePermissions(ctx, s)

	rd, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Equal(t, before, rd.Permissions,
		"backfill should be a no-op when all permissions are already present")
}
