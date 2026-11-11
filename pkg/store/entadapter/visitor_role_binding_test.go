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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project visitor role and visitor grants (ptone/scion#4603), store half.

// visitorEnv holds a store with the project roles a visitor test needs:
// the system project-visitor role (does not confer membership) and the
// system project-member role.
type visitorEnv struct {
	cs        *CompositeStore
	visitor   *store.RoleDefinition
	member    *store.RoleDefinition
	projectID string
}

func newVisitorEnv(t *testing.T) *visitorEnv {
	t.Helper()
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	noMembership := false
	visitor, err := cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:              store.ProjectRoleVisitor,
		ScopeType:         store.RoleScopeProject,
		Permissions:       []string{"agent.message", "agent.list", "agent.read"},
		System:            true,
		ConfersMembership: &noMembership,
	})
	require.NoError(t, err)
	member, err := cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        store.ProjectRoleMember,
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
		System:      true,
	})
	require.NoError(t, err)
	return &visitorEnv{cs: cs, visitor: visitor, member: member, projectID: uuid.NewString()}
}

func (e *visitorEnv) humanUser(t *testing.T) *store.User {
	t.Helper()
	u := &store.User{ID: uuid.NewString(), Email: "h-" + uuid.NewString()[:8] + "@corp.com", DisplayName: "Human", Status: store.UserStatusActive}
	require.NoError(t, e.cs.CreateUser(context.Background(), u))
	return u
}

func (e *visitorEnv) visitorBinding(userID string) *store.RoleBinding {
	return &store.RoleBinding{
		RoleDefinitionID: e.visitor.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          e.projectID,
		CreatedBy:        "test",
	}
}

func TestRoleDefinition_ConfersMembershipDefaultsTrue(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()

	custom, err := env.cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "custom-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"agent.list"},
	})
	require.NoError(t, err)
	for _, id := range []string{custom.ID, env.member.ID} {
		got, err := env.cs.GetRoleDefinition(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got.ConfersMembership)
		assert.True(t, *got.ConfersMembership, "role %s", got.Name)
		assert.True(t, got.ConfersProjectMembership())
	}

	got, err := env.cs.GetRoleDefinitionByName(ctx, store.ProjectRoleVisitor, store.RoleScopeProject)
	require.NoError(t, err)
	require.NotNil(t, got.ConfersMembership)
	assert.False(t, *got.ConfersMembership)
	assert.False(t, got.ConfersProjectMembership())

	// A role definition that was not resolved confers nothing.
	var missing *store.RoleDefinition
	assert.False(t, missing.ConfersProjectMembership())
}

