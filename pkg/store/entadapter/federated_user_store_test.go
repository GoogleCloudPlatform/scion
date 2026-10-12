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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Federated accounts (ptone/scion#4603), store half.

const testFedIssuer = "https://hub-a.example.com"

func newFederatedUser(issuer, email, status string) *store.User {
	return &store.User{
		DisplayName:    "Visitor",
		Kind:           store.UserKindFederated,
		Status:         status,
		HomeIssuer:     &issuer,
		FederatedEmail: &email,
	}
}

func createFederatedUser(t *testing.T, cs *CompositeStore, issuer, email, status string) *store.User {
	t.Helper()
	u := newFederatedUser(issuer, email, status)
	require.NoError(t, cs.CreateFederatedUser(context.Background(), u))
	return u
}

func TestCreateFederatedUser_WritesSyntheticEmailAndFields(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	u := createFederatedUser(t, cs, testFedIssuer+"/", " Alice@Corp.COM ", store.UserStatusPending)

	require.NotEmpty(t, u.ID)
	assert.Equal(t, "fed+"+u.ID+"@hub-a.example.com.invalid", u.Email)
	assert.True(t, store.IsFederatedSyntheticEmail(u.Email))

	got, err := cs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserKindFederated, got.Kind)
	assert.True(t, got.IsFederated())
	assert.Equal(t, store.UserStatusPending, got.Status)
	assert.Equal(t, store.UserRoleViewer, got.Role)
	assert.Equal(t, u.Email, got.Email)
	require.NotNil(t, got.HomeIssuer)
	assert.Equal(t, testFedIssuer, *got.HomeIssuer, "issuer stored without the trailing slash")
	require.NotNil(t, got.FederatedEmail)
	assert.Equal(t, "alice@corp.com", *got.FederatedEmail, "email stored trimmed and lower-cased")

	active := createFederatedUser(t, cs, testFedIssuer, "bob@corp.com", store.UserStatusActive)
	got, err = cs.GetUser(ctx, active.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, got.Status)
}

func TestCreateFederatedUser_RefusesInvalidInput(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	note := "note"
	for name, mutate := range map[string]func(u *store.User){
		"human kind":       func(u *store.User) { u.Kind = store.UserKindHuman },
		"empty kind":       func(u *store.User) { u.Kind = "" },
		"invited status":   func(u *store.User) { u.Status = store.UserStatusInvited },
		"suspended status": func(u *store.User) { u.Status = store.UserStatusSuspended },
		"empty status":     func(u *store.User) { u.Status = "" },
		"member role":      func(u *store.User) { u.Role = store.UserRoleMember },
		"caller email":     func(u *store.User) { u.Email = "alice@corp.com" },
		"no issuer":        func(u *store.User) { u.HomeIssuer = nil },
		"hostless issuer":  func(u *store.User) { s := "not a url"; u.HomeIssuer = &s },
		"no email":         func(u *store.User) { u.FederatedEmail = nil },
		"email without @":  func(u *store.User) { s := "alice"; u.FederatedEmail = &s },
		"expiry":           func(u *store.User) { u.ExpiresAt = &exp },
		"invite note":      func(u *store.User) { u.InviteNote = &note },
		"bad id":           func(u *store.User) { u.ID = "not-a-uuid" },
	} {
		t.Run(name, func(t *testing.T) {
			u := newFederatedUser(testFedIssuer, "alice@corp.com", store.UserStatusPending)
			mutate(u)
			err := cs.CreateFederatedUser(ctx, u)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}
}

func TestCreateFederatedUser_UniquePerIssuerAndEmail(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)

	dup := newFederatedUser(testFedIssuer+"/", "ALICE@corp.com", store.UserStatusActive)
	assert.ErrorIs(t, cs.CreateFederatedUser(ctx, dup), store.ErrAlreadyExists,
		"a second account for the same normalized (issuer, email) is refused")

	// The same email from another issuer is a different account.
	createFederatedUser(t, cs, "https://hub-b.example.com", "alice@corp.com", store.UserStatusPending)

	// A local human with the same real email coexists: the federated rows
	// hold synthetic emails.
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: uuid.NewString(), Email: "alice@corp.com", DisplayName: "Alice", Status: store.UserStatusActive,
	}))
}

