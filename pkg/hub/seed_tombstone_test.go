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

// TestSeedPolicy_SeededPoliciesHaveOrigin verifies that after CO1 cutover,
// seedPolicy is a no-op — no policies are created. All authorization is
// handled by RoleBindings and the AK1 kernel.
func TestSeedPolicy_SeededPoliciesHaveOrigin(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Get the hub-members group for the seedPolicy call.
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	// seedPolicy is now a no-op after CO1 cutover.
	seedPolicy(ctx, s, group.ID, &store.Policy{
		ID:           api.NewUUID(),
		Name:         "test-seed-origin-check",
		Description:  "Test policy for origin check",
		ScopeType:    "hub",
		ResourceType: "user",
		Actions:      []string{"read", "list"},
		Effect:       "allow",
	})

	// Verify no policy was created (seedPolicy is a no-op in CO1).
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: "test-seed-origin-check"}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Len(t, res.Items, 0, "seedPolicy should be a no-op after CO1 cutover")
}

// TestSeedPolicy_SkipsRecreationWhenTombstoneExists verifies that after CO1
// cutover, seedPolicy is a no-op regardless of tombstone state. The tombstone
// mechanism (hasSeedPolicyTombstone) is still functional for any code that
// checks it directly.
func TestSeedPolicy_SkipsRecreationWhenTombstoneExists(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	const policyName = "test-tombstone-policy"

	// Plant a tombstone hub setting.
	key := seedPolicyTombstoneKey(policyName)
	_, err := s.UpsertHubSetting(ctx, key, json.RawMessage(`"true"`), "system", -1, "managed")
	require.NoError(t, err)

	// Verify the tombstone check still works.
	hasTomb, err := hasSeedPolicyTombstone(ctx, s, policyName)
	require.NoError(t, err)
	assert.True(t, hasTomb, "tombstone should be detected")

	// Get the hub-members group for the seedPolicy call.
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	// seedPolicy is a no-op after CO1 — nothing is created.
	seedPolicy(ctx, s, group.ID, &store.Policy{
		ID:           api.NewUUID(),
		Name:         policyName,
		Description:  "Test policy for tombstone check",
		ScopeType:    "hub",
		ResourceType: "user",
		Actions:      []string{"read", "list"},
		Effect:       "allow",
	})

	// Confirm no policy exists (seedPolicy is a no-op).
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, res.TotalCount, "seedPolicy should be a no-op after CO1 cutover")
}

// TestDeletePolicy_SeededPolicyCreatesTombstone verifies that after CO1 cutover,
// the policy DELETE endpoint returns 410 Gone since policies are no longer
// the authorization mechanism (replaced by RoleBindings).
func TestDeletePolicy_SeededPolicyCreatesTombstone(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	const policyName = "test-tombstone-seeded-delete"

	// Create a seeded policy directly in the store.
	policy := &store.Policy{
		ID:           api.NewUUID(),
		Name:         policyName,
		Description:  "A test seeded policy",
		ScopeType:    "hub",
		ResourceType: "user",
		Actions:      []string{"read"},
		Effect:       "allow",
		Origin:       store.PolicyOriginSeeded,
		PolicyKind:   store.PolicyKindDefault,
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))

	// CO1: Policy DELETE endpoint returns 410 Gone.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/policies/"+policy.ID, nil)
	assert.Equal(t, http.StatusGone, rec.Code)

	// Verify the policy still exists in the store (not deleted via HTTP).
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount, "policy should still exist since HTTP DELETE returns 410")
}

// TestDeletePolicy_UserCreatedPolicyNoTombstone verifies that after CO1
// cutover, the policy DELETE endpoint returns 410 Gone for user-created
// policies as well.
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

	// CO1: Policy DELETE endpoint returns 410 Gone.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/policies/"+policy.ID, nil)
	assert.Equal(t, http.StatusGone, rec.Code)

	// Verify NO tombstone was created (endpoint returned 410, no deletion occurred).
	key := seedPolicyTombstoneKey(policy.Name)
	_, err := s.GetHubSetting(ctx, key)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"no tombstone should be created when policy DELETE returns 410")
}

// TestBackfillSeededPolicyOrigin verifies that after CO1 cutover,
// backfillSeededPolicyOrigin is a no-op — policy origin tracking is no
// longer needed since authorization uses RoleBindings.
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

	// backfillSeededPolicyOrigin is a no-op after CO1 cutover.
	backfillSeededPolicyOrigin(ctx, s, []string{testPolicyName})

	// Verify Origin was NOT set (backfill is a no-op).
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: testPolicyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "", res.Items[0].Origin,
		"backfillSeededPolicyOrigin should be a no-op after CO1 cutover")
}

// TestBackfillSeededPolicyOrigin_SkipsAlreadySet verifies that backfill does
// not re-update a policy that already has Origin="seeded".
func TestBackfillSeededPolicyOrigin_SkipsAlreadySet(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	const policyName = "test-backfill-skip-origin"

	// Create a policy with Origin already set.
	policy := &store.Policy{
		ID:           api.NewUUID(),
		Name:         policyName,
		Description:  "A policy already backfilled",
		ScopeType:    "hub",
		ResourceType: "user",
		Actions:      []string{"read"},
		Effect:       "allow",
		Origin:       store.PolicyOriginSeeded,
		PolicyKind:   store.PolicyKindDefault,
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))

	// Record the Updated timestamp before backfill.
	res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	updatedBefore := res.Items[0].Updated

	// Run backfill again.
	backfillSeededPolicyOrigin(ctx, s, []string{policyName})

	// Verify it was not updated (timestamp should be unchanged).
	res, err = s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, updatedBefore, res.Items[0].Updated,
		"backfill should skip policies that already have Origin set")
}
