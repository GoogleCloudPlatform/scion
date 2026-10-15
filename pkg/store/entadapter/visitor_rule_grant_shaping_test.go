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

// Project visitor rules under grant shaping (admin tier A3). Every
// identity here is synthetic, in an in-memory store.

// visitorShapingEnv is a visitor env with one admin-kind test identity and
// the context of a request it makes.
type visitorShapingEnv struct {
	*visitorEnv
	admin   *store.User
	shaping store.GrantShaping
	shaped  context.Context
}

func newVisitorShapingEnv(t *testing.T) visitorShapingEnv {
	t.Helper()
	env := newVisitorEnv(t)
	ctx := context.Background()
	rd := newTestAdminSystemRole(t, env.cs, store.SystemRoleHubAdmin)
	admin := newTestAdminUser(store.UserKindTestHubAdmin, uuid.NewString(), time.Now().Add(30*time.Minute))
	_, err := env.cs.CreateTestAdminUser(ctx, admin, rd.ID)
	require.NoError(t, err)
	admin, err = env.cs.GetUser(ctx, admin.ID)
	require.NoError(t, err)
	g := store.GrantShaping{IdentityID: admin.ID, IssuanceID: *admin.IssuanceID, ExpiresAt: *admin.ExpiresAt}
	shaped := store.ContextWithGrantShaping(store.ContextWithGrantActor(ctx, admin.ID), g)
	return visitorShapingEnv{visitorEnv: env, admin: admin, shaping: g, shaped: shaped}
}