func TestGetFederatedUserByEmail(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	u := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)

	got, err := cs.GetFederatedUserByEmail(ctx, testFedIssuer+"/", " Alice@Corp.com")
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)

	_, err = cs.GetFederatedUserByEmail(ctx, "https://hub-b.example.com", "alice@corp.com")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetFederatedUserByEmail(ctx, testFedIssuer, "bob@corp.com")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// A human row with that email is never returned by this lookup.
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: uuid.NewString(), Email: "carol@corp.com", DisplayName: "Carol", Status: store.UserStatusActive,
	}))
	_, err = cs.GetFederatedUserByEmail(ctx, testFedIssuer, "carol@corp.com")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestLinkFederatedIdentity_LinksAndResolvesBySubject(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	u := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)

	binding, err := cs.LinkFederatedIdentity(ctx, u.ID, testFedIssuer+"/", "sub-alice", "Alice@Corp.com")
	require.NoError(t, err)
	assert.Equal(t, store.FederationIdentityProvider, binding.Provider)
	assert.Equal(t, testFedIssuer, binding.Issuer)
	assert.Equal(t, "sub-alice", binding.Subject)
	assert.Equal(t, u.ID, binding.UserID)
	assert.Equal(t, "alice@corp.com", binding.Email)

	got, err := cs.GetFederatedUserBySubject(ctx, testFedIssuer+"/", "sub-alice")
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)
	assert.Equal(t, store.UserStatusPending, got.Status, "linking does not change the status")

	_, err = cs.GetFederatedUserBySubject(ctx, testFedIssuer, "sub-other")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetFederatedUserBySubject(ctx, "https://hub-b.example.com", "sub-alice")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestLinkFederatedIdentity_Refusals(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	alice := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)
	bob := createFederatedUser(t, cs, testFedIssuer, "bob@corp.com", store.UserStatusPending)
	human := &store.User{ID: uuid.NewString(), Email: "dave@corp.com", DisplayName: "Dave", Status: store.UserStatusActive}
	require.NoError(t, cs.CreateUser(ctx, human))

	_, err := cs.LinkFederatedIdentity(ctx, human.ID, testFedIssuer, "sub-dave", "dave@corp.com")
	assert.ErrorIs(t, err, store.ErrInvalidInput, "a human row is never linked")

	_, err = cs.LinkFederatedIdentity(ctx, alice.ID, "https://hub-b.example.com", "sub-alice", "alice@corp.com")
	assert.ErrorIs(t, err, store.ErrInvalidInput, "the issuer must be the account's home issuer")

	_, err = cs.LinkFederatedIdentity(ctx, alice.ID, testFedIssuer, "", "alice@corp.com")
	assert.ErrorIs(t, err, store.ErrInvalidInput, "a subject is required")

	_, err = cs.LinkFederatedIdentity(ctx, alice.ID, testFedIssuer, "sub-alice", "alice@corp.com")
	require.NoError(t, err)

	_, err = cs.LinkFederatedIdentity(ctx, alice.ID, testFedIssuer, "sub-alice-2", "alice@corp.com")
	assert.ErrorIs(t, err, store.ErrInvalidInput, "an account is linked at most once")

	_, err = cs.LinkFederatedIdentity(ctx, bob.ID, testFedIssuer, "sub-alice", "bob@corp.com")
	assert.ErrorIs(t, err, store.ErrAlreadyExists, "a pair is linked to at most one account")

	_, err = cs.LinkFederatedIdentity(ctx, uuid.NewString(), testFedIssuer, "sub-x", "x@corp.com")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// An account holds at most one federation binding even when the read in