func TestCreateVisitorRoleBinding_BindsFederatedAccount(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	fed := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)

	notBefore := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := notBefore.Add(30 * 24 * time.Hour)
	rb := env.visitorBinding(fed.ID)
	rb.NotBefore, rb.ExpiresAt = &notBefore, &expiresAt
	created, err := env.cs.CreateVisitorRoleBinding(ctx, rb)
	require.NoError(t, err)
	assert.Equal(t, env.visitor.ID, created.RoleDefinitionID)
	require.NotNil(t, created.NotBefore)
	require.NotNil(t, created.ExpiresAt)
	assert.True(t, created.NotBefore.Equal(notBefore))
	assert.True(t, created.ExpiresAt.Equal(expiresAt))

	// An empty role definition ID resolves the project-visitor role.
	other := createFederatedUser(t, env.cs, testFedIssuer, "bob@corp.com", store.UserStatusActive)
	rb = env.visitorBinding(other.ID)
	rb.RoleDefinitionID = ""
	created, err = env.cs.CreateVisitorRoleBinding(ctx, rb)
	require.NoError(t, err)
	assert.Equal(t, env.visitor.ID, created.RoleDefinitionID)

	// A duplicate is ErrAlreadyExists.
	_, err = env.cs.CreateVisitorRoleBinding(ctx, env.visitorBinding(other.ID))
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestCreateVisitorRoleBinding_Refusals(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	fed := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	human := env.humanUser(t)

	customVisitorName, err := env.cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "custom-visitor-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"agent.message"},
	})
	require.NoError(t, err)
	systemVisitorScope, err := env.cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: store.ProjectRoleVisitor, ScopeType: store.RoleScopeSystem, Permissions: []string{"agent.message"}, System: true,
		ConfersMembership: func() *bool { b := false; return &b }(),
	})
	require.NoError(t, err)

	groupID := uuid.NewString()
	require.NoError(t, env.cs.CreateGroup(ctx, &store.Group{ID: groupID, Name: "g-" + groupID, Slug: "g-" + groupID}))

	for name, tc := range map[string]struct {
		mutate func(rb *store.RoleBinding)
		want   error
	}{
		"local account": {func(rb *store.RoleBinding) { rb.PrincipalID = human.ID }, store.ErrVisitorBindingRefused},
		"member role":   {func(rb *store.RoleBinding) { rb.RoleDefinitionID = env.member.ID }, store.ErrVisitorBindingRefused},
		"custom role":   {func(rb *store.RoleBinding) { rb.RoleDefinitionID = customVisitorName.ID }, store.ErrVisitorBindingRefused},
		"system scope": {func(rb *store.RoleBinding) {
			rb.RoleDefinitionID = systemVisitorScope.ID
			rb.ScopeType = store.RoleScopeSystem
			rb.ScopeID = ""
		}, store.ErrVisitorBindingRefused},
		"system scope on project role": {func(rb *store.RoleBinding) {
			rb.ScopeType = store.RoleScopeSystem
			rb.ScopeID = ""
		}, store.ErrVisitorBindingRefused},
		"group principal": {func(rb *store.RoleBinding) {
			rb.PrincipalType = store.RoleBindingPrincipalGroup
			rb.PrincipalID = groupID
		}, store.ErrVisitorBindingRefused},
		"empty project":  {func(rb *store.RoleBinding) { rb.ScopeID = "" }, store.ErrInvalidInput},
		"missing user":   {func(rb *store.RoleBinding) { rb.PrincipalID = uuid.NewString() }, store.ErrNotFound},
		"invalid role":   {func(rb *store.RoleBinding) { rb.RoleDefinitionID = "not-a-uuid" }, store.ErrInvalidInput},
		"unknown role":   {func(rb *store.RoleBinding) { rb.RoleDefinitionID = uuid.NewString() }, store.ErrNotFound},
		"bad principal":  {func(rb *store.RoleBinding) { rb.PrincipalType = "robot" }, store.ErrInvalidInput},
		"invalid userID": {func(rb *store.RoleBinding) { rb.PrincipalID = "nope" }, store.ErrInvalidInput},
	} {
		t.Run(name, func(t *testing.T) {
			rb := env.visitorBinding(fed.ID)
			tc.mutate(rb)
			_, err := env.cs.CreateVisitorRoleBinding(ctx, rb)
			assert.ErrorIs(t, err, tc.want)
			if tc.want != store.ErrVisitorBindingRefused {
				// ErrVisitorBindingRefused wraps ErrInvalidInput: the
				// refusal must come from the named check, not the guard.
				assert.NotErrorIs(t, err, store.ErrVisitorBindingRefused)
			}
		})
	}

	bindings, err := env.cs.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, fed.ID)
	require.NoError(t, err)
	assert.Empty(t, bindings, "no refused binding was written")
	_, err = env.cs.CreateVisitorRoleBinding(ctx, nil)
	assert.ErrorIs(t, err, store.ErrVisitorBindingRefused)
}

