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

// TestSeedPolicy_SeededPoliciesHaveOrigin verifies that the startup seeder sets
// Origin="seeded" on the default policies.
func TestSeedPolicy_SeededPoliciesHaveOrigin(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	for _, name := range []string{"hub-member-read-all", "hub-member-create-projects"} {
		res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: name}, store.ListOptions{Limit: 1})
		require.NoError(t, err)
		require.Len(t, res.Items, 1, "expected seeded policy %q to exist", name)
		assert.Equal(t, store.PolicyOriginSeeded, res.Items[0].Origin,
			"seeded policy %q should have Origin=%q", name, store.PolicyOriginSeeded)
	}
}

// TestSeedPolicy_SkipsRecreationWhenTombstoneExists verifies that seedPolicy
// does not recreate a policy when a deletion tombstone hub setting exists.
func TestSeedPolicy_SkipsRecreationWhenTombstoneExists(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	const policyName = "hub-member-read-all"

	// Delete the seeded policy so we can test recreation.
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	require.NoError(t, s.DeletePolicy(ctx, res.Items[0].ID))

	// Plant a tombstone hub setting.
	key := seedPolicyTombstoneKey(policyName)
	_, err = s.UpsertHubSetting(ctx, key, json.RawMessage(`"true"`), "system", -1, "managed")
	require.NoError(t, err)

	// Get the hub-members group for the seedPolicy call.
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	// Re-run seedPolicy — it should NOT recreate.
	seedPolicy(ctx, s, group.ID, &store.Policy{
		ID:           api.NewUUID(),
		Name:         policyName,
		Description:  "Allow hub members to read all resources",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read", "list"},
		Effect:       "allow",
	})

	// Confirm it was not recreated.
	res, err = s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, res.TotalCount, "tombstoned policy should not be recreated")
}

// TestDeletePolicy_SeededPolicyCreatesTombstone verifies that deleting a seeded
// policy via the HTTP handler records a tombstone hub setting.
func TestDeletePolicy_SeededPolicyCreatesTombstone(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	const policyName = "hub-member-read-all"

	// Find the seeded policy.
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	policyID := res.Items[0].ID

	// Confirm it has seeded origin.
	assert.Equal(t, store.PolicyOriginSeeded, res.Items[0].Origin)

	// Delete via HTTP handler (as admin).
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/policies/"+policyID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify tombstone was created.
	key := seedPolicyTombstoneKey(policyName)
	setting, err := s.GetHubSetting(ctx, key)
	require.NoError(t, err, "tombstone hub setting should exist after deleting seeded policy")
	assert.Equal(t, key, setting.Section)
}

// TestDeletePolicy_UserCreatedPolicyNoTombstone verifies that deleting a
// user-created policy (non-seeded) does NOT create a tombstone hub setting.
func TestDeletePolicy_UserCreatedPolicyNoTombstone(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user-created policy (no Origin set).
	policy := &store.Policy{
		ID:           api.NewUUID(),
		Name:         "user-custom-policy",
		Description:  "A user-created policy",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read"},
		Effect:       "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))

	// Delete via HTTP handler.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/policies/"+policy.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify NO tombstone was created.
	key := seedPolicyTombstoneKey(policy.Name)
	_, err := s.GetHubSetting(ctx, key)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"no tombstone should be created for user-created policy deletion")
}

// TestBackfillSeededPolicyOrigin verifies that backfillSeededPolicyOrigin marks
// existing policies with Origin="seeded" when they have empty Origin.
func TestBackfillSeededPolicyOrigin(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	const testPolicyName = "test-backfill-policy"

	// Create a policy with empty Origin (simulating a pre-upgrade policy).
	policy := &store.Policy{
		ID:           api.NewUUID(),
		Name:         testPolicyName,
		Description:  "A policy needing backfill",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"read"},
		Effect:       "allow",
		// Origin intentionally empty — simulates pre-upgrade state.
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))

	// Run backfill with this policy name.
	backfillSeededPolicyOrigin(ctx, s, []string{testPolicyName})

	// Verify Origin was set.
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: testPolicyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, store.PolicyOriginSeeded, res.Items[0].Origin,
		"backfill should set Origin to %q", store.PolicyOriginSeeded)
}

// TestBackfillSeededPolicyOrigin_SkipsAlreadySet verifies that backfill does
// not re-update a policy that already has Origin="seeded".
func TestBackfillSeededPolicyOrigin_SkipsAlreadySet(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// The seeded policies already have Origin set by the seeder.
	// Record the Updated timestamp before backfill.
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: "hub-member-read-all"}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	updatedBefore := res.Items[0].Updated

	// Run backfill again.
	backfillSeededPolicyOrigin(ctx, s, []string{"hub-member-read-all"})

	// Verify it was not updated (timestamp should be unchanged).
	res, err = s.ListPolicies(ctx, store.PolicyFilter{Name: "hub-member-read-all"}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, updatedBefore, res.Items[0].Updated,
		"backfill should skip policies that already have Origin set")
}