// LinkFederatedIdentity is skipped, as when two links of one account run
// at the same time: the database refuses the second insert. Other
// providers may still bind several subjects to one user.
func TestLinkFederatedIdentity_OneFederationBindingPerAccountInDatabase(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	alice := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusPending)
	_, err := cs.LinkFederatedIdentity(ctx, alice.ID, testFedIssuer, "sub-alice", "alice@corp.com")
	require.NoError(t, err)

	second := func() *store.ExternalIdentityBinding {
		return &store.ExternalIdentityBinding{
			Provider: store.FederationIdentityProvider, Issuer: testFedIssuer, Subject: "sub-alice-2", UserID: alice.ID,
		}
	}
	err = cs.CreateExternalIdentity(ctx, second())
	require.Error(t, err, "a second federation binding for one account is refused by the database")
	assert.True(t, isUniqueViolation(err), "want a unique violation, got %v", err)
	assert.ErrorIs(t, cs.insertFederationBinding(ctx, second()), store.ErrAlreadyExists)

	got, err := cs.GetFederatedUserBySubject(ctx, testFedIssuer, "sub-alice")
	require.NoError(t, err)
	assert.Equal(t, alice.ID, got.ID)
	_, err = cs.GetFederatedUserBySubject(ctx, testFedIssuer, "sub-alice-2")
	assert.ErrorIs(t, err, store.ErrNotFound)

	human := &store.User{ID: uuid.NewString(), Email: "frank@corp.com", DisplayName: "Frank", Status: store.UserStatusActive}
	require.NoError(t, cs.CreateUser(ctx, human))
	for _, sub := range []string{"sub-frank-1", "sub-frank-2"} {
		require.NoError(t, cs.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
			Provider: "google", Issuer: "https://accounts.google.com", Subject: sub, UserID: human.ID,
		}), "the one-binding rule applies only to the federation provider")
	}
}

// A binding under the federation provider that points at a non-federated
// row (no store path writes one; this inserts it directly) never resolves.
func TestGetFederatedUserBySubject_IgnoresNonFederatedRow(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	human := &store.User{ID: uuid.NewString(), Email: "erin@corp.com", DisplayName: "Erin", Status: store.UserStatusActive}
	require.NoError(t, cs.CreateUser(ctx, human))
	require.NoError(t, cs.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		Provider: store.FederationIdentityProvider, Issuer: testFedIssuer, Subject: "sub-erin", UserID: human.ID,
	}))

	_, err := cs.GetFederatedUserBySubject(ctx, testFedIssuer, "sub-erin")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// Email-keyed lookups and the general user list never return a federated
// account, neither by its synthetic address nor by its real one.
func TestEmailKeyedLookups_NeverReturnFederatedRows(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	fed := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	human := &store.User{ID: uuid.NewString(), Email: "frank@corp.com", DisplayName: "Frank", Status: store.UserStatusActive}
	require.NoError(t, cs.CreateUser(ctx, human))

	for _, email := range []string{fed.Email, strings.ToUpper(fed.Email), "alice@corp.com"} {
		_, err := cs.GetUserByEmail(ctx, email)
		assert.ErrorIs(t, err, store.ErrNotFound, "GetUserByEmail(%q)", email)

		ok, err := cs.IsUserInvitedOrActive(ctx, email)
		require.NoError(t, err)
		assert.False(t, ok, "IsUserInvitedOrActive(%q)", email)
	}

	list, err := cs.ListUsers(ctx, store.UserFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, list.TotalCount)
	require.Len(t, list.Items, 1)
	assert.Equal(t, human.ID, list.Items[0].ID)

	list, err = cs.ListUsers(ctx, store.UserFilter{Search: "fed+"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)

	domains, err := cs.ListEmailDomains(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"corp.com"}, domains)
}

// The general CreateUser refuses the federated kind, its fields, the
// pending status and any address of the synthetic shape.
func TestCreateUser_RefusesFederatedShapes(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	issuer := testFedIssuer
	email := "alice@corp.com"
	for name, u := range map[string]*store.User{
		"federated kind":  {Kind: store.UserKindFederated, Email: "a@corp.com"},
		"home issuer":     {Email: "b@corp.com", HomeIssuer: &issuer},
		"federated email": {Email: "c@corp.com", FederatedEmail: &email},
		"pending status":  {Email: "d@corp.com", Status: store.UserStatusPending},
		"synthetic email": {Email: "fed+" + uuid.NewString() + "@hub-a.example.com.invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			u.ID = uuid.NewString()
			u.DisplayName = "x"
			assert.ErrorIs(t, cs.CreateUser(ctx, u), store.ErrFederatedAccountRefused)
		})
	}
}