// A custom role named project-visitor (created by an operator before the
// built-in role was seeded) is not the visitor role.
func TestCreateVisitorRoleBinding_RefusesCustomRoleWithVisitorName(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	custom, err := cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: store.ProjectRoleVisitor, ScopeType: store.RoleScopeProject, Permissions: []string{"agent.message"},
	})
	require.NoError(t, err)
	fed := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)

	for _, roleID := range []string{custom.ID, ""} {
		_, err = cs.CreateVisitorRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: roleID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      fed.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          uuid.NewString(),
		})
		assert.ErrorIs(t, err, store.ErrVisitorBindingRefused, "role id %q", roleID)
	}
}

// The general CreateRoleBinding refuses the project-visitor role for any
// principal, and keeps refusing federated accounts for every role.
func TestCreateRoleBinding_RefusesVisitorRoleAndFederatedAccount(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	human := env.humanUser(t)
	fed := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)

	_, err := env.cs.CreateRoleBinding(ctx, env.visitorBinding(human.ID))
	assert.ErrorIs(t, err, store.ErrNonMembershipRoleRefused)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	_, err = env.cs.CreateRoleBinding(ctx, env.visitorBinding(fed.ID))
	assert.ErrorIs(t, err, store.ErrNonMembershipRoleRefused)

	rb := env.visitorBinding(fed.ID)
	rb.RoleDefinitionID = env.member.ID
	_, err = env.cs.CreateRoleBinding(ctx, rb)
	assert.ErrorIs(t, err, store.ErrFederatedAccountRefused)

	// A local account still receives an ordinary member binding.
	rb = env.visitorBinding(human.ID)
	rb.RoleDefinitionID = env.member.ID
	_, err = env.cs.CreateRoleBinding(ctx, rb)
	require.NoError(t, err)
}

// The visitor path joins the caller's transaction: a binding created in a
// transaction that then fails is rolled back.
func TestCreateVisitorRoleBinding_InsideTransaction(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	fed := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)

	errAbort := errors.New("abort")
	err := env.cs.WithTx(ctx, func(tx store.Store) error {
		if _, err := tx.CreateVisitorRoleBinding(ctx, env.visitorBinding(fed.ID)); err != nil {
			return err
		}
		return errAbort
	})
	require.ErrorIs(t, err, errAbort)
	bindings, err := env.cs.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, fed.ID)
	require.NoError(t, err)
	assert.Empty(t, bindings, "the binding was rolled back with the transaction")

	require.NoError(t, env.cs.WithTx(ctx, func(tx store.Store) error {
		_, err := tx.CreateVisitorRoleBinding(ctx, env.visitorBinding(fed.ID))
		return err
	}))
	bindings, err = env.cs.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, fed.ID)
	require.NoError(t, err)
	assert.Len(t, bindings, 1)
}

// A federated account holding only a project-visitor binding is not a
// member in the store's project membership reads; a member binding still
// counts.
func TestProjectMembershipReads_IgnoreVisitorBinding(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	fed := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	_, err := env.cs.CreateVisitorRoleBinding(ctx, env.visitorBinding(fed.ID))
	require.NoError(t, err)

	_, err = env.cs.GetProjectMembership(ctx, env.projectID, fed.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	ok, err := env.cs.IsProjectMember(ctx, env.projectID, fed.ID)
	require.NoError(t, err)
	assert.False(t, ok)
	members, err := env.cs.ListProjectMembers(ctx, env.projectID)
	require.NoError(t, err)
	assert.Empty(t, members)

	human := env.humanUser(t)
	rb := env.visitorBinding(human.ID)
	rb.RoleDefinitionID = env.member.ID
	_, err = env.cs.CreateRoleBinding(ctx, rb)
	require.NoError(t, err)

	m, err := env.cs.GetProjectMembership(ctx, env.projectID, human.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleMember, m.Role)
	ok, err = env.cs.IsProjectMember(ctx, env.projectID, human.ID)
	require.NoError(t, err)
	assert.True(t, ok)
	members, err = env.cs.ListProjectMembers(ctx, env.projectID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, human.ID, members[0].UserID)
}
