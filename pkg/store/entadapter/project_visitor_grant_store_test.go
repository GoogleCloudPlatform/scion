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

// Project visitor grants by email and first contact (ptone/scion#4603),
// store half.

func TestListVisitorRoleBindings_OnlyVisitorBindingsOfTheProject(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	alice := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)
	bob := createFederatedUser(t, env.cs, testFedIssuer, "bob@corp.com", store.UserStatusActive)

	first, err := env.cs.CreateVisitorRoleBinding(ctx, env.visitorBinding(alice.ID))
	require.NoError(t, err)
	second, err := env.cs.CreateVisitorRoleBinding(ctx, env.visitorBinding(bob.ID))
	require.NoError(t, err)

	// A member binding on the same project and a visitor binding on
	// another project are not listed.
	human := env.humanUser(t)
	_, err = env.cs.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.member.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: human.ID,
		ScopeType: store.RoleScopeProject, ScopeID: env.projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	other := env.visitorBinding(alice.ID)
	other.ScopeID = uuid.NewString()
	_, err = env.cs.CreateVisitorRoleBinding(ctx, other)
	require.NoError(t, err)

	got, err := env.cs.ListVisitorRoleBindings(ctx, env.projectID)
	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, rb := range got {
		ids = append(ids, rb.ID)
	}
	assert.ElementsMatch(t, []string{first.ID, second.ID}, ids)

	empty, err := env.cs.ListVisitorRoleBindings(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Empty(t, empty)

	_, err = env.cs.ListVisitorRoleBindings(ctx, "")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestUpdateVisitorRoleBindingWindow(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	alice := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	rb, err := env.cs.CreateVisitorRoleBinding(ctx, env.visitorBinding(alice.ID))
	require.NoError(t, err)

	notBefore := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := notBefore.Add(48 * time.Hour)
	updated, err := env.cs.UpdateVisitorRoleBindingWindow(ctx, rb.ID, &notBefore, &expiresAt)
	require.NoError(t, err)
	require.NotNil(t, updated.NotBefore)
	require.NotNil(t, updated.ExpiresAt)
	assert.True(t, updated.NotBefore.Equal(notBefore))
	assert.True(t, updated.ExpiresAt.Equal(expiresAt))

	got, err := env.cs.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, got.ExpiresAt.Equal(expiresAt))

	cleared, err := env.cs.UpdateVisitorRoleBindingWindow(ctx, rb.ID, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, cleared.NotBefore)
	assert.Nil(t, cleared.ExpiresAt)

	// A member binding is not a visitor grant.
	human := env.humanUser(t)
	member, err := env.cs.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.member.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: human.ID,
		ScopeType: store.RoleScopeProject, ScopeID: env.projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = env.cs.UpdateVisitorRoleBindingWindow(ctx, member.ID, nil, &expiresAt)
	assert.ErrorIs(t, err, store.ErrVisitorBindingRefused)
	unchanged, err := env.cs.GetRoleBinding(ctx, member.ID)
	require.NoError(t, err)
	assert.Nil(t, unchanged.ExpiresAt, "a refused update changes nothing")

	_, err = env.cs.UpdateVisitorRoleBindingWindow(ctx, uuid.NewString(), nil, nil)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestLinkPendingFederatedUser_LinksAndActivates(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	u := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)

	got, err := cs.LinkPendingFederatedUser(ctx, u.ID, testFedIssuer+"/", "sub-alice", " Alice@Corp.com")
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)
	assert.Equal(t, store.UserStatusActive, got.Status)

	bySubject, err := cs.GetFederatedUserBySubject(ctx, testFedIssuer, "sub-alice")
	require.NoError(t, err)
	assert.Equal(t, u.ID, bySubject.ID)
	assert.Equal(t, store.UserStatusActive, bySubject.Status)

	binding, err := cs.GetExternalIdentity(ctx, store.FederationIdentityProvider, testFedIssuer, "sub-alice")
	require.NoError(t, err)
	assert.Equal(t, "alice@corp.com", binding.Email)

	// A second first contact for the same account, with the same or
	// another subject, reports that another one got there first.
	_, err = cs.LinkPendingFederatedUser(ctx, u.ID, testFedIssuer, "sub-alice", "alice@corp.com")
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
	_, err = cs.LinkPendingFederatedUser(ctx, u.ID, testFedIssuer, "sub-alice-2", "alice@corp.com")
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
	_, err = cs.GetFederatedUserBySubject(ctx, testFedIssuer, "sub-alice-2")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestLinkPendingFederatedUser_Refusals(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	pending := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)
	active := createFederatedUser(t, cs, testFedIssuer, "bob@corp.com", store.UserStatusActive)
	other := createFederatedUser(t, cs, testFedIssuer, "carol@corp.com", store.UserStatusPending)
	human := &store.User{ID: uuid.NewString(), Email: "dave@corp.com", DisplayName: "Dave", Status: store.UserStatusActive}
	require.NoError(t, cs.CreateUser(ctx, human))
	_, err := cs.LinkFederatedIdentity(ctx, other.ID, testFedIssuer, "sub-taken", "carol@corp.com")
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		userID, issuer, subject, email string
		want                           error
	}{
		"human account":       {human.ID, testFedIssuer, "sub-dave", "dave@corp.com", store.ErrInvalidInput},
		"other issuer":        {pending.ID, "https://hub-b.example.com", "sub-alice", "alice@corp.com", store.ErrInvalidInput},
		"other email":         {pending.ID, testFedIssuer, "sub-alice", "mallory@corp.com", store.ErrInvalidInput},
		"no subject":          {pending.ID, testFedIssuer, " ", "alice@corp.com", store.ErrInvalidInput},
		"no email":            {pending.ID, testFedIssuer, "sub-alice", "", store.ErrInvalidInput},
		"active not pending":  {active.ID, testFedIssuer, "sub-bob", "bob@corp.com", store.ErrInvalidInput},
		"pair already linked": {pending.ID, testFedIssuer, "sub-taken", "alice@corp.com", store.ErrAlreadyExists},
		"missing account":     {uuid.NewString(), testFedIssuer, "sub-x", "x@corp.com", store.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cs.LinkPendingFederatedUser(ctx, tc.userID, tc.issuer, tc.subject, tc.email)
			assert.ErrorIs(t, err, tc.want)
		})
	}

	// Nothing above linked or activated the pending account.
	got, err := cs.GetUser(ctx, pending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusPending, got.Status)
	bindings, err := cs.GetExternalIdentitiesByUserID(ctx, pending.ID)
	require.NoError(t, err)
	assert.Empty(t, bindings)
}