// An admin-kind test identity adds no visitor rule, changes no rule's
// window (not even to shorten it) and writes no rule binding: none of it
// could be bounded by the identity's expiry. Without the shaping its
// request is refused as unshaped. A person's request is not affected.
func TestGrantShaping_VisitorRuleWritesRefused(t *testing.T) {
	e := newVisitorShapingEnv(t)
	ctx := context.Background()

	err := e.cs.CreateProjectVisitorRule(e.shaped, &store.ProjectVisitorRule{
		ProjectID: e.projectID, Issuer: testFedIssuer, Domain: "shaped.example.com", CreatedBy: e.admin.ID})
	assert.ErrorIs(t, err, store.ErrTestAdminGrantRefused, "rule create under shaping")
	err = e.cs.CreateProjectVisitorRule(store.ContextWithGrantActor(ctx, e.admin.ID), &store.ProjectVisitorRule{
		ProjectID: e.projectID, Issuer: testFedIssuer, Domain: "unshaped.example.com", CreatedBy: e.admin.ID})
	assert.ErrorIs(t, err, store.ErrTestAdminGrantUnshaped, "rule create by the identity without shaping")
	rules, err := e.cs.ListProjectVisitorRules(ctx, e.projectID)
	require.NoError(t, err)
	assert.Empty(t, rules, "no rule was written")

	// A rule a person added, with one binding.
	rule := e.createVisitorRule(t, testFedIssuer, "corp.com")
	alice := createFederatedUser(t, e.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	rb, err := e.cs.CreateVisitorRoleBinding(ctx, e.ruleBinding(alice.ID, rule))
	require.NoError(t, err)

	shorter := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	_, _, err = e.cs.UpdateProjectVisitorRuleWindow(e.shaped, rule.ID, nil, &shorter)
	assert.ErrorIs(t, err, store.ErrTestAdminGrantRefused, "rule window update under shaping, even a shorter one")
	gotRule, err := e.cs.GetProjectVisitorRule(ctx, rule.ID)
	require.NoError(t, err)
	assert.Nil(t, gotRule.ExpiresAt, "the rule's window is unchanged")
	gotRB, err := e.cs.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	assert.Nil(t, gotRB.ExpiresAt, "its binding's window is unchanged")

	bob := createFederatedUser(t, e.cs, testFedIssuer, "bob@corp.com", store.UserStatusActive)
	inside := time.Now().Add(5 * time.Minute)
	ruleRB := e.ruleBinding(bob.ID, rule)
	ruleRB.ExpiresAt = &inside
	_, err = e.cs.CreateVisitorRoleBinding(e.shaped, ruleRB)
	assert.ErrorIs(t, err, store.ErrTestAdminGrantRefused, "a rule binding under shaping, whatever its window")
	listed, err := e.cs.ListVisitorRoleBindings(ctx, e.projectID)
	require.NoError(t, err)
	assert.Len(t, listed, 1, "only the person's rule binding exists")

	// Person control: the same window update succeeds.
	_, changed, err := e.cs.UpdateProjectVisitorRuleWindow(ctx, rule.ID, nil, &shorter)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	require.NotNil(t, changed[0].ExpiresAt)
	assert.True(t, changed[0].ExpiresAt.Equal(shorter))
}

// Removal only takes access away, so an admin-kind test identity may
// remove a rule (with its bindings) and exclude a visitor from a rule,
// as it may delete a role binding.
func TestGrantShaping_VisitorRuleRemovalAllowed(t *testing.T) {
	e := newVisitorShapingEnv(t)
	ctx := context.Background()
	rule := e.createVisitorRule(t, testFedIssuer, "corp.com")
	alice := createFederatedUser(t, e.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	_, err := e.cs.CreateVisitorRoleBinding(ctx, e.ruleBinding(alice.ID, rule))
	require.NoError(t, err)

	require.NoError(t, e.cs.CreateProjectVisitorExclusion(e.shaped, rule.ID, alice.ID, e.admin.ID), "exclusion under shaping")
	excluded, err := e.cs.IsProjectVisitorExcluded(ctx, rule.ID, alice.ID)
	require.NoError(t, err)
	assert.True(t, excluded)

	deleted, err := e.cs.DeleteProjectVisitorRule(e.shaped, rule.ID)
	require.NoError(t, err, "rule delete under shaping")
	assert.Len(t, deleted, 1, "its binding was deleted with it")
	_, err = e.cs.GetProjectVisitorRule(ctx, rule.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	listed, err := e.cs.ListVisitorRoleBindings(ctx, e.projectID)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

// A visitor binding written under shaping never ends after the identity,
// whatever window is requested: the clamp is applied after every other
// window the store sets.
func TestGrantShaping_VisitorBindingClampedLast(t *testing.T) {
	e := newVisitorShapingEnv(t)
	ctx := context.Background()
	identityExp := testAdminExpiry(*e.admin.ExpiresAt)
	start := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	later := identityExp.Add(time.Hour)
	earlier := identityExp.Add(-10 * time.Minute).UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name      string
		requested *time.Time
		want      time.Time
	}{
		{"no expiry", nil, identityExp},
		{"later than the identity", &later, identityExp},
		{"earlier than the identity", &earlier, earlier},
	} {
		u := createFederatedUser(t, e.cs, testFedIssuer, "v-"+uuid.NewString()[:8]+"@corp.com", store.UserStatusActive)
		rb := e.visitorBinding(u.ID)
		rb.CreatedBy = e.admin.ID
		rb.NotBefore = &start
		rb.ExpiresAt = tc.requested
		created, err := e.cs.CreateVisitorRoleBinding(e.shaped, rb)
		require.NoError(t, err, tc.name)
		got, err := e.cs.GetRoleBinding(ctx, created.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ExpiresAt, tc.name)
		assert.True(t, got.ExpiresAt.Equal(tc.want), "%s: stored %v want %v", tc.name, got.ExpiresAt, tc.want)
		assert.False(t, got.ExpiresAt.After(identityExp), "%s: never after the identity", tc.name)
		require.NotNil(t, got.IssuanceID, tc.name)
		assert.Equal(t, e.shaping.IssuanceID, *got.IssuanceID, tc.name)
		assert.Nil(t, got.SourceRuleID, tc.name)
	}
}