func TestUpdateUser_FederatedRowKeepsIdentity(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	fed := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)

	got, err := cs.GetUser(ctx, fed.ID)
	require.NoError(t, err)
	got.Status = store.UserStatusSuspended
	require.NoError(t, cs.UpdateUser(ctx, got), "an admin may suspend a federated account")

	for name, mutate := range map[string]func(u *store.User){
		"email change":   func(u *store.User) { u.Email = "alice@corp.com" },
		"invited status": func(u *store.User) { u.Status = store.UserStatusInvited },
		"member role":    func(u *store.User) { u.Role = store.UserRoleMember },
		"invited by":     func(u *store.User) { by := uuid.NewString(); u.InvitedBy = &by },
		"invite note":    func(u *store.User) { note := "welcome"; u.InviteNote = &note },
	} {
		t.Run(name, func(t *testing.T) {
			u, err := cs.GetUser(ctx, fed.ID)
			require.NoError(t, err)
			mutate(u)
			assert.ErrorIs(t, cs.UpdateUser(ctx, u), store.ErrFederatedAccountRefused)
		})
	}

	human := &store.User{ID: uuid.NewString(), Email: "gina@corp.com", DisplayName: "Gina", Status: store.UserStatusActive}
	require.NoError(t, cs.CreateUser(ctx, human))
	for name, mutate := range map[string]func(u *store.User){
		"synthetic email": func(u *store.User) { u.Email = "fed+" + uuid.NewString() + "@x.invalid" },
		"pending status":  func(u *store.User) { u.Status = store.UserStatusPending },
	} {
		t.Run("human "+name, func(t *testing.T) {
			u, err := cs.GetUser(ctx, human.ID)
			require.NoError(t, err)
			mutate(u)
			assert.ErrorIs(t, cs.UpdateUser(ctx, u), store.ErrFederatedAccountRefused)
		})
	}
}

// The general role binding, group membership and user access credential paths refuse
// a federated account.
func TestGeneralGrantPaths_RefuseFederatedAccount(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	fed := createFederatedUser(t, cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)

	rd, err := cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID: uuid.NewString(), Name: "fed-test-" + uuid.NewString()[:8], Description: "test",
		ScopeType: store.RoleScopeSystem, Permissions: []string{"user.read"},
	})
	require.NoError(t, err)
	_, err = cs.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      fed.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	assert.ErrorIs(t, err, store.ErrFederatedAccountRefused)

	groupID := uuid.NewString()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{ID: groupID, Name: "g-" + groupID, Slug: "g-" + groupID}))
	err = cs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   fed.ID,
		Role:       store.GroupMemberRoleMember,
	})
	assert.ErrorIs(t, err, store.ErrFederatedAccountRefused)

	err = cs.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID:           uuid.NewString(),
		UserID:       fed.ID,
		Name:         "fed-token",
		Prefix:       "scion_pat_fed",
		KeyHash:      "fed-hash",
		BoundaryKind: "hub",
		Scopes:       []string{"agent:list"},
	})
	assert.ErrorIs(t, err, store.ErrFederatedAccountRefused)
}

func TestFederatedSyntheticEmailHelpers(t *testing.T) {
	id := uuid.NewString()
	got, err := store.FederatedSyntheticEmail(id, "https://Hub-A.example.com:8443/path/")
	require.NoError(t, err)
	assert.Equal(t, "fed+"+id+"@hub-a.example.com.invalid", got)

	_, err = store.FederatedSyntheticEmail("", testFedIssuer)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = store.FederatedSyntheticEmail(id, "")
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	assert.True(t, store.IsFederatedSyntheticEmail(" FED+x@host.INVALID "))
	assert.False(t, store.IsFederatedSyntheticEmail("fed+x@host.com"))
	assert.False(t, store.IsFederatedSyntheticEmail("alice@host.invalid"))
	assert.False(t, store.IsFederatedSyntheticEmail("no-at-sign"))
}
